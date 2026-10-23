package app

import (
	"context"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"strings"
	"testing"
	"time"
)

func TestSummaryChunksBounded(t *testing.T) {
	h := []session.Msg{{Role: "user", Content: strings.Repeat("长", 30000)}, {Role: "tool", Content: strings.Repeat("大", 20000)}}
	chunks := summaryChunks(h, 2000)
	if len(chunks) < 2 {
		t.Fatal("not chunked")
	}
	for _, c := range chunks {
		if requestTokens(llm.SummaryRequest("", c)) > 2000 {
			t.Fatal("oversized chunk")
		}
	}
}
func TestIdleCompactAndQueuedInput(t *testing.T) {
	e := setup(t)
	seedCompaction(t, e)
	e.a.cfg.Context.IdleCompact = "30m"
	e.a.cfg.Context.IdleCompactMin = 0
	release := make(chan struct{})
	m := &summaryLLM{requests: make(chan llm.Request, 50), summarize: func(ctx context.Context) (*llm.Result, error) {
		select {
		case <-release:
			return &llm.Result{Reason: llm.ReasonFinal, Content: "摘要"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	e.a.SetLLM(m)
	e.a.mu.Lock()
	s := e.a.sessOf(e.sid)
	s.lastActivity = e.a.now().Add(-time.Hour)
	e.a.mu.Unlock()
	e.a.Maintain()
	m.next(t)
	e.say("整理期间排队")
	e.a.mu.Lock()
	if len(s.queue) != 1 || s.run != nil {
		t.Fatal("input did not queue")
	}
	e.a.mu.Unlock()
	close(release)
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if !s.idleCompacted && s.lastActivity.IsZero() {
		t.Fatal("idle state")
	}
}
func TestManualCompactFocus(t *testing.T) {
	e := setup(t)
	seedCompaction(t, e)
	m := &summaryLLM{requests: make(chan llm.Request, 50), summarize: func(context.Context) (*llm.Result, error) {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "保留部署目录"}, nil
	}}
	e.a.SetLLM(m)
	e.say("/compact 保留部署目录")
	if !strings.Contains(m.next(t).Messages[1].Content, "保留部署目录") {
		t.Fatal("focus missing")
	}
	waitFor(t, func() bool { e.a.mu.Lock(); defer e.a.mu.Unlock(); return !e.a.sessOf(e.sid).compacting })
}

func TestSummaryFailureKeepsFirstLayerOnly(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	for i := 0; i < 6; i++ {
		_ = e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "读文件"})
		_ = e.a.store.Append(e.sid, session.Msg{Role: "tool", Content: strings.Repeat("原", 5000)})
	}
	before := e.a.store.Archive(e.sid)
	e.a.mu.Unlock()
	m := &summaryLLM{requests: make(chan llm.Request, 50), summarize: func(context.Context) (*llm.Result, error) { return nil, context.DeadlineExceeded }}
	e.a.SetLLM(m)
	e.a.mu.Lock()
	s := e.a.sessOf(e.sid)
	e.a.beginCompact(s, 1000000, 0, "", true)
	e.a.mu.Unlock()
	m.next(t)
	waitFor(t, func() bool { e.a.mu.Lock(); defer e.a.mu.Unlock(); return !s.compacting })
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	st := e.a.store.Settings(e.sid)
	if st.Base != 0 || len(st.Omitted) != 2 || e.a.store.Archive(e.sid)[1].Content != before[1].Content {
		t.Fatal("failure discarded history or first layer")
	}
}

func TestContextText(t *testing.T) {
	for n, want := range map[int]string{999: "999", 1000: "1k", 41234: "41.2k", 96000: "96k", 128000: "128k"} {
		if got := tokenText(n); got != want {
			t.Errorf("tokenText(%d) = %q，想要 %q", n, got, want)
		}
	}
	if durationText(30*time.Minute) != "30 分钟" || durationText(2*time.Hour) != "2 小时" {
		t.Error("durationText")
	}
	e := setup(t)
	e.say("/context")
	e.ch.wait(t, "上次请求：暂无（新会话或刚整理过），上面的数字是估算")
	e.say("你好")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.a.mu.Lock()
	e.a.cfg.Context.IdleCompact = "0"
	e.a.mu.Unlock()
	e.say("/context")
	if got := e.ch.wait(t, "上次请求：输入 "); !strings.Contains(got, "时自动整理；空闲不自动整理；/compact 现在整理") {
		t.Fatalf("/context：%s", got)
	}
}
