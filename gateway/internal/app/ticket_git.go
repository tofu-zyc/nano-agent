package app

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
	"nano-agent/gateway/internal/ticket"
)

// 工单的 git 步骤（设计 §16.2）：建 worktree、验收、提交、清理都由宿主做，不经过模型，
// 通过 gatewayShell 在仓库所在的机器上按明确的读写范围执行。每一步都可以重做：
// worktree、分支已经在了就接着用；提交带 Nano-Ticket 行，已经提交过就收养那一个。
// 这些脚本假定 POSIX shell；Windows 上的仓库暂不支持。

// gitPrelude 放在每条脚本前面：沙箱读不了 ~/.gitconfig，而 git 把读不了当成致命错误，
// 所以不读全局、系统和 XDG 的配置，也不在终端上问口令。
const gitPrelude = "export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null XDG_CONFIG_HOME=/nonexistent GIT_TERMINAL_PROMPT=0\n"

const (
	ticketCheckTimeout = 30 * time.Minute
	ticketGitTimeout   = 5 * time.Minute
)

// ticketRepo 是工单的仓库：团队设置里的工作目录。
type ticketRepo struct {
	machine string
	root    string // 仓库在那台机器上的路径
}

func (r ticketRepo) worktree(id string) string { return path.Join(r.root, ".nano", "worktrees", id) }
func (r ticketRepo) gitDir() string            { return path.Join(r.root, ".git") }

func ticketBranch(id string) string { return "nano/" + id }

// ticketRepoOf 找工单的仓库。调用方持有锁。
func (a *App) ticketRepoOf(t ticket.Ticket) (ticketRepo, error) {
	ws := a.findWorkspace(t.Auth.Workspace)
	if ws == nil {
		return ticketRepo{}, errors.New("团队的工作目录已经不存在")
	}
	if a.machineOS(ws.Host) == "windows" {
		return ticketRepo{}, errors.New("暂不支持 Windows 上的仓库")
	}
	return ticketRepo{machine: ws.Host, root: ws.Path}, nil
}

// gitSpec 是宿主那一步的读写范围：dirs 可写，只读是仓库的 .git 和技能缓存。
func (r ticketRepo) gitSpec(dirs ...string) proto.WorkspaceSpec {
	return proto.WorkspaceSpec{Name: r.root, Dirs: dirs, RO: []string{skills.CacheDir, r.gitDir()}}
}

// runTicketShell 跑一条脚本，沙箱必须有（"on"）。非零退出码返回错误，错误里带输出的最后几行。
func (a *App) runTicketShell(ctx context.Context, t ticket.Ticket, repo ticketRepo, spec proto.WorkspaceSpec, script, key string, timeout time.Duration) (string, error) {
	res, err := a.gatewayShell(ctx, shellRun{Machine: repo.machine, Spec: spec, Sandbox: "on", Command: gitPrelude + script, Timeout: timeout,
		Key: t.ID + "-" + key, Agent: t.Agent, Session: t.Auth.Scene, By: "ticket:" + t.ID, Priority: 10})
	if err != nil {
		return "", err
	}
	if !res.OK {
		return res.Content, errors.New(tailLines(res.Content, 20))
	}
	return res.Content, nil
}

// tailLines 取最后 n 行。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// marker 从输出里取 NANO_<名>=<值> 的值（最后一次出现）。
func marker(out, name string) string {
	v := ""
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "NANO_"+name+"="); ok {
			v = rest
		}
	}
	return v
}

// createWorktree 建工单的 worktree 和分支；已经在了就接着用。返回起点提交和 worktree 自己的 git 目录。
func (a *App) createWorktree(ctx context.Context, t ticket.Ticket, repo ticketRepo) (base, gitDir string, err error) {
	root, wt, branch := shellLiteral(repo.root, false), shellLiteral(repo.worktree(t.ID), false), shellLiteral(ticketBranch(t.ID), false)
	script := `set -eu
repo=` + root + `; wt=` + wt + `; branch=` + branch + `
cd "$repo"
mkdir -p "$repo/.nano/worktrees" "$repo/.git/info"
grep -qx '/.nano/' "$repo/.git/info/exclude" 2>/dev/null || echo '/.nano/' >> "$repo/.git/info/exclude"
if git remote | grep -q .; then git fetch --quiet origin || true; fi
ref=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)
[ -n "$ref" ] || ref=$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)
[ -n "$ref" ] || ref=HEAD
if git show-ref --verify --quiet "refs/heads/$branch"; then
  if [ ! -d "$wt" ]; then git worktree prune; git worktree add -q "$wt" "$branch"; fi
  base=$(git merge-base "$branch" "$ref" 2>/dev/null || git rev-parse "$branch")
else
  base=$(git rev-parse "$ref^{commit}")
  git worktree add -q -b "$branch" "$wt" "$base"
fi
echo "NANO_BASE=$base"
echo "NANO_GITDIR=$(git -C "$wt" rev-parse --absolute-git-dir)"
`
	out, err := a.runTicketShell(ctx, t, repo, repo.gitSpec(repo.root), script, "worktree", ticketGitTimeout)
	if err != nil {
		return "", "", fmt.Errorf("建 worktree 失败：%w", err)
	}
	base, gitDir = marker(out, "BASE"), marker(out, "GITDIR")
	if base == "" || gitDir == "" {
		return "", "", errors.New("建 worktree 的输出看不懂：" + tailLines(out, 5))
	}
	return base, gitDir, nil
}

// removeWorktree 删 worktree（git 分支留着）。已经没有了也算成功。
func (a *App) removeWorktree(ctx context.Context, t ticket.Ticket, repo ticketRepo) error {
	root, wt := shellLiteral(repo.root, false), shellLiteral(repo.worktree(t.ID), false)
	script := `repo=` + root + `; wt=` + wt + `
cd "$repo" || exit 0
git worktree remove --force "$wt" 2>/dev/null || rm -rf "$wt"
git worktree prune
`
	_, err := a.runTicketShell(ctx, t, repo, repo.gitSpec(repo.root), script, "cleanup", ticketGitTimeout)
	return err
}

// branchSpec 是工单的分支和验收命令的读写范围：可写 worktree 和它自己的 git 目录，
// 只读 <仓库>/.git 和技能缓存；系统目录和 PATH 里的目录执行器本来就放行。
func (r ticketRepo) branchSpec(t ticket.Ticket) proto.WorkspaceSpec {
	dirs := []string{r.worktree(t.ID)}
	if t.GitDir != "" {
		dirs = append(dirs, t.GitDir)
	}
	return proto.WorkspaceSpec{Name: r.worktree(t.ID), Dirs: dirs, RO: []string{skills.CacheDir, r.gitDir()}}
}

// checkResult 是验收命令的结果：退出码是否为 0，和输出的最后 40 行。
type checkResult struct {
	Passed bool
	Log    string
}

// runCheck 在 worktree 里跑验收命令。没有验收命令算通过（调用方再看有没有改动）。
func (a *App) runCheck(ctx context.Context, t ticket.Ticket, repo ticketRepo) (checkResult, error) {
	if strings.TrimSpace(t.Check) == "" {
		return checkResult{Passed: true, Log: "（没有验收命令，只看有没有改动）"}, nil
	}
	script := gitPrelude + "cd " + shellLiteral(repo.worktree(t.ID), false) + " && " + t.Check
	res, err := a.gatewayShell(ctx, shellRun{Machine: repo.machine, Spec: repo.branchSpec(t), Sandbox: "on", Command: script, Timeout: ticketCheckTimeout,
		Key: t.ID + "-check", Agent: t.Agent, Session: t.Auth.Scene, By: "ticket:" + t.ID, Priority: 10})
	if err != nil {
		return checkResult{}, err
	}
	return checkResult{Passed: res.OK, Log: tailLines(res.Content, 40)}, nil
}

// commitResult 是宿主提交的结果。
type commitResult struct {
	Commit  string // 提交；没有改动为空
	Changed bool
	Summary string // 改动摘要（文件数、增删行）
}

// commitWork 在 worktree 里 git add -A 并提交：作者用 agent 的名字，说明是标题加工单号，末尾一行 Nano-Ticket。
// 重做时 worktree 是干净的、已经有带这一行的提交，就收养它，不再提交。宿主这一步可写 worktree 和 <仓库>/.git。
func (a *App) commitWork(ctx context.Context, t ticket.Ticket, repo ticketRepo, author string) (commitResult, error) {
	wt := shellLiteral(repo.worktree(t.ID), false)
	trailer := "Nano-Ticket: " + t.ID
	subject := strings.TrimSpace(strings.ReplaceAll(t.Title, "\n", " ")) + " (" + t.ID + ")"
	script := `set -eu
cd ` + wt + `
base=` + shellLiteral(t.Base, false) + `
trailer=` + shellLiteral(trailer, false) + `
if [ -z "$(git status --porcelain)" ] && git log --format=%B "$base..HEAD" | grep -qxF "$trailer"; then
  echo "NANO_COMMIT=$(git rev-parse HEAD)"
  echo "NANO_STAT=$(git diff --shortstat "$base" HEAD)"
  exit 0
fi
git add -A
if git diff --cached --quiet; then
  if git log --format=%B "$base..HEAD" | grep -qxF "$trailer"; then
    echo "NANO_COMMIT=$(git rev-parse HEAD)"
    echo "NANO_STAT=$(git diff --shortstat "$base" HEAD)"
  else
    echo "NANO_NOCHANGE=1"
  fi
  exit 0
fi
git -c user.name=` + shellLiteral(author, false) + ` -c user.email=nano-agent@localhost -c core.hooksPath=/dev/null commit -q --no-verify -m ` + shellLiteral(subject, false) + ` -m "$trailer"
echo "NANO_COMMIT=$(git rev-parse HEAD)"
echo "NANO_STAT=$(git diff --shortstat "$base" HEAD)"
`
	out, err := a.runTicketShell(ctx, t, repo, repo.gitSpec(repo.worktree(t.ID), repo.gitDir()), script, "commit", ticketGitTimeout)
	if err != nil {
		return commitResult{}, fmt.Errorf("提交失败：%w", err)
	}
	c := marker(out, "COMMIT")
	if c == "" {
		if marker(out, "NOCHANGE") != "" {
			return commitResult{}, nil
		}
		return commitResult{}, errors.New("提交的输出看不懂：" + tailLines(out, 5))
	}
	return commitResult{Commit: c, Changed: true, Summary: strings.TrimSpace(marker(out, "STAT"))}, nil
}
