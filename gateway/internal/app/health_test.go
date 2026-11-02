package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

func healthOf(a *App, name string) (health.Entry, bool) {
	for _, e := range a.Status().Health {
		if e.Name == name {
			return e, true
		}
	}
	return health.Entry{}, false
}

func TestHealthStartsAllOKAndIsOnStatus(t *testing.T) {
	e := setup(t)
	st := e.a.Status()
	var names []string
	for _, h := range st.Health {
		names = append(names, h.Name)
		if h.State != health.OK {
			t.Fatalf("新网关应全部正常: %+v", h)
		}
	}
	if strings.Join(names, ",") != "autonomy,commitments,dispatch,outbox,runtime_events,schedule,stage,storage" {
		t.Fatalf("第一批条目: %v", names)
	}
	if strings.Contains(e.a.StatusBrief(), "健康：") {
		t.Fatalf("/status 不列正常的条目: %s", e.a.StatusBrief())
	}
}

func TestHealthStorageListsIssuesExceptSeparatelyReported(t *testing.T) {
	e := setup(t)
	boom := errors.New("坏了")
	e.a.mu.Lock()
	e.a.store.RecordStorageIssue("execution_leases", boom)
	e.a.store.RecordStorageIssue("runtime_events", boom)
	e.a.mu.Unlock()
	if h, _ := healthOf(e.a, "storage"); h.State != health.OK {
		t.Fatalf("这两个 scope 由自己的条目报告，storage 不重复: %+v", h)
	}
	e.a.mu.Lock()
	for _, s := range []string{"hook_event", "jobs:laptop", "tg:1", "persona"} {
		e.a.store.RecordStorageIssue(s, boom)
	}
	e.a.mu.Unlock()
	h, _ := healthOf(e.a, "storage")
	if h.State != health.Degraded || !strings.HasPrefix(h.Detail, "4 处：") || !strings.Contains(h.Detail, " 等") ||
		!strings.Contains(h.Detail, "修复后重启网关重新检查") || strings.Contains(h.Detail, "execution_leases") {
		t.Fatalf("storage 条目运行中也要刷新，列概括: %+v", h)
	}
	if brief := e.a.StatusBrief(); !strings.Contains(brief, "健康：storage（degraded）") {
		t.Fatalf("/status 列出不正常的: %s", brief)
	}
}

// 启动时恢复失败的部件各自报 error，而且互相不牵连。
func TestHealthStartupFailures(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	os.MkdirAll(filepath.Join(data, "runtime"), 0o755)
	for _, f := range []string{"runtime/leases.json", "runtime/events.jsonl", "schedules.json", "autonomy.json"} {
		if err := os.WriteFile(filepath.Join(data, f), []byte("{坏的\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		LLM:       config.LLM{Provider: "mock", MaxHistoryTokens: 10000, TrimTo: 0.5},
		Persona:   config.Persona{Dir: filepath.Join(dir, "personas")},
		Skills:    config.Skills{Dir: filepath.Join(dir, "skills")},
		Workspace: config.Workspace{Mode: "ask"},
		Dir:       filepath.Join(dir, "conf"),
	}
	store, err := session.New(data)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(ctx, cfg, &recLLM{}, store, persona.New(cfg.Persona.Dir, log), log)
	a.mu.Lock()
	a.loadMinds()
	a.mu.Unlock()
	for _, name := range []string{"dispatch", "runtime_events", "schedule", "autonomy"} {
		h, ok := healthOf(a, name)
		if !ok || h.State != health.Error || h.Detail == "" {
			t.Fatalf("%s 应报 error: %+v", name, h)
		}
	}
	// 同一个故障只在一处出现：租约和事件日志不重复进 storage
	if h, _ := healthOf(a, "storage"); h.State != health.OK {
		t.Fatalf("storage 不重复列: %+v", h)
	}
	if !health.HasError(a.Status().Health) {
		t.Fatal("有 error 条目")
	}
}

// statusCh 是能报状态的渠道。
type statusCh struct {
	fakeCh
	mu sync.Mutex
	cb func(channel.Status)
	st channel.Status
}

func (c *statusCh) Status() channel.Status { c.mu.Lock(); defer c.mu.Unlock(); return c.st }
func (c *statusCh) OnStatus(f func(channel.Status)) {
	c.mu.Lock()
	c.cb = f
	c.mu.Unlock()
}
func (c *statusCh) report(st channel.Status) {
	c.mu.Lock()
	c.st = st
	cb := c.cb
	c.mu.Unlock()
	cb(st)
}

func TestHealthChannelLifecycle(t *testing.T) {
	e := setup(t)
	e.a.channelGrace = time.Hour // 这个测试不关心宽限过后的升级
	cfg := e.a.cfg
	e.a.ReconcileChannels(cfg) // 初始化渠道表，网页和终端渠道没有健康条目
	if _, ok := healthOf(e.a, "channel:web"); ok {
		t.Fatal("网页渠道不该有条目")
	}
	mk := func() *statusCh {
		return &statusCh{fakeCh: fakeCh{name: "tg.t1", sent: make(chan string, 10)}, st: channel.Status{State: channel.StateStarting}}
	}
	reconcile := func(want bool, build func() (channel.Channel, error)) {
		e.a.chMu.Lock()
		defer e.a.chMu.Unlock()
		e.a.reconcile("tg.t1", want, "conf", build)
	}

	// 构建失败也有条目，显示 error
	reconcile(true, func() (channel.Channel, error) { return nil, errors.New("token 无效") })
	if h, ok := healthOf(e.a, "channel:tg.t1"); !ok || h.State != health.Error || h.Detail != "token 无效" {
		t.Fatalf("构建失败: %+v %v", h, ok)
	}

	// 重新启用且构建成功：同名重新认领，连接中还算正常
	first := mk()
	reconcile(true, func() (channel.Channel, error) { return first, nil })
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.OK || h.Detail != "连接中" {
		t.Fatalf("连接中: %+v", h)
	}
	first.report(channel.Status{State: channel.StateOnline})
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.OK || h.Detail != "" {
		t.Fatalf("在线: %+v", h)
	}
	first.report(channel.Status{State: channel.StateError, Error: "掉线了"})
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.Degraded || h.Detail != "掉线了" {
		t.Fatalf("渠道自己报错先算 degraded: %+v", h)
	}

	// 停用：条目消失
	reconcile(false, nil)
	if _, ok := healthOf(e.a, "channel:tg.t1"); ok {
		t.Fatal("停用的渠道不留条目")
	}
	// 渠道停止之后迟到的回报写不到新认领的同名条目上
	second := mk()
	reconcile(true, func() (channel.Channel, error) { return second, nil })
	first.report(channel.Status{State: channel.StateError, Error: "迟到的旧回报"})
	if h, ok := healthOf(e.a, "channel:tg.t1"); !ok || h.State != health.OK || h.Detail != "连接中" {
		t.Fatalf("旧渠道的回报不能改新条目: %+v", h)
	}
	// 配置变了重启：旧回报同样无效
	third := mk()
	e.a.chMu.Lock()
	e.a.reconcile("tg.t1", true, "conf2", func() (channel.Channel, error) { return third, nil })
	e.a.chMu.Unlock()
	second.report(channel.Status{State: channel.StateStopped})
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.OK {
		t.Fatalf("重启后旧渠道的回报无效: %+v", h)
	}
	third.report(channel.Status{State: channel.StateStopped})
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.Error || h.Detail != "已停止" {
		t.Fatalf("启用的渠道停了算 error: %+v", h)
	}

	// 机器人被删除：不在列表里的条目撤掉，包括没在跑的（构建失败的）
	e.a.chMu.Lock()
	e.a.reconcile("tg.gone", true, "c", func() (channel.Channel, error) { return nil, errors.New("x") })
	e.a.reconcileBotsLocked(cfg, nil)
	e.a.chMu.Unlock()
	for _, n := range []string{"channel:tg.gone", "channel:tg.t1"} {
		if _, ok := healthOf(e.a, n); ok {
			t.Fatalf("机器人删除后 %s 应撤掉", n)
		}
	}
	e.a.chMu.Lock()
	_, stale := e.a.chErr["tg.gone"]
	e.a.chMu.Unlock()
	if stale {
		t.Fatal("删除的机器人不该留下 chErr")
	}
}

// 渠道自己报的 error 持续超过宽限才算 error；一次抖动不算，前一次的定时器也不能提前判后一次。
func TestHealthChannelErrorGrace(t *testing.T) {
	e := setup(t)
	e.a.channelGrace = 600 * time.Millisecond
	e.a.ReconcileChannels(e.a.cfg)
	ch := &statusCh{fakeCh: fakeCh{name: "tg.g1", sent: make(chan string, 10)}, st: channel.Status{State: channel.StateOnline}}
	e.a.chMu.Lock()
	e.a.reconcile("tg.g1", true, "c", func() (channel.Channel, error) { return ch, nil })
	e.a.chMu.Unlock()
	state := func() health.State { h, _ := healthOf(e.a, "channel:tg.g1"); return h.State }

	ch.report(channel.Status{State: channel.StateError, Error: "断了"}) // 第一次：0ms，定时器在 600ms
	if state() != health.Degraded {
		t.Fatalf("宽限内是 degraded: %v", state())
	}
	time.Sleep(50 * time.Millisecond)
	ch.report(channel.Status{State: channel.StateOnline}) // 抖动：恢复
	if state() != health.OK {
		t.Fatalf("恢复: %v", state())
	}
	time.Sleep(150 * time.Millisecond)
	ch.report(channel.Status{State: channel.StateError, Error: "又断了"}) // 第二次：200ms，定时器在 800ms
	time.Sleep(450 * time.Millisecond)                                 // 650ms：第一次的定时器已经过期，但不能算数
	if state() != health.Degraded {
		t.Fatalf("前一次的定时器不能提前判后一次: %v", state())
	}
	deadline := time.Now().Add(3 * time.Second)
	for state() != health.Error {
		if time.Now().After(deadline) {
			t.Fatal("持续出错超过宽限应升级为 error")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h, _ := healthOf(e.a, "channel:tg.g1"); h.Detail != "又断了" {
		t.Fatalf("说明是这一次的: %+v", h)
	}
	ch.report(channel.Status{State: channel.StateOnline}) // 升级以后恢复
	if state() != health.OK {
		t.Fatalf("恢复后回到 ok: %v", state())
	}
	// 渠道停了（启用的渠道不该停）不等宽限，直接 error
	ch.report(channel.Status{State: channel.StateStopped})
	if state() != health.Error {
		t.Fatalf("停止直接 error: %v", state())
	}
}

// 出错期间报来的“连接中”不算恢复：error → starting → error 超过宽限仍要变成 error。
func TestHealthChannelStartingDoesNotResetGrace(t *testing.T) {
	e := setup(t)
	e.a.channelGrace = 300 * time.Millisecond
	e.a.ReconcileChannels(e.a.cfg)
	ch := &statusCh{fakeCh: fakeCh{name: "tg.g2", sent: make(chan string, 10)}, st: channel.Status{State: channel.StateOnline}}
	e.a.chMu.Lock()
	e.a.reconcile("tg.g2", true, "c", func() (channel.Channel, error) { return ch, nil })
	e.a.chMu.Unlock()
	state := func() health.State { h, _ := healthOf(e.a, "channel:tg.g2"); return h.State }
	// 每 50ms 在 error 和 starting 之间来回报，始终连不上
	deadline := time.Now().Add(3 * time.Second)
	for state() != health.Error {
		if time.Now().After(deadline) {
			t.Fatal("始终连不上的渠道应在宽限后变成 error")
		}
		ch.report(channel.Status{State: channel.StateError, Error: "连不上"})
		time.Sleep(25 * time.Millisecond)
		ch.report(channel.Status{State: channel.StateStarting})
		if s := state(); s == health.OK {
			t.Fatalf("starting 不能把出错中的条目改回 ok: %v", s)
		}
		time.Sleep(25 * time.Millisecond)
	}
	ch.report(channel.Status{State: channel.StateOnline})
	if state() != health.OK {
		t.Fatal("online 才算恢复")
	}
}
