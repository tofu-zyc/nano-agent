package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
)

// 这个测试进程也能当 MCP 服务用（和 internal/mcp 的 stdio 测试同一个思路）：
// 环境变量一设就只跑服务，不跑测试。用来真起一个子进程验完整链路。
const mcpHelperEnv = "NANO_APP_MCP_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(mcpHelperEnv) == "1" {
		mcpHelper()
		return
	}
	os.Exit(m.Run())
}

func mcpHelper() {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "helper", Version: "v0"}, nil)
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
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
