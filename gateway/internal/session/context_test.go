package session

import (
	"os"
	"reflect"
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

func TestCutPointCutsAtUserMessagesAndKeepsCurrentTurn(t *testing.T) {
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
	trim := func(h []Msg, budget int) []Msg { return h[CutPoint(h, budget):] }
	// 预算只够最后一轮
	if got := roles(trim(h, 400)); got != "u" {
		t.Fatalf("budget 400: got %s", got)
	}
	// 够最后两轮：从第二条用户消息开始，工具调用和结果一起保留
	if got := roles(trim(h, 1300)); got != "uat(x)au" {
		t.Fatalf("budget 1300: got %s", got)
	}
	// 当前这一轮本身超预算也不能丢
	if got := roles(trim(h, 10)); got != "u" {
		t.Fatalf("budget 10: got %s", got)
	}
	if got := roles(trim(h, 1<<20)); got != roles(h) {
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
	msgs := BuildMessages("SYS", "", h, true)
	if msgs[0].Role != "system" || msgs[0].Content != "SYS" {
		t.Fatalf("system: %+v", msgs[0])
	}
	if msgs[2].ReasoningContent != "" || msgs[4].ReasoningContent != "new" {
		t.Fatalf("reasoning: %q %q", msgs[2].ReasoningContent, msgs[4].ReasoningContent)
	}
	if got := BuildMessages("", "", h, false); got[3].ReasoningContent != "" || got[0].Role != "user" {
		t.Fatalf("no reasoning / no system: %+v", got)
	}
}

func TestStorePersistsAndResets(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sid := "tg:-100/1"
	for _, m := range []Msg{{Role: "user", Content: "你好"}, {Role: "assistant", Content: "嗨"}} {
		if err := s.Append(sid, m); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 2 || got[1].Content != "嗨" {
		t.Fatalf("reload: %+v", got)
	}
	if err := s2.Reset(sid); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	if got := s3.History(sid); len(got) != 0 {
		t.Fatalf("after reset: %+v", got)
	}
}

// 前缀缓存的前提：没有裁剪时，每次请求都是上一次请求原样加上新消息
func TestRequestsOnlyGrow(t *testing.T) {
	s, _ := New(t.TempDir())
	sid := "cli:x"
	var prev []llm.Message
	for i := 0; i < 20; i++ {
		s.Append(sid, Msg{Role: "user", Content: strings.Repeat("问", 50)})
		s.Append(sid, Msg{Role: "assistant", ToolCalls: []llm.ToolCall{call("c" + string(rune('a'+i)))}})
		s.Append(sid, Msg{Role: "tool", ToolCallID: "c" + string(rune('a'+i)), Content: "结果"})
		s.Append(sid, Msg{Role: "assistant", Content: "好"})
		if n, _ := s.Compact(sid, 1<<20, 1<<19, ""); n != 0 {
			t.Fatalf("不该裁剪")
		}
		st := s.Settings(sid)
		msgs := BuildMessages("SYS", st.Summary, s.History(sid), false)
		if len(prev) > 0 && !reflect.DeepEqual(msgs[:len(prev)], prev) {
			t.Fatalf("第 %d 轮的请求不是上一轮的延续", i)
		}
		prev = msgs
	}
}

func TestCompactOnceThenAppendOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "cli:x"
	st := s.Settings(sid)
	st.EnvNote = "[系统] 工作目录：/w"
	s.SaveSettings(sid, st)
	long := strings.Repeat("字", 300) // ≈ 300 token
	for i := 0; i < 6; i++ {         // 12 条，约 3700 token
		s.Append(sid, Msg{Role: "user", Content: long})
		s.Append(sid, Msg{Role: "assistant", Content: long})
	}
	// 超过 high（4000）才裁
	if n, _ := s.Compact(sid, 4000, 2000, ""); n != 0 {
		t.Fatalf("没超过上限不该裁剪：%d", n)
	}
	for i := 0; i < 5; i++ {
		s.Append(sid, Msg{Role: "user", Content: long})
		s.Append(sid, Msg{Role: "assistant", Content: long})
	}
	n, err := s.Compact(sid, 4000, 2000, "")
	if err != nil || n == 0 || Estimate(s.History(sid)) > 2000 || s.History(sid)[0].Role != "user" {
		t.Fatalf("裁剪：n=%d err=%v 剩 %d", n, err, Estimate(s.History(sid)))
	}
	kept := len(s.History(sid))
	st = s.Settings(sid)
	if st.Base != 22-kept || !strings.Contains(st.Summary, "已经省略") || !strings.Contains(st.Summary, "工作目录：/w") {
		t.Fatalf("settings: %+v", st)
	}
	msgs := BuildMessages("SYS", st.Summary, s.History(sid), false)
	if !strings.HasPrefix(msgs[1].Content, st.Summary+"\n") {
		t.Fatalf("summary 应该放在第一条用户消息前面：%q", msgs[1].Content[:80])
	}
	// 裁过之后又是只追加：再加一轮不会再裁
	s.Append(sid, Msg{Role: "user", Content: "短"})
	if n, _ := s.Compact(sid, 4000, 2000, ""); n != 0 {
		t.Fatalf("刚裁过不该再裁：%d", n)
	}
	// 重新加载：从 Base 开始读
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != kept+1 || got[len(got)-1].Content != "短" {
		t.Fatalf("reload: %d 条", len(got))
	}
	// 清空上下文：Base、Summary、EnvNote 都归零
	s2.Reset(sid)
	if st := s2.Settings(sid); st.Base != 0 || st.Summary != "" || st.EnvNote != "" {
		t.Fatalf("after reset: %+v", st)
	}
}

func TestCompactStoresSummaryWithoutRewritingHistory(t *testing.T) {
	s, _ := New(t.TempDir())
	sid := "main"
	for _, m := range []Msg{
		{Role: "user", Content: strings.Repeat("旧", 300)},
		{Role: "assistant", Content: "已完成"},
		{Role: "user", Content: "当前任务"},
	} {
		if err := s.Append(sid, m); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(s.file(sid))
	n, err := s.Compact(sid, 200, 100, "已完成第一阶段，下一步继续实现。")
	if err != nil || n != 2 || !strings.Contains(s.Settings(sid).Summary, "已完成第一阶段") {
		t.Fatalf("compact: n=%d err=%v settings=%+v", n, err, s.Settings(sid))
	}
	after, _ := os.ReadFile(s.file(sid))
	if string(before) != string(after) {
		t.Fatal("压缩改写了原始历史")
	}
	// 后续摘要失败时，前一次摘要仍保留，省略说明也不重复堆叠。
	for i := 0; i < 2; i++ {
		s.Append(sid, Msg{Role: "assistant", Content: strings.Repeat("长", 300)})
		s.Append(sid, Msg{Role: "user", Content: "继续"})
		if _, err := s.Compact(sid, 200, 100, ""); err != nil {
			t.Fatal(err)
		}
	}
	if summary := s.Settings(sid).Summary; !strings.Contains(summary, "已完成第一阶段") || strings.Count(summary, "已经省略") != 1 {
		t.Fatalf("降级摘要: %q", summary)
	}
}

func TestCompactSaveFailureLeavesContextIntact(t *testing.T) {
	s, _ := New(t.TempDir())
	sid := "main"
	s.Append(sid, Msg{Role: "user", Content: strings.Repeat("旧", 300)})
	s.Append(sid, Msg{Role: "user", Content: "当前任务"})
	// 占住临时文件路径，让保存失败。
	if err := os.Mkdir(s.settingsFile(sid)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(sid, 200, 100, "摘要"); err == nil {
		t.Fatal("应当报告保存失败")
	}
	if st := s.Settings(sid); st.Base != 0 || st.Summary != "" || len(s.History(sid)) != 2 {
		t.Fatalf("保存失败却改变了上下文: %+v", st)
	}
}
