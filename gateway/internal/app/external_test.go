package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/extagent"
	"nano-agent/gateway/internal/host"
)

func testExternal() extagent.Agent {
	return extagent.Agent{Name: "claude", Argv: []string{"claude", "-p", extagent.PromptSlot}, Dirs: []string{"~/.claude"}}
}

// 登记存在 data/external-agents.json；工作目录上默认都不开，只能开登记了的；删掉登记时所有工作目录上都关掉。
func TestExternalAgentRegistrationAndWorkspaceSwitch(t *testing.T) {
	e := setup(t)
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID).External; len(got) != 0 {
		t.Fatalf("默认都不开：%v", got)
	}
	if err := e.a.SetWorkspaceExternal(ws.ID, []string{"claude"}); err == nil {
		t.Fatal("没登记的开不了")
	}
	bad := testExternal()
	bad.Dirs = []string{"~/"}
	if err := e.a.SaveExternalAgent(bad); err == nil {
		t.Fatal("校验不过的不登记")
	}
	if err := e.a.SaveExternalAgent(testExternal()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(e.a.store.Dir(), "external-agents.json")); err != nil || !strings.Contains(string(b), `"{prompt}"`) {
		t.Fatalf("存在 data/external-agents.json：%s %v", b, err)
	}
	if err := e.a.SetWorkspaceExternal(ws.ID, []string{"claude", "claude"}); err != nil {
		t.Fatal(err)
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID).External; len(got) != 1 || got[0] != "claude" {
		t.Fatalf("开启了（去重）：%v", got)
	}
	raw, _ := os.ReadFile(e.a.workspacesFile())
	var saved []Workspace
	json.Unmarshal(raw, &saved)
	if len(saved) == 0 || len(saved[0].External) != 1 {
		t.Fatalf("开关写进 workspaces.json：%s", raw)
	}
	if err := e.a.DeleteExternalAgent("claude"); err != nil {
		t.Fatal(err)
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID).External; len(got) != 0 {
		t.Fatalf("删掉登记时关掉：%v", got)
	}
}

func (a *App) findWorkspaceInfo(t *testing.T, id string) WorkspaceInfo {
	t.Helper()
	for _, w := range a.Workspaces() {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("没有工作目录 %s", id)
	return WorkspaceInfo{}
}
