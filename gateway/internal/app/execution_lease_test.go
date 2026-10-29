package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/proto"
	"strings"
	"testing"
	"time"
)

func waitLeaseState(t *testing.T, a *App, id, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		cp, _ := a.dispatcher.Snapshot()
		for _, l := range cp.Leases {
			if l.ID == id && l.State == state {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s never became %s: %+v", id, state, cp)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestGatewayLeaseCancellationWhileQueued(t *testing.T) {
	e := setup(t)
	req := dispatch.Request{ID: "held", Owner: "peer/boot", Machine: "shared", Session: "other", Exclusive: []string{"machine:files"}}
	if !e.a.dispatcher.Submit(dispatch.Command{Action: "admit", Request: req}) {
		t.Fatal("admit")
	}
	waitLeaseState(t, e.a, "held", "granted")
	ctx, cancel := context.WithCancel(e.a.ctx)
	request := dispatch.Request{ID: "waiting", Owner: "gateway/" + e.a.dispatchBoot, Machine: "shared", Session: "s",
		RunID: "r", CallID: "write", Exclusive: []string{"machine:files"}}
	result := make(chan error, 1)
	go func() {
		release, err := e.a.acquireExecution(ctx, request)
		if release != nil {
			release(false)
		}
		result <- err
	}()
	waitLeaseState(t, e.a, "waiting", "queued")
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled admission was granted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stuck")
	}
	e.a.submitExecutionCleanup(dispatch.Command{Action: "release", ID: req.ID, Owner: req.Owner})
	deadline := time.Now().Add(3 * time.Second)
	for {
		cp, _ := e.a.dispatcher.Snapshot()
		if len(cp.Leases) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued cancellation leaked: %+v", cp)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestGatewayLeaseAmbiguousExecutionKeepsResource(t *testing.T) {
	e := setup(t)
	first := dispatch.Request{ID: "lost", Owner: "gateway/" + e.a.dispatchBoot, Machine: "shared", Session: "a", Exclusive: []string{"machine:files"}}
	release, err := e.a.acquireExecution(e.a.ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	release(true)
	waitLeaseState(t, e.a, "lost", "unknown")
	next := first
	next.ID = "next"
	next.Session = "b"
	ctx, cancel := context.WithCancel(e.a.ctx)
	done := make(chan error, 1)
	go func() {
		rel, err := e.a.acquireExecution(ctx, next)
		if rel != nil {
			rel(false)
		}
		done <- err
	}()
	waitLeaseState(t, e.a, "next", "queued")
	cancel()
	if err := <-done; err == nil {
		t.Fatal("unknown process released its resource")
	}
}
func TestGatewayRunCapacityLeavesMessagesQueuedAndRotates(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.runtimeLimits.MaxActiveRunsPerNode = 1
	for i, sid := range []string{e.sid, "waiting-b", "waiting-c"} {
		s := e.a.sessOf(sid)
		s.queue = []queued{{who: "fake:u1", ep: endpoint{ch: "fake", chat: "c1"}, m: channel.Message{Text: "message", ChatID: "c1"}}}
		e.a.startRun(s)
		if i > 0 && (s.run != nil || len(s.queue) != 1) {
			t.Fatal("started beyond capacity or lost message")
		}
	}
	e.a.mu.Unlock()
	first := e.k.expect(proto.TInput)
	if first.SID != e.sid {
		t.Fatal(first)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	second := e.k.expect(proto.TInput)
	if second.SID != "waiting-b" {
		t.Fatalf("oldest session not served: %+v", second)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: second.SID, Status: proto.StatusOK})
	third := e.k.expect(proto.TInput)
	if third.SID != "waiting-c" {
		t.Fatalf("rotation: %+v", third)
	}
}
func TestGatewayMessageQueueBound(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	s := e.a.sessOf("bounded")
	s.queue = make([]queued, 32)
	if e.a.queueFits(s, channel.Message{Text: "one more"}) {
		t.Fatal("session queue grew beyond 32")
	}
	s.queue = nil
	e.a.sessOf("other").queue = make([]queued, 256)
	if e.a.queueFits(s, channel.Message{Text: "one more"}) {
		t.Fatal("Gateway queue grew beyond 256")
	}
}

func TestRemoteExecutionTimeoutStartsAfterAdmission(t *testing.T) {
	e := setup(t)
	remote := e.hosts.set("remote", "ssh", true, 7, "")
	remote.block = true
	ws, err := e.a.AddWorkspace(context.Background(), "remote", e.dir, true)
	if err != nil {
		t.Fatal(err)
	}
	held := dispatch.Request{ID: "held-remote", Owner: "other/boot", Machine: "remote", Session: "other", Exclusive: []string{"machine:files"}}
	e.a.dispatcher.Submit(dispatch.Command{Action: "admit", Request: held})
	waitLeaseState(t, e.a, held.ID, "granted")
	e.say("run")
	input := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TExec, SID: input.SID, RunID: input.RunID, ID: "sleep", Name: "Shell", Workspace: ws.Label(),
		TimeoutMs: 20, Args: json.RawMessage("{\"command\":\"sleep\"}")})
	deadline := time.Now().Add(time.Second)
	for {
		e.a.mu.Lock()
		waits := len(e.a.dispatchWait)
		e.a.mu.Unlock()
		if waits > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no queued remote execution")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(70 * time.Millisecond) // exceed the execution timeout while still queued
	remote.mu.Lock()
	started := len(remote.reqs)
	remote.mu.Unlock()
	if started != 0 {
		t.Fatal("remote process bypassed the held resource")
	}
	e.a.submitExecutionCleanup(dispatch.Command{Action: "release", ID: held.ID, Owner: held.Owner})
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK {
		t.Fatal("timeout did not terminate the running host")
	}
	remote.mu.Lock()
	started = len(remote.reqs)
	remote.mu.Unlock()
	if started != 1 {
		t.Fatalf("queue time consumed the execution budget: starts=%d", started)
	}
}

func TestNodeLeaseDetachProtocolKeepsForegroundUntilCommit(t *testing.T) {
	e := setup(t)
	epoch := "00112233445566778899aabbccddeeff"
	e.a.mu.Lock()
	e.a.nodes["test"].info.NodeEpoch = epoch
	e.a.nodes["test"].info.Features = map[string]int{"machine_leases": 1, "lease_detach": 1}
	e.a.mu.Unlock()
	req := dispatch.Request{ID: "l" + epoch + "-1", Owner: "test/" + epoch, Machine: "shared", Session: "source", RunID: "r", CallID: "shell", Agent: "a"}
	e.a.dispatcher.Submit(dispatch.Command{Action: "admit", Request: req})
	waitLeaseState(t, e.a, req.ID, "granted")
	// Consume the grant published to this authenticated execution owner.
	e.k.expect(proto.TLeaseGrant)
	roundtrip := func(kind, control, phase string, ok bool) {
		t.Helper()
		e.k.send(proto.Msg{T: kind, LeaseID: req.ID, NodeEpoch: epoch, RequestID: control})
		result := e.k.expect(proto.TLeaseDetachResult)
		if result.RequestID != control || result.Phase != phase || result.OK == nil || *result.OK != ok {
			t.Fatalf("handoff reply %+v", result)
		}
	}
	roundtrip(proto.TLeaseDetachPrepare, "first", "prepare", true)
	cp, _ := e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 1 || cp.Leases[0].Background || cp.Leases[0].DetachRequest != "first" {
		t.Fatal("prepare activated ownership", cp)
	}
	// A node-side persistence failure aborts the reservation, retaining its
	// original wait relationship and resources.
	roundtrip(proto.TLeaseDetachAbort, "first", "abort", true)
	cp, _ = e.a.dispatcher.Snapshot()
	if cp.Leases[0].Background || cp.Leases[0].DetachRequest != "" {
		t.Fatal(cp)
	}
	roundtrip(proto.TLeaseDetachPrepare, "second", "prepare", true)
	roundtrip(proto.TLeaseDetachCommit, "wrong", "commit", false)
	roundtrip(proto.TLeaseDetachCommit, "second", "commit", true)
	cp, _ = e.a.dispatcher.Snapshot()
	if !cp.Leases[0].Background || cp.Leases[0].DetachRequest != "" {
		t.Fatal("commit did not publish", cp)
	}
	roundtrip(proto.TLeaseDetachCommit, "second", "commit", true)
}

func TestNodeJobRecoveryErrorPreservesLeases(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.NodeEpoch = "job-boot"
	e.a.mu.Unlock()
	request := dispatch.Request{ID: "ljob-boot-1", Owner: "test/job-boot", Machine: "job-machine", Session: "job-source",
		Exclusive: []string{"machine:files"}}
	if !e.a.dispatcher.Submit(dispatch.Command{Action: "admit", Request: request}) {
		t.Fatal("admit")
	}
	waitLeaseState(t, e.a, request.ID, "granted")
	e.k.expect(proto.TLeaseGrant)
	e.k.send(proto.Msg{T: proto.TLeaseSync, NodeEpoch: "job-boot", JobStorageError: "future metadata version"})
	reply := e.k.expect(proto.TLeaseSynced)
	if reply.OK == nil || *reply.OK || !strings.Contains(reply.Detail, "future metadata version") {
		t.Fatal(reply)
	}
	waitLeaseState(t, e.a, request.ID, "granted")
}

func TestNodeJobRebootInventoryRequiresMatchingDurableReceipts(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.NodeEpoch = "oldboot"
	e.a.mu.Unlock()
	req := dispatch.Request{JobID: "j_oldboot-1", ID: "loldboot-1", Name: "Shell", Owner: "test/oldboot",
		Machine: e.a.machineOf("test"), Session: "source", RunID: "run", CallID: "call", Agent: "agent",
		Shell: true, Background: true, Exclusive: []string{"dir:/work"}}
	e.a.dispatcher.Submit(dispatch.Command{Action: "admit", Request: req})
	waitLeaseState(t, e.a, req.ID, "granted")
	e.k.expect(proto.TLeaseGrant)
	e.a.mu.Lock()
	e.a.nodes["test"].info.NodeEpoch = "newboot"
	e.a.mu.Unlock()
	spec := proto.LeaseSpec{LeaseID: req.ID, JobID: req.JobID, SID: req.Session, RunID: req.RunID, CallID: req.CallID,
		Agent: req.Agent, Name: req.Name, Exclusive: "dir:/work", Background: true, State: "unknown"}
	roundtrip := func(message proto.Msg, want bool) {
		t.Helper()
		message.T = proto.TLeaseSync
		message.NodeEpoch = "newboot"
		e.k.send(message)
		reply := e.k.expect(proto.TLeaseSynced)
		if reply.OK == nil || *reply.OK != want {
			t.Fatal(reply)
		}
	}
	roundtrip(proto.Msg{Leases: []proto.LeaseSpec{spec}}, true)
	cp, _ := e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 1 || cp.Leases[0].Owner != "test/newboot" || cp.Leases[0].State != "unknown" {
		t.Fatal(cp)
	}
	// A receipt sent through the active inventory is not a completion proof.
	spec.State = "succeeded"
	spec.TerminalReceipt = true
	roundtrip(proto.Msg{Leases: []proto.LeaseSpec{spec}}, false)
	roundtrip(proto.Msg{LeaseReceipts: []proto.LeaseSpec{spec}}, true)
	cp, _ = e.a.dispatcher.Snapshot()
	if len(cp.Leases) != 0 {
		t.Fatal("terminal did not settle", cp)
	}
	roundtrip(proto.Msg{LeaseReceipts: []proto.LeaseSpec{spec}}, true)
	spec.State = "unknown"
	spec.TerminalReceipt = false
	roundtrip(proto.Msg{Leases: []proto.LeaseSpec{spec}}, false)
}
