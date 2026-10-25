package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nano-agent/gateway/internal/mcp"
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
