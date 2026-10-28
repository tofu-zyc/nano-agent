package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
)

// Called with App.mu; freeze the authenticated call before entering another
// goroutine. This schedules execution only; C continues to drive model turns.
func (a *App) gatewayLeaseRequest(s *sess, m *proto.Msg, machine string, shared, exclusive []string) dispatch.Request {
	a.hostSeq++
	priority := 0
	if s.run.inner || a.isBranchSID(s.sid) {
		priority = 10
	}
	return dispatch.Request{
		ID: fmt.Sprintf("g%s-%d", a.dispatchBoot, a.hostSeq), Name: m.Name, Owner: "gateway/" + a.dispatchBoot,
		Machine: machine, Session: s.sid, RunID: s.run.runID, TurnID: s.run.turnID, CallID: m.ID,
		Agent: a.execAgentID(s.sid), Priority: priority, Shell: m.Name == "Shell",
		Shared: shared, Exclusive: exclusive, At: time.Now(),
	}
}

// acquireExecution waits outside App.mu. Callback delivery never blocks the
// persistence worker. Cleanup is submitted after admission, including cancellation
// racing a grant; a running caller must invoke it only after its process stopped.
func (a *App) acquireExecution(ctx context.Context, req dispatch.Request) (func(bool), error) {
	ch := make(chan dispatch.Change, 1)
	a.mu.Lock()
	engine := a.dispatcher
	if engine == nil {
		a.mu.Unlock()
		return nil, errors.New("执行租约存储不可用，未执行")
	}
	a.dispatchWait[req.ID] = func(c dispatch.Change) {
		if c.Err != nil || c.Lease.State == "granted" {
			select {
			case ch <- c:
			default:
			}
		} else if c.Action == "admit" {
			a.publish(events.Event{T: "tool_queued", SID: req.Session, RunID: req.RunID, TurnID: req.TurnID, ID: req.CallID,
				Text: "等待" + c.Lease.WaitFor + " " + c.Lease.Holder})
		}
	}
	accepted := engine.Submit(dispatch.Command{Action: "admit", Request: req})
	a.mu.Unlock()
	cleanupWait := func() {
		a.mu.Lock()
		delete(a.dispatchWait, req.ID)
		a.mu.Unlock()
	}
	release := func(unknown bool) {
		action := "release"
		if unknown {
			action = "unknown"
		}
		a.submitExecutionCleanup(dispatch.Command{Action: action, ID: req.ID, Owner: req.Owner})
	}
	if !accepted {
		cleanupWait()
		return nil, errors.New("执行租约队列已满，本次未受理或执行")
	}
	select {
	case change := <-ch:
		cleanupWait()
		if change.Err != nil {
			return nil, change.Err
		}
		if ctx.Err() != nil {
			release(false)
			return nil, ctx.Err()
		}
		return release, nil
	case <-ctx.Done():
		cleanupWait()
		release(false) // still queued or reserved: no process has been started
		return nil, ctx.Err()
	}
}

// A completion must not disappear merely because the bounded persistence inbox
// is momentarily full. Each caller has at most one cleanup; retry outside App.mu.
func (a *App) submitExecutionCleanup(command dispatch.Command) {
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		if a.dispatcher == nil {
			return
		}
		if a.dispatcher.Submit(command) {
			return
		}
		select {
		case <-a.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// The coordinator must stop its dispatch timer while a Gateway-owned operation
// awaits approval, a resource or a remote process slot.
func (a *App) execQueued(node string, m *proto.Msg) {
	if peer := a.nodes[node]; peer != nil && peer.info.Features["machine_leases"] > 0 {
		a.hub.Send(node, proto.Msg{T: proto.TExecQueued, SID: m.SID, RunID: m.RunID, TurnID: m.TurnID, ID: m.ID})
	}
}

func (a *App) executionLeaseStarted(req dispatch.Request) {
	a.submitExecutionCleanup(dispatch.Command{Action: "started", ID: req.ID, Owner: req.Owner})
}
