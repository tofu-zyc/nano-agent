package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nano-agent/gateway/internal/secret"
)

func TestStoreSaveAndNormalize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NANO_HOME", home)
	if err := secret.Init(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	path := filepath.Join(t.TempDir(), "mcp.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.List()) != 0 {
		t.Fatalf("没有文件时应该是空的：%v", st.List())
	}
	if err := st.Save([]Server{{Name: "demo", Command: "demo-server", Enabled: true, Env: map[string]string{"TOKEN": "sk-plain"}}}); err != nil {
		t.Fatal(err)
	}
	// 重新打开：env 存的是密文，解出来还是明文；approval 默认 per_turn
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := st2.Get("demo")
	if !ok {
		t.Fatal("读回来了但没有 demo")
	}
	if s.Approval != ApprovalNever {
		t.Fatalf("approval 默认应该是 never（审批还没接上），实际 %q", s.Approval)
	}
	if strings.Contains(s.Env["TOKEN"], "sk-plain") {
		t.Fatalf("env 里的密钥应该加密存：%q", s.Env["TOKEN"])
	}
	env, err := s.EnvFor()
	if err != nil || env["TOKEN"] != "sk-plain" {
		t.Fatalf("解密 env 不对：%v %q", err, env)
	}
	// 文件权限 0600
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应该是 0600：%v %v", fi.Mode(), err)
	}
}

func TestStoreRejectsBadConfig(t *testing.T) {
	t.Setenv("NANO_HOME", t.TempDir())
	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []Server{
		{Name: "", Command: "x"},                             // 没名字
		{Name: "a b", Command: "x"},                          // 名字带空格
		{Name: "ok", URL: "http://x", Command: "y"},          // 两个都给
		{Name: "ok", Command: "x", Approval: "sometimes"},    // 审批方式不对
		{Name: "ok", Command: "x", Approval: ApprovalAlways}, // 审批还没接上，明确拒绝
		{Name: "ok", Command: "x", Machine: "windows-pc"},    // 现在只支持本机
	}
	for _, s := range bad {
		if err := st.Save([]Server{s}); err == nil {
			t.Fatalf("%+v 应该被拒绝", s)
		}
	}
	// 重名
	if err := st.Save([]Server{{Name: "dup", Command: "x"}, {Name: "dup", Command: "y"}}); err == nil {
		t.Fatal("重名应该被拒绝")
	}
}

func TestExposedNameRoundTrip(t *testing.T) {
	if got := ExposedName("fs", "read_file"); got != "fs__read_file" {
		t.Fatalf("ExposedName: %q", got)
	}
	srv, tool, ok := SplitExposed("fs__read_file")
	if !ok || srv != "fs" || tool != "read_file" {
		t.Fatalf("SplitExposed: %q %q %v", srv, tool, ok)
	}
	if _, _, ok := SplitExposed("nope"); ok {
		t.Fatal("不是 <服务>__<工具> 的应该拆不出来")
	}
}

// fakeServer 起一个进程内的 MCP 服务（不启动子进程），返回它的服务端会话。
func fakeServer(t *testing.T, ctx context.Context) (*sdkmcp.InMemoryTransport, func()) {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake", Version: "v0"}, nil)
	echo := func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok:" + in.Text}}}, nil
	}
	for _, name := range []string{"echo", "hidden"} {
		srv.AddTool(&sdkmcp.Tool{
			Name:        name,
			Description: name + " 工具",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required": []string{"text"}},
		}, echo)
	}
	cTransport, sTransport := sdkmcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, sTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cTransport, func() { _ = ss.Close() }
}

func TestManagerListAndCall(t *testing.T) {
	t.Setenv("NANO_HOME", t.TempDir())
	ctx := context.Background()
	cTransport, stop := fakeServer(t, ctx)
	defer stop()

	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	// hidden 在黑名单里：不该出现在工具列表，也不许调用
	if err := st.Save([]Server{{Name: "demo", Command: "demo", Enabled: true, Tools: []string{"!hidden"}}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, nil)
	m.dial = func(Server) (sdkmcp.Transport, error) { return cTransport, nil }
	defer m.Close()

	tools, err := m.Tools(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Exposed != "demo__echo" {
		t.Fatalf("工具列表不对：%+v", tools)
	}
	if len(tools[0].Schema) == 0 || !strings.Contains(string(tools[0].Schema), "text") {
		t.Fatalf("参数 schema 应该原样透传：%s", tools[0].Schema)
	}

	res, err := m.Call(ctx, "demo", "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			got += tc.Text
		}
	}
	if got != "ok:hi" {
		t.Fatalf("调用结果不对：%q", got)
	}

	if _, err := m.Call(ctx, "demo", "hidden", map[string]any{"text": "x"}); err == nil {
		t.Fatal("黑名单里的工具不该能调")
	}
	if _, err := m.Call(ctx, "nope", "echo", nil); err == nil {
		t.Fatal("没有这个服务应该报错")
	}

	// ToolsFor 只给开了的服务
	all := m.ToolsFor(ctx, "default")
	if len(all) != 1 || all[0].Exposed != "demo__echo" {
		t.Fatalf("ToolsFor: %+v", all)
	}
}

func TestManagerDisabledAndAgents(t *testing.T) {
	t.Setenv("NANO_HOME", t.TempDir())
	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]Server{
		{Name: "off", Command: "x", Enabled: false},
		{Name: "bob", Command: "x", Enabled: true, Agents: []string{"bob"}},
	}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, nil)
	m.dial = func(Server) (sdkmcp.Transport, error) {
		t.Fatal("不该连：没有启用的服务对这个 agent 可用")
		return nil, nil
	}
	if got := m.ToolsFor(context.Background(), "default"); len(got) != 0 {
		t.Fatalf("default 不该拿到 bob 的服务：%+v", got)
	}
}

var _ = json.Marshal
