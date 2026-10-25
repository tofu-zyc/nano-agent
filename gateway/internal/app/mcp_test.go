package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
)

// MCP 工具的名字内核不认识（<服务>__<工具>），会被转回网关。这里确认它掉进的是 MCP 那条路
// 而不是“这一轮没有挂载工作目录”。服务起不来时模型要收到一条说得清的错误。
func TestMCPToolDispatch(t *testing.T) {
	e := setup(t)
	e.say("你好")
	e.k.expect(proto.TInput)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{Name: "demo", Command: "/nonexistent-demo-server", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	defer mgr.Close()
	e.a.SetMCP(st, mgr)
	e.a.mu.Lock()
	s := e.a.sessOf(e.sid)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: e.sid, ID: "c1", Name: "demo__echo", Args: json.RawMessage(`{"text":"hi"}`)})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK {
		t.Fatalf("服务起不来时应该回失败：%+v", res)
	}
	if !strings.Contains(res.Content, "demo") {
		t.Fatalf("错误里应该提到是哪个服务：%q", res.Content)
	}
}

// 没有接 MCP 的时候，带 __ 的陌生工具名不应该被当成 MCP 工具。
func TestMCPToolIgnoredWithoutManager(t *testing.T) {
	e := setup(t)
	e.say("你好")
	e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(e.sid)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: e.sid, ID: "c1", Name: "demo__echo", Args: json.RawMessage(`{}`)})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK {
		t.Fatalf("没接 MCP 时应该走原来那条路（回失败）：%+v", res)
	}
}
