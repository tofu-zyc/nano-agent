package app

// 真实 Cua Driver 通过完整网关链路（管理员会话 → 模型工具调用 → MCP → Driver）的测试。
// 默认跳过；在装好 Driver 和图形会话的 Linux 上跑：
//
//	NANO_CUA_REAL=1 NANO_CUA_BIN=/home/me/.local/bin/cua-driver go test ./internal/app -run TestRealCuaThroughGateway -v
//
// 只读：列窗口。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
)

func TestRealCuaThroughGateway(t *testing.T) {
	if os.Getenv("NANO_CUA_REAL") != "1" {
		t.Skip("没有设置 NANO_CUA_REAL=1")
	}
	bin := os.Getenv("NANO_CUA_BIN")
	if bin == "" {
		bin = "cua-driver"
	}
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{Name: "desktop", Command: bin, Args: []string{"mcp"}, Enabled: true, Approval: "never"}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr)
	waitFor(t, func() bool { return len(mgr.CachedTools("default")) > 0 })

	e.a.mu.Lock()
	system, tools, _, _ := e.a.contextParts(mainSID)
	e.a.mu.Unlock()
	if !strings.Contains(system, "Cua Driver") || !strings.Contains(system, desktopSessionID(mainSID)) {
		t.Fatalf("Driver 的版本/说明没有进上下文: %q", system)
	}
	if !hasTool(tools, "desktop__list_windows") {
		t.Fatalf("模型没拿到 Driver 的工具: %v", toolNames(tools))
	}

	// 模拟模型发起一次 list_windows（参数为空，session 由网关注入）
	e.say("看看桌面")
	in := e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: "cua1", Name: "desktop__list_windows", Args: json.RawMessage("{}")})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK {
		t.Fatalf("Driver 调用失败: %+v", res)
	}
	if !strings.Contains(res.Content, "[结构化结果]") || !strings.Contains(res.Content, "windows") {
		t.Fatalf("Driver 结果没有保留结构化内容: %q", cutRunes(res.Content, 800))
	}
	t.Logf("list_windows 经过网关: %s", cutRunes(strings.ReplaceAll(res.Content, "\n", " "), 500))
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
}
