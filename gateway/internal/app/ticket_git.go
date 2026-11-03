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
//
// 宿主的 git 步骤不信 worktree 里的任何东西：worktree 是分支能写的（里面的 .git 文件可以指到它自己造的
// git 目录，在那里的配置里设 core.fsmonitor 之类会执行命令的项），而宿主那一步可写 <仓库>/.git。
// 所以宿主的每个 git 命令都用自己算出的路径（GIT_DIR、GIT_COMMON_DIR、GIT_WORK_TREE），不从 worktree 里发现仓库；
// 都关掉 core.fsmonitor 和 hooks，全局和系统配置置空；提交前核对 HEAD 是工单自己的分支。

// gitPrelude 放在每条脚本前面：不读全局、系统和 XDG 的 git 配置，不在终端上问口令。
const gitPrelude = "export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null XDG_CONFIG_HOME=/nonexistent GIT_TERMINAL_PROMPT=0 GIT_OPTIONAL_LOCKS=0\n"

// gitFn 定义 shell 函数 g：每条 git 命令都关掉会执行命令的配置项。
const gitFn = "g() { git -c core.fsmonitor=false -c core.hooksPath=/dev/null \"$@\"; }\n"

var ticketCheckTimeout = 30 * time.Minute // 变量：测试里缩短

const ticketGitTimeout = 5 * time.Minute

// errBadHead 是 worktree 的 HEAD 不是工单自己的分支：不提交，工单 blocked。
var errBadHead = errors.New("worktree 的 HEAD 不是这张工单自己的分支，没有提交")

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

// repoEnv 是在主仓库上跑 git 的环境：显式指定 git 目录和工作区，不靠当前目录发现。
func (r ticketRepo) repoEnv() string {
	return gitPrelude + "export GIT_DIR=" + shellLiteral(r.gitDir(), false) + " GIT_COMMON_DIR=" + shellLiteral(r.gitDir(), false) + " GIT_WORK_TREE=" + shellLiteral(r.root, false) + "\n" + gitFn
}

// worktreeEnv 是在工单的 worktree 上跑 git 的环境：git 目录是宿主算出来的 <仓库>/.git/worktrees/<名字>，
// 不读 worktree 里的 .git 文件。
func (r ticketRepo) worktreeEnv(t ticket.Ticket) string {
	return gitPrelude + "export GIT_DIR=" + shellLiteral(t.GitDir, false) + " GIT_COMMON_DIR=" + shellLiteral(r.gitDir(), false) + " GIT_WORK_TREE=" + shellLiteral(r.worktree(t.ID), false) + "\n" + gitFn
}

// hostSpec 是宿主那一步的读写范围：dirs 可写；只读是 ro、仓库的 .git 和技能缓存。
func (r ticketRepo) hostSpec(dirs, ro []string) proto.WorkspaceSpec {
	return proto.WorkspaceSpec{Name: r.root, Dirs: dirs, RO: append([]string{skills.CacheDir, r.gitDir()}, ro...)}
}

// branchSpec 是工单的分支和验收命令的读写范围：可写只有 worktree；只读是 <仓库>/.git（包括 worktree 自己的 git 目录：
// 它能写就能把 HEAD 指到主分支）和技能缓存，系统目录和 PATH 里的目录执行器本来就放行。
func (r ticketRepo) branchSpec(t ticket.Ticket) proto.WorkspaceSpec {
	return proto.WorkspaceSpec{Name: r.worktree(t.ID), Dirs: []string{r.worktree(t.ID)}, RO: []string{skills.CacheDir, r.gitDir()}}
}

// runTicketShell 跑一条脚本，沙箱必须有（"on"）。非零退出码返回错误，错误里带输出的最后几行。
func (a *App) runTicketShell(ctx context.Context, t ticket.Ticket, repo ticketRepo, spec proto.WorkspaceSpec, script, key string, timeout time.Duration) (string, error) {
	return a.runTicketShellEnv(ctx, t, repo, spec, script, key, timeout, nil)
}

// runTicketShellEnv 同 runTicketShell，还带环境变量（只有推送和连了仓库时的 fetch 带凭据）。
func (a *App) runTicketShellEnv(ctx context.Context, t ticket.Ticket, repo ticketRepo, spec proto.WorkspaceSpec, script, key string, timeout time.Duration, env map[string]string) (string, error) {
	res, err := a.gatewayShell(ctx, shellRun{Machine: repo.machine, Spec: spec, Sandbox: "on", Command: script, Timeout: timeout,
		Key: t.ID + "-" + key, Agent: t.Agent, Session: t.Auth.Scene, By: "ticket:" + t.ID, Priority: 10, Env: env})
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
// git 目录从主仓库的 worktree 登记里查（worktree list 里有这个路径，再按管理目录里记的 gitdir 找名字），不读 worktree 里的 .git 文件。
func (a *App) createWorktree(ctx context.Context, t ticket.Ticket, repo ticketRepo, src ...repoSource) (base, gitDir string, err error) {
	root, wt, branch := shellLiteral(repo.root, false), shellLiteral(repo.worktree(t.ID), false), shellLiteral(ticketBranch(t.ID), false)
	// 起点：连了 GitHub 的仓库，从连接的那个网址 fetch 默认分支到一个私有的引用（不碰 origin：凭据只给连接的这个仓库），
	// fetch 失败就是失败；没连的，有远端就 fetch origin、取 origin/HEAD，没有就取当前签出的分支。
	startRef := "if g remote | grep -q .; then g fetch --quiet origin || true; fi\n" +
		"ref=$(g symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)\n" +
		"[ -n \"$ref\" ] || ref=$(g symbolic-ref --quiet --short HEAD 2>/dev/null || true)\n" +
		"[ -n \"$ref\" ] || ref=HEAD\n"
	var env map[string]string
	if len(src) > 0 && src[0].URL != "" {
		startRef = "g fetch --quiet --no-tags " + shellLiteral(src[0].URL, false) + " " + shellLiteral("+refs/heads/"+src[0].Base+":refs/nano/base", false) + "\n" +
			"ref=refs/nano/base\n"
		env = src[0].Env
	}
	script := repo.repoEnv() + `set -eu
repo=` + root + `; wt=` + wt + `; branch=` + branch + `
cd "$repo"
mkdir -p "$repo/.nano/worktrees" "$repo/.git/info"
grep -qx '/.nano/' "$repo/.git/info/exclude" 2>/dev/null || echo '/.nano/' >> "$repo/.git/info/exclude"
` + startRef + `registered() { g worktree list --porcelain | grep -qxF "worktree $wt"; }
if g show-ref --verify --quiet "refs/heads/$branch"; then
  if ! registered; then
    if [ -e "$wt" ]; then echo "NANO_ERR=worktree 目录在但没有登记，不动它" ; exit 0; fi
    g worktree prune
    g worktree add -q "$wt" "$branch"
  fi
  base=$(g merge-base "$branch" "$ref" 2>/dev/null || g rev-parse "$branch")
else
  base=$(g rev-parse "$ref^{commit}")
  g worktree add -q -b "$branch" "$wt" "$base"
fi
registered || { echo "NANO_ERR=建完 worktree 却没有登记"; exit 0; }
for d in "$repo"/.git/worktrees/*/; do
  d=${d%/}
  if [ "$(cat "$d/gitdir" 2>/dev/null)" = "$wt/.git" ]; then echo "NANO_GITDIR=$d"; fi
done
echo "NANO_BASE=$base"
`
	var roExtra []string
	if len(src) > 0 && strings.HasPrefix(src[0].URL, "/") { // 本地路径的“远端”（只有测试里会这样）要读得到
		roExtra = []string{src[0].URL}
	}
	out, err := a.runTicketShellEnv(ctx, t, repo, repo.hostSpec([]string{repo.root}, roExtra), script, "worktree", ticketGitTimeout, env)
	if err != nil {
		return "", "", fmt.Errorf("建 worktree 失败：%w", err)
	}
	if e := marker(out, "ERR"); e != "" {
		return "", "", errors.New("建 worktree 失败：" + e)
	}
	base, gitDir = marker(out, "BASE"), marker(out, "GITDIR")
	if base == "" || gitDir == "" {
		return "", "", errors.New("建 worktree 的输出看不懂：" + tailLines(out, 5))
	}
	return base, gitDir, nil
}

// removeWorktree 删 worktree（git 分支留着）。已经没有了也算成功。不用 git worktree remove（它会读 worktree 里的东西），
// 直接删目录，再让主仓库清掉登记。
func (a *App) removeWorktree(ctx context.Context, t ticket.Ticket, repo ticketRepo) error {
	root, wt := shellLiteral(repo.root, false), shellLiteral(repo.worktree(t.ID), false)
	script := repo.repoEnv() + `repo=` + root + `; wt=` + wt + `
cd "$repo" || exit 0
rm -rf "$wt"
g worktree prune
`
	_, err := a.runTicketShell(ctx, t, repo, repo.hostSpec([]string{repo.root}, nil), script, "cleanup", ticketGitTimeout)
	return err
}

// worktreeDirty 看 worktree 里有没有改动（宿主用自己算出的 git 目录看，不读 worktree 的 .git 文件）。
func (a *App) worktreeDirty(ctx context.Context, t ticket.Ticket, repo ticketRepo) (bool, error) {
	script := repo.worktreeEnv(t) + `cd ` + shellLiteral(repo.worktree(t.ID), false) + `
if [ -n "$(g status --porcelain)" ]; then echo NANO_DIRTY=1; else echo NANO_DIRTY=0; fi
`
	out, err := a.runTicketShell(ctx, t, repo, repo.hostSpec([]string{repo.gitDir()}, []string{repo.worktree(t.ID)}), script, "dirty", ticketGitTimeout)
	if err != nil {
		return false, err
	}
	switch marker(out, "DIRTY") {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, errors.New("看改动的输出看不懂：" + tailLines(out, 3))
}

// checkResult 是验收命令的结果：退出码是否为 0，和输出的最后 40 行。
type checkResult struct {
	Passed bool
	Log    string
}

// runCheck 在 worktree 里跑验收命令。没有验收命令算通过（调用方再看有没有改动）。
// 返回错误表示命令没跑起来（机器不可用、没拿到租约、沙箱起不来、执行出错）：不是改得不对，不算返工。
// 退出码非零和超时算没过。
func (a *App) runCheck(ctx context.Context, t ticket.Ticket, repo ticketRepo) (checkResult, error) {
	if strings.TrimSpace(t.Check) == "" {
		return checkResult{Passed: true, Log: "（没有验收命令，只看有没有改动）"}, nil
	}
	script := "cd " + shellLiteral(repo.worktree(t.ID), false) + " && " + t.Check
	res, err := a.gatewayShell(ctx, shellRun{Machine: repo.machine, Spec: repo.branchSpec(t), Sandbox: "on", Command: script, Timeout: ticketCheckTimeout,
		Key: t.ID + "-check", Agent: t.Agent, Session: t.Auth.Scene, By: "ticket:" + t.ID, Priority: 10})
	if err != nil {
		return checkResult{}, err
	}
	if res.OK {
		return checkResult{Passed: true, Log: tailLines(res.Content, 40)}, nil
	}
	if res.Lost || !(strings.Contains(res.Content, "（退出码 ") || strings.Contains(res.Content, "（执行超时")) {
		return checkResult{}, errors.New("验收命令没跑起来：" + clip(tailLines(res.Content, 5), 400))
	}
	return checkResult{Passed: false, Log: tailLines(res.Content, 40)}, nil
}

// commitResult 是宿主提交的结果。
type commitResult struct {
	Commit  string // 提交；没有改动为空
	Changed bool
	Summary string // 改动摘要（文件数、增删行）
}

// commitWork 在 worktree 里 git add -A 并提交：作者用 agent 的名字，说明是标题加工单号，末尾一行 Nano-Ticket。
// 先核对 HEAD 是 refs/heads/nano/tk-<序号>（不是返回 errBadHead）。重做时 worktree 是干净的、已经有带这一行的提交，就收养它，
// 不再提交。宿主这一步可写 <仓库>/.git，只读 worktree；git 目录是宿主算出来的。
func (a *App) commitWork(ctx context.Context, t ticket.Ticket, repo ticketRepo, author string) (commitResult, error) {
	wt := shellLiteral(repo.worktree(t.ID), false)
	trailer := "Nano-Ticket: " + t.ID
	subject := strings.TrimSpace(strings.ReplaceAll(t.Title, "\n", " ")) + " (" + t.ID + ")"
	script := repo.worktreeEnv(t) + `set -eu
cd ` + wt + `
base=` + shellLiteral(t.Base, false) + `
trailer=` + shellLiteral(trailer, false) + `
head=$(g symbolic-ref -q HEAD || true)
if [ "$head" != ` + shellLiteral("refs/heads/"+ticketBranch(t.ID), false) + ` ]; then echo "NANO_BADHEAD=$head"; exit 0; fi
if [ -z "$(g status --porcelain)" ] && g log --format=%B "$base..HEAD" | grep -qxF "$trailer"; then
  echo "NANO_COMMIT=$(g rev-parse HEAD)"
  echo "NANO_STAT=$(g diff --shortstat "$base" HEAD)"
  exit 0
fi
g add -A
if g diff --cached --quiet; then
  if g log --format=%B "$base..HEAD" | grep -qxF "$trailer"; then
    echo "NANO_COMMIT=$(g rev-parse HEAD)"
    echo "NANO_STAT=$(g diff --shortstat "$base" HEAD)"
  else
    echo "NANO_NOCHANGE=1"
  fi
  exit 0
fi
g -c user.name=` + shellLiteral(author, false) + ` -c user.email=nano-agent@localhost commit -q --no-verify -m ` + shellLiteral(subject, false) + ` -m "$trailer"
echo "NANO_COMMIT=$(g rev-parse HEAD)"
echo "NANO_STAT=$(g diff --shortstat "$base" HEAD)"
`
	out, err := a.runTicketShell(ctx, t, repo, repo.hostSpec([]string{repo.gitDir()}, []string{repo.worktree(t.ID)}), script, "commit", ticketGitTimeout)
	if err != nil {
		return commitResult{}, fmt.Errorf("提交失败：%w", err)
	}
	if marker(out, "BADHEAD") != "" || strings.Contains(out, "NANO_BADHEAD=") {
		return commitResult{}, errBadHead
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
