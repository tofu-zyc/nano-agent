package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nano-agent/gateway/internal/config"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	temp := 0.3
	c, err := NewOpenAI(config.LLM{
		BaseURL: srv.URL + "/v1/", APIKey: "sk-test", Model: "m1", Timeout: 5, Proxy: "none",
		Temperature: &temp, MaxTokens: 100, Extra: map[string]any{"chat_template_kwargs": map[string]any{"thinking": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	return c
}

func reply(w http.ResponseWriter, finish string, msg map[string]any) {
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
}

func TestRequestShape(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		reply(w, "stop", map[string]any{"content": "你好", "reasoning": "想了想"})
	})
	res, err := c.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonFinal || res.Content != "你好" || res.Reasoning != "想了想" || res.Usage.PromptTokens != 10 {
		t.Fatalf("result: %+v", res)
	}
	if got["model"] != "m1" || got["temperature"] != 0.3 || got["max_tokens"] != float64(100) {
		t.Fatalf("body: %v", got)
	}
	if kw, _ := got["chat_template_kwargs"].(map[string]any); kw["thinking"] != false {
		t.Fatalf("extra not merged: %v", got)
	}
	if tools, _ := got["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools: %v", got["tools"])
	}
}

func TestToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, "tool_calls", map[string]any{"content": nil, "tool_calls": []any{
			map[string]any{"id": "x1", "type": "function", "function": map[string]any{"name": "get_time", "arguments": ""}},
			map[string]any{"type": "function", "function": map[string]any{"name": "echo", "arguments": `{"a":1}`}},
		}})
	})
	res, err := c.Chat(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonToolUse || len(res.Calls) != 2 {
		t.Fatalf("%+v", res)
	}
	if res.Calls[0].Function.Arguments != "{}" || res.Calls[1].ID != "call_1" {
		t.Fatalf("normalization: %+v", res.Calls)
	}
}

func TestToolCallsCutOffAreAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, "length", map[string]any{"tool_calls": []any{
			map[string]any{"id": "x", "type": "function", "function": map[string]any{"name": "t", "arguments": `{"a":`}},
		}})
	})
	if _, err := c.Chat(context.Background(), Request{}, nil); err == nil || !strings.Contains(err.Error(), "长度上限") {
		t.Fatalf("want error, got %v", err)
	}
}

func TestFinishReasons(t *testing.T) {
	cases := map[string]struct {
		finish string
		msg    map[string]any
		reason string
		trunc  bool
	}{
		"length":   {"length", map[string]any{"content": "半截"}, ReasonFinal, true},
		"filter":   {"content_filter", map[string]any{"content": ""}, ReasonRefused, false},
		"refusal":  {"stop", map[string]any{"content": nil, "refusal": "不行"}, ReasonRefused, false},
		"stopNull": {"stop", map[string]any{"content": nil}, ReasonFinal, false},
	}
	for name, tc := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.finish, tc.msg) })
		res, err := c.Chat(context.Background(), Request{}, nil)
		if err != nil || res.Reason != tc.reason || res.Truncated != tc.trunc {
			t.Errorf("%s: res=%+v err=%v", name, res, err)
		}
	}
}

func TestRetries(t *testing.T) {
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable)
			return
		}
		reply(w, "stop", map[string]any{"content": "ok"})
	})
	if res, err := c.Chat(context.Background(), Request{}, nil); err != nil || res.Content != "ok" || n.Load() != 3 {
		t.Fatalf("res=%+v err=%v n=%d", res, err, n.Load())
	}

	n.Store(0)
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, `{"error":{"message":"bad tool schema"}}`, http.StatusBadRequest)
	})
	_, err := c.Chat(context.Background(), Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad tool schema") || n.Load() != 1 {
		t.Fatalf("400 should not retry: err=%v n=%d", err, n.Load())
	}
}

func TestMock(t *testing.T) {
	m := &Mock{}
	res, _ := m.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: `[张三] !call echo {"a":1}`}}}, nil)
	if res.Reason != ReasonToolUse || res.Calls[0].Function.Name != "echo" || res.Calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("%+v", res)
	}
	res, _ = m.Chat(context.Background(), Request{Messages: []Message{{Role: "user"}, {Role: "tool", Content: "A"}, {Role: "tool", Content: "B"}}}, nil)
	if res.Content != "（mock）工具结果：A | B" {
		t.Fatalf("%+v", res)
	}
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		io.WriteString(w, "data: "+c+"\n\n")
		w.(http.Flusher).Flush()
	}
}

func TestStreaming(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		sse(w,
			`{"choices":[{"delta":{"role":"assistant","reasoning":"先想"}}]}`,
			`{"choices":[{"delta":{"reasoning":"一想"}}]}`,
			`{"choices":[{"delta":{"content":"你"}}]}`,
			`{"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":4}}}`,
			`[DONE]`)
	})
	var deltas []Delta
	res, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}, func(d Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if got["stream"] != true || got["stream_options"] == nil {
		t.Fatalf("body: %v", got)
	}
	want := []Delta{{Reasoning: "先想"}, {Reasoning: "一想"}, {Content: "你"}, {Content: "好"}}
	if len(deltas) != len(want) {
		t.Fatalf("deltas: %+v", deltas)
	}
	for i := range want {
		if deltas[i] != want[i] {
			t.Fatalf("delta %d: %+v", i, deltas[i])
		}
	}
	if res.Reason != ReasonFinal || res.Content != "你好" || res.Reasoning != "先想一想" {
		t.Fatalf("result: %+v", res)
	}
	if u := res.Usage; u.PromptTokens != 100 || u.CachedTokens != 64 || u.ReasoningTokens != 4 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestStreamingToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Shell"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": "}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"Read","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":5,"prompt_cache_hit_tokens":3}}`,
			`[DONE]`)
	})
	res, err := c.Chat(context.Background(), Request{}, func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonToolUse || len(res.Calls) != 2 || res.Usage.CachedTokens != 3 {
		t.Fatalf("result: %+v", res)
	}
	if c0 := res.Calls[0]; c0.ID != "c1" || c0.Function.Name != "Shell" || c0.Function.Arguments != `{"command": "ls"}` {
		t.Fatalf("call 0: %+v", c0)
	}
	if c1 := res.Calls[1]; c1.ID != "c2" || c1.Function.Arguments != "{}" {
		t.Fatalf("call 1: %+v", c1)
	}
}

func TestStreamingErrors(t *testing.T) {
	// 还没输出任何东西就失败：可以重试
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`)
	})
	if res, err := c.Chat(context.Background(), Request{}, func(Delta) {}); err != nil || res.Content != "ok" || n.Load() != 2 {
		t.Fatalf("res=%+v err=%v n=%d", res, err, n.Load())
	}
	// 输出了一半断开：不能重试（用户已经看到了前半截），报错
	n.Store(0)
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		sse(w, `{"choices":[{"delta":{"content":"半"}}]}`)
	})
	if _, err := c.Chat(context.Background(), Request{}, func(Delta) {}); err == nil || !strings.Contains(err.Error(), "断开") || n.Load() != 1 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
	// 流里的错误事件
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"error":{"message":"context too long"}}`)
	})
	if _, err := c.Chat(context.Background(), Request{}, func(Delta) {}); err == nil || !strings.Contains(err.Error(), "context too long") {
		t.Fatalf("err=%v", err)
	}
}

func TestThinkingStyles(t *testing.T) {
	cases := []struct {
		style, level string
		check        func(map[string]any) bool
	}{
		{"vllm", "high", func(b map[string]any) bool {
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			return kw["thinking"] == true && kw["reasoning_effort"] == "high"
		}},
		{"vllm", "50", func(b map[string]any) bool {
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			return kw["reasoning_effort"] == float64(50)
		}},
		{"vllm", "off", func(b map[string]any) bool {
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			_, has := kw["reasoning_effort"]
			return kw["thinking"] == false && !has
		}},
		{"openai", "low", func(b map[string]any) bool { return b["reasoning_effort"] == "low" }},
		{"openai", "off", func(b map[string]any) bool { return b["reasoning_effort"] == "none" }},
		{"vllm", "", func(b map[string]any) bool { // 不设置时只有 extra 里原来的参数
			kw, _ := b["chat_template_kwargs"].(map[string]any)
			_, has := kw["reasoning_effort"]
			return kw["thinking"] == false && !has
		}},
	}
	for _, tc := range cases {
		var got map[string]any
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &got)
			reply(w, "stop", map[string]any{"content": "ok"})
		})
		c.thinkingStyle = tc.style
		if _, err := c.Chat(context.Background(), Request{Thinking: tc.level}, nil); err != nil || !tc.check(got) {
			t.Errorf("%s/%s: body=%v err=%v", tc.style, tc.level, got, err)
		}
		// extra 是共享的配置，不能被改掉
		if kw := c.extra["chat_template_kwargs"].(map[string]any); kw["thinking"] != false || len(kw) != 1 {
			t.Fatalf("extra was modified: %v", c.extra)
		}
	}
}
