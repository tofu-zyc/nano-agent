package session

import (
	"strings"
	"testing"

	"nano-agent/gateway/internal/llm"
)

func call(id string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "t", Arguments: "{}"}}
}

func roles(h []Msg) string {
	var b strings.Builder
	for _, m := range h {
		b.WriteString(m.Role[:1])
		if m.ToolCallID != "" {
			b.WriteString("(" + m.ToolCallID + ")")
		}
	}
	return b.String()
}

func TestSanitizeFillsMissingResultsAndDropsOrphans(t *testing.T) {
	h := []Msg{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("a"), call("b")}},
		{Role: "tool", ToolCallID: "a", Content: "A"},
		// b 的结果丢了（比如进程崩溃）
		{Role: "user", Content: "again"},
		{Role: "tool", ToolCallID: "zzz", Content: "孤立结果"},
		{Role: "assistant", Content: "ok"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("c")}},
	}
	got := roles(Sanitize(h))
	want := "uat(a)t(b)uaat(c)"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestTrimCutsAtUserMessagesAndKeepsCurrentTurn(t *testing.T) {
	long := strings.Repeat("字", 300) // 900 字节 ≈ 300 token
	h := []Msg{
		{Role: "user", Content: long},
		{Role: "assistant", Content: long},
		{Role: "user", Content: long},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("x")}},
		{Role: "tool", ToolCallID: "x", Content: long},
		{Role: "assistant", Content: "short"},
		{Role: "user", Content: long},
	}
	// 预算只够最后一轮
	if got := roles(Trim(h, 400)); got != "u" {
		t.Fatalf("budget 400: got %s", got)
	}
	// 够最后两轮：从第二条用户消息开始，工具调用和结果一起保留
	if got := roles(Trim(h, 1300)); got != "uat(x)au" {
		t.Fatalf("budget 1300: got %s", got)
	}
	// 当前这一轮本身超预算也不能丢
	if got := roles(Trim(h, 10)); got != "u" {
		t.Fatalf("budget 10: got %s", got)
	}
	if got := roles(Trim(h, 1<<20)); got != roles(h) {
		t.Fatalf("budget big: got %s", got)
	}
}

func TestBuildMessagesSendsReasoningOnlyForCurrentTurn(t *testing.T) {
	h := []Msg{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", Reasoning: "old"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("c")}, Reasoning: "new"},
		{Role: "tool", ToolCallID: "c", Content: "r"},
	}
	msgs := BuildMessages("SYS", h, 1<<20, true)
	if msgs[0].Role != "system" || msgs[0].Content != "SYS" {
		t.Fatalf("system: %+v", msgs[0])
	}
	if msgs[2].ReasoningContent != "" || msgs[4].ReasoningContent != "new" {
		t.Fatalf("reasoning: %q %q", msgs[2].ReasoningContent, msgs[4].ReasoningContent)
	}
	if got := BuildMessages("", h, 1<<20, false); got[3].ReasoningContent != "" || got[0].Role != "user" {
		t.Fatalf("no reasoning / no system: %+v", got)
	}
}

func TestStorePersistsAndResets(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	sid := "tg:-100/1"
	for _, m := range []Msg{{Role: "user", Content: "你好"}, {Role: "assistant", Content: "嗨"}} {
		if err := s.Append(sid, m); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := New(dir, 1000)
	if got := s2.History(sid); len(got) != 2 || got[1].Content != "嗨" {
		t.Fatalf("reload: %+v", got)
	}
	if err := s2.Reset(sid); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir, 1000)
	if got := s3.History(sid); len(got) != 0 {
		t.Fatalf("after reset: %+v", got)
	}
}
