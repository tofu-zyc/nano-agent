package app

// computer use 接入的测试：用假的 Cua Driver MCP 服务走真实的 MCP 子进程链路，
// 检查操作说明进上下文、session 自动带上、结构化结果保留、同一台桌面串行、结束会话清理。
// 真 Driver 的实机验证见 internal/mcp/cua_real_test.go（要 NANO_CUA_REAL=1）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// startDesktopHelper 起一个假的 Driver MCP 服务，返回它的调用日志文件。
func startDesktopHelper(t *testing.T, e *env) (*mcp.Manager, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1", mcpHelperLog: logPath}, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr)
	waitFor(t, func() bool { return len(mgr.CachedTools("default")) > 0 })
	return mgr, logPath
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestDesktopGuideAndSessionInjection(t *testing.T) {
	e := setup(t)
	_, logPath := startDesktopHelper(t, e)

	e.a.mu.Lock()
	system, tools, _, _ := e.a.contextParts(mainSID)
	e.a.mu.Unlock()
	if !strings.Contains(system, "桌面操作（computer use）") || !strings.Contains(system, "先观察再动手") ||
		!strings.Contains(system, "helper（Cua Driver v9.9）") || !strings.Contains(system, desktopSessionID(mainSID)) {
		t.Fatalf("Driver 的操作说明没有接进上下文: %q", system)
	}
	if !hasTool(tools, "helper__get_window_state") {
		t.Fatalf("模型没拿到桌面工具: %v", toolNames(tools))
	}

	// 模型要读窗口状态：内核会把这种工具转回网关执行
	e.say(`!call helper__get_window_state {"pid":42,"window_id":7}`)
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonToolUse || len(turn.Calls) != 1 {
		t.Fatalf("模型应该调桌面工具: %+v", turn)
	}
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: turn.Calls[0].ID,
		Name: turn.Calls[0].Name, Args: turn.Calls[0].Args})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK {
		t.Fatalf("桌面工具执行失败: %+v", res)
	}
	// 文字和 structuredContent 都要在：截图/元素才是 Driver 的真实结果
	if !strings.Contains(res.Content, "窗口：测试窗口") || !strings.Contains(res.Content, "[结构化结果]") ||
		!strings.Contains(res.Content, `"elements"`) || !strings.Contains(res.Content, `"session":"na-`) {
		t.Fatalf("结构化结果丢了: %q", res.Content)
	}
	// 发给 Driver 的参数里自动带了稳定的 session
	if got := readLog(t, logPath); !strings.Contains(got, "get_window_state") || !strings.Contains(got, `"session":"`+desktopSessionID(mainSID)+`"`) {
		t.Fatalf("没有自动带 session: %q", got)
	}

	e.k.send(proto.Msg{T: proto.TResult, SID: in.SID, ID: turn.Calls[0].ID, OK: proto.Bool(true), Content: res.Content})
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)

	// 重置会话：结束 Driver session（异步，等日志）
	if err := e.a.ResetSession(in.SID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readLog(t, logPath), "end_session") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := readLog(t, logPath)
	if !strings.Contains(got, "end_session") || !strings.Contains(got, `"session":"`+desktopSessionID(in.SID)+`"`) {
		t.Fatalf("重置会话没有结束 Driver session: %q", got)
	}
}

func TestDesktopSerializesSameMachine(t *testing.T) {
	e := setup(t)
	mgr, _ := startDesktopHelper(t, e)
	info, ok := mgr.ToolInfo("helper", "get_window_state")
	if !ok {
		t.Fatal("没有 get_window_state")
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := e.a.desktopCall(context.Background(), "s"+string(rune('0'+n)), "helper", info,
				map[string]any{"pid": 1, "window_id": 2, "delay_ms": 250})
			if err != nil {
				t.Errorf("desktopCall: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// 同一台桌面上串行：两次 250ms 的调用不能重叠
	if d := time.Since(start); d < 450*time.Millisecond {
		t.Fatalf("同一台桌面上的桌面调用没有串行: %v", d)
	}
}

func TestTrimToolImages(t *testing.T) {
	h := []session.Msg{
		{Role: "user", Content: "开始"},
		{Role: "tool", Content: "旧截图1", Images: []media.Attachment{mediaRef()}},
		{Role: "tool", Content: "旧截图2", Images: []media.Attachment{mediaRef()}},
		{Role: "tool", Content: "新截图1", Images: []media.Attachment{mediaRef()}},
		{Role: "tool", Content: "新截图2", Images: []media.Attachment{mediaRef()}},
	}
	out := trimToolImages(h, 2)
	if len(out[1].Images) != 0 || len(out[2].Images) != 0 {
		t.Fatal("旧的截图应该从模型请求里去掉")
	}
	if len(out[3].Images) != 1 || len(out[4].Images) != 1 {
		t.Fatal("最近两张应该保留")
	}
	if !strings.Contains(out[1].Content, "以前的截图已省略") || !strings.Contains(out[2].Content, "以前的截图已省略") {
		t.Fatal("省略的地方要说明，让模型需要时重新截图")
	}
	if len(h[1].Images) != 1 {
		t.Fatal("不能改动原历史")
	}
}

func TestInjectDesktopSession(t *testing.T) {
	withSession := mcp.ToolInfo{Schema: json.RawMessage(`{"type":"object","properties":{"session":{"type":"string"},"x":{"type":"integer"}}}`)}
	if got, ok := injectDesktopSession(withSession, nil, "s1").(map[string]any); !ok || got["session"] != desktopSessionID("s1") {
		t.Fatalf("空参数也应该补上 session: %#v", got)
	}
	if got, ok := injectDesktopSession(withSession, map[string]any{"session": "mine"}, "s1").(map[string]any); !ok || got["session"] != "mine" {
		t.Fatal("模型自己写的 session 要尊重")
	}
	without := mcp.ToolInfo{Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)}
	if got, ok := injectDesktopSession(without, map[string]any{"x": 1}, "s1").(map[string]any); !ok || got["session"] != nil {
		t.Fatal("tool 不支持 session 时不要硬塞")
	}
}

// mediaRef 是测试里的一张小图引用（只需要有字段，不会真的读图）。
func mediaRef() media.Attachment {
	return media.Attachment{ID: "pic", MIME: "image/png", Name: "s.png"}
}
