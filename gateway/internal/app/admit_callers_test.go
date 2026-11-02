package app

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/schedule"
)

// 收成 admitRun 之后，各处的延后时间和失败的说法保持原样。

// dueAgenda 布置好一个到点的约定，时间推到它之后一分钟，返回现在的时间。
func dueAgenda(t *testing.T, e *env) time.Time {
	t.Helper()
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	addAgendaCall(t, e, `{"action":"add","title":"去查一件事","kind":"commitment","when":"15:10"}`)
	now := base.Add(11 * time.Minute)
	setNow(e, now)
	return now
}

func firstAgenda(e *env) autonomy.AgendaItem {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.mind("default").OpenAgenda()[0]
}

func TestAdmitAutonomyTickBusyWaitsTwoMinutes(t *testing.T) {
	e := setup(t)
	now := dueAgenda(t, e)
	planned := firstAgenda(e).At
	e.a.mu.Lock()
	e.a.sessOf(lifeSID("default")).compacting = true
	e.a.mu.Unlock()
	e.a.autonomyTick()
	e.k.none()
	e.a.mu.Lock()
	next := e.a.mind("default").NextCheck
	e.a.sessOf(lifeSID("default")).compacting = false
	e.a.mu.Unlock()
	if next != now.Add(2*time.Minute).Unix() {
		t.Fatalf("会话忙：2 分钟后再看，实际 %v", time.Unix(next, 0).Sub(now))
	}
	if got := firstAgenda(e); got.At != planned || got.Result != "" {
		t.Fatalf("忙的时候不能动约定：%+v", got)
	}
}

func TestAdmitAutonomyTickNoModelAndNoKernelRetry(t *testing.T) {
	for name, c := range map[string]struct {
		cut   func(*testing.T, *env)
		delay time.Duration
		text  string
	}{
		"没模型": {func(t *testing.T, e *env) { noModels(t, e) }, 30 * time.Minute, "还没有配置模型"},
		"没内核": {func(t *testing.T, e *env) { e.a.mu.Lock(); noKernels(e); e.a.mu.Unlock() }, 5 * time.Minute, "没有在线的内核"},
	} {
		e := setup(t)
		now := dueAgenda(t, e)
		c.cut(t, e)
		e.a.autonomyTick()
		e.k.none()
		got := firstAgenda(e)
		if got.At != now.Add(c.delay).Unix() || !strings.Contains(got.Result, c.text) {
			t.Fatalf("%s：约定改到 %v 后，说明 %q", name, time.Unix(got.At, 0).Sub(now), got.Result)
		}
	}
}

func TestAdmitStartLifeMessages(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	start := func() string {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		err := e.a.startLife("default", true)
		if err == nil {
			return ""
		}
		return err.Error()
	}
	e.a.mu.Lock()
	e.a.sessOf(lifeSID("default")).compacting = true
	e.a.mu.Unlock()
	if got := start(); got != "它正在想事情" {
		t.Fatalf("忙：%q", got)
	}
	e.a.mu.Lock()
	e.a.sessOf(lifeSID("default")).compacting = false
	noKernels(e)
	e.a.mu.Unlock()
	if got := start(); got != "没有在线的内核" {
		t.Fatalf("没内核：%q", got)
	}
	noModels(t, e)
	if got := start(); got != "还没有配置模型" {
		t.Fatalf("没模型在没内核前面：%q", got)
	}
}

func TestAdmitStartDreamMessages(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	withModels(t, e)
	start := func() string {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		if err := e.a.startDream("default", true); err != nil {
			return err.Error()
		}
		return ""
	}
	e.a.mu.Lock()
	e.a.sessOf(dreamSID("default")).compacting = true
	e.a.mu.Unlock()
	if got := start(); got != "正在整理记忆" {
		t.Fatalf("忙：%q", got)
	}
	e.a.mu.Lock()
	e.a.sessOf(dreamSID("default")).compacting = false
	noKernels(e)
	e.a.mu.Unlock()
	if got := start(); got != "没有在线的内核" {
		t.Fatalf("没内核：%q", got)
	}
	noModels(t, e)
	if got := start(); got != "还没有配置模型" {
		t.Fatalf("没模型：%q", got)
	}
}

func TestAdmitLaunchScheduleBusy(t *testing.T) {
	e := setup(t)
	task := schedule.Task{ID: "t1", Name: "x", Prompt: "p"}
	e.a.mu.Lock()
	e.a.sessOf(cronSID(task)).queue = append(e.a.sessOf(cronSID(task)).queue, queued{})
	err := e.a.launchSchedule(task, false)
	e.a.sessOf(cronSID(task)).queue = nil
	e.a.mu.Unlock()
	if err == nil || err.Error() != "任务会话正在使用" {
		t.Fatalf("%v", err)
	}
}

// FollowUp 到点：会话忙就等；没模型、没内核不拦（照旧发出去，由聊天那边告诉对方），约定照旧取走。
func TestAdmitFollowUpOnlyWaitsWhenBusy(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.say("你好")
	e.k.expect("input")
	e.roundTrip(t)
	ag, _ := store.Get(agents.DefaultID)
	due := func() {
		e.a.mu.Lock()
		e.a.mind("default").FollowUpMap()[mainSID] = autonomy.FollowUp{At: e.a.now().Add(-time.Minute).Unix(), Note: "问问"}
		e.a.mu.Unlock()
	}
	left := func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		_, ok := e.a.mind("default").FollowUps[mainSID]
		return ok
	}
	due()
	e.a.mu.Lock()
	e.a.sessOf(mainSID).compacting = true
	e.a.followUpTick(ag, e.a.mind("default"), e.a.now())
	e.a.sessOf(mainSID).compacting = false
	e.a.mu.Unlock()
	if !left() {
		t.Fatal("正在聊：约定要留着")
	}
	noModels(t, e)
	e.a.mu.Lock()
	e.a.followUpTick(ag, e.a.mind("default"), e.a.now())
	e.a.mu.Unlock()
	if left() {
		t.Fatal("没模型不拦：约定照旧取走")
	}
}
