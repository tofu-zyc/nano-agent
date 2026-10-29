package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/proto"
)

type jobPresenterCh struct {
	*fakeCh
	cards   chan channel.JobCard
	updates chan channel.JobCard
	gate    <-chan struct{}
}

func (c *jobPresenterCh) SendJob(ctx context.Context, _ string, card channel.JobCard) (string, error) {
	c.cards <- card
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "job-card", nil
}
func (c *jobPresenterCh) UpdateJob(_ context.Context, _, _ string, card channel.JobCard) error {
	c.updates <- card
	return nil
}
func prepareJobHint(t *testing.T, e *env, id string, presenter bool) (proto.JobSnapshot, *jobPresenterCh) {
	t.Helper()
	q := admittedJob(t, e, id)
	c := &jobPresenterCh{fakeCh: e.ch, cards: make(chan channel.JobCard, 16), updates: make(chan channel.JobCard, 16)}
	if presenter {
		e.a.AddChannel(c)
	}
	e.a.mu.Lock()
	s := e.a.sessOf(q.OriginSID)
	s.run = &run{runID: q.OriginRunID, turnID: q.Origin.TurnID, from: "fake:u1", node: "test", mode: "auto",
		start: e.a.now(), stopTyping: func() {}, callArgs: map[string]proto.Call{q.OriginCallID: {Name: "Shell", Args: json.RawMessage(`{"command":"make test 中文"}`)}},
		outs: []out{{ep: endpoint{"fake", "c1"}}}}
	e.a.eps["fake:c1"] = &epRec{Session: q.OriginSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	q.Status = "running"
	q.StartedAt = 101
	q.Revision++
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Job: &q})
	waitJobHint(t, e, q.JobID, func() bool { return e.a.jobHints[jobPersistKey("test", q.JobID)] != nil })
	return q, c
}
func waitJobHint(t *testing.T, e *env, id string, check func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for {
		e.a.mu.Lock()
		ok := check()
		e.a.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(until) {
			t.Fatal("job presentation did not advance", id)
		}
		time.Sleep(time.Millisecond)
	}
}
func advanceHint(e *env, d time.Duration) {
	e.a.mu.Lock()
	now := e.a.now().Add(d)
	e.a.now = func() time.Time { return now }
	e.a.jobHintTick()
	e.a.mu.Unlock()
}
func TestJobHintOnceUsesActualStartAndDirectCommandsWithoutPresenter(t *testing.T) {
	e := setup(t)
	q, _ := prepareJobHint(t, e, "hint-plain", false)
	advanceHint(e, 9*time.Second)
	select {
	case text := <-e.ch.sent:
		t.Fatal("early hint", text)
	default:
	}
	advanceHint(e, time.Second)
	text := e.ch.wait(t, "命令已运行至少 10 秒")
	if !strings.Contains(text, "/bg "+q.JobID) || !strings.Contains(text, "make test 中文") {
		t.Fatal(text)
	}
	advanceHint(e, time.Minute)
	select {
	case text := <-e.ch.sent:
		t.Fatal("repeated hint", text)
	default:
	}
}
func TestJobButtonsFreezeIdentityDeduplicateAndCloseAtDurableTerminal(t *testing.T) {
	e := setup(t)
	q, c := prepareJobHint(t, e, "hint-card", true)
	advanceHint(e, 10*time.Second)
	var card channel.JobCard
	select {
	case card = <-c.cards:
	case <-time.After(time.Second):
		t.Fatal("no card")
	}
	if len(card.Buttons) != 3 || card.JobID != q.JobID {
		t.Fatal(card)
	}
	waitJobHint(t, e, q.JobID, func() bool {
		p := e.a.jobCards[jobCardKey(e.a.jobHints[jobPersistKey("test", q.JobID)].info, endpoint{"fake", "c1"})]
		return p != nil && p.ref == "job-card"
	})
	detach := card.Buttons[0].Token
	click := func(sender, chat, ref string) {
		e.a.OnMessage(c, channel.Message{ChatID: chat, SenderID: sender, Action: channel.ActionJob, Text: detach, MsgID: ref})
	}
	click("u2", "c1", "job-card")
	e.ch.wait(t, "没有这个任务的操作权限")
	click("u1", "wrong-chat", "job-card")
	e.ch.wait(t, "任务按钮已过期")
	click("u1", "c1", "other-message")
	e.ch.wait(t, "任务按钮已过期")
	// Moving the window to another conversation for the same agent must not
	// retarget the action or attach the window to the original conversation.
	e.a.mu.Lock()
	e.a.eps["fake:c1"].Session = "another-chat"
	e.a.mu.Unlock()
	click("u1", "c1", "job-card")
	control := e.k.expect(proto.TJobControl)
	if control.Action != "detach" || control.JobID != q.JobID {
		t.Fatal(control)
	}
	click("u1", "c1", "job-card")
	e.ch.wait(t, "正在处理")
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, RequestID: control.RequestID, OK: proto.Bool(true)})
	e.ch.wait(t, "操作已受理")
	click("u1", "c1", "job-card")
	e.ch.wait(t, "已经受理")
	e.a.mu.Lock()
	if e.a.eps["fake:c1"].Session != "another-chat" {
		t.Error("button rebound the window")
	}
	e.a.mu.Unlock()
	q.Status = "succeeded"
	q.Revision++
	q.EndedAt = 200
	q.LastEventSeq = 1
	q.Result = &proto.JobResult{OK: true, Content: "done"}
	e.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Job: &q, EventSeq: 1})
	e.k.expect(proto.TEventsAck)
	advanceHint(e, time.Second)
	select {
	case update := <-c.updates:
		if !update.Finished || len(update.Buttons) != 1 || update.Buttons[0].Label != "查看输出" {
			t.Fatal(update)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal card did not refresh")
	}
	click("u1", "c1", "job-card")
	e.ch.wait(t, "任务按钮已过期")
	e.k.none()
}
func TestJobCardSlowIOReleasesAppAndExpiresItsTokens(t *testing.T) {
	e := setup(t)
	q, c := prepareJobHint(t, e, "hint-slow", true)
	gate := make(chan struct{})
	c.gate = gate
	advanceHint(e, 10*time.Second)
	select {
	case <-c.cards:
	case <-time.After(time.Second):
		t.Fatal("no card request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err := e.a.JobAction(ctx, "fake:u1", q.OriginSID, JobRequest{Action: "status", JobID: q.JobID})
	cancel()
	if err != nil {
		close(gate)
		t.Fatal("card I/O held App lock", err)
	}
	close(gate)
	waitJobHint(t, e, q.JobID, func() bool {
		for _, p := range e.a.jobCards {
			if p.info.JobID == q.JobID {
				return !p.sending && p.ref != ""
			}
		}
		return false
	})
	advanceHint(e, 24*time.Hour)
	e.a.mu.Lock()
	if len(e.a.jobButtons) != 0 {
		t.Error("expired action cache retained")
	}
	e.a.mu.Unlock()
	advanceHint(e, 6*time.Minute)
	e.a.mu.Lock()
	if len(e.a.jobCards) != 0 {
		t.Error("expired card cache retained")
	}
	e.a.mu.Unlock()
}
func TestJobHintSilentExecutionNeverPushesOrCreatesButtons(t *testing.T) {
	e := setup(t)
	q, _ := prepareJobHint(t, e, "hint-silent", true)
	e.a.mu.Lock()
	key := jobPersistKey("test", q.JobID)
	info := e.a.jobHints[key].info
	info.Silent = true
	e.a.observeJobPresentation(info)
	e.a.mu.Unlock()
	advanceHint(e, time.Hour)
	select {
	case text := <-e.ch.sent:
		t.Fatal("silent job pushed", text)
	default:
	}
	e.a.mu.Lock()
	if len(e.a.jobCards) != 0 || len(e.a.jobButtons) != 0 {
		t.Error("silent job created channel controls")
	}
	e.a.mu.Unlock()
}

func TestJobButtonsAreRevokedWhenChannelIsReplaced(t *testing.T) {
	e := setup(t)
	q, c := prepareJobHint(t, e, "hint-replace", true)
	advanceHint(e, 10*time.Second)
	select {
	case <-c.cards:
	case <-time.After(time.Second):
		t.Fatal("no card")
	}
	waitJobHint(t, e, q.JobID, func() bool {
		for _, p := range e.a.jobCards {
			if p.info.JobID == q.JobID {
				return p.ref != ""
			}
		}
		return false
	})
	e.a.AddChannel(&fakeCh{sent: make(chan string, 10)})
	advanceHint(e, time.Second)
	e.a.mu.Lock()
	if len(e.a.jobCards) != 0 || len(e.a.jobButtons) != 0 {
		t.Error("replaced channel retained action authority")
	}
	e.a.mu.Unlock()
	e.k.none()
}
