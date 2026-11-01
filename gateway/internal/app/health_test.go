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
	if strings.Join(names, ",") != "autonomy,dispatch,runtime_events,schedule,storage" {
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
	if h, _ := healthOf(e.a, "channel:tg.t1"); h.State != health.Error || h.Detail != "掉线了" {
		t.Fatalf("渠道自己报错: %+v", h)
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
