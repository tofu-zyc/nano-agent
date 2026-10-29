package app

import (
	"context"
	"strconv"
	"testing"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

func TestRemoteJobStartUsesDurablePolicyBeforeConsumingEitherSignal(t *testing.T) {
	for _, seconds := range []int{0, 1800, 31536000} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			e := setup(t)
			q := registerJobFixture(t, e, "start-policy")
			q.TimeoutSeconds = seconds
			j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
			if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
				t.Fatal(err)
			}
			got := make(chan host.StartInfo, 2)
			e.a.mu.Lock()
			wait := e.a.hostWaits[q.Lease.HostID]
			wait.jobID = q.JobID
			wait.started = func(info host.StartInfo) { got <- info }
			e.a.hostWaits[q.Lease.HostID] = wait
			if e.a.notifyHostStarted("test", q.Lease.HostID, "", 0, true) {
				t.Error("missing Job label consumed managed start")
			}
			if e.a.notifyHostStarted("other", q.Lease.HostID, q.JobID, int64(seconds)*1000, true) {
				t.Error("wrong node")
			}
			if e.a.notifyHostStarted("test", q.Lease.HostID, q.JobID, 100, true) {
				t.Error("unverified timeout")
			}
			// lease_started arrives first, before run_started.
			if !e.a.notifyHostStarted("test", q.Lease.HostID, q.JobID, 0, false) {
				t.Error("lost verified start")
			}
			if e.a.notifyHostStarted("test", q.Lease.HostID, q.JobID, int64(seconds)*1000, true) {
				t.Error("duplicate consumed start twice")
			}
			e.a.mu.Unlock()
			select {
			case info := <-got:
				expected := int64(seconds) * 1000
				if expected == 0 {
					expected = -1
				}
				if info.JobID != q.JobID || info.TimeoutMS != expected {
					t.Fatal(info)
				}
			case <-time.After(time.Second):
				t.Fatal("no start policy")
			}
			select {
			case info := <-got:
				t.Fatal("duplicate start", info)
			default:
			}
		})
	}
}
func TestHostedJobStartedFrameForwardsVerifiedDeadlineAndLegacyRemainsUnlabelled(t *testing.T) {
	e := setup(t)
	q := registerJobFixture(t, e, "wire-start")
	q.TimeoutSeconds = 31536000
	j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
	if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.nodes["test"].info.Features["exec_started_timeout"] = 1
	wait := e.a.hostWaits[q.Lease.HostID]
	wait.started = func(info host.StartInfo) {
		e.a.mu.Lock()
		e.a.execStarted("test", &proto.Msg{SID: q.OriginSID, RunID: q.OriginRunID, TurnID: q.Origin.TurnID, ID: q.OriginCallID,
			JobID: info.JobID, TimeoutMs: info.TimeoutMS})
		e.a.mu.Unlock()
	}
	e.a.hostWaits[q.Lease.HostID] = wait
	e.a.mu.Unlock()
	e.k.send(proto.Msg{T: proto.TRunStarted, ID: q.Lease.HostID, JobID: q.JobID, TimeoutMs: 31536000000})
	notice := e.k.expect(proto.TExecStarted)
	if notice.JobID != q.JobID || notice.TimeoutMs != 31536000000 || notice.ID != q.OriginCallID || notice.TurnID != q.Origin.TurnID {
		t.Fatal(notice)
	}
	e.a.mu.Lock()
	e.a.execStarted("test", &proto.Msg{SID: q.OriginSID, ID: "legacy", TimeoutMs: 610000})
	e.a.mu.Unlock()
	legacy := e.k.expect(proto.TExecStarted)
	if legacy.JobID != "" || legacy.TimeoutMs != 0 {
		t.Fatal("legacy start incorrectly replaced policy", legacy)
	}
}
