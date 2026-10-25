package app

import (
	"encoding/json"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/schedule"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newSchedule(t *testing.T, e *env, patch ScheduleRequest) schedule.Task {
	t.Helper()
	e.say("/cron")
	e.ch.wait(t, "定时任务")
	p := ScheduleRequest{Action: "create", Name: "早报", When: "every 30m", Prompt: "给我早报", Deliver: []string{"fake:c1"}}
	if patch.When != "" {
		p.When = patch.When
	}
	p.Mode = patch.Mode
	p.Timeout = patch.Timeout
	p.CatchUp = patch.CatchUp
	v, err := e.a.ScheduleAction(e.sid, "fake:u1", p)
	if err != nil {
		t.Fatal(err)
	}
	return v.(schedule.Task)
}
func scheduledRound(t *testing.T, e *env, task schedule.Task) {
	t.Helper()
	m := e.k.expect(proto.TInput)
	if m.SID != "cron:"+task.ID || m.Mode != task.Mode || m.ApprovalTimeoutMs != 1800000 {
		t.Fatalf("input: %+v", m)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: m.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: m.SID, Status: proto.StatusOK})
	e.ch.wait(t, "[定时任务 早报]")
	waitFor(t, func() bool { return !e.a.Schedules().Tasks[0].Running })
}
func TestScheduleExecutionDeliveryAndArchive(t *testing.T) {
	e := setup(t)
	e.say("原聊天")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	before := e.a.FullHistory(e.sid)
	task := newSchedule(t, e, ScheduleRequest{})
	for i := 0; i < 2; i++ {
		if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err != nil {
			t.Fatal(err)
		}
		scheduledRound(t, e, task)
	}
	tasks := e.a.Schedules().Tasks
	if len(tasks[0].Results) != 2 || !tasks[0].Last.OK {
		t.Fatal(tasks)
	}
	if len(e.a.Archives("cron:"+task.ID)) != 1 {
		t.Fatal("previous run not archived")
	}
	if !reflect.DeepEqual(before, e.a.FullHistory(e.sid)) {
		t.Fatal("delivery polluted original context")
	}
}
func TestScheduleCatchUpAndNoOverlap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		late  time.Duration
		catch bool
		run   bool
	}{
		{"within hour", 45 * time.Minute, true, true}, {"too late", 2 * time.Hour, true, false},
		{"interval long outage", 2 * time.Hour, true, true}, {"disabled catch-up", 45 * time.Minute, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			p := ScheduleRequest{CatchUp: &tc.catch}
			if tc.name == "too late" {
				p.When = "at 2026-10-21 15:38"
			}
			task := newSchedule(t, e, p)
			now := task.Next.Add(tc.late)
			e.a.mu.Lock()
			e.a.now = func() time.Time { return now }
			e.a.schedulerStarted = false
			e.a.mu.Unlock()
			e.a.ScheduleTick()
			if tc.run {
				if !e.a.Schedules().Tasks[0].Running {
					t.Fatal("not caught up")
				}
				e.a.ScheduleTick()
				scheduledRound(t, e, task)
			} else {
				ts := e.a.Schedules().Tasks
				if ts[0].Running || ts[0].Last == nil || !strings.Contains(ts[0].Last.Summary, "错过") || (!ts[0].Next.After(now) && !(ts[0].Next.IsZero() && !ts[0].Enabled)) {
					t.Fatal(ts)
				}
			}
		})
	}
}
func TestScheduleTimeoutAndDeliveryWindowApproval(t *testing.T) {
	e := setup(t)
	task := newSchedule(t, e, ScheduleRequest{Timeout: "5s"})
	_, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	m := e.k.expect(proto.TInput)
	if m.RunTimeoutMs != 5000 {
		t.Fatal(m)
	}
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: m.SID, ID: "call", Name: "Write", Args: json.RawMessage(`{"path":"x","content":"y"}`), From: "fake:u1"})
	e.ch.wait(t, "需要审批 #1")
	e.say("/approve 1")
	ap := e.k.expect(proto.TApprove)
	if ap.SID != m.SID || ap.OK == nil || !*ap.OK {
		t.Fatal(ap)
	}
	e.a.mu.Lock()
	now := e.a.now().Add(6 * time.Second)
	e.a.now = func() time.Time { return now }
	e.a.mu.Unlock()
	e.a.ScheduleTick()
	e.k.expect(proto.TCancel)
	e.ch.wait(t, "运行超时")
	ts := e.a.Schedules().Tasks
	if ts[0].Running || ts[0].Last.OK {
		t.Fatal(ts)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: m.SID, Status: proto.StatusCancelled})
}
func TestSchedulePermissionsAndTool(t *testing.T) {
	e := setup(t)
	task := newSchedule(t, e, ScheduleRequest{})
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "update", ID: task.ID, Mode: "auto"}); err == nil {
		t.Fatal("raised ceiling")
	}
	if _, err := e.a.ScheduleAction(e.sid, "fake:u2", ScheduleRequest{Action: "list"}); err == nil {
		t.Fatal("guest")
	}
	e.say("/mode readonly")
	e.ch.wait(t, "只读")
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "delete", ID: task.ID}); err == nil {
		t.Fatal("readonly mutation")
	}
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err == nil {
		t.Fatal("readonly escalation")
	}
	e.say("/mode ask")
	e.ch.wait(t, "审批")
	e.say("列出任务")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TExec, SID: e.sid, ID: "list", Name: "Schedule", Args: json.RawMessage(`{"action":"list"}`)})
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || !strings.Contains(res.Content, task.ID) {
		t.Fatal(res)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	waitFor(t, func() bool { e.a.mu.Lock(); defer e.a.mu.Unlock(); return e.a.sessions[e.sid].run == nil })
}

func TestScheduleOfflineFailureRecordedOnce(t *testing.T) {
	e := setup(t)
	task := newSchedule(t, e, ScheduleRequest{})
	e.a.KernelDown("test")
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err == nil {
		t.Fatal("offline run accepted")
	}
	e.ch.wait(t, "[定时任务 早报]")
	tasks := e.a.Schedules().Tasks
	if len(tasks[0].Results) != 1 || tasks[0].Last.OK || tasks[0].Running {
		t.Fatal(tasks)
	}
}

func TestScheduleOverlappingOccurrenceSkipped(t *testing.T) {
	e := setup(t)
	task := newSchedule(t, e, ScheduleRequest{Timeout: "2h"})
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	now := task.Next.Add(time.Minute)
	e.a.now = func() time.Time { return now }
	e.a.mu.Unlock()
	e.a.ScheduleTick()
	current := e.a.Schedules().Tasks[0]
	if !current.Running || !current.Next.After(now) || current.Last == nil || !strings.Contains(current.Last.Summary, "上一轮仍在运行") {
		t.Fatal(current)
	}
	scheduledRound(t, e, task)
	if current = e.a.Schedules().Tasks[0]; len(current.Results) != 2 || !current.Last.OK {
		t.Fatal(current)
	}
}

// 模型建任务时不写 deliver：默认只放任务页，不往聊天窗口推（以前默认发到创建任务的那个窗口）
func TestScheduleWithoutDeliverStaysOnTaskPage(t *testing.T) {
	e := setup(t)
	e.say("/cron")
	e.ch.wait(t, "定时任务")
	v, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "create", Name: "值守", When: "every 30m", Prompt: "看看有没有新消息"})
	if err != nil {
		t.Fatal(err)
	}
	task := v.(schedule.Task)
	if len(task.Deliver) != 0 {
		t.Fatalf("没写 deliver 就不该有投递窗口：%v", task.Deliver)
	}
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	m := e.k.expect(proto.TInput)
	if m.SID != "cron:"+task.ID {
		t.Fatalf("input: %+v", m)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: m.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: m.SID, Status: proto.StatusOK})
	waitFor(t, func() bool { return !e.a.Schedules().Tasks[0].Running })
	if got := e.a.Schedules().Tasks[0].Results; len(got) != 1 || !got[0].OK {
		t.Fatalf("结果该记进任务页：%+v", got)
	}
	e.ch.none(t) // 不往聊天窗口投递
}

// 模型回了 [不回复]：结果只记在任务页，不推到投递窗口（每 3 分钟查一次私信的任务，没新消息时
// 以前每次都往飞书推一条“[定时任务 …] [不回复]”）。模型也要知道可以这么回。
func TestScheduleSilentNotPushed(t *testing.T) {
	e := setup(t)
	task := newSchedule(t, e, ScheduleRequest{})
	e.llm.say("[不回复]")
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	m := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: m.SID})
	e.k.expect(proto.TTurn)
	req := e.llm.lastReq()
	if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "只回 [不回复]") || !strings.Contains(last, "给我早报") {
		t.Fatalf("模型不知道没事可以不推送：%q", last)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: m.SID, Status: proto.StatusOK})
	waitFor(t, func() bool { return !e.a.Schedules().Tasks[0].Running })
	e.ch.none(t)
	if last := e.a.Schedules().Tasks[0].Last; last == nil || !last.OK || strings.Contains(last.Summary, "[不回复]") || !strings.Contains(last.Summary, "没有推送") {
		t.Fatalf("任务页上该记一笔“没有推送”：%+v", last)
	}
	// 有话要说的时候照常推送
	e.llm.say("有一条新私信，已经回了")
	if _, err := e.a.ScheduleAction(e.sid, "fake:u1", ScheduleRequest{Action: "run_now", ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	m = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: m.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: m.SID, Status: proto.StatusOK})
	if got := e.ch.wait(t, "[定时任务 早报]"); !strings.Contains(got, "已经回了") {
		t.Fatalf("推送内容：%q", got)
	}
}

// 一样的失败连着出现：只推第一次，后面的只记在任务页；恢复以后再失败又会推。
func TestScheduleRepeatedFailurePushedOnce(t *testing.T) {
	e := setup(t)
	newSchedule(t, e, ScheduleRequest{})
	e.a.KernelDown("test")
	run := func() {
		task := e.a.Schedules().Tasks[0]
		e.a.mu.Lock()
		e.a.launchSchedule(task, false)
		e.a.mu.Unlock()
	}
	run()
	e.ch.wait(t, "没有在线的内核")
	run()
	run()
	e.ch.none(t)
	if got := e.a.Schedules().Tasks[0].Results; len(got) != 3 {
		t.Fatalf("每次失败都该记在任务页：%d", len(got))
	}
}

func TestScheduleSilentRule(t *testing.T) {
	for text, want := range map[string]bool{
		"[不回复]":       true,
		"  [不回复]\n":   true,
		"没有新私信。[不回复]": true,
		"[不回复] 没什么新的": true,
		"按规则，没事就回 [不回复]，这次有新私信": false,
		"有新私信，已经回了":             false,
	} {
		if got := scheduleSilent(text); got != want {
			t.Errorf("%q：%v，应该是 %v", text, got, want)
		}
	}
}
