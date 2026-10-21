package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

type summaryLLM struct {
	llm.Mock
	requests  chan llm.Request
	summarize func(context.Context) (*llm.Result, error)
}

func (m *summaryLLM) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Result, error) {
	m.requests <- req
	if req.MaxTokens > 0 {
		return m.summarize(ctx)
	}
	return m.Mock.Chat(ctx, req, onDelta)
}

func (m *summaryLLM) next(t *testing.T) llm.Request {
	t.Helper()
	select {
	case req := <-m.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到模型请求")
		return llm.Request{}
	}
}

func seedCompaction(t *testing.T, e *env) {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	e.a.cfg.LLM.MaxHistoryTokens = 1000
	e.a.cfg.LLM.ThinkingLevels = []string{"none", "high"}
	st := e.a.store.Settings(e.sid)
	st.Summary = "[系统] 更早对话的摘要：\n已决定使用 C 内核和 Go 网关。"
	if err := e.a.store.SaveSettings(e.sid, st); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		for _, role := range []string{"user", "assistant"} {
			if err := e.a.store.Append(e.sid, session.Msg{Role: role, Content: strings.Repeat("旧", 300), Reasoning: "不应交给摘要的推理"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 最近一轮的工具调用与结果应当完整保留。
	for _, msg := range []session.Msg{
		{Role: "user", Content: "检查实现"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "Read", Arguments: `{"path":"main.c"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "最近的工具结果"},
		{Role: "assistant", Content: "检查完成"},
	} {
		if err := e.a.store.Append(e.sid, msg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSummaryCompactionMergesOldSummaryAndPreservesRecentTurn(t *testing.T) {
	e := setup(t)
	seedCompaction(t, e)
	e.a.cfg.Workspace.Dirs = []string{e.dir}
	m := &summaryLLM{requests: make(chan llm.Request, 10), summarize: func(context.Context) (*llm.Result, error) {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "已采用 C 内核和 Go 网关，待继续实现。", Usage: llm.Usage{PromptTokens: 800, CompletionTokens: 30, CachedTokens: 200}}, nil
	}}
	e.a.SetLLM(m)
	e.say("继续当前任务")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	if turn := e.k.expect(proto.TTurn); turn.Reason != proto.ReasonFinal {
		t.Fatalf("turn: %+v", turn)
	}
	summaryReq, normalReq := m.next(t), m.next(t)
	if len(summaryReq.Tools) != 0 || summaryReq.MaxTokens != 2048 || summaryReq.Thinking != "none" {
		t.Fatalf("摘要请求: %+v", summaryReq)
	}
	var data struct {
		Previous string        `json:"previous_summary"`
		Messages []llm.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(summaryReq.Messages[1].Content), &data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data.Previous, "C 内核和 Go 网关") || len(data.Messages) != 6 {
		t.Fatalf("未合并旧摘要或切错历史: %+v", data)
	}
	for _, msg := range data.Messages {
		if msg.ReasoningContent != "" || strings.Contains(msg.Content, "继续当前任务") {
			t.Fatalf("摘要里出现推理或当前任务: %+v", msg)
		}
	}
	if len(normalReq.Tools) == 0 || normalReq.MaxTokens != 0 || len(normalReq.Messages) != 6 || !strings.Contains(normalReq.Messages[1].Content, "待继续实现") || normalReq.Messages[3].ToolCallID != "c1" || !strings.HasSuffix(lastUser(normalReq.Messages), "继续当前任务") {
		t.Fatalf("正常请求未保留摘要、近期工具链或当前输入: %+v", normalReq)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：继续当前任务")
	e.a.mu.Lock()
	st := e.a.store.Settings(e.sid)
	totals := e.a.totals
	e.a.mu.Unlock()
	if st.Base != 6 || !strings.Contains(st.Summary, "工作目录") || totals.ModelCalls != 2 || totals.PromptTokens != 900 || totals.CompletionTokens != 40 || totals.CachedTokens != 290 {
		t.Fatalf("设置或统计: %+v %+v", st, totals)
	}
	store, err := session.New(e.a.store.Dir())
	if err != nil || store.Settings(e.sid).Summary != st.Summary || len(store.History(e.sid)) != 6 {
		t.Fatalf("重启未恢复摘要和历史: %v", err)
	}
	// 刚压缩完不再调摘要，新的请求沿用相同前缀。
	e.say("接着做")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	next := m.next(t)
	if next.MaxTokens != 0 || !reflect.DeepEqual(next.Messages[:len(normalReq.Messages)], normalReq.Messages) {
		t.Fatalf("压缩后请求没有只追加: %+v", next)
	}
}

func TestSummaryFailureFallsBackAndKeepsPreviousSummary(t *testing.T) {
	for _, kind := range []string{"error", "timeout", "empty", "refused", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t)
			seedCompaction(t, e)
			m := &summaryLLM{requests: make(chan llm.Request, 10), summarize: func(context.Context) (*llm.Result, error) {
				switch kind {
				case "error":
					return nil, errors.New("模型忙")
				case "timeout":
					return nil, context.DeadlineExceeded
				case "refused":
					return &llm.Result{Reason: llm.ReasonRefused, Content: "不行"}, nil
				case "truncated":
					return &llm.Result{Reason: llm.ReasonFinal, Content: "半截", Truncated: true}, nil
				default:
					return &llm.Result{Reason: llm.ReasonFinal}, nil
				}
			}}
			e.a.SetLLM(m)
			e.say("继续")
			e.k.expect(proto.TInput)
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
			if turn := e.k.expect(proto.TTurn); turn.Reason != proto.ReasonFinal {
				t.Fatalf("降级后未继续: %+v", turn)
			}
			m.next(t)
			req := m.next(t)
			if !strings.Contains(req.Messages[1].Content, "C 内核和 Go 网关") || !strings.Contains(req.Messages[1].Content, "已经省略") {
				t.Fatalf("降级丢失已有摘要或缺少说明: %+v", req)
			}
		})
	}
}

func TestResetCancelsSummaryWithoutChangingNewHistory(t *testing.T) {
	e := setup(t)
	seedCompaction(t, e)
	release, returned := make(chan struct{}), make(chan struct{})
	m := &summaryLLM{requests: make(chan llm.Request, 10), summarize: func(ctx context.Context) (*llm.Result, error) {
		<-ctx.Done()
		<-release // 模拟取消后才迟到的结果
		defer close(returned)
		return &llm.Result{Reason: llm.ReasonFinal, Content: "不应写入的旧摘要"}, nil
	}}
	e.a.SetLLM(m)
	e.say("继续")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	m.next(t)
	e.say("/reset")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "上下文已清空")
	e.say("新任务")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	req := m.next(t)
	close(release)
	<-returned
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：新任务")
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if st := e.a.store.Settings(e.sid); st.Base != 0 || st.Summary != "" || len(req.Messages) != 2 {
		t.Fatalf("迟到的摘要改动了新上下文: %+v %+v", st, req)
	}
}
