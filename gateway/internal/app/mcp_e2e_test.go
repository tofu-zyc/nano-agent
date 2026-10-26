package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
)

// 这个测试进程也能当 MCP 服务用（和 internal/mcp 的 stdio 测试同一个思路）：
// 环境变量一设就只跑服务，不跑测试。用来真起一个子进程验完整链路。
const mcpHelperEnv = "NANO_APP_MCP_HELPER"

// NANO_APP_MCP_LOG 让 helper 把收到的工具调用（名字 + 参数）追加到文件里，
// 测试用来确认 Driver session 有没有带上、结束 session 有没有调用。
const mcpHelperLog = "NANO_APP_MCP_LOG"

func helperLog(tool string, args json.RawMessage) {
	p := os.Getenv(mcpHelperLog)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", tool, string(args))
}

func TestMain(m *testing.M) {
	if os.Getenv(mcpHelperEnv) == "1" {
		mcpHelper()
		return
	}
	os.Exit(m.Run())
}

func mcpHelper() {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "helper", Version: "v9.9"},
		&sdkmcp.ServerOptions{Instructions: "桌面操作：先观察再动手；动作以后重新读取界面确认。"})
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	// 一个假的 Cua Driver：会话、窗口、结构化结果、截图、输入。工具组合能被网关认出来。
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	sessionProp := map[string]any{"session": map[string]any{"type": "string"}}
	srv.AddTool(&sdkmcp.Tool{Name: "start_session", Description: "声明一个 session", InputSchema: obj(sessionProp, "session")},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			helperLog("start_session", req.Params.Arguments)
			var in struct {
				Session string `json:"session"`
			}
			json.Unmarshal(req.Params.Arguments, &in)
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "session " + in.Session}},
				StructuredContent: map[string]any{"ok": true, "session": in.Session}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{Name: "get_window_state", Description: "窗口树 + 截图", InputSchema: obj(map[string]any{
		"pid": map[string]any{"type": "integer"}, "window_id": map[string]any{"type": "integer"},
		"session": sessionProp["session"], "delay_ms": map[string]any{"type": "integer"},
	}, "pid", "window_id")},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			helperLog("get_window_state", req.Params.Arguments)
			var in struct {
				PID     int    `json:"pid"`
				Window  int    `json:"window_id"`
				Session string `json:"session"`
				DelayMS int    `json:"delay_ms"`
			}
			json.Unmarshal(req.Params.Arguments, &in)
			if in.DelayMS > 0 {
				time.Sleep(time.Duration(in.DelayMS) * time.Millisecond)
			}
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "窗口：测试窗口\n[element_index 0] 按钮 「好了」"},
				&sdkmcp.ImageContent{Data: png, MIMEType: "image/png"},
			}, StructuredContent: map[string]any{
				"elements": []any{map[string]any{"element_index": 0, "role": "push button", "label": "好了"}},
				"window":   map[string]any{"pid": in.PID, "window_id": in.Window, "title": "测试窗口"},
				"session":  in.Session,
			}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{Name: "list_windows", Description: "窗口列表", InputSchema: obj(sessionProp)},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			helperLog("list_windows", req.Params.Arguments)
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "1 个窗口"}},
				StructuredContent: map[string]any{"windows": []any{map[string]any{"window_id": 7, "pid": 42, "title": "测试窗口"}}}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{Name: "click", Description: "点一下", InputSchema: obj(map[string]any{
		"x": map[string]any{"type": "integer"}, "y": map[string]any{"type": "integer"}, "session": sessionProp["session"],
	}, "x", "y")},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			helperLog("click", req.Params.Arguments)
			var in struct{ X, Y int }
			json.Unmarshal(req.Params.Arguments, &in)
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "点了"}},
				StructuredContent: map[string]any{"ok": true, "x": in.X, "y": in.Y}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{Name: "end_session", Description: "结束 session", InputSchema: obj(sessionProp, "session")},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			helperLog("end_session", req.Params.Arguments)
			var in struct {
				Session string `json:"session"`
			}
			json.Unmarshal(req.Params.Arguments, &in)
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "结束 " + in.Session}},
				StructuredContent: map[string]any{"ended": true, "session": in.Session}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{
		Name:        "pic",
		Description: "返回一张小图",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "图来啦"},
			&sdkmcp.ImageContent{Data: png, MIMEType: "image/png"},
		}}, nil
	})
	srv.AddTool(&sdkmcp.Tool{
		Name:        "echo",
		Description: "原样返回",
		InputSchema: map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo:" + in.Text}}}, nil
	})
	srv.AddTool(&sdkmcp.Tool{
		Name:        "pics",
		Description: "返回两张小图",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
			&sdkmcp.ImageContent{Data: png, MIMEType: "image/png"},
			&sdkmcp.ImageContent{Data: png, MIMEType: "image/png"},
		}}, nil
	})
	// wait 一直等到被取消（最多 10 秒）：测「调用到一半用户停下」
	srv.AddTool(&sdkmcp.Tool{
		Name:        "wait",
		Description: "等一会儿",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "等完了"}}}, nil
	})
	if err := srv.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

// 完整链路：配置一个真 MCP 服务 → 模型能看到 helper__echo 这个工具 → 调用能拿到结果。
func TestMCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	// 后台连上以后工具定义里才有它（算工具定义只读缓存，不在总锁里去连）
	waitFor(t, func() bool { return len(mgr.CachedTools("default")) > 0 })

	// 管理员发一条消息：模型请求里的工具定义要带上 helper__echo
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	if !hasTool(e.llm.lastReq().Tools, "helper__echo") {
		t.Fatalf("模型没拿到 MCP 工具：%v", toolNames(e.llm.lastReq().Tools))
	}

	// 模型要调它：内核不认识这个名字，转回网关
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: "c1", Name: "helper__echo",
		Args: json.RawMessage(`{"text":"世界"}`)})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || res.Content != "echo:世界" {
		t.Fatalf("MCP 调用结果不对：%+v", res)
	}
	if !strings.Contains(res.Content, "世界") {
		t.Fatal(res.Content)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
}

// approval=always：每次调用都先出审批单，批了才真的执行。
func TestMCPApprovalAlways(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true, Approval: mcp.ApprovalAlways,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: "c1", Name: "helper__echo",
		Args: json.RawMessage(`{"text":"世界"}`)})
	e.a.mu.Unlock()
	// 先不该执行，而是挂一张审批单
	e.k.none()
	e.a.mu.Lock()
	n := 0
	for k := range e.a.approvals {
		n = k
	}
	e.a.mu.Unlock()
	if n == 0 {
		t.Fatal("没有出审批单")
	}
	if err := e.a.Approve(n, true); err != nil {
		t.Fatal(err)
	}
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || res.Content != "echo:世界" {
		t.Fatalf("批准以后应该执行：%+v", res)
	}
	e.a.mu.Lock()
	left := len(e.a.approvals)
	e.a.mu.Unlock()
	if left != 0 {
		t.Fatalf("审批单应该消掉，还剩 %d", left)
	}
}

// approval=per_turn：一轮里第一次要批，批过之后同一个服务不再问。
func TestMCPApprovalPerTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true, Approval: mcp.ApprovalPerTurn,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	call := func(id, text string) {
		e.a.mu.Lock()
		s := e.a.sessOf(in.SID)
		e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: id, Name: "helper__echo",
			Args: json.RawMessage(`{"text":"` + text + `"}`)})
		e.a.mu.Unlock()
	}
	call("c1", "a")
	e.k.none() // 第一次要审批，先不执行
	e.a.mu.Lock()
	n := 0
	for k := range e.a.approvals {
		n = k
	}
	e.a.mu.Unlock()
	if n == 0 {
		t.Fatal("第一次调用没有出审批单")
	}
	if err := e.a.Approve(n, true); err != nil {
		t.Fatal(err)
	}
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || res.Content != "echo:a" {
		t.Fatalf("第一次：%+v", res)
	}
	// 同一轮第二次：直接执行，不该再出审批单
	call("c2", "b")
	res = e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || res.Content != "echo:b" {
		t.Fatalf("第二次：%+v", res)
	}
	e.a.mu.Lock()
	left := len(e.a.approvals)
	e.a.mu.Unlock()
	if left != 0 {
		t.Fatalf("审批单应该只有一张，还剩 %d", left)
	}
}

// MCP 工具返回的图片：文字照旧给模型，图片存进媒体库、挂在工具结果上（网页的工具卡片会显示）。
func TestMCPImageResult(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	mstore, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetMedia(mstore)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: "c1", Name: "helper__pic", Args: json.RawMessage(`{}`)})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK {
		t.Fatalf("调用失败：%+v", res)
	}
	if !strings.Contains(res.Content, "图来啦") || !strings.Contains(res.Content, "图片") {
		t.Fatalf("文字结果不对：%q", res.Content)
	}
	e.a.mu.Lock()
	sent := 0
	if s.run != nil {
		sent = len(s.run.sent["c1"])
	}
	e.a.mu.Unlock()
	if sent != 1 {
		t.Fatalf("图片应该挂在工具结果上，实际 %d 张", sent)
	}
}

// 死锁回归：算工具定义时（拿着网关的总锁）绝不能去连 MCP。
// 用一台不存在的机器上的服务，以前会卡死在这里。
func TestMCPNoLockDuringContextParts(t *testing.T) {
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{Name: "off", Machine: "no-such-machine", Command: "x", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	done := make(chan []llm.Tool, 1)
	go func() {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		_, tools, _, _ := e.a.contextParts(mainSID)
		done <- tools
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("算工具定义时卡住了（又去连 MCP 了）")
	}
}

// 保存 MCP 配置以后，已有会话的工具快照要重建，不然新加的服务看不见。
func TestMCPSaveRefreshesTools(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	// 先跑一轮，让快照落下（这时还没有 MCP）
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.a.mu.Lock()
	_, before, _, _ := e.a.contextParts(in.SID)
	e.a.mu.Unlock()
	if hasTool(before, "helper__echo") {
		t.Fatal("还没有 MCP 服务，不该有它的工具")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.SaveMCP([]mcp.Server{{Name: "helper", Command: exe,
		Args: []string{"-test.run=TestNoSuchTest"}, Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(mgr.CachedTools("default")) >= 1 })
	e.a.mu.Lock()
	_, after, _, _ := e.a.contextParts(in.SID)
	e.a.mu.Unlock()
	if !hasTool(after, "helper__echo") {
		t.Fatalf("保存 MCP 以后快照没重建：%v", toolNames(after))
	}
}
