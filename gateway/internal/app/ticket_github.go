package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/repos"
	"nano-agent/gateway/internal/ticket"
)

// 连了 GitHub 的工作目录，工单都走 PR 交付（设计 §16.3）：验收通过、宿主提交之后，推送、开 PR；之后 PR 合并、关闭、
// CI 失败、有人审阅，都由轮询或 webhook 来的事件推成工单的事实。每一步都先查再做，不重放已经成功的那一步。

// repoSource 是连了仓库时建 worktree 的起点来源：连接的那个网址、默认分支，和 fetch 要用的凭据。
type repoSource struct {
	URL  string
	Base string
	Env  map[string]string
}

// repoOfTicket 是这张工单的仓库连接（工作目录连了 GitHub 才有）。调用方持有锁。
func (a *App) repoOfTicket(t ticket.Ticket) (repos.Repo, bool) {
	if a.repos == nil {
		return repos.Repo{}, false
	}
	return a.repos.ByWorkspace(t.Auth.Workspace)
}

// pushURL 是推送和 fetch 的目标：显式的 https://github.com/<Owner>/<Name>.git，不是 origin
// （origin 指到哪里由仓库配置决定，凭据只该给连接的这个仓库）。测试里可以换成本地的裸仓库。
func (a *App) pushURL(r repos.Repo) string {
	if a.repoPushURL != nil {
		return a.repoPushURL(r)
	}
	return "https://github.com/" + r.Owner + "/" + r.Name + ".git"
}

// gitAuthEnv 是推送和 fetch 的凭据：只走环境变量，键按网址限定（只发给 github.com），不进参数、不落文件。
func gitAuthEnv(token string) map[string]string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + basic,
	}
}

// scrubToken 把 token 从错误文本里去掉。
func scrubToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "***")
	return strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)), "***")
}

// startSource 是连了仓库时工单开工的起点来源；没连返回空。不持有锁调用。
func (a *App) startSource(t ticket.Ticket) (repoSource, error) {
	a.mu.Lock()
	r, ok := a.repoOfTicket(t)
	a.mu.Unlock()
	if !ok {
		return repoSource{}, nil
	}
	c, err := a.repoClient(r)
	if err != nil {
		return repoSource{}, err
	}
	base := r.Base
	if base == "" { // 没填默认分支：问 GitHub
		ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
		defer cancel()
		if base, err = c.CheckAccess(ctx); err != nil || base == "" {
			return repoSource{}, fmt.Errorf("查不到仓库的默认分支：%w", err)
		}
	}
	return repoSource{URL: a.pushURL(r), Base: base, Env: gitAuthEnv(c.Token)}, nil
}

// pushBranch 把工单的分支推到连接的仓库。先核对本地分支的提交就是工单记下的 Commit；
// 远端已经是这个提交就不重推（不重放已经成功的那一步）。
func (a *App) pushBranch(ctx context.Context, t ticket.Ticket, repo ticketRepo, r repos.Repo, token string) (string, error) {
	b := ticketBranch(t.ID)
	script := repo.repoEnv() + `set -eu
b=` + shellLiteral(b, false) + `
url=` + shellLiteral(a.pushURL(r), false) + `
local=$(g rev-parse "refs/heads/$b")
[ "$local" = ` + shellLiteral(t.Commit, false) + ` ] || { echo "NANO_BADREF=$local"; exit 0; }
remote=$(g ls-remote "$url" "refs/heads/$b" | cut -f1)
if [ "$remote" = "$local" ]; then echo NANO_PUSH=skip; exit 0; fi
g push -q "$url" "refs/heads/$b:refs/heads/$b"
echo NANO_PUSH=done
`
	dirs := []string{repo.gitDir()}
	if strings.HasPrefix(a.pushURL(r), "/") { // 本地路径的“远端”（只有测试里会这样）要写得进去
		dirs = append(dirs, a.pushURL(r))
	}
	out, err := a.runTicketShellEnv(ctx, t, repo, repo.hostSpec(dirs, nil), script, "push", ticketGitTimeout, gitAuthEnv(token))
	if err != nil {
		return "", errors.New(scrubToken(err.Error(), token))
	}
	if strings.Contains(out, "NANO_BADREF=") {
		return "", errors.New("本地分支的提交不是工单记下的提交，没有推送")
	}
	p := marker(out, "PUSH")
	if p != "done" && p != "skip" {
		return "", errors.New("推送的输出看不懂：" + scrubToken(tailLines(out, 3), token))
	}
	a.mu.Lock()
	a.lastPushOutcome = p
	a.mu.Unlock()
	return p, nil
}

// deleteBranch 删本地分支（PR 合并之后）。
func (a *App) deleteBranch(ctx context.Context, t ticket.Ticket, repo ticketRepo) error {
	script := repo.repoEnv() + "g branch -D " + shellLiteral(ticketBranch(t.ID), false) + " >/dev/null 2>&1 || true\n"
	_, err := a.runTicketShell(ctx, t, repo, repo.hostSpec([]string{repo.gitDir()}, nil), script, "branch", ticketGitTimeout)
	return err
}

// prBody 是 PR 的正文：目标、谁派的、原消息链接、验收命令和结果。
func prBody(t ticket.Ticket, r repos.Repo, summary, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**目标**：%s\n\n**谁派的**：%s\n", t.Goal, t.Auth.By)
	src := t.Auth.Source
	if src.Channel == "gh" {
		if repo, n, ok := splitChat(src.Chat); ok {
			fmt.Fprintf(&b, "**原消息**：https://github.com/%s/issues/%d（评论 %s）\n", repo, n, src.MsgID)
		}
	} else {
		fmt.Fprintf(&b, "**原消息**：%s 的 %s（%s）\n", src.Channel, src.Chat, src.MsgID)
	}
	check := "没有验收命令，只看了改动"
	if strings.TrimSpace(t.Check) != "" {
		check = "`" + t.Check + "`"
	}
	fmt.Fprintf(&b, "**验收**：%s\n", check)
	if note != "" {
		fmt.Fprintf(&b, "\n> %s\n", note)
	} else if t.CheckLog != "" {
		fmt.Fprintf(&b, "\n验收输出（最后几行）：\n```\n%s\n```\n", clip(t.CheckLog, 800))
	}
	if summary != "" {
		fmt.Fprintf(&b, "\n改动：%s\n", summary)
	}
	return b.String()
}

func splitChat(chat string) (string, int, bool) {
	repo, num, ok := strings.Cut(chat, "#")
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	return repo, n, err == nil
}

// deliverTicketPR 是交付：推送、开 PR（开着的 PR 已经在了就接着用）、记下 PR，状态 ready，结果发回原消息。
// 失败就 blocked，带上（打码的）错误。forced 是“先收下”：正文写明验收没过、由谁收下。不持有锁调用。
func (a *App) deliverTicketPR(id, summary string, forced bool, by string) {
	a.mu.Lock()
	t, ok := a.tickets.Get(id)
	if !ok || ticket.IsEnded(t.Status) {
		a.mu.Unlock()
		return
	}
	r, connected := a.repoOfTicket(t)
	repo, rerr := a.ticketRepoOf(t)
	if connected && forced && t.ForcedBy != by { // 记下是谁先收下的：交付失败重试时，PR 正文还要写这句
		if _, err := a.tickets.Update(id, func(t *ticket.Ticket) { t.ForcedBy = by }); err != nil {
			a.ticketHealth(err)
		}
	}
	a.mu.Unlock()
	if !connected {
		return
	}
	hadPR := t.PR != "" // PR 开了以后的各轮（审阅意见、CI 返工）：结果发在 PR 上
	block := func(by, why string) {
		a.mu.Lock()
		a.ticketBlock(id, by, why, "", true)
		a.mu.Unlock()
	}
	c, err := a.repoClient(r)
	if err != nil || rerr != nil {
		block("error", "交付时出错："+firstNonEmpty(errText(err), errText(rerr)))
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	pushed, err := a.pushBranch(ctx, t, repo, r, c.Token)
	if err != nil {
		block("push", "推送失败："+clip(err.Error(), 500))
		return
	}
	// 开 PR：先按分支查，开着的就接着用，不重复开
	base := r.Base
	if base == "" {
		if base, err = c.CheckAccess(ctx); err != nil {
			block("pr", "开 PR 失败（查不到默认分支）："+clip(err.Error(), 400))
			return
		}
	}
	head := ticketBranch(id)
	pull, found, err := c.PullByHead(ctx, head) // 只认开着的：合并了或关了的可能是以前留下的同名分支
	if err != nil {
		block("pr", "开 PR 失败："+clip(err.Error(), 400))
		return
	}
	if !found {
		note := ""
		if forced {
			note = fmt.Sprintf("验收没过，由 %s 收下：现在的改动先放上来。", by)
		}
		pull, err = c.CreatePull(ctx, t.Title, prBody(t, r, summary, note), head, base)
		if err != nil {
			block("pr", "开 PR 失败："+clip(err.Error(), 400))
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.tickets.Get(id); !ok || ticket.IsEnded(cur.Status) {
		return
	}
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) {
		t.PR, t.PRHead, t.Status, t.WantCheck, t.BlockedBy, t.Reason = strconv.Itoa(pull.Number), t.Commit, ticket.StatusReady, false, "", ""
	}); err != nil {
		a.ticketHealth(err)
		return
	}
	a.publishCommitments()
	if hadPR {
		short := t.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		text := fmt.Sprintf("改了：%s（提交 %s）", firstNonEmpty(summary, "有改动"), short)
		if pushed == "skip" {
			text = "看过了，没有新的改动：" + firstNonEmpty(summary, "没有要改的")
		}
		a.mu.Unlock()
		cctx, ccancel := context.WithTimeout(a.ctx, 20*time.Second)
		if err := c.Comment(cctx, pull.Number, text); err != nil {
			a.log.Warn("在 PR 上回复失败", "ticket", id, "err", scrubToken(err.Error(), c.Token))
		}
		ccancel()
		a.mu.Lock()
		return
	}
	text := fmt.Sprintf("改好了，PR 在这里：%s。%s", pull.HTMLURL, firstNonEmpty(summary, "有改动"))
	if forced {
		text = fmt.Sprintf("验收没过，由 %s 收下，先开了 PR：%s。%s", by, pull.HTMLURL, firstNonEmpty(summary, "有改动"))
	}
	a.ticketPush(id, "ready", text, "")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- PR 和 CI 的事件变成工单的事实 ----

// repoTrackedPRsLocked 是这个仓库里工单相关的、还没结束的 PR 号。
func (a *App) repoTrackedPRsLocked(r repos.Repo) []int {
	var out []int
	if a.tickets == nil {
		return nil
	}
	for _, t := range a.tickets.List() {
		if t.Auth.Workspace == r.Workspace && t.PR != "" && !ticket.IsEnded(t.Status) {
			if n, err := strconv.Atoi(t.PR); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// ticketByPR 找这个 PR 对应的工单。调用方持有锁。
func (a *App) ticketByPR(r repos.Repo, n int) (ticket.Ticket, bool) {
	if a.tickets == nil {
		return ticket.Ticket{}, false
	}
	for _, t := range a.tickets.List() {
		if t.Auth.Workspace == r.Workspace && t.PR == strconv.Itoa(n) {
			return t, true
		}
	}
	return ticket.Ticket{}, false
}

// onPRState 是 PR 的终态：合并了 → done（证据是合并提交，不再发消息，删 worktree 和本地分支）；关了没合并 → stopped。
func (a *App) onPRState(r repos.Repo, ev repos.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.ticketByPR(r, ev.Number)
	if !ok || ticket.IsEnded(t.Status) {
		return nil
	}
	status, reason := ticket.StatusStopped, "PR 被关了，没有合并"
	if ev.State == "merged" {
		status, reason = ticket.StatusDone, fmt.Sprintf("PR #%d 已合并：%s", ev.Number, firstNonEmpty(ev.SHA, "（没有合并提交）"))
	}
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) { t.Status, t.Reason, t.WantCheck, t.BlockedBy = status, reason, false, "" }); err != nil {
		a.ticketHealth(err)
		return err
	}
	a.publishCommitments()
	done, _ := a.tickets.Get(t.ID)
	a.wrapUpTicket(done)
	if status == ticket.StatusDone {
		if repo, err := a.ticketRepoOf(done); err == nil {
			go func() {
				ctx, cancel := context.WithTimeout(a.ctx, 2*ticketGitTimeout)
				defer cancel()
				// 分支还签出在 worktree 里：先删 worktree（幂等，和 wrapUp 的清理一样），再删本地分支
				_ = a.removeWorktree(ctx, done, repo)
				_ = a.deleteBranch(ctx, done, repo)
			}()
		}
	}
	return nil
}

// onCheck 是 PR 上的检查结果：失败算一次返工（和本地验收的返工共用计数）；每个提交只算一次，只认推上去的最新提交。
func (a *App) onCheck(r repos.Repo, ev repos.Event) error {
	if !repos.FailedConclusion(ev.State) {
		return nil
	}
	a.mu.Lock()
	t, ok := a.ticketByPR(r, ev.Number)
	if !ok || ticket.IsEnded(t.Status) || t.Status != ticket.StatusReady || t.PRHead != ev.SHA || t.CIHandled == ev.SHA {
		a.mu.Unlock()
		return nil
	}
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) { t.CIHandled = ev.SHA }); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	log := "CI 失败（" + ev.State + "）"
	if c, err := a.repoClient(r); err == nil {
		ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
		runs, rerr := c.CheckRuns(ctx, ev.SHA)
		cancel()
		if rerr == nil {
			var b strings.Builder
			for _, run := range runs {
				switch run.Conclusion {
				case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
					fmt.Fprintf(&b, "- %s：%s\n%s\n", run.Name, run.Conclusion, clip(strings.TrimSpace(run.Output.Summary), 600))
				}
			}
			if b.Len() > 0 {
				log = clip(b.String(), 2000)
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.tickets.Get(t.ID)
	if !ok || ticket.IsEnded(cur.Status) || cur.Status != ticket.StatusReady {
		return nil
	}
	a.reworkTicketFrom(cur, "CI", log)
	return nil
}

// onReview 是名单里的人在 PR 上提的审阅意见（审阅、行内意见、PR 对话里的评论）：作为 note 送进分支，不算返工。
func (a *App) onReview(r repos.Repo, ev repos.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.ticketByPR(r, ev.Number)
	if !ok || ticket.IsEnded(t.Status) {
		return nil
	}
	// PR 对话里的普通评论是人和人说话的地方，要点名才送；批准不是要它再干一轮
	if strings.EqualFold(ev.Actor, r.Bot) || !a.allowedOnRepo(r, ev.Actor) || strings.TrimSpace(ev.Text) == "" ||
		ev.Kind == repos.KindComment && !mentions(ev.Text, r.Bot) || ev.Kind == repos.KindReview && ev.State == "approved" {
		a.emitRepoLogOnce(r, ev, "repo.ignored")
		return nil
	}
	text := ev.Text
	if ev.Kind == repos.KindComment {
		text = stripMention(text, r.Bot)
	}
	text = clip(strings.TrimSpace(text), 2000)
	if err := a.ticketDeliver(t, "gh:"+ev.Actor, "[审阅意见 · "+ev.Actor+"] "+text); err != nil {
		if errors.Is(err, errTicketStore) {
			return err // 存储的错误：留着重放
		}
		a.log.Info("审阅意见没有送进分支", "ticket", t.ID, "why", err.Error()) // 业务上的拒绝（续满了、没有分支）算处理完
	}
	return nil
}

var _ = commit.KindTicket
