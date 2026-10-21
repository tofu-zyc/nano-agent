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
	})
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
	res, err := c.Chat(context.Background(), Request{})
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
	if _, err := c.Chat(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "长度上限") {
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
		res, err := c.Chat(context.Background(), Request{})
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
	if res, err := c.Chat(context.Background(), Request{}); err != nil || res.Content != "ok" || n.Load() != 3 {
		t.Fatalf("res=%+v err=%v n=%d", res, err, n.Load())
	}

	n.Store(0)
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, `{"error":{"message":"bad tool schema"}}`, http.StatusBadRequest)
	})
	_, err := c.Chat(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad tool schema") || n.Load() != 1 {
		t.Fatalf("400 should not retry: err=%v n=%d", err, n.Load())
	}
}

func TestMock(t *testing.T) {
	m := &Mock{}
	res, _ := m.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: `[张三] !call echo {"a":1}`}}})
	if res.Reason != ReasonToolUse || res.Calls[0].Function.Name != "echo" || res.Calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("%+v", res)
	}
	res, _ = m.Chat(context.Background(), Request{Messages: []Message{{Role: "user"}, {Role: "tool", Content: "A"}, {Role: "tool", Content: "B"}}})
	if res.Content != "（mock）工具结果：A | B" {
		t.Fatalf("%+v", res)
	}
}
