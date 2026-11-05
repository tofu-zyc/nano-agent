package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/ticket"
)

// 网关的每个网关工具都要在真内核报的名单里：以后加网关工具忘了改内核，这里失败
// （漏了的工具，模型调用时会落到执行器，回“未知工具”）。
func TestRealKernelHandsBackEveryGatewayTool(t *testing.T) {
	e, _ := setupRealKernel(t)
	e.a.mu.Lock()
	var n *node
	for _, x := range e.a.nodes {
		n = x
	}
	got := slices.Clone(n.info.GatewayTools)
	e.a.mu.Unlock()
	if got == nil {
		t.Fatal("真内核要在 hello 里报交给网关的工具名单")
	}
	for _, name := range gatewayToolNames() {
		if !slices.Contains(got, name) {
			t.Errorf("网关工具 %s 不在真内核报的名单里：%v", name, got)
		}
	}
}

// ticketModel 是工单这一路上的模型替身：按这一轮最后一条用户消息决定做什么。
type ticketModel struct{}

func (ticketModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	last, done := -1, map[string]bool{}
	for i, m := range req.Messages {
		if m.Role == "user" {
			last, done = i, map[string]bool{}
		}
		if m.Role == "tool" {
			done[m.ToolCallID] = true
		}
	}
	text := ""
	if last >= 0 {
		text = req.Messages[last].Content
	}
	call := func(id, name, args string) (*llm.Result, error) {
		return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}}}, nil
	}
	final := func(s string) (*llm.Result, error) { return &llm.Result{Reason: llm.ReasonFinal, Content: s}, nil }
	switch {
	case strings.Contains(text, "[工单 tk-1"): // 分支：改文件，凭这次写汇报 ready
		if !done["w1"] {
			return call("w1", "Write", `{"path":"a.txt","content":"一\n二\n"}`)
		}
		if !done["r1"] {
			return call("r1", "Report", `{"status":"ready","text":"加上了第二行","evidence":["w1"]}`)
		}
		return final("做完了")
	case strings.Contains(text, "收下"):
		if !done["acc"] {
			return call("acc", "Ticket", `{"action":"accept","id":"tk-1"}`)
		}
		return final("收下了")
	case strings.Contains(text, "修登录页"):
		if !done["tc"] {
			return call("tc", "Ticket", `{"action":"create","title":"修登录页","goal":"a.txt 加一行 二"}`)
		}
		return final("建好了")
	}
	return final("嗯")
}

// 真内核把一张没连仓库的工单从头走到底：团队群点名 → Ticket create → 分支 Write → Report ready → 验收 → ready → accept → done。
// Report、Ticket 以前落到执行器（回“未知工具”），假内核测不出来。
func TestRealKernelTicketEndToEnd(t *testing.T) {
	executable, err := host.NewExecutor("../../../kernel/nano-exec")
	if err != nil {
		t.Skip("先 make：", err)
	}
	e, _ := setupRealKernel(t)
	fake := newFakeHosts()
	e.a.SetHosts(hookRealHosts{host.NewLocal(executable, nil), fake})
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("一\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "first")
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.llm = ticketModel{}
	e.a.cfg.Workspace.Mode = "auto" // 分支按 auto 跑，Write 不用审批
	e.a.mu.Unlock()
	sid := extGroup(t, e)
	setScene(t, e, sid, &scene.Rules{Team: &scene.Team{Roster: []string{"fake:u2"}, Workspace: ws.ID, Check: "grep -q 二 a.txt", Budget: 5000}})

	mention(e, "@机器人 修登录页", "g100", "u2", "李四")
	eventually(t, "验收通过、工单 ready", func() bool {
		return ticketNow(e, "tk-1").Status != ticket.StatusOpen && ticketNow(e, "tk-1").Status != ticket.StatusWorking
	})
	got := e.ch.wait(t, "改好了：")
	tk := ticketNow(e, "tk-1")
	if tk.Status != ticket.StatusReady || tk.Commit == "" || !strings.Contains(got, "验收命令 grep -q 二 a.txt 通过") {
		t.Fatalf("验收通过、工单 ready：%q %+v", got, tk)
	}
	if subj := gitRun(t, repo, "show", tk.Commit+":a.txt"); subj != "一\n二" {
		t.Fatalf("提交里是分支写的内容：%q", subj)
	}
	// 分支的 Write 由真内核交给执行器，Report 交给网关（分支的历史里还带着派活之前群里的 Ticket create）
	e.a.mu.Lock()
	results := map[string]string{}
	for _, m := range e.a.store.History(tk.RunSID) {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	e.a.mu.Unlock()
	if !strings.HasPrefix(results["w1"], "已覆盖") || !strings.HasPrefix(results["r1"], "已汇报（ready") {
		t.Fatalf("分支的工具结果：%v", results)
	}

	mention(e, "@机器人 收下吧", "g101", "u2", "李四")
	eventually(t, "done", func() bool { return ticketNow(e, "tk-1").Status == ticket.StatusDone })
	if done := ticketNow(e, "tk-1"); !strings.Contains(done.Reason, tk.Commit) {
		t.Fatalf("done 带证据：%+v", done)
	}
}

// ticketNow 在锁下取一张工单。
func ticketNow(e *env, id string) ticket.Ticket {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	tk, _ := e.a.tickets.Get(id)
	return tk
}
