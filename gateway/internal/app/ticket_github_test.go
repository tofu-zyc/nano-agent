package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/repos"
	"nano-agent/gateway/internal/repos/fakegithub"
	"nano-agent/gateway/internal/ticket"
)

// 连了 GitHub 的工作目录：工单走 PR 交付。用本地的假 GitHub、临时目录里的 git 仓库和一个本地的裸仓库当“远端”。

// recHost 记下每次发给执行器的请求（断言推送的命令行和环境）。
type recHost struct {
	host.Host
	mu   sync.Mutex
	reqs []string
}

func (h *recHost) Exec(ctx context.Context, req []byte) host.Result {
	h.mu.Lock()
	h.reqs = append(h.reqs, string(req))
	h.mu.Unlock()
	return h.Host.Exec(ctx, req)
}

func (h *recHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reqs...)
}

type recHosts struct {
	rec      *recHost
	fallback *fakeHosts
}

func (h recHosts) Host(name string) (host.Host, host.Info, bool) {
	if name == host.LocalName {
		return h.rec, host.Info{Name: name, Kind: "local", Ready: true, Landlock: 7, OS: "linux"}, true
	}
	return h.fallback.Host(name)
}
func (h recHosts) Infos() []host.Info { return h.fallback.Infos() }

type prEnv struct {
	*gitEnv
	gh    *fakegithub.Server
	bare  string // 连接的仓库（本地的裸仓库）
	decoy string // origin 指向的另一个裸仓库：不该收到任何东西
	rec   *recHost
	repo  RepoView
}

func bareRun(t *testing.T, bare string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", bare}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newPREnv(t *testing.T, roster ...string) *prEnv {
	t.Helper()
	g := newGitEnv(t)
	executable, err := host.NewExecutor("../../../kernel/nano-exec")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recHost{Host: host.NewLocal(executable, nil)}
	g.a.SetHosts(recHosts{rec, g.hosts})
	p := &prEnv{gitEnv: g, rec: rec}
	p.bare = filepath.Join(t.TempDir(), "connected.git")
	p.decoy = filepath.Join(t.TempDir(), "decoy.git")
	for _, d := range []string{p.bare, p.decoy} {
		gitRun(t, filepath.Dir(d), "init", "-q", "--bare", "-b", "main", d)
	}
	gitRun(t, g.repo, "push", "-q", p.bare, "main")
	gitRun(t, g.repo, "remote", "add", "origin", p.decoy) // origin 指向别处：推送不能用它
	p.gh = fakegithub.New("acme", "app", testToken, "nano-bot")
	t.Cleanup(p.gh.Close)
	p.gh.HeadResolver = func(branch string) string { return bareRun(t, p.bare, "rev-parse", "refs/heads/"+branch) }
	g.a.SetRepoAPIBase(p.gh.URL)
	g.a.SetRepoNoLoop(true)
	g.a.mu.Lock()
	g.a.repoPushURL = func(repos.Repo) string { return p.bare }
	g.a.mu.Unlock()
	if len(roster) == 0 {
		roster = []string{"gh:alice"}
	}
	v, err := g.a.SaveRepo(RepoSave{Workspace: g.t0.Auth.Workspace, Owner: "acme", Name: "app", Base: "main", Token: testToken, Intake: "poll", Roster: roster})
	if err != nil {
		t.Fatal(err)
	}
	p.repo = v
	if _, err := g.a.PollRepo(context.Background(), v.ID); err != nil { // 基线
		t.Fatal(err)
	}
	return p
}

func (p *prEnv) poll(t *testing.T) {
	t.Helper()
	if _, err := p.a.PollRepo(context.Background(), p.repo.ID); err != nil {
		t.Fatal(err)
	}
}

// openPR 点名建工单，分支改了文件、汇报 ready、这一轮结束，验收通过之后推送并开 PR；返回分支的 TInput 和 PR 号。
func (p *prEnv) openPR(t *testing.T, sid string) (proto.Msg, int) {
	t.Helper()
	in := p.startWork(t, sid)
	p.editWorktree(t, "a.txt", "一\n二\n")
	p.a.mu.Lock()
	p.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	p.a.mu.Unlock()
	p.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "PR 开了、工单 ready", func() bool { tk := p.tk("tk-1"); return tk.Status == ticket.StatusReady && tk.PR != "" })
	n := 0
	for _, pl := range p.gh.Pulls() {
		n = pl.Number
	}
	return in, n
}

func TestTicketOnConnectedRepoPushesToTheConnectedURLAndOpensAPR(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "grep -q 二 a.txt")
	drainSent(p.env)
	_, n := p.openPR(t, sid)
	tk := p.tk("tk-1")
	if tk.PR == "" || tk.PRHead != tk.Commit || tk.Commit == "" {
		t.Fatalf("记下 PR 和推上去的提交：%+v", tk)
	}
	// 推到了连接的仓库，没推到 origin
	if got := bareRun(t, p.bare, "rev-parse", "refs/heads/nano/tk-1"); got != tk.Commit {
		t.Fatalf("连接的仓库收到了分支：%s vs %s", got, tk.Commit)
	}
	if out, err := exec.Command("git", "--git-dir", p.decoy, "branch", "--list", "nano/*").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("origin 指向的仓库不该收到任何东西：%q %v", out, err)
	}
	// PR：标题是工单标题，正文有目标、谁派的、原消息、验收命令；目标分支是 Base
	pl, _ := p.gh.Pull(n)
	body := p.gh.PullBody(n)
	for _, want := range []string{"手机上错位", "fake:u2", "grep -q 二 a.txt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR 正文缺 %q：%s", want, body)
		}
	}
	if pl.Head.Ref != "nano/tk-1" || pl.Title != "修登录页" {
		t.Fatalf("PR：%+v", pl)
	}
	reqs := p.gh.Requests()
	var created string
	for _, r := range reqs {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/pulls") {
			created = r.Body
		}
	}
	if !strings.Contains(created, `"base":"main"`) {
		t.Fatalf("PR 的目标分支是 Base：%s", created)
	}
	// 结果回到原消息：PR 链接和一句摘要
	got := p.ch.wait(t, "PR 在这里")
	if !strings.Contains(got, "/pull/") {
		t.Fatalf("结果：%q", got)
	}
	// 推送的命令行参数里没有 token；凭据只在 env 里；验收和提交的请求里没有 env
	var pushReq string
	for _, r := range p.rec.requests() {
		if strings.Contains(r, `g push -q`) {
			pushReq = r
		}
	}
	if pushReq == "" {
		t.Fatal("没有找到推送的请求")
	}
	var parsed struct {
		Args struct {
			Command string `json:"command"`
		} `json:"args"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(pushReq), &parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.Args.Command, testToken) || strings.Contains(parsed.Args.Command, "origin") || !strings.Contains(parsed.Args.Command, p.bare) {
		t.Fatalf("推送的命令行：没有 token、没有 origin、目标是连接的网址：%s", parsed.Args.Command)
	}
	if parsed.Env["GIT_CONFIG_COUNT"] != "1" || parsed.Env["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" ||
		!strings.HasPrefix(parsed.Env["GIT_CONFIG_VALUE_0"], "Authorization: Basic ") {
		t.Fatalf("凭据走按网址限定的环境变量：%+v", parsed.Env)
	}
	for _, r := range p.rec.requests() {
		if strings.Contains(r, `g push -q`) || strings.Contains(r, `"fetch`) || strings.Contains(r, "g fetch") {
			continue
		}
		if strings.Contains(r, `"env"`) {
			t.Fatalf("只有推送和 fetch 带 env：%s", r[:min(len(r), 300)])
		}
		if strings.Contains(r, testToken) {
			t.Fatalf("别的请求里不能有 token：%s", r[:min(len(r), 300)])
		}
	}
	// 连了仓库的工单不能 accept（以 PR 合并为准）
	mention(p.env, "@机器人 收下", "g500", "u2", "李四")
	p.k.expect(proto.TInput)
	if _, err := callTicket(p.env, sid, `{"action":"accept","id":"tk-1"}`); err == nil || !strings.Contains(err.Error(), "PR 合并为准") {
		t.Fatalf("accept 要拒绝：%v", err)
	}
	finishGroupRun(t, p.env, sid)
}

func TestPRMergedFinishesTheTicketWithoutAMessage(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	_, n := p.openPR(t, sid)
	drainSent(p.env)
	p.gh.MergePull(n, "mergecommit123")
	p.poll(t)
	tk := p.tk("tk-1")
	if tk.Status != ticket.StatusDone || !strings.Contains(tk.Reason, "mergecommit123") || !strings.Contains(tk.Reason, "已合并") {
		t.Fatalf("done，证据是合并提交：%+v", tk)
	}
	if sent := drainSent(p.env); len(sent) != 0 {
		t.Fatalf("合并了不再发消息：%v", sent)
	}
	eventually(t, "worktree 和本地分支删掉", func() bool {
		_, err := os.Stat(p.r.worktree("tk-1"))
		out, _ := exec.Command("git", "--git-dir", filepath.Join(p.repo.Workspace, ".git"), "branch", "--list", "nano/tk-1").CombinedOutput()
		_ = out
		return os.IsNotExist(err) && !strings.Contains(gitRun(t, p.gitEnv.repo, "branch", "--list", "nano/tk-1"), "nano/tk-1")
	})
	if v := findCommit(p.a.Commitments("", ""), commit.KindTicket, "tk-1"); v == nil || v.Phase != commit.Done {
		t.Fatalf("承诺列表里是 done：%+v", v)
	}
}

func TestPRClosedWithoutMergingStopsTheTicket(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	_, n := p.openPR(t, sid)
	p.gh.ClosePull(n)
	p.poll(t)
	if tk := p.tk("tk-1"); tk.Status != ticket.StatusStopped || !strings.Contains(tk.Reason, "PR 被关了") {
		t.Fatalf("stopped：%+v", tk)
	}
	eventually(t, "worktree 清理", func() bool { _, err := os.Stat(p.r.worktree("tk-1")); return os.IsNotExist(err) })
}

// CI 失败算一次返工（每个提交只算一次，和本地验收共用计数）；返工用完 blocked。
func TestCIFailureCountsAsOneRework(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	pl, _ := p.gh.Pull(n)
	p.gh.SetChecks(pl.Head.SHA, fakegithub.Check("build", 21, "completed", "failure", "2 个测试失败：TestLogin"))
	p.poll(t)
	p.poll(t) // 同一个失败不重复算
	tk := p.tk("tk-1")
	if tk.Status != ticket.StatusWorking || tk.Rework != 1 || tk.CIHandled != pl.Head.SHA {
		t.Fatalf("CI 失败算一次返工：%+v", tk)
	}
	in2 := p.k.expect(proto.TInput)
	if in2.SID != in.SID {
		t.Fatalf("返工在分支里：%s", in2.SID)
	}
	hist := p.history(in.SID)
	if !strings.Contains(hist, "[CI没过]") || !strings.Contains(hist, "TestLogin") {
		t.Fatalf("返工消息带 CI 的失败信息：%s", hist)
	}
	// 分支又改了、验收、推送：同一个 PR，新的提交；再失败又算一次，到上限 blocked
	p.editWorktree(t, "a.txt", "一\n二\n三\n")
	p.a.mu.Lock()
	p.a.ticketReported("tk-1", commit.StatusReady, "改了", "")
	p.a.mu.Unlock()
	p.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "重新推送、ready", func() bool { tk := p.tk("tk-1"); return tk.Status == ticket.StatusReady && tk.PRHead != pl.Head.SHA })
	if len(p.gh.Pulls()) != 1 {
		t.Fatalf("还是同一个 PR：%d", len(p.gh.Pulls()))
	}
	tk = p.tk("tk-1")
	pl, _ = p.gh.Pull(n)
	p.gh.SetHead(n, tk.PRHead)
	p.gh.SetChecks(tk.PRHead, fakegithub.Check("build", 22, "completed", "failure", "还是挂"))
	p.poll(t)
	eventually(t, "返工用完受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := p.tk("tk-1"); tk.BlockedBy != "rework" || !strings.Contains(tk.Reason, "CI没过") {
		t.Fatalf("受阻：%+v", tk)
	}
}

// 名单里的人的审阅意见作为 note 送进分支（不算返工）；名单外的忽略。
func TestReviewCommentsFromTheRosterBecomeNotes(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	p.gh.AddReview(n, "mallory", "COMMENTED", "名单外的审阅 EVIL")
	p.gh.AddReviewComment(n, "mallory", "名单外的行内意见 EVIL2")
	p.gh.AddReview(n, "alice", "CHANGES_REQUESTED", "这里改成 B 方案")
	p.poll(t)
	tk := p.tk("tk-1")
	if tk.Status != ticket.StatusWorking || tk.Rework != 0 {
		t.Fatalf("审阅意见回到干活，不算返工：%+v", tk)
	}
	if in2 := p.k.expect(proto.TInput); in2.SID != in.SID {
		t.Fatalf("送进分支：%s", in2.SID)
	}
	hist := p.history(in.SID)
	if !strings.Contains(hist, "这里改成 B 方案") || strings.Contains(hist, "EVIL") {
		t.Fatalf("名单里的进、名单外的不进：%s", hist)
	}
}

// 推送成功但开 PR 失败：blocked，错误里没有 token；续上时先查远端，不重推、不重复开 PR。
func TestDeliveryRetryDoesNotReplayWhatSucceeded(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	p.gh.FailNext("/pulls", 500, "boom "+testToken, nil)
	in := p.startWork(t, sid)
	p.editWorktree(t, "a.txt", "一\n二\n")
	p.a.mu.Lock()
	p.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	p.a.mu.Unlock()
	p.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	// PullByHead 先走 GET /pulls（也会命中 FailNext）——让失败落在开 PR 这一步：先放过查询，再让创建失败
	eventually(t, "受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	tk := p.tk("tk-1")
	if tk.BlockedBy != "pr" || strings.Contains(tk.Reason, testToken) || !strings.Contains(tk.Reason, "***") {
		t.Fatalf("开 PR 失败，理由里没有 token：%+v", tk)
	}
	if got := bareRun(t, p.bare, "rev-parse", "refs/heads/nano/tk-1"); got != tk.Commit {
		t.Fatal("推送是成功的")
	}
	if len(p.gh.Pulls()) != 0 {
		t.Fatal("PR 没开成")
	}
	// 名单里的人让它重试：推送先查远端（已经是这个提交，不重推），PR 只开一个
	mention(p.env, "@机器人 再试一次", "g600", "u2", "李四")
	p.k.expect(proto.TInput)
	if _, err := callTicket(p.env, sid, `{"action":"note","id":"tk-1","text":"重试一下"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, p.env, sid)
	eventually(t, "重试后 ready", func() bool { return p.tk("tk-1").Status == ticket.StatusReady })
	p.a.mu.Lock()
	outcome := p.a.lastPushOutcome
	p.a.mu.Unlock()
	if outcome != "skip" {
		t.Fatalf("远端已经有这个提交：不重推：%q", outcome)
	}
	if len(p.gh.Pulls()) != 1 {
		t.Fatalf("只有一个 PR：%d", len(p.gh.Pulls()))
	}
	if tk := p.tk("tk-1"); tk.Renewals != 1 || tk.Renewed[0].What != "重试交付" {
		t.Fatalf("记在工单上：%+v", tk)
	}
}

// 开工时连接的仓库 fetch 不了：blocked，不是静默跳过。推送失败：blocked，理由写明，不算返工。
func TestFetchAndPushFailuresBlock(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	bad := filepath.Join(t.TempDir(), "does-not-exist.git")
	p.a.mu.Lock()
	p.a.repoPushURL = func(repos.Repo) string { return bad }
	p.a.mu.Unlock()
	mention(p.env, "@机器人 修登录页", "g100", "u2", "李四")
	p.k.expect(proto.TInput)
	if _, err := callTicket(p.env, sid, `{"action":"create","title":"修登录页","goal":"x"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, p.env, sid)
	eventually(t, "受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := p.tk("tk-1"); !strings.Contains(tk.Reason, "建 worktree 失败") || tk.Worktree != "" {
		t.Fatalf("fetch 失败就是失败：%+v", tk)
	}
	// 推送失败：换一张工单，开工正常，交付时远端坏了
	p2 := newPREnv(t)
	sid2 := p2.teamGroupOnRepo(t, "true")
	in := p2.startWork(t, sid2)
	p2.a.mu.Lock()
	p2.a.repoPushURL = func(repos.Repo) string { return bad }
	p2.a.mu.Unlock()
	p2.editWorktree(t, "a.txt", "一\n二\n")
	p2.a.mu.Lock()
	p2.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	p2.a.mu.Unlock()
	p2.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "推送失败受阻", func() bool { return p2.tk("tk-1").Status == ticket.StatusBlocked })
	tk := p2.tk("tk-1")
	if tk.BlockedBy != "push" || !strings.Contains(tk.Reason, "推送失败") || tk.Rework != 0 || strings.Contains(tk.Reason, testToken) {
		t.Fatalf("推送失败：%+v", tk)
	}
}

// “先收下”也开 PR，正文写明验收没过、由谁收下；工单等合并才 done。
func TestForcedAcceptOpensAPRAndWaitsForTheMerge(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "exit 1")
	in := p.startWork(t, sid)
	for i := 0; i < 2; i++ {
		p.editWorktree(t, "a.txt", strings.Repeat("改\n", i+1))
		p.a.mu.Lock()
		p.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
		p.a.mu.Unlock()
		p.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
		if i == 0 {
			p.k.expect(proto.TInput)
		}
	}
	eventually(t, "返工用完受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	mention(p.env, "@机器人 先收下", "g700", "u2", "李四")
	p.k.expect(proto.TInput)
	if out, err := callTicket(p.env, sid, `{"action":"accept","id":"tk-1"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	eventually(t, "PR 开了", func() bool { return p.tk("tk-1").PR != "" })
	tk := p.tk("tk-1")
	if tk.Status != ticket.StatusReady {
		t.Fatalf("等 PR 合并才 done：%s", tk.Status)
	}
	var n int
	for _, pl := range p.gh.Pulls() {
		n = pl.Number
	}
	if body := p.gh.PullBody(n); !strings.Contains(body, "验收没过，由 fake:u2 收下") {
		t.Fatalf("正文写明验收没过、由谁收下：%s", body)
	}
	finishGroupRun(t, p.env, sid)
}

func TestScrubTokenAndAuthEnv(t *testing.T) {
	if got := scrubToken("fatal: "+testToken+" bad "+gitAuthEnv(testToken)["GIT_CONFIG_VALUE_0"], testToken); strings.Contains(got, testToken) {
		t.Fatalf("打码：%s", got)
	}
	env := gitAuthEnv("abc")
	if len(env) != 3 || env["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" {
		t.Fatalf("凭据的键按网址限定：%+v", env)
	}
	body := prBody(ticket.Ticket{Goal: "目标", Auth: ticket.Auth{By: "gh:alice", Source: ticket.Source{Channel: "gh", Chat: "acme/app#5", MsgID: "77"}}, Check: "make test"}, repos.Repo{}, "1 file changed", "")
	if !strings.Contains(body, "https://github.com/acme/app/issues/5") || !strings.Contains(body, "gh:alice") || !strings.Contains(body, "make test") {
		t.Fatalf("PR 正文：%s", body)
	}
}

func (g *gitEnv) history(sid string) string {
	g.a.mu.Lock()
	defer g.a.mu.Unlock()
	var b strings.Builder
	for _, m := range g.a.store.History(sid) {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

// round 让分支改一次文件（content 为空就不改）、汇报 ready、这一轮结束。
func (p *prEnv) round(t *testing.T, in proto.Msg, content string) {
	t.Helper()
	if content != "" {
		p.editWorktree(t, "a.txt", content)
	}
	p.a.mu.Lock()
	p.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	p.a.mu.Unlock()
	p.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
}

func (p *prEnv) prComments(n int) string {
	var b strings.Builder
	for _, c := range p.gh.Comments(n) {
		b.WriteString(c.Body + "\n")
	}
	return b.String()
}

// APPROVED 的审阅只记事件日志：工单还是 ready，分支没跑。
func TestApprovedReviewDoesNotRunTheBranch(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	p.gh.AddReview(n, "alice", "APPROVED", "LGTM 可以合")
	p.poll(t)
	if tk := p.tk("tk-1"); tk.Status != ticket.StatusReady {
		t.Fatalf("批准不是要它再干一轮：%+v", tk)
	}
	if strings.Contains(p.history(in.SID), "LGTM") {
		t.Fatal("批准的正文不送进分支")
	}
}

// PR 对话里的普通评论要点名才送进分支（送之前去掉点名）；审阅和行内意见不用点名。
func TestPlainPRCommentsNeedAMention(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	p.gh.AddComment(n, "alice", "我觉得这样挺好的 CHAT-ONLY")
	p.poll(t)
	if tk := p.tk("tk-1"); tk.Status != ticket.StatusReady || strings.Contains(p.history(in.SID), "CHAT-ONLY") {
		t.Fatalf("没点名的评论不进分支：%+v", tk)
	}
	p.gh.AddComment(n, "alice", "@nano-bot 把变量改个名 DO-IT")
	p.poll(t)
	eventually(t, "点名的送进分支", func() bool { return p.tk("tk-1").Status == ticket.StatusWorking })
	p.k.expect(proto.TInput)
	hist := p.history(in.SID)
	if !strings.Contains(hist, "把变量改个名 DO-IT") || strings.Contains(hist, "@nano-bot 把变量") {
		t.Fatalf("送进去的是去掉点名的正文：%s", hist)
	}
}

// PR 开了以后的各轮：结果发在 PR 上，原消息里不再有第二次“改好了”。
func TestRoundsAfterThePRIsOpenReportOnThePR(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	drainSent(p.env)
	p.gh.AddReview(n, "alice", "CHANGES_REQUESTED", "这里改成 B 方案")
	p.poll(t)
	p.k.expect(proto.TInput)
	p.round(t, in, "一\n二\n三\n")
	eventually(t, "PR 上有结果", func() bool { return strings.Contains(p.prComments(n), "改了：") })
	eventually(t, "回到 ready", func() bool { return p.tk("tk-1").Status == ticket.StatusReady })
	tk := p.tk("tk-1")
	if !strings.Contains(p.prComments(n), "（提交 "+tk.Commit[:7]+"）") {
		t.Fatalf("写明提交前 7 位：%s", p.prComments(n))
	}
	if sent := drainSent(p.env); len(sent) != 0 {
		t.Fatalf("原消息里不再有第二次“改好了”：%v", sent)
	}
	// 再来一轮，分支看过了但没改：远端已经是这个提交
	p.gh.AddReview(n, "alice", "COMMENTED", "还有个问题")
	p.poll(t)
	p.k.expect(proto.TInput)
	p.round(t, in, "")
	eventually(t, "没有新改动的回复", func() bool { return strings.Contains(p.prComments(n), "看过了，没有新的改动") })
	if sent := drainSent(p.env); len(sent) != 0 {
		t.Fatalf("原消息里仍然没有：%v", sent)
	}
}

// 按分支查到的 PR 合并了：不算，照样开新的。
func TestMergedPullOnTheSameBranchIsNotReused(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	old := p.gh.AddMergedPull("nano/tk-1")
	p.openPR(t, sid)
	tk := p.tk("tk-1")
	if tk.Status != ticket.StatusReady || tk.PR == strconv.Itoa(old) || len(p.gh.Pulls()) != 2 {
		t.Fatalf("工单挂在新的 PR 上、还在等合并：%+v", tk)
	}
}

// 先收下、开 PR 失败、续上：PR 正文里还有“验收没过、由谁收下”；重试占着工单。
func TestForcedAcceptSurvivesADeliveryRetry(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "exit 1")
	in := p.startWork(t, sid)
	for i := 0; i < 2; i++ {
		p.round(t, in, strings.Repeat("改\n", i+1))
		if i == 0 {
			p.k.expect(proto.TInput)
		}
	}
	eventually(t, "返工用完受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	mention(p.env, "@机器人 先收下", "g700", "u2", "李四")
	p.k.expect(proto.TInput)
	p.gh.FailNext("/pulls", 500, "boom", nil)
	if _, err := callTicket(p.env, sid, `{"action":"accept","id":"tk-1"}`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "开 PR 失败受阻", func() bool { tk := p.tk("tk-1"); return tk.Status == ticket.StatusBlocked && tk.BlockedBy == "pr" })
	finishGroupRun(t, p.env, sid)
	mention(p.env, "@机器人 再试一次", "g701", "u2", "李四")
	p.k.expect(proto.TInput)
	if _, err := callTicket(p.env, sid, `{"action":"note","id":"tk-1","text":"重试一下"}`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "重试后开了 PR", func() bool { return p.tk("tk-1").PR != "" })
	var n int
	for _, pl := range p.gh.Pulls() {
		n = pl.Number
	}
	if body := p.gh.PullBody(n); !strings.Contains(body, "验收没过，由 fake:u2 收下") {
		t.Fatalf("重试时 PR 正文还写明验收没过、由谁收下：%s", body)
	}
	finishGroupRun(t, p.env, sid)
}

// 检查的失败结论：timed_out、cancelled、action_required、startup_failure 都算失败，webhook 和轮询一致。
func TestEveryFailedConclusionCountsOnWebhookToo(t *testing.T) {
	for _, c := range []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure"} {
		if !repos.FailedConclusion(c) {
			t.Fatalf("%s 算失败", c)
		}
	}
	if repos.FailedConclusion("success") || repos.FailedConclusion("neutral") || repos.FailedConclusion("") {
		t.Fatal("成功、中性不算失败")
	}
	for _, c := range []string{"action_required", "startup_failure"} {
		p := newPREnv(t)
		sid := p.teamGroupOnRepo(t, "true")
		_, n := p.openPR(t, sid)
		pl, _ := p.gh.Pull(n)
		ev := repos.Event{Kind: repos.KindCheck, Number: n, State: c, SHA: pl.Head.SHA}
		if err := p.a.onCheck(p.repoOf(t), ev); err != nil {
			t.Fatal(err)
		}
		if tk := p.tk("tk-1"); tk.Status != ticket.StatusWorking || tk.Rework != 1 {
			t.Fatalf("%s 算一次返工：%+v", c, tk)
		}
	}
}

func (p *prEnv) repoOf(t *testing.T) repos.Repo {
	t.Helper()
	r, ok := p.a.repos.Get(p.repo.ID)
	if !ok {
		t.Fatal("没有连接")
	}
	return r
}

// 审阅意见送不进分支（续满了）是业务拒绝：处理完，不留着重放。
func TestReviewRefusedByTheTicketIsNotReplayed(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	_, n := p.openPR(t, sid)
	p.a.mu.Lock()
	p.a.tickets.Update("tk-1", func(t *ticket.Ticket) { t.RunSID = "" })
	p.a.mu.Unlock()
	p.gh.AddReview(n, "alice", "CHANGES_REQUESTED", "改一下")
	p.poll(t)
	p.a.mu.Lock()
	rt := p.a.repoRTs[p.repo.ID]
	p.a.mu.Unlock()
	if len(rt.state.Pending()) != 0 {
		t.Fatalf("业务上的拒绝算处理完：%+v", rt.state.Pending())
	}
}

// env 的值里有 NUL：执行器的 JSON 库会截掉，所以网关填的时候就拒绝，命令不执行。
func TestEnvWithNULIsRefusedBeforeExecuting(t *testing.T) {
	p := newPREnv(t)
	before := len(p.rec.requests())
	_, err := p.a.gatewayShell(context.Background(), shellRun{Machine: host.LocalName, Key: "nul", Command: "true", Timeout: 5 * time.Second,
		Env: map[string]string{"A": "a\x00b"}})
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("值里有 NUL：%v", err)
	}
	if _, err := p.a.gatewayShell(context.Background(), shellRun{Machine: host.LocalName, Key: "nul2", Command: "true", Timeout: 5 * time.Second,
		Env: map[string]string{"A\x00": "x"}}); err == nil {
		t.Fatal("名字里有 NUL 也拒绝")
	}
	if got := len(p.rec.requests()); got != before {
		t.Fatalf("没有发给执行器：%d -> %d", before, got)
	}
}

// PR 开了以后的一轮推了新提交、查 PR 那步失败、续上重试时推送会跳过：PR 上仍然说“改了”。
func TestZZRetryInALaterRoundStillSaysChanged(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	p.gh.AddReview(n, "alice", "CHANGES_REQUESTED", "这里改成 B 方案")
	p.poll(t)
	p.k.expect(proto.TInput)
	p.gh.FailNext("/pulls", 500, "boom", nil)
	p.round(t, in, "一\n二\n三\n")
	eventually(t, "受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := p.tk("tk-1"); tk.BlockedBy != "pr" {
		t.Fatalf("查 PR 失败受阻：%+v", tk)
	}
	finishGroupRun(t, p.env, sid)
	mention(p.env, "@机器人 再试一次", "g601", "u2", "李四")
	p.k.expect(proto.TInput)
	if _, err := callTicket(p.env, sid, `{"action":"note","id":"tk-1","text":"重试一下"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, p.env, sid)
	eventually(t, "重试后 ready", func() bool { return p.tk("tk-1").Status == ticket.StatusReady })
	eventually(t, "PR 上有回复", func() bool {
		c := p.prComments(n)
		return strings.Contains(c, "改了：") || strings.Contains(c, "看过了")
	})
	if c := p.prComments(n); !strings.Contains(c, "改了：") || strings.Contains(c, "看过了") {
		t.Fatalf("这一轮推了新提交，PR 上却说：%s", c)
	}
}

// PR 开了以后返工用完、先收下：这一轮 PR 上的回复写明由谁收下。
func TestForcedRoundAfterThePRIsOpenSaysWhoAccepted(t *testing.T) {
	p := newPREnv(t)
	sid := p.teamGroupOnRepo(t, "true")
	in, n := p.openPR(t, sid)
	for i, content := range []string{"一\n二\n三\n", "一\n二\n三\n四\n"} {
		tk := p.tk("tk-1")
		pl, _ := p.gh.Pull(n)
		p.gh.SetHead(n, tk.PRHead)
		p.gh.SetChecks(tk.PRHead, fakegithub.Check("build", int64(30+i), "completed", "failure", "挂了"))
		_ = pl
		p.poll(t)
		if i == 0 {
			p.k.expect(proto.TInput)
			p.round(t, in, content)
			eventually(t, "新提交", func() bool { return p.tk("tk-1").Status == ticket.StatusReady && p.tk("tk-1").PRHead != tk.PRHead })
		}
	}
	eventually(t, "返工用完受阻", func() bool { return p.tk("tk-1").Status == ticket.StatusBlocked })
	mention(p.env, "@机器人 先收下", "g800", "u2", "李四")
	p.k.expect(proto.TInput)
	if out, err := callTicket(p.env, sid, `{"action":"accept","id":"tk-1"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	eventually(t, "PR 上有先收下的回复", func() bool { return strings.Contains(p.prComments(n), "验收没过，由 fake:u2 收下：") })
	eventually(t, "回到 ready", func() bool { return p.tk("tk-1").Status == ticket.StatusReady })
	finishGroupRun(t, p.env, sid)
}
