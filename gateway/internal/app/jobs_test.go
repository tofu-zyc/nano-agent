package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/proto"
)

func admittedJob(t *testing.T, e *env, id string) proto.JobSnapshot {
	t.Helper()
	q := registerJobFixture(t, e, id)
	j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
	if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
		t.Fatal(err)
	}
	return q
}
func TestJobActionOutputRPCAndCommandBypassModelQueue(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "10")
	e.say("/job " + q.JobID + " output 4")
	control := e.k.expect(proto.TJobControl)
	if control.Action != "output" || control.Cursor != 4 || control.JobID != q.JobID {
		t.Fatal(control)
	}
	raw := json.RawMessage(`{"chunks":[{"cursor":4,"next_cursor":9,"stream":"stdout","data":"aGVsbG8="}],"next_cursor":9,"output_cursor":9,"truncated_before":0,"gap":false}`)
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", RequestID: control.RequestID, JobID: q.JobID, OK: proto.Bool(true), JobData: raw})
	e.ch.wait(t, "下次游标：9")
	e.a.mu.Lock()
	s := e.a.sessions[e.sid]
	hasRun := s != nil && s.run != nil
	e.a.mu.Unlock()
	if hasRun {
		t.Fatal("slash command started a model run")
	}
	e.say("/bg " + q.JobID)
	control = e.k.expect(proto.TJobControl)
	if control.Action != "detach" {
		t.Fatal(control)
	}
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", RequestID: control.RequestID, JobID: q.JobID, OK: proto.Bool(true)})
	e.ch.wait(t, "操作已受理")
}
func TestJobActionWaitIsEventDrivenAndBounded(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "11")
	type result struct {
		out JobResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "wait", JobID: q.JobID, WaitMS: 1000})
		done <- result{out, err}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		e.a.mu.Lock()
		n := len(e.a.jobCompletions)
		e.a.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no event subscription")
		}
		time.Sleep(time.Millisecond)
	}
	q.Status = "succeeded"
	q.Revision++
	q.EndedAt = 200
	q.LastEventSeq = 1
	q.Result = &proto.JobResult{OK: true, Content: "done"}
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Job: &q, Kind: "terminal", EventSeq: 1})
	e.k.expect(proto.TEventsAck)
	select {
	case got := <-done:
		if got.err != nil || got.out.Job == nil || got.out.Job.Status != "succeeded" || got.out.WaitExpired {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("completion did not wake subscriber")
	}
	if _, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "wait", JobID: q.JobID, WaitMS: 10001}); err == nil {
		t.Fatal("unbounded wait")
	}
	e.a.mu.Lock()
	remaining := len(e.a.jobCompletions)
	e.a.mu.Unlock()
	if remaining != 0 {
		t.Fatal("wait cache retained")
	}
}
func TestJobActionCancellationCleansOnlyItsWait(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "12")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.a.JobAction(ctx, "fake:u1", "job-source", JobRequest{Action: "output", JobID: q.JobID})
		done <- err
	}()
	control := e.k.expect(proto.TJobControl)
	// A different node epoch cannot satisfy the outstanding control request.
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "foreign", RequestID: control.RequestID, JobID: q.JobID, OK: proto.Bool(true)})
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited for RPC timeout")
	}
	e.a.mu.Lock()
	n := len(e.a.jobControls)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatal("control waiter retained")
	}
	if saved, _ := e.a.jobEvents.Job("test", q.JobID); saved.Receipt.Status != "queued" {
		t.Fatal("cancelling a query cancelled execution")
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() {
		_, err := e.a.JobAction(ctx, "fake:u1", "job-source", JobRequest{Action: "wait", JobID: q.JobID})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		e.a.mu.Lock()
		n = len(e.a.jobCompletions)
		e.a.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wait not registered")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	n = len(e.a.jobCompletions)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatal("completion waiter retained")
	}
}
func TestJobActionUnavailableTargetAndAccessScope(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "13")
	if _, err := e.a.JobAction(context.Background(), "fake:guest", "other", JobRequest{Action: "status", JobID: q.JobID}); err == nil {
		t.Fatal("foreign creator read private Job")
	}
	if _, err := e.a.JobAction(context.Background(), "", "", JobRequest{Action: "list"}); err == nil {
		t.Fatal("anonymous list")
	}
	e.a.mu.Lock()
	delete(e.a.nodes, "test")
	e.a.mu.Unlock()
	status, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "status", JobID: q.JobID})
	if err != nil || status.Job.Online {
		t.Fatal(status, err)
	}
	if _, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "cancel", JobID: q.JobID}); err == nil {
		t.Fatal("offline cancellation declared success")
	}
	if _, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "output", JobID: q.JobID, Cursor: 9007199254740992}); err == nil {
		t.Fatal("unsafe C cursor")
	}
}
func TestJobListPaginatesOnlyVisibleHeaders(t *testing.T) {
	e := setup(t)
	for i := 1; i <= 8; i++ {
		q := registerJobFixture(t, e, fmt.Sprint(20+i))
		if i%2 == 0 {
			q.OwnerAgent = "foreign"
			q.Origin.Agent = "foreign"
			q.Lease.Agent = "foreign"
		}
		q.OutputTail = strings.Repeat("x", 8192)
		j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
		if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "list", Limit: 2})
	if err != nil || len(first.Jobs) != 2 || first.Next == "" {
		t.Fatal(first, err)
	}
	second, err := e.a.JobAction(context.Background(), "fake:u1", "job-source", JobRequest{Action: "list", Limit: 2, After: first.Next})
	if err != nil || len(second.Jobs) != 2 || second.Next != "" {
		t.Fatal(second, err)
	}
	for _, row := range append(first.Jobs, second.Jobs...) {
		if row.Agent != "default" {
			t.Fatal("cross-agent list", row)
		}
	}
	rows, _ := e.a.jobEvents.Jobs("", 2)
	for _, row := range rows {
		if row.Receipt.Result != nil || row.Receipt.Lease != nil || row.Receipt.OutputTail != "" {
			t.Fatal("list loaded logs/lease body")
		}
	}
}

func TestCancelledJobActionDoesNotSendControl(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "90")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.a.JobAction(ctx, "fake:u1", "job-source", JobRequest{Action: "cancel", JobID: q.JobID}); err != context.Canceled {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	n := len(e.a.jobControls)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatal("cancelled caller enqueued control")
	}
}
