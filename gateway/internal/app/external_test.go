package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if _, err := e.a.SaveExternalAgent(bad); err == nil {
		t.Fatal("校验不过的不登记")
	}
	if _, err := e.a.SaveExternalAgent(testExternal()); err != nil {
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

// 改登记就关掉开关：命令、目录、环境有任何变化，所有工作目录上关掉它（开启时确认的是那时候登记的目录），
// 只改时限不关；新登记一个名字时，先清掉工作目录上残留的同名开关。
func TestChangingTheRegistrationTurnsTheSwitchOff(t *testing.T) {
	e := setup(t)
	var ids []string
	for i := 0; i < 2; i++ {
		ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ws.ID)
	}
	enable := func() {
		t.Helper()
		for _, id := range ids {
			if err := e.a.SetWorkspaceExternal(id, []string{"claude"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := e.a.SaveExternalAgent(testExternal()); err != nil {
		t.Fatal(err)
	}
	enable()
	longer := testExternal()
	longer.Timeout = 60
	if off, err := e.a.SaveExternalAgent(longer); err != nil || len(off) != 0 {
		t.Fatalf("只改时限不关：%v %v", off, err)
	}
	changes := map[string]func(*extagent.Agent){
		"命令": func(a *extagent.Agent) { a.Argv = []string{"claude", "--yolo", "-p", extagent.PromptSlot} },
		"目录": func(a *extagent.Agent) { a.Dirs = []string{"~/.claude", "~/.ssh"} },
		"环境": func(a *extagent.Agent) { a.Env = map[string]string{"CLAUDE_CONFIG_DIR": "~/.other"} },
	}
	for what, change := range changes {
		enable()
		next := testExternal()
		change(&next)
		off, err := e.a.SaveExternalAgent(next)
		if err != nil || len(off) != 2 {
			t.Fatalf("改了%s：返回关掉的工作目录：%v %v", what, off, err)
		}
		for _, id := range ids {
			if got := e.a.findWorkspaceInfo(t, id).External; len(got) != 0 {
				t.Fatalf("改了%s，%s 上要关掉：%v", what, id, got)
			}
		}
		if _, err := e.a.SaveExternalAgent(testExternal()); err != nil {
			t.Fatal(err)
		}
	}
	// 残留的同名开关（比如删登记时写不进 workspaces.json）：新登记时清掉
	if err := e.a.DeleteExternalAgent("claude"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.findWorkspace(ids[0]).External = []string{"claude"}
	e.a.mu.Unlock()
	if off, err := e.a.SaveExternalAgent(testExternal()); err != nil || len(off) != 1 || off[0] != ids[0] {
		t.Fatalf("新登记清掉残留的开关：%v %v", off, err)
	}
	if got := e.a.findWorkspaceInfo(t, ids[0]).External; len(got) != 0 {
		t.Fatalf("残留的开关：%v", got)
	}
}

// 开关在 a.mu 下查登记：拿到锁之前登记被删了就开不了。以前先在锁外查登记、删登记也先删再拿锁，
// 两边交错时工作目录上会留下没登记的名字。
func TestSwitchChecksTheRegistrationUnderTheLock(t *testing.T) {
	e := setup(t)
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.SaveExternalAgent(testExternal()); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- e.a.SetWorkspaceExternal(ws.ID, []string{"claude"}) }()
	time.Sleep(50 * time.Millisecond)                      // 让它走到拿锁那里
	if err := e.a.extAgents.Delete("claude"); err != nil { // DeleteExternalAgent 删登记的那一步（也在锁下）
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("拿到锁时登记已经没了，要拒绝")
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID).External; len(got) != 0 {
		t.Fatalf("工作目录上留下了没登记的名字：%v", got)
	}
}

// 改工作目录的两项：一项不对，另一项也不改。
func TestPatchWorkspaceValidatesBothFirst(t *testing.T) {
	e := setup(t)
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	on, bad := "on", "sometimes"
	if err := e.a.PatchWorkspace(ws.ID, &on, &[]string{"nope"}); err == nil {
		t.Fatal("没登记的外部 agent 要拒绝")
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID); got.SandboxMode != "" {
		t.Fatalf("外部 agent 不对，执行隔离也不改：%q", got.SandboxMode)
	}
	if _, err := e.a.SaveExternalAgent(testExternal()); err != nil {
		t.Fatal(err)
	}
	if err := e.a.PatchWorkspace(ws.ID, &bad, &[]string{"claude"}); err == nil {
		t.Fatal("执行隔离的值不对要拒绝")
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID); len(got.External) != 0 {
		t.Fatalf("执行隔离不对，外部 agent 也不开：%v", got.External)
	}
	if err := e.a.PatchWorkspace(ws.ID, &on, &[]string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if got := e.a.findWorkspaceInfo(t, ws.ID); got.SandboxMode != "on" || len(got.External) != 1 {
		t.Fatalf("两项都改：%+v", got.Workspace)
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
