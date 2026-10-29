package app

import (
	"context"
	"testing"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/proto"
)

func TestStopAllOnlyCancelsOriginalCreatorsCurrentSessionJobs(t *testing.T) {
	e := setup(t)
	add := func(id, creator, sid string) string {
		q := registerJobFixture(t, e, id)
		q.OriginSID = sid
		q.Origin.SID = sid
		q.Lease.SID = sid
		q.CreatedBy = creator
		q.Origin.CreatedBy = creator
		q.Lease.CreatedBy = creator
		q.Background = true
		q.Lease.Background = true
		j := eventlog.Job{SourceID: "test", Creator: creator, Receipt: q}
		if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
			t.Fatal(err)
		}
		return q.JobID
	}
	wanted := map[string]bool{add("own-1", "fake:u1", e.sid): true, add("own-2", "fake:u1", e.sid): true}
	add("foreign", "fake:u2", e.sid)
	add("other-session", "fake:u1", "different")
	e.say("/stop all")
	requests := []proto.Msg{e.k.expect(proto.TJobControl), e.k.expect(proto.TJobControl)}
	for _, request := range requests {
		if !wanted[request.JobID] || request.Action != "cancel" {
			t.Fatal("stop all exceeded creator/session scope", request)
		}
		delete(wanted, request.JobID)
	}
	if len(wanted) != 0 {
		t.Fatal("missing cancellations", wanted)
	}
	e.say("/stop all")
	e.ch.wait(t, "任务取消仍在处理")
	for _, request := range requests {
		e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: request.JobID, RequestID: request.RequestID, OK: proto.Bool(true)})
	}
	e.ch.wait(t, "2 个已受理，0 个未确认")
	e.a.mu.Lock()
	if len(e.a.jobStopGroups) != 0 || e.a.sessions[e.sid].run != nil {
		t.Error("batch retained cache or started model")
	}
	e.a.mu.Unlock()
	e.k.none()
}
func TestJobLogsAliasReadsOriginalNodeCursor(t *testing.T) {
	e := setup(t)
	q := admittedJob(t, e, "logs-alias")
	e.say("/job " + q.JobID + " logs 4")
	request := e.k.expect(proto.TJobControl)
	if request.Action != "output" || request.Cursor != 4 || request.JobID != q.JobID {
		t.Fatal(request)
	}
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, RequestID: request.RequestID, OK: proto.Bool(false), Detail: "fixture done"})
	e.ch.wait(t, "fixture done")
}
func TestAsyncJobReplyCannotFollowReplacedChannel(t *testing.T) {
	e := setup(t)
	replacement := &fakeCh{sent: make(chan string, 10)}
	e.a.AddChannel(replacement)
	e.a.mu.Lock()
	sent := e.a.jobReply(e.ch, endpoint{"fake", "c1"}, "default", e.sid, "private output from old operation")
	e.a.mu.Unlock()
	if sent {
		t.Fatal("old operation retargeted replacement channel")
	}
	select {
	case text := <-replacement.sent:
		t.Fatal("leaked reply", text)
	default:
	}
}
