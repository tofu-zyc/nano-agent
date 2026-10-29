package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
)

func openTest(t *testing.T, path string, limits Limits) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func commit(t *testing.T, s *Store, tx Transaction) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
}
func fixture(id int) Job {
	jobID := fmt.Sprintf("j_boot-%d", id)
	lease := proto.LeaseSpec{LeaseID: fmt.Sprintf("lboot-%d", id), JobID: jobID, Agent: "agent", CreatedBy: "fake:user", SID: "chat", RunID: "run", TurnID: 3, CallID: fmt.Sprint(id), Name: "Shell", Exclusive: "files:/work"}
	return Job{SourceID: "node", Creator: "fake:user", Receipt: proto.JobSnapshot{
		Version: 1, JobID: jobID, NodeEpoch: "boot", OwnerAgent: "agent", CreatedBy: "fake:user",
		OriginSID: "chat", OriginRunID: "run", OriginCallID: fmt.Sprint(id),
		Origin: lease, Lease: &lease, Status: "queued", Execution: "auto", Revision: 1, CreatedAt: 100, TimeoutSeconds: 1800}}
}
func completed(j Job) (Job, Event) {
	j.Receipt.Status = "succeeded"
	j.Receipt.Revision++
	j.Receipt.EndedAt = 200
	j.Receipt.LastEventSeq = 1
	j.Receipt.Result = &proto.JobResult{OK: true, Content: "actual result"}
	e := Event{Name: "job.completed", SourceID: j.SourceID + "/" + j.Receipt.JobID, SourceEpoch: "boot", Seq: 1,
		OccurredAt: 200, ObservedAt: 201, AgentID: "agent", RunID: "run", JobID: j.Receipt.JobID, DataRef: j.Receipt.JobID}
	e.ID = EventID(e.SourceID, e.SourceEpoch, e.Seq)
	return j, e
}
func notice(seq uint64) Event {
	e := Event{Name: "device.online", SourceID: "device", SourceEpoch: "boot", Seq: seq, OccurredAt: 100, ObservedAt: 101, AgentID: "agent"}
	e.ID = EventID(e.SourceID, e.SourceEpoch, e.Seq)
	return e
}
func delivery(e Event, hook string) Delivery {
	return Delivery{ID: DeliveryID(e.ID, hook, 1), EventID: e.ID, HookID: hook, Revision: 1,
		AgentID: "agent", Creator: "fake:user", Rule: json.RawMessage(`{"handler":{"type":"notify","text":"done"}}`), Status: "pending"}
}
func TestJobTerminalReservationAndAtomicRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	s := openTest(t, path, Limits{Jobs: 1, Events: 1, LiveBytes: 40 << 10})
	j := fixture(1)
	if err := s.Commit(context.Background(), Transaction{Job: &j}); err == nil {
		t.Fatal("observation admitted untrusted Job")
	}
	commit(t, s, Transaction{Admit: true, Job: &j})
	stats := s.Stats()
	if stats.Reserved != 1 || stats.ReservedBytes == 0 || stats.Events != 0 {
		t.Fatal(stats)
	}
	e := notice(1)
	if err := s.Commit(context.Background(), Transaction{Event: &e}); !errors.Is(err, ErrCapacity) {
		t.Fatal("event consumed terminal slot", err)
	}
	j, e = completed(j)
	j.Receipt.Result.Content = strings.Repeat("x", 28<<10)
	if err := s.Commit(context.Background(), Transaction{Job: &j}); err == nil {
		t.Fatal("terminal saved without event")
	}
	commit(t, s, Transaction{Job: &j, Event: &e})
	if stats = s.Stats(); stats.Reserved != 0 || stats.ReservedBytes != 0 || stats.Events != 1 {
		t.Fatal(stats)
	}
	s.Close()
	recovered := openTest(t, path, Limits{Jobs: 1, Events: 1, LiveBytes: 40 << 10})
	got, ok := recovered.Job("node", j.Receipt.JobID)
	fact, exists := recovered.Event(e.ID)
	if !ok || !exists || got.TerminalEventID != fact.ID || got.Receipt.Status != "succeeded" {
		t.Fatal(got, fact)
	}
	// Caller-owned mutable buffers must not mutate a committed fact.
	got.Receipt.Result.Content = "changed"
	again, _ := recovered.Job("node", j.Receipt.JobID)
	if again.Receipt.Result.Content == "changed" {
		t.Fatal("aliased index")
	}
	e.ObservedAt++
	commit(t, recovered, Transaction{Job: &j, Event: &e})
	if recovered.Stats().Events != 1 {
		t.Fatal("duplicate terminal")
	}
}
func TestByteReservationSurvivesOtherEvents(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{Jobs: 1, Events: 100, LiveBytes: 40 << 10})
	j := fixture(1)
	commit(t, s, Transaction{Admit: true, Job: &j})
	count := 0
	for seq := uint64(1); seq < 100; seq++ {
		e := notice(seq)
		e.Data = json.RawMessage(`"` + strings.Repeat("x", 1500) + `"`)
		err := s.Commit(context.Background(), Transaction{Event: &e})
		if errors.Is(err, ErrCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count == 0 || count > 5 {
		t.Fatal("unreserved byte budget", count, s.Stats())
	}
	j, e := completed(j)
	j.Receipt.Result.Content = strings.Repeat("x", 28<<10)
	commit(t, s, Transaction{Job: &j, Event: &e})
}
func TestIdentityOutcomeAndEventConflict(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{})
	j := fixture(1)
	commit(t, s, Transaction{Admit: true, Job: &j})
	changes := []func(*Job){
		func(j *Job) { j.Creator = "other"; j.Receipt.CreatedBy = "other"; j.Receipt.Origin.CreatedBy = "other" },
		func(j *Job) { j.Silent = true; j.Receipt.Origin.Silent = true },
		func(j *Job) { j.Receipt.Lease.Exclusive = "files:/elsewhere" },
		func(j *Job) { j.Receipt.OriginSID = "other"; j.Receipt.Origin.SID = "other" },
		func(j *Job) { j.Receipt.Origin.TurnID++ },
		func(j *Job) { j.Receipt.OwnerAgent = "other"; j.Receipt.Origin.Agent = "other" },
	}
	for i, change := range changes {
		next := copyValue(j)
		change(&next)
		if err := s.Commit(context.Background(), Transaction{Job: &next}); err == nil {
			t.Fatalf("identity change %d accepted", i)
		}
	}
	j, e := completed(j)
	bad := e
	bad.Name = "job.failed"
	if err := s.Commit(context.Background(), Transaction{Job: &j, Event: &bad}); err == nil {
		t.Fatal("mismatched terminal")
	}
	commit(t, s, Transaction{Job: &j, Event: &e})
	changed := copyValue(j)
	changed.Receipt.Result.Content = "invented result"
	if err := s.Commit(context.Background(), Transaction{Job: &changed, Event: &e}); err == nil {
		t.Fatal("terminal result overwritten")
	}
	active := fixture(1)
	active.Receipt.Revision = 10
	if err := s.Commit(context.Background(), Transaction{Job: &active}); err == nil {
		t.Fatal("resurrected terminal")
	}
	e.Data = json.RawMessage(`{"different":true}`)
	if err := s.Commit(context.Background(), Transaction{Job: &j, Event: &e}); err == nil {
		t.Fatal("event ID changed fact")
	}
}
func TestDurableMatchDeliveryAndPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	s := openTest(t, path, Limits{CompactBytes: 256})
	j, e := completed(fixture(1))
	commit(t, s, Transaction{Admit: true, Job: &j, Event: &e})
	d := delivery(e, "notify")
	commit(t, s, Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}})
	commit(t, s, Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}})
	if s.Stats().Deliveries != 1 {
		t.Fatal(s.Stats())
	}
	d.Rule = json.RawMessage(`{"handler":{"type":"command"}}`)
	if err := s.Commit(context.Background(), Transaction{Delivery: &d}); err == nil {
		t.Fatal("rule revision mutated")
	}
	d, _ = s.Delivery(d.ID)
	d.Status = "running"
	d.Attempt = 1
	commit(t, s, Transaction{Delivery: &d})
	if err := s.Commit(context.Background(), Transaction{Forget: []string{e.ID}}); err == nil {
		t.Fatal("pruned in-flight side effect")
	}
	s.Close()
	s = openTest(t, path, Limits{CompactBytes: 256})
	d, ok := s.Delivery(d.ID)
	if !ok || d.Status != "running" || d.Attempt != 1 {
		t.Fatal("lost uncertain external attempt", d)
	}
	d.Status = "uncertain"
	d.Error = "delivery may have happened before crash"
	commit(t, s, Transaction{Delivery: &d})
	commit(t, s, Transaction{Forget: []string{e.ID}})
	// Keep the terminal receipt while freeing handled event/Hook bodies.
	if stats := s.Stats(); stats.Events != 0 || stats.Deliveries != 0 || stats.Jobs != 1 {
		t.Fatal(stats)
	}
	for i := 0; i < 30; i++ {
		j.Receipt.Revision++
		j.Receipt.NotificationState = "delivered"
		commit(t, s, Transaction{Job: &j, Event: &e})
	}
	if s.Stats().JournalBytes > 12000 {
		t.Fatal("journal did not rotate", s.Stats())
	}
	s.Close()
	s = openTest(t, path, Limits{CompactBytes: 256})
	commit(t, s, Transaction{Job: &j, Event: &e})
	if stats := s.Stats(); stats.Events != 0 || stats.Deliveries != 0 || stats.Jobs != 1 {
		t.Fatal("terminal retried after prune", stats)
	}
	if err := Verify(path); err != nil {
		t.Fatal(err)
	}
}
func TestTornTransactionIsNeverPartiallyPublished(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	s := openTest(t, original, Limits{})
	j := fixture(1)
	commit(t, s, Transaction{Admit: true, Job: &j})
	prefix, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	j, e := completed(j)
	commit(t, s, Transaction{Job: &j, Event: &e})
	s.Close()
	full, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	tail := full[len(prefix):]
	for _, cut := range []int{0, 1, 7, len(tail) / 2, len(tail) - 2, len(tail) - 1, len(tail)} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprint(cut))
			if err := os.WriteFile(path, append(bytes.Clone(prefix), tail[:cut]...), 0600); err != nil {
				t.Fatal(err)
			}
			x := openTest(t, path, Limits{})
			got, _ := x.Job("node", j.Receipt.JobID)
			_, found := x.Event(e.ID)
			if cut < len(tail) {
				if found || got.Receipt.Status != "queued" || x.Stats().Reserved != 1 {
					t.Fatal("partial terminal visible", got, found)
				}
				info, _ := os.Stat(path)
				if info.Size() != int64(len(prefix)) {
					t.Fatal("partial tail not removed")
				}
			} else if !found || got.Receipt.Status != "succeeded" || x.Stats().Reserved != 0 {
				t.Fatal("complete terminal lost")
			}
		})
	}
}
func TestCorruptAndFutureRecordsRemainVisibleAndUntouched(t *testing.T) {
	dir := t.TempDir()
	e := notice(1)
	good, err := encode(1, patch{Events: []EventRecord{{Event: e}}})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(good, []byte("device.online"), []byte("device.offline"), 1)
	future := bytes.Replace(good, []byte(`"version":1`), []byte(`"version":2`), 1)
	for i, data := range [][]byte{corrupt, future, append(bytes.Clone(corrupt), good...), []byte("bad complete line\n")} {
		path := filepath.Join(dir, fmt.Sprint(i))
		os.WriteFile(path, data, 0600)
		if _, err := Open(context.Background(), path, Limits{}); err == nil {
			t.Fatal("corrupt journal activated", i)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(data, after) {
			t.Fatal("corrupt source overwritten", i)
		}
	}
}
func TestFailedSyncFreezesAcknowledgements(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{})
	// The same real write failure occurs if a storage device closes/fails the
	// journal handle. No callback may report a durable event afterward.
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	e := notice(1)
	if err := s.Commit(context.Background(), Transaction{Event: &e}); err == nil {
		t.Fatal("ACKed failed write")
	}
	if _, ok := s.Event(e.ID); ok {
		t.Fatal("failed event published")
	}
	e = notice(2)
	if err := s.Commit(context.Background(), Transaction{Event: &e}); err == nil {
		t.Fatal("accepted after ambiguous write")
	}
	if s.Stats().Err == nil {
		t.Fatal("storage error hidden")
	}
}
func TestBoundedQueueCallbacksAndDrain(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{Queue: 1})
	entered, release := make(chan struct{}), make(chan struct{})
	e := notice(1)
	if err := s.Submit(Transaction{Event: &e}, func(err error) {
		if err != nil {
			t.Error(err)
		}
		// Callback may read/submit, so it cannot run under the store's locks.
		if _, ok := s.Event(e.ID); !ok {
			t.Error("callback precedes publish")
		}
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	second := notice(2)
	done := make(chan error, 1)
	if err := s.Submit(Transaction{Event: &second}, func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	third := notice(3)
	if err := s.Submit(Transaction{Event: &third}, nil); !errors.Is(err, ErrBusy) {
		t.Fatal("unbounded queue", err)
	}
	s.Stop()
	if err := s.Submit(Transaction{Event: &third}, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s.Stats().Events != 2 {
		t.Fatal("accepted work lost on stop", s.Stats())
	}
}
func TestConcurrentReadersAndStableReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	s := openTest(t, path, Limits{CompactBytes: 2048})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.Stats()
				s.PendingEvents(3)
			}
		}()
	}
	for i := 1; i <= 60; i++ {
		e := notice(uint64(i))
		commit(t, s, Transaction{Event: &e})
		if i%2 == 0 {
			commit(t, s, Transaction{Match: &Match{EventID: e.ID}})
		}
	}
	wg.Wait()
	before := s.Stats()
	s.Close()
	s = openTest(t, path, Limits{CompactBytes: 2048})
	after := s.Stats()
	if after.Events != 60 || before.LiveBytes != after.LiveBytes || len(s.PendingEvents(128)) != 30 {
		t.Fatal(before, after)
	}
}
func TestCausalAndFrozenDeliveryLimits(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{})
	e := notice(1)
	e.Depth = 5
	if err := s.Commit(context.Background(), Transaction{Event: &e}); err == nil {
		t.Fatal("unbounded causal depth")
	}
	e.Depth = 4
	e.OriginHook = "self"
	commit(t, s, Transaction{Event: &e})
	d := delivery(e, "self")
	if err := s.Commit(context.Background(), Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}}); err == nil {
		t.Fatal("self-trigger loop")
	}
	d = delivery(e, "other")
	commit(t, s, Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}})
	d.Status = "running"
	if err := s.Commit(context.Background(), Transaction{Delivery: &d}); err == nil {
		t.Fatal("attempt not recorded before external action")
	}
	d.Attempt = 1
	commit(t, s, Transaction{Delivery: &d})
	d.Status = "succeeded"
	commit(t, s, Transaction{Delivery: &d})
	d.Status = "running"
	d.Attempt = 2
	if err := s.Commit(context.Background(), Transaction{Delivery: &d}); err == nil {
		t.Fatal("completed action restarted")
	}
}

func TestSourceCursorDeduplicatesAfterPruneAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	s := openTest(t, path, Limits{CompactBytes: 128})
	for _, seq := range []uint64{3, 1, 2} {
		e := notice(seq)
		commit(t, s, Transaction{Event: &e})
		commit(t, s, Transaction{Match: &Match{EventID: e.ID}})
		commit(t, s, Transaction{Forget: []string{e.ID}})
	}
	s.Close()
	s = openTest(t, path, Limits{CompactBytes: 128})
	if stats := s.Stats(); stats.Events != 0 || stats.Sources != 1 {
		t.Fatal(stats)
	}
	for _, seq := range []uint64{1, 2, 3} {
		e := notice(seq)
		e.Data = json.RawMessage(`{"old_body_cannot_be_reimported":true}`)
		commit(t, s, Transaction{Event: &e})
	}
	if s.Stats().Events != 0 {
		t.Fatal("pruned source receipts reactivated")
	}
	e := notice(4)
	commit(t, s, Transaction{Event: &e})
	if s.Stats().Events != 1 {
		t.Fatal("new event suppressed")
	}
	distant := notice(99999)
	if err := s.Commit(context.Background(), Transaction{Event: &distant}); err == nil {
		t.Fatal("unbounded source gap")
	}
}
func TestSilentEventCannotCreateAudibleDelivery(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{})
	j := fixture(1)
	j.Silent = true
	j.Receipt.Origin.Silent = true
	j, e := completed(j)
	e.Silent = true
	commit(t, s, Transaction{Admit: true, Job: &j, Event: &e})
	d := delivery(e, "notify")
	if err := s.Commit(context.Background(), Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}}); err == nil {
		t.Fatal("silent source escaped through a Hook")
	}
	d.Silent = true
	commit(t, s, Transaction{Match: &Match{EventID: e.ID, Deliveries: []Delivery{d}}})
}
func TestCompactionFailureNeverPublishesPendingCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	s := openTest(t, path, Limits{CompactBytes: 128})
	e := notice(1)
	commit(t, s, Transaction{Event: &e})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".next", 0700); err != nil {
		t.Fatal(err)
	}
	e = notice(2)
	if err := s.Commit(context.Background(), Transaction{Event: &e}); err == nil {
		t.Fatal("failed replacement acknowledged")
	}
	s.Close()
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed snapshot changed published journal")
	}
	s = openTest(t, path, Limits{CompactBytes: 128})
	if _, ok := s.Event(e.ID); ok {
		t.Fatal("unpublished event activated")
	}
	if s.Stats().Events != 1 {
		t.Fatal(s.Stats())
	}
}
func TestPendingCursorHolesStayBounded(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "journal"), Limits{Sources: 1})
	for seq := uint64(2); seq <= 256; seq += 2 {
		e := notice(seq)
		commit(t, s, Transaction{Event: &e})
	}
	e := notice(258)
	if err := s.Commit(context.Background(), Transaction{Event: &e}); !errors.Is(err, ErrCapacity) {
		t.Fatal("unbounded holes", err)
	}
	for seq := uint64(1); seq < 256; seq += 2 {
		e := notice(seq)
		commit(t, s, Transaction{Event: &e})
	}
	commit(t, s, Transaction{Event: &e})
	other := notice(1)
	other.SourceEpoch = "other"
	other.ID = EventID(other.SourceID, other.SourceEpoch, other.Seq)
	if err := s.Commit(context.Background(), Transaction{Event: &other}); !errors.Is(err, ErrCapacity) {
		t.Fatal("unbounded sources", err)
	}
}
