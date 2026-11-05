package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/ticket"
)

// 工单的 git 步骤：用临时目录里的真 git 仓库和真执行器，不碰任何真实仓库。

type gitEnv struct {
	*env
	repo string
	t0   ticket.Ticket
	r    ticketRepo
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitEnv 起一个带真执行器和临时 git 仓库的环境；没有 git、执行器或 Landlock 就跳过。
func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	executable, err := host.NewExecutor("../../../kernel/nano-exec")
	if err != nil {
		t.Skip("先 make：", err)
	}
	local := host.NewLocal(executable, nil)
	if landlock, _, ok, _ := local.Probe(); !ok || landlock == 0 {
		t.Skip("这台机器没有 Landlock")
	}
	e := setup(t)
	e.a.SetHosts(hookRealHosts{local, e.hosts})
	e.a.mu.Lock()
	e.a.nodes["test"].info.Features = map[string]int{"hook_check": 1} // 工单的分支要求内核支持调用前检查
	e.a.mu.Unlock()
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
	g := &gitEnv{env: e, repo: ws.Path}
	g.t0 = ticket.Ticket{ID: "tk-1", Agent: "default", Title: "修登录", Goal: "x", Check: "true", Budget: 100,
		Auth: ticket.Auth{By: "telegram:1", Scene: "g1", Workspace: ws.ID, Mode: "auto", Tools: []string{"Read"}}}
	e.a.mu.Lock()
	g.r, err = e.a.ticketRepoOf(g.t0)
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *gitEnv) mkWorktree(t *testing.T) (base string) {
	t.Helper()
	base, gd, err := g.a.createWorktree(context.Background(), g.t0, g.r)
	if err != nil {
		t.Fatal(err)
	}
	g.t0.Base, g.t0.GitDir = base, gd
	return base
}

func TestCreateWorktreeIsIdempotentAndLeavesTheMainRepoAlone(t *testing.T) {
	g := newGitEnv(t)
	head := gitRun(t, g.repo, "rev-parse", "HEAD")
	base := g.mkWorktree(t)
	if base != head {
		t.Fatalf("起点是仓库当前签出的分支的提交：%s vs %s", base, head)
	}
	wt := g.r.worktree("tk-1")
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatalf("worktree 里有仓库的文件：%v", err)
	}
	if got := gitRun(t, g.repo, "branch", "--list", "nano/tk-1"); !strings.Contains(got, "nano/tk-1") {
		t.Fatalf("分支 nano/tk-1：%q", got)
	}
	if gd := g.t0.GitDir; !strings.HasSuffix(gd, ".git/worktrees/tk-1") {
		t.Fatalf("worktree 自己的 git 目录：%s", gd)
	}
	// 主仓库的工作区看不到 .nano；分支和 HEAD 没动
	if st := gitRun(t, g.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("主仓库的 git status 要是干净的：%q", st)
	}
	if got := gitRun(t, g.repo, "rev-parse", "HEAD"); got != head {
		t.Fatal("主仓库的 HEAD 不该动")
	}
	if got := gitRun(t, g.repo, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("主仓库还在 main：%s", got)
	}
	// 重做：已经在了就接着用，不再建
	base2, gd2, err := g.a.createWorktree(context.Background(), g.t0, g.r)
	if err != nil || base2 != base || gd2 != g.t0.GitDir {
		t.Fatalf("重做：%s %s %v", base2, gd2, err)
	}
	if n := strings.Count(gitRun(t, g.repo, "worktree", "list"), "\n") + 1; n != 2 {
		t.Fatalf("只有主仓库和一个 worktree：%d", n)
	}
	// 清理：worktree 没了，分支留着
	if err := g.a.removeWorktree(context.Background(), g.t0, g.r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree 要删掉")
	}
	if got := gitRun(t, g.repo, "branch", "--list", "nano/tk-1"); !strings.Contains(got, "nano/tk-1") {
		t.Fatal("git 分支留到交付完")
	}
	if err := g.a.removeWorktree(context.Background(), g.t0, g.r); err != nil {
		t.Fatalf("已经没有了也算成功：%v", err)
	}
	// 分支还在、worktree 没了：再建一次接着用同一个分支
	if base3, _, err := g.a.createWorktree(context.Background(), g.t0, g.r); err != nil || base3 != base {
		t.Fatalf("分支在、目录没了：%s %v", base3, err)
	}
}

// 分支只能写 worktree 和它自己的 git 目录；读不了仓库以外的文件，读得了 <仓库>/.git 但写不了；提交不了。
func TestBranchSandboxScope(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("别人的密码\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := g.r.branchSpec(g.t0)
	sh := func(script string) (string, error) {
		return g.a.runTicketShell(context.Background(), g.t0, g.r, spec, "cd "+shellLiteral(g.r.worktree("tk-1"), false)+" && "+script, "probe", ticketGitTimeout)
	}
	if out, err := sh("echo 新内容 >> a.txt && echo 新文件 > b.txt && git status --short"); err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("能改 worktree 里的文件、git status：%q %v", out, err)
	}
	if _, err := sh("git diff >/dev/null && git log --oneline | head -1"); err != nil {
		t.Fatalf("能 git diff、git log：%v", err)
	}
	if _, err := sh("cat " + shellLiteral(secret, false)); err == nil {
		t.Fatal("读仓库以外的临时文件要被拒绝")
	}
	if out, err := sh("cat " + shellLiteral(filepath.Join(g.repo, ".git", "HEAD"), false)); err != nil || !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("读 <仓库>/.git 可以：%q %v", out, err)
	}
	if _, err := sh("echo x > " + shellLiteral(filepath.Join(g.repo, ".git", "evil"), false)); err == nil {
		t.Fatal("写 <仓库>/.git 要被拒绝")
	}
	if _, err := sh("echo x > " + shellLiteral(filepath.Join(g.repo, "a.txt"), false)); err == nil {
		t.Fatal("写主仓库的工作区要被拒绝")
	}
	if _, err := sh("echo '#!/bin/sh' > " + shellLiteral(filepath.Join(g.repo, ".git", "hooks", "pre-commit"), false)); err == nil {
		t.Fatal("改仓库的 hooks 要被拒绝")
	}
	if _, err := sh("git add -A && git -c user.name=x -c user.email=x@x commit -q -m evil"); err == nil {
		t.Fatal("分支里提交不了")
	}
	if got := gitRun(t, g.repo, "log", "--oneline", "nano/tk-1"); strings.Contains(got, "evil") {
		t.Fatal("分支上不该有提交")
	}
	if st := gitRun(t, g.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("主仓库的工作区没被改动：%q", st)
	}
}

func TestRunCheckPassFailAndTail(t *testing.T) {
	g := newGitEnv(t)
	g.mkWorktree(t)
	ctx := context.Background()
	g.t0.Check = "echo 通过了"
	if r, err := g.a.runCheck(ctx, g.t0, g.r); err != nil || !r.Passed || !strings.Contains(r.Log, "通过了") {
		t.Fatalf("通过：%+v %v", r, err)
	}
	g.t0.Check = "for i in $(seq 1 100); do echo 行$i; done; exit 3"
	r, err := g.a.runCheck(ctx, g.t0, g.r)
	if err != nil || r.Passed {
		t.Fatalf("退出码不是 0 算没过：%+v %v", r, err)
	}
	if lines := strings.Count(r.Log, "\n") + 1; lines > 40 || !strings.Contains(r.Log, "行100") || strings.Contains(r.Log, "行1\n") {
		t.Fatalf("只留最后 40 行：%d 行\n%s", lines, r.Log)
	}
	g.t0.Check = ""
	if r, _ := g.a.runCheck(ctx, g.t0, g.r); !r.Passed {
		t.Fatal("没有验收命令算通过，只看有没有改动")
	}
}

func TestCommitWorkIsOnceAndByTheHost(t *testing.T) {
	g := newGitEnv(t)
	base := g.mkWorktree(t)
	ctx := context.Background()
	wt := g.r.worktree("tk-1")
	// 仓库里有一个会拒绝提交的 pre-commit hook：宿主提交不执行仓库的 hook
	hook := filepath.Join(g.repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 没改动：不提交
	if r, err := g.a.commitWork(ctx, g.t0, g.r, "小纳"); err != nil || r.Changed || r.Commit != "" {
		t.Fatalf("没改动：%+v %v", r, err)
	}
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("一\n二\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := g.a.commitWork(ctx, g.t0, g.r, "小纳")
	if err != nil || !r.Changed || r.Commit == "" || !strings.Contains(r.Summary, "1 file") {
		t.Fatalf("提交：%+v %v", r, err)
	}
	msg := gitRun(t, wt, "log", "-1", "--format=%an|%B")
	if !strings.HasPrefix(msg, "小纳|修登录 (tk-1)") || !strings.Contains(msg, "\nNano-Ticket: tk-1") {
		t.Fatalf("作者用 agent 的名字，说明带标题和工单号，末尾有 Nano-Ticket 行：%q", msg)
	}
	// 重做：worktree 干净、已经有带这一行的提交，收养它，不再提交
	r2, err := g.a.commitWork(ctx, g.t0, g.r, "小纳")
	if err != nil || r2.Commit != r.Commit {
		t.Fatalf("重做收养同一个提交：%+v %v", r2, err)
	}
	if n := gitRun(t, wt, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Fatalf("只有一个提交：%s", n)
	}
	// 主仓库的工作区、HEAD 和 hooks 不被改动
	if st := gitRun(t, g.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("主仓库的工作区：%q", st)
	}
	if got := gitRun(t, g.repo, "rev-parse", "HEAD"); got != base {
		t.Fatal("主仓库的 HEAD 不该动")
	}
	if b, _ := os.ReadFile(hook); string(b) != "#!/bin/sh\nexit 1\n" {
		t.Fatal("仓库的 hooks 不被改动")
	}
}
