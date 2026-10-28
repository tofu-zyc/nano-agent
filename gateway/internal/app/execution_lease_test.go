package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/proto"
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
