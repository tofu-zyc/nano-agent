package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/ticket"
)

// 审查里复现的沙箱逃逸，和分支能不能改宿主依赖的东西。分支能写的只有 worktree；宿主的 git 步骤不信 worktree 里的任何东西。

// 分支把 worktree 的 .git 文件指向自己造的 git 目录，里面的配置设 core.fsmonitor：宿主提交不能执行它，
// 不能往主仓库种 hook，记下的提交必须在真仓库里。
func TestBranchPlantsFsmonitorForHostCommit(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	wt := g.r.worktree("tk-1")
	pwn := filepath.Join(g.repo, ".git", "hooks", "post-checkout")
	fake := filepath.Join(wt, ".fakegit")
	gitRun(t, wt, "init", "-q", "--bare", fake)
	mon := filepath.Join(wt, ".mon.sh")
	if err := os.WriteFile(mon, []byte("#!/bin/sh\nmkdir -p '"+filepath.Dir(pwn)+"'\necho '#!/bin/sh' > '"+pwn+"'\necho 'echo pwned' >> '"+pwn+"'\nchmod +x '"+pwn+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(fake, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("[core]\n\tbare = false\n\tfsmonitor = " + mon + "\n")
	f.Close()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("改了\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := g.a.worktreeDirty(ctx, g.t0, g.r); err != nil {
		t.Fatalf("看改动：%v", err)
	}
	r, err := g.a.commitWork(ctx, g.t0, g.r, "小纳")
	if err != nil || !r.Changed {
		t.Fatalf("宿主提交：%+v %v", r, err)
	}
	if b, err := os.ReadFile(pwn); err == nil {
		t.Fatalf("主仓库被种了 hook：%q", b)
	}
	if got := gitRun(t, g.repo, "cat-file", "-t", r.Commit); got != "commit" {
		t.Fatalf("记下的提交不在真仓库里：%s", r.Commit)
	}
	if got := gitRun(t, g.repo, "rev-parse", "nano/tk-1"); got != r.Commit {
		t.Fatalf("提交在工单自己的分支上：%s vs %s", got, r.Commit)
	}
	if got := gitRun(t, g.repo, "rev-parse", "main"); got != g.t0.Base {
		t.Fatal("主分支不能被动")
	}
}

// 分支改不了 worktree 自己的 git 目录（HEAD、commondir、config）：它是只读的。真改了（绕过沙箱，这里直接改）宿主也不提交到别的分支。
func TestBranchCannotEditItsOwnGitDirAndHostChecksHead(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	spec := g.r.branchSpec(g.t0)
	sh := func(script string) (string, error) {
		return g.a.runTicketShell(context.Background(), g.t0, g.r, spec, "cd "+shellLiteral(g.r.worktree("tk-1"), false)+" && "+script, "probe", ticketGitTimeout)
	}
	gd := g.t0.GitDir
	for name, script := range map[string]string{
		"改 HEAD":       "echo 'ref: refs/heads/main' > " + shellLiteral(filepath.Join(gd, "HEAD"), false),
		"改 commondir":  "echo ../../.. > " + shellLiteral(filepath.Join(gd, "commondir"), false),
		"改 config":     "echo '[core]' >> " + shellLiteral(filepath.Join(gd, "config.worktree"), false),
		"改 index":      "echo x > " + shellLiteral(filepath.Join(gd, "index"), false),
		"换掉 gitdir 文件": "echo x > " + shellLiteral(filepath.Join(gd, "gitdir"), false),
		"改仓库 config":   "echo '[core]' >> " + shellLiteral(filepath.Join(g.repo, ".git", "config"), false),
	} {
		if _, err := sh(script); err == nil {
			t.Errorf("分支不能：%s", name)
		}
	}
	// git status、git diff 在只读索引下照常能用
	if err := os.WriteFile(filepath.Join(g.r.worktree("tk-1"), "a.txt"), []byte("改了\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sh("git status --short && git diff --stat"); err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("git status、git diff 照常能用：%q %v", out, err)
	}
	// 宿主这边：HEAD 不是工单自己的分支就不提交（直接把 HEAD 改成 main，模拟被绕过）
	if err := os.WriteFile(filepath.Join(gd, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.a.commitWork(context.Background(), g.t0, g.r, "小纳"); err != errBadHead {
		t.Fatalf("HEAD 不对要拒绝：%v", err)
	}
	if got := gitRun(t, g.repo, "rev-parse", "main"); got != g.t0.Base {
		t.Fatal("主分支不能被提交")
	}
}

// 验收命令里 git status 和往 $HOME 写一个文件都成功（执行器在家目录读不了时换私有 HOME）。
func TestCheckCommandHasAUsablePrivateHome(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	g.t0.Check = `git status --short >/dev/null && echo 缓存 > "$HOME/cache.txt" && cat "$HOME/cache.txt" && git config --global user.name 工单 && git config --global user.name`
	r, err := g.a.runCheck(context.Background(), g.t0, g.r)
	if err != nil || !r.Passed || !strings.Contains(r.Log, "缓存") || !strings.Contains(r.Log, "工单") {
		t.Fatalf("验收命令里 git status 和写 $HOME 都要成功：%+v %v", r, err)
	}
	real, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(real, "cache.txt")); err == nil {
		t.Fatal("写的是私有 HOME，不是真实的家目录")
	}
}

// 验收命令没跑起来（机器不可用）：blocked，理由写明，不算返工；超时算没过。
func TestCheckNotRunningIsBlockedNotRework(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.editWorktree(t, "a.txt", "改了\n")
	g.a.mu.Lock()
	g.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	for i := range g.a.wss { // 把仓库所在的机器换成一台不存在的：验收命令跑不起来
		if g.a.wss[i].ID == g.t0.Auth.Workspace {
			g.a.wss[i].Host = "不存在的机器"
		}
	}
	g.a.mu.Unlock()
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "blocked", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	tk := g.tk("tk-1")
	if tk.Rework != 0 || tk.BlockedBy != "error" || tk.Reason == "" {
		t.Fatalf("没跑起来不算返工：%+v", tk)
	}
}

func TestCheckTimeoutCountsAsFailed(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	old := ticketCheckTimeout
	ticketCheckTimeout = 1 * 1e9
	defer func() { ticketCheckTimeout = old }()
	g.t0.Check = "sleep 20"
	r, err := g.a.runCheck(context.Background(), g.t0, g.r)
	if err != nil || r.Passed {
		t.Fatalf("超时算没过：%+v %v", r, err)
	}
}

// 这一轮结束了没 Report：worktree 有改动就照常验收；没改动才受阻。
func TestSilentRunWithChangesIsStillChecked(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.editWorktree(t, "a.txt", "改了\n")
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK}) // 没 Report
	eventually(t, "照常验收到 ready", func() bool { return g.tk("tk-1").Status == ticket.StatusReady })
	if tk := g.tk("tk-1"); tk.Commit == "" {
		t.Fatalf("提交了：%+v", tk)
	}
}

func TestSilentRunWithoutChangesIsBlocked(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := g.tk("tk-1"); tk.BlockedBy != "silent" || !strings.Contains(tk.Reason, "没有改动也没有汇报") {
		t.Fatalf("没有改动才受阻：%+v", tk)
	}
}

// 先收下：证据写明验收没过、由谁收下。
func TestForcedAcceptNamesWhoAndThatCheckFailed(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "exit 1")
	in := g.startWork(t, sid)
	for i := 0; i < 2; i++ {
		g.editWorktree(t, "a.txt", strings.Repeat("改\n", i+1))
		g.a.mu.Lock()
		g.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
		g.a.mu.Unlock()
		g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
		if i == 0 {
			g.k.expect(proto.TInput)
		}
	}
	eventually(t, "返工用完受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	mention(g.env, "@机器人 先收下", "g400", "u2", "李四")
	g.k.expect(proto.TInput)
	if out, err := callTicket(g.env, sid, `{"action":"accept","id":"tk-1"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	eventually(t, "done", func() bool { return g.tk("tk-1").Status == ticket.StatusDone })
	tk := g.tk("tk-1")
	if !strings.Contains(tk.Reason, "验收没过，由 fake:u2 收下") || !strings.Contains(tk.Reason, tk.Commit) {
		t.Fatalf("证据：%q", tk.Reason)
	}
	finishGroupRun(t, g.env, sid)
}

// 内核不支持调用前检查（hook_check）：不开这一轮，工单 blocked。
func TestTicketNeedsHookCheckKernel(t *testing.T) {
	g := newGitEnv(t)
	g.a.mu.Lock()
	delete(g.a.nodes["test"].info.Features, "hook_check")
	g.a.mu.Unlock()
	sid := g.teamGroupOnRepo(t, "true")
	mention(g.env, "@机器人 修登录页", "g100", "u2", "李四")
	g.k.expect(proto.TInput)
	if _, err := callTicket(g.env, sid, `{"action":"create","title":"修登录页","goal":"x"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, g.env, sid)
	eventually(t, "受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	tk := g.tk("tk-1")
	if tk.BlockedBy != "kernel" || !strings.Contains(tk.Reason, "内核太旧，做不了调用前检查") {
		t.Fatalf("内核太旧：%+v", tk)
	}
	if m, _ := g.k.recv(300 * 1e6); m.T == proto.TInput {
		t.Fatal("不该开这一轮")
	}
}

// 模式和工作目录以工单为准：网页上改分支会话的这两项被拒；会话设置里更高的模式不生效，更低的照办。
func TestTicketBranchModeAndWorkspaceAreTheTicketsNotTheSessions(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	hi, ro := "full", "readonly"
	if err := g.a.UpdateSession(in.SID, SessionPatch{Mode: &hi}); err == nil {
		t.Fatal("改工单分支的模式要拒绝")
	}
	if err := g.a.UpdateSession(in.SID, SessionPatch{Workspaces: &[]string{g.t0.Auth.Workspace}}); err == nil {
		t.Fatal("给工单分支挂主仓库要拒绝")
	}
	// 即使设置被改成了别的（绕过接口直接写），生效的仍以工单为准
	g.a.mu.Lock()
	st := g.a.store.Settings(in.SID)
	st.Mode, st.Workspaces, st.WorkspacesSet = "full", []string{g.t0.Auth.Workspace}, true
	_ = g.a.store.SaveSettings(in.SID, st)
	w := g.a.workspaceOf(in.SID)
	tk, _ := g.a.tickets.Get("tk-1")
	g.a.mu.Unlock()
	if w.mode != tk.Auth.Mode || len(w.wss) != 1 || w.wss[0].ID != "w-tk-1" {
		t.Fatalf("以工单为准：mode=%s ws=%v", w.mode, w.ids())
	}
	g.a.mu.Lock()
	st.Mode = ro
	_ = g.a.store.SaveSettings(in.SID, st)
	w = g.a.workspaceOf(in.SID)
	g.a.mu.Unlock()
	if w.mode != "readonly" {
		t.Fatalf("会话设置里更低的模式照办：%s", w.mode)
	}
}
