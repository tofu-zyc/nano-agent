package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/proto"
)

func registerJobFixture(t *testing.T, e *env, id string) proto.JobSnapshot {
	t.Helper()
	origin := proto.LeaseSpec{SID: "job-source", RunID: "source-run", TurnID: 1, CallID: "call-" + id,
		Agent: "default", CreatedBy: "fake:u1", Name: "Shell"}
	lease := origin
	lease.LeaseID = "lboot-" + id
	lease.JobID = "j_boot-" + id
	lease.HostID = "host-" + id
	lease.Exclusive = "dir:/work"
	lease.State = "queued"
	e.a.mu.Lock()
	n := e.a.nodes["test"]
	n.info.NodeEpoch = "boot"
	n.info.Features = map[string]int{"job_manager": 1, "job_stream": 1, "machine_leases": 1, "lease_detach": 1, "max_execs": 8, "max_parallel_shells": 2}
	e.a.hostWaits[lease.HostID] = hostWait{node: "test", tool: "Shell", origin: &origin, ch: make(chan proto.Msg, 1)}
	e.a.mu.Unlock()
	return proto.JobSnapshot{Version: 1, JobID: lease.JobID, NodeEpoch: "boot", OwnerAgent: "default", CreatedBy: "fake:u1",
		OriginSID: origin.SID, OriginRunID: origin.RunID, OriginCallID: origin.CallID, Origin: origin, Lease: &lease,
		Status: "queued", Execution: "auto", Revision: 1, CreatedAt: 100, TimeoutSeconds: 1800}
}
func acquireJob(q proto.JobSnapshot) proto.Msg {
	return proto.Msg{T: proto.TLeaseAcquire, NodeEpoch: "boot", SID: q.OriginSID, RunID: q.OriginRunID,
		TurnID: q.Origin.TurnID, ID: q.OriginCallID, LeaseID: q.Lease.LeaseID, Lease: q.Lease}
}
func TestJobIndexPrecedesLeaseAndGatewayAcknowledgesOnlyDurableTerminal(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "1")
	// Hold the reliable worker outside App.mu and after one durable callback.
	// Both following packets can arrive while admission still awaits disk.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	event := eventlog.Event{SourceID: "gate", SourceEpoch: "boot", Seq: 1, Name: "device.online", AgentID: "default", OccurredAt: 100, ObservedAt: 101}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	if err := e.a.jobEvents.Submit(eventlog.Transaction{Event: &event}, func(err error) {
		if err != nil {
			t.Error(err)
		}
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "created", Job: &q})
	e.k.send(acquireJob(q))
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.a.mu.Lock()
		p := e.a.jobPersist[jobPersistKey("test", q.JobID)]
		waiting := p != nil && p.lease != nil
		e.a.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not wait for durable Job reservation")
		}
		time.Sleep(time.Millisecond)
	}
	cp, _ := e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 0 {
		t.Fatal("execution authorized before durable index", cp)
	}
	once.Do(func() { close(release) })
	grant := e.k.expect(proto.TLeaseGrant)
	if grant.LeaseID != q.Lease.LeaseID {
		t.Fatal(grant)
	}
	saved, ok := e.a.jobEvents.Job("test", q.JobID)
	if !ok || !saved.Reserved || saved.Creator != "fake:u1" {
		t.Fatal(saved, ok)
	}
	cp, _ = e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 1 || cp.Leases[0].Creator != "fake:u1" {
		t.Fatal("creator lost at scheduler", cp)
	}
	q.Status = "succeeded"
	q.Revision++
	q.EndedAt = 200
	q.LastEventSeq = 1
	q.Result = &proto.JobResult{OK: true, Content: "done"}
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "terminal", EventSeq: 1, Job: &q})
	ack := e.k.expect(proto.TEventsAck)
	if ack.JobID != q.JobID || ack.EventSeq != 1 {
		t.Fatal(ack)
	}
	saved, ok = e.a.jobEvents.Job("test", q.JobID)
	fact, exists := e.a.jobEvents.Event(saved.TerminalEventID)
	if !ok || !exists || saved.Reserved || fact.Name != "job.completed" || fact.JobID != q.JobID {
		t.Fatal(saved, fact)
	}
	// Retransmit the same source event. The ACK can repeat, never the fact.
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "terminal", EventSeq: 1, Job: &q})
	e.k.expect(proto.TEventsAck)
	if e.a.jobEvents.Stats().Events != 2 {
		t.Fatal("terminal duplicate", e.a.jobEvents.Stats())
	}
}
func TestJobAdmissionCapacityFailsBeforeExecutionLease(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "2")
	e.a.jobEvents.Close()
	small, err := eventlog.Open(e.a.ctx, filepath.Join(e.dir, "small", "events.jsonl"), eventlog.Limits{Jobs: 1, Events: 1, LiveBytes: 20 << 10})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.jobEvents = small
	e.a.mu.Unlock()
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "created", Job: &q})
	e.k.send(acquireJob(q))
	deny := e.k.expect(proto.TLeaseDeny)
	if deny.LeaseID != q.Lease.LeaseID {
		t.Fatal(deny)
	}
	if _, ok := small.Job("test", q.JobID); ok {
		t.Fatal("uncommitted Job published")
	}
	cp, _ := e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 0 {
		t.Fatal("failed Job acquired execution", cp)
	}
	if small.Stats().Reserved != 0 {
		t.Fatal("failed admission leaked outbox reservation")
	}
}
func TestJobCannotChangeCreatorOrImportWithoutOriginAuthority(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "3")
	q.CreatedBy = "fake:guest"
	q.Origin.CreatedBy = "fake:guest"
	q.Lease.CreatedBy = "fake:guest"
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "created", Job: &q})
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.a.mu.Lock()
		issues := e.a.store.StorageIssues()
		e.a.mu.Unlock()
		if len(issues) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("invalid Job was not diagnosed")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := e.a.jobEvents.Job("test", q.JobID); ok {
		t.Fatal("node's claimed creator granted authority")
	}
	e.k.send(acquireJob(q))
	e.k.expect(proto.TLeaseDeny)
	// A copied terminal under a new job_id also cannot revive expired origin.
	q = registerJobFixture(t, e, "4")
	e.a.mu.Lock()
	delete(e.a.hostWaits, q.Lease.HostID)
	e.a.mu.Unlock()
	q.Status = "succeeded"
	q.Revision = 2
	q.EndedAt = 200
	q.LastEventSeq = 1
	e.a.mu.Lock()
	_, err := e.a.authorizeNewJob("test", e.a.nodes["test"], q)
	e.a.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "失效") {
		t.Fatal("expired source imported", err)
	}
}
func TestJobLeaseIncludesFullFrozenSourceAndResourceDomain(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "5")
	record := eventlog.JobRecord{Job: eventlog.Job{SourceID: "test", Creator: "fake:u1", Receipt: q}, Reserved: true}
	e.a.mu.Lock()
	req, err := e.a.leaseRequest("test", e.a.nodes["test"], ptr(acquireJob(q)), false)
	e.a.mu.Unlock()
	if err != nil || !sameJobLease(record, req) {
		t.Fatal(req, err)
	}
	changes := []func(*dispatch.Request){
		func(r *dispatch.Request) { r.Creator = "other" },
		func(r *dispatch.Request) { r.Silent = true },
		func(r *dispatch.Request) { r.Shared = []string{"different:domain"} },
		func(r *dispatch.Request) { r.Exclusive = []string{"dir:/elsewhere"} },
		func(r *dispatch.Request) { r.HostID = "other" },
		func(r *dispatch.Request) { r.Agent = "other" },
		func(r *dispatch.Request) { r.Priority = 10 },
		func(r *dispatch.Request) { r.TurnID++ },
	}
	for i, change := range changes {
		next := req
		change(&next)
		if sameJobLease(record, next) {
			t.Fatalf("frozen identity change %d allowed", i)
		}
	}
}
func ptr[T any](v T) *T { return &v }
func TestJobIntakeStorageShutdownHasNoSuccessCallback(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "6")
	e.a.jobEvents.Close()
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "created", Job: &q})
	e.k.send(acquireJob(q))
	deny := e.k.expect(proto.TLeaseDeny)
	if deny.OK != nil && *deny.OK {
		t.Fatal(deny)
	}
	if _, ok := e.a.jobEvents.Job("test", q.JobID); ok {
		t.Fatal("closed storage activated source")
	}
}
