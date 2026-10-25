package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 测试自己也能当 MCP 服务：TestMain 看到这个环境变量就直接跑服务、不跑测试。
// 这样能真起一个子进程走一遍 stdio（而不是 SDK 的内存传输），不用额外编译一个可执行文件。
const helperEnv = "NANO_MCP_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		helperServer()
		return
	}
	os.Exit(m.Run())
}

func helperServer() {
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

// 真起一个 stdio 子进程：连上、列工具、调工具、空闲/关闭。
func TestManagerRealStdioServer(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	t.Setenv("NANO_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]Server{{
		Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{helperEnv: "1"}, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, nil)
	defer m.Close()
	ctx := context.Background()
	tools, err := m.Tools(ctx, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Exposed != "helper__echo" {
		t.Fatalf("工具列表不对：%+v", tools)
	}
	res, err := m.Call(ctx, "helper", "echo", map[string]any{"text": "世界"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(res); got != "echo:世界" {
		t.Fatalf("调用结果不对：%q", got)
	}
}
