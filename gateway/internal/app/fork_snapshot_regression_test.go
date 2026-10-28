package app

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

func TestForkSnapshotAndHistoryOverviewStayBoundedFor100IdleBranches(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: strings.Repeat("large source ", 350000)}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var branches []string
	var snapshot string
	for i := 0; i < 100; i++ {
		res, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: fmt.Sprintf("memory-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		branches = append(branches, res.BranchSID)
		if snapshot != "" && snapshot != res.SnapshotID {
			t.Fatal("identical frozen versions did not deduplicate")
		}
		snapshot = res.SnapshotID
	}
	// Exercise the real overview and detail paths, including branches previously opened.
	for _, sid := range branches {
		e.a.History(sid)
	}
	for i := 0; i < 3; i++ {
		snap := e.a.Snapshot()
		if len(snap.Sessions) < 100 {
			t.Fatal("missing branches in overview")
		}
	}
	e.a.Maintain()
	e.a.mu.Lock()
	histories, snapshots, cacheBytes := e.a.store.CacheUsage()
	e.a.mu.Unlock()
	if histories > 1 || cacheBytes > 8<<20 {
		t.Fatalf("idle histories retained: histories=%d snapshots=%d bytes=%d", histories, snapshots, cacheBytes)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("100 idle branches: TotalAlloc delta=%d HeapAlloc delta=%d snapshot cache=%d bytes; source snapshot=%s", after.TotalAlloc-before.TotalAlloc, retained, cacheBytes, snapshot)
	if retained > 16<<20 {
		t.Fatalf("idle branch heap scales with source: %d", retained)
	}
	if b, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Rss:") || strings.HasPrefix(line, "Pss:") {
				t.Log(line)
			}
		}
	}
}

func TestForkCancelledSourceNeverPublishesReady(t *testing.T) {
	e := setup(t)
	e.say("running")
	e.k.expect(proto.TInput)
	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: "cancel-source"})
	if err != nil {
		t.Fatal(err)
	}
	e.say("/stop")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	waitIdle(t, e, mainSID)
	e.a.mu.Lock()
	br, _ := e.a.store.Branch(res.BranchSID)
	e.a.mu.Unlock()
	if br.Status != session.BranchCancelled || br.SnapshotID != "" {
		t.Fatalf("cancelled source published context: %+v", br)
	}
	if info, err := e.a.SessionBranchAction("continue", res.BranchSID, "continue", "fake:u1"); err == nil {
		t.Fatalf("unready branch ran: %+v", info)
	}
}

func TestPendingForkRecoveryIsVisibleAndKeepsReservedIdentity(t *testing.T) {
	e := setup(t)
	e.say("running")
	e.k.expect(proto.TInput)
	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: "recover-pending"})
	if err != nil {
		t.Fatal(err)
	}
	a2 := reopen(t, e)
	retry, err := a2.SessionFork(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: "recover-pending"})
	if err != nil {
		t.Fatal(err)
	}
	if retry.BranchSID != res.BranchSID || retry.Status != session.BranchNeedsAttention {
		t.Fatalf("pending operation lost or stuck: %+v", retry)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	waitIdle(t, e, mainSID)
}

func TestSlowForkCopyKeepsOtherSessionsResponsive(t *testing.T) {
	e := setup(t)
	_ = e.a.store.Append(mainSID, session.Msg{Role: "user", Content: strings.Repeat("x", 4<<20)})
	e.a.mu.Lock()
	e.a.forkWork = map[string]chan struct{}{}
	e.a.forkSlots = make(chan struct{}, 1)
	e.a.forkSlots <- struct{}{}
	res, err := e.a.sessionForkLocked(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: "slow"})
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.a.Branches(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("copy queue held App lock")
	}
	e.say("foreground still works")
	e.k.expect(proto.TInput)
	e.a.mu.Lock()
	work := e.a.forkWork[res.BranchSID]
	e.a.mu.Unlock()
	<-e.a.forkSlots
	select {
	case <-work:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot never finished")
	}
	e.roundTrip(t)
}
