package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/ticket"
)

// 工单的执行（设计 §16.2）：真执行器、临时目录里的真 git 仓库、模型和内核用替身。

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

func (g *gitEnv) tk(id string) ticket.Ticket {
	g.a.mu.Lock()
	defer g.a.mu.Unlock()
	t, _ := g.a.tickets.Get(id)
	return t
}

// teamGroupOnRepo 把外部群设成团队，仓库是 gitEnv 里的临时仓库；名单是 fake:u2，验收命令是 check。
func (g *gitEnv) teamGroupOnRepo(t *testing.T, check string) string {
	t.Helper()
	sid := extGroup(t, g.env)
	setScene(t, g.env, sid, &scene.Rules{Team: &scene.Team{Roster: []string{"fake:u2"}, Workspace: g.t0.Auth.Workspace, Check: check, Budget: 5000}})
	return sid
}

// startWork 点名建一张工单，结束群里这一轮，等分支开跑。返回群会话 ID 和分支的 TInput。
func (g *gitEnv) startWork(t *testing.T, sid string) proto.Msg {
	t.Helper()
	mention(g.env, "@机器人 修登录页", "g100", "u2", "李四")
	g.k.expect(proto.TInput)
	if out, err := callTicket(g.env, sid, `{"action":"create","title":"修登录页","goal":"手机上错位"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	finishGroupRun(t, g.env, sid) // 群里这一轮结束了，才能取快照
	in := g.k.expect(proto.TInput)
	if in.SID == sid {
		t.Fatalf("分支开跑，不是群：%s", in.SID)
	}
	return in
}

func (g *gitEnv) editWorktree(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.r.worktree("tk-1"), name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 整条路：点名 → 工单 → worktree → 分支开跑 → 改文件、Report ready、这一轮结束 → 验收 → 宿主提交 → 发回原消息 → 收下 → 清理。
func TestTicketEndToEndToLocalBranch(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "grep -q 二 a.txt")
	in := g.startWork(t, sid)
	tk := g.tk("tk-1")
	if tk.Status != ticket.StatusWorking || tk.RunSID != in.SID || tk.Worktree != g.r.worktree("tk-1") || tk.Branch != "nano/tk-1" || tk.Base == "" || tk.GitDir == "" {
		t.Fatalf("工单开工了：%+v", tk)
	}
	// 分支以 ticket:tk-1 的身份、按 Auth.Mode 跑，不能提权；派活的人不是管理员
	g.a.mu.Lock()
	if g.a.isAdmin("ticket:tk-1") || g.a.isAdmin("fake:u2") {
		t.Fatal("ticket:<编号> 和派活的人都不是管理员")
	}
	sb := g.a.sessions[in.SID]
	write, mode := sb.run.write, g.a.store.Settings(in.SID).Mode
	isGroup := g.a.store.Settings(in.SID).Group
	wsIDs := g.a.store.Settings(in.SID).Workspaces
	g.a.mu.Unlock()
	if in.From != "ticket:tk-1" || in.Elevate || in.Mode != tk.Auth.Mode || mode != tk.Auth.Mode || !write || isGroup || len(wsIDs) != 1 || wsIDs[0] != "w-tk-1" {
		t.Fatalf("分支的身份、权限和工作目录：from=%s elevate=%v mode=%s/%s write=%v group=%v ws=%v", in.From, in.Elevate, in.Mode, mode, write, isGroup, wsIDs)
	}
	if len(in.Workspaces) != 1 {
		t.Fatalf("工作目录：%+v", in.Workspaces)
	}
	ws := in.Workspaces[0]
	if len(ws.Dirs) != 2 || ws.Dirs[0] != g.r.worktree("tk-1") || ws.Dirs[1] != tk.GitDir {
		t.Fatalf("可写只有 worktree 和它自己的 git 目录：%v", ws.Dirs)
	}
	for _, ro := range ws.RO {
		if ro == "/" {
			t.Fatal("不给整台机器只读")
		}
	}
	if !slices.Contains(ws.RO, g.r.gitDir()) {
		t.Fatalf("<仓库>/.git 只读：%v", ws.RO)
	}
	// 开工说明是分支的第一条消息
	g.a.mu.Lock()
	first := g.a.store.History(in.SID)
	g.a.mu.Unlock()
	var kick string
	for _, m := range first {
		kick += m.Content
	}
	for _, want := range []string{"[工单 tk-1「修登录页」]", "手机上错位", "grep -q 二 a.txt", "不要提交或推送", "Report ready"} {
		if !strings.Contains(kick, want) {
			t.Fatalf("开工说明缺 %q：%s", want, kick)
		}
	}
	// 分支改了文件、汇报 ready（只记“要验收”，这一轮结束后才跑）
	g.editWorktree(t, "a.txt", "一\n二\n")
	g.a.mu.Lock()
	g.a.ticketReported("tk-1", commit.StatusReady, "改好了", "")
	g.a.mu.Unlock()
	if g.tk("tk-1").Status != ticket.StatusWorking || !g.tk("tk-1").WantCheck {
		t.Fatal("Report ready 只记要验收，还没跑")
	}
	if got := gitRun(t, g.repo, "log", "--oneline", "nano/tk-1"); strings.Count(got, "\n") != 0 {
		t.Fatalf("还没提交：%s", got)
	}
	drainSent(g.env)
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "验收通过、工单 ready", func() bool { return g.tk("tk-1").Status == ticket.StatusReady })
	tk = g.tk("tk-1")
	if tk.Commit == "" || tk.WantCheck || !strings.Contains(tk.CheckLog, "") {
		t.Fatalf("验收通过并提交：%+v", tk)
	}
	if subj := gitRun(t, g.repo, "log", "-1", "--format=%s|%an", "nano/tk-1"); !strings.HasPrefix(subj, "修登录页 (tk-1)|") {
		t.Fatalf("宿主提交：%s", subj)
	}
	// 结果发回原消息（作为对原消息的回复）：只有这一条
	got := g.ch.wait(t, "改好了：")
	if !strings.Contains(got, "验收命令 grep -q 二 a.txt 通过") || !strings.Contains(got, "nano/tk-1") || !strings.Contains(got, tk.Commit[:8]) {
		t.Fatalf("交付的话：%q", got)
	}
	if rs := g.ch.replies(); len(rs) == 0 || rs[len(rs)-1] != "g100" {
		t.Fatalf("作为原消息的回复：%v", rs)
	}
	if sent := drainSent(g.env); len(sent) != 0 {
		t.Fatalf("群里没有别的话：%v", sent)
	}
	// 主仓库没被动过
	if st := gitRun(t, g.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("主仓库的工作区：%q", st)
	}
	// accept：名单里的人收下，工单 done，worktree 清掉，派生工作目录去掉
	mention(g.env, "@机器人 收下吧", "g101", "u2", "李四")
	g.k.expect(proto.TInput)
	out, err := callTicket(g.env, sid, `{"action":"accept","id":"tk-1"}`)
	if err != nil || !strings.Contains(out, "已收下") {
		t.Fatalf("accept：%q %v", out, err)
	}
	finishGroupRun(t, g.env, sid)
	if done := g.tk("tk-1"); done.Status != ticket.StatusDone || !strings.Contains(done.Reason, tk.Commit) {
		t.Fatalf("done 带证据：%+v", done)
	}
	eventually(t, "worktree 清理掉", func() bool { _, err := os.Stat(g.r.worktree("tk-1")); return os.IsNotExist(err) })
	g.a.mu.Lock()
	ws2 := g.a.findWorkspace("w-tk-1")
	g.a.mu.Unlock()
	if ws2 != nil {
		t.Fatal("派生工作目录要去掉")
	}
	if got := gitRun(t, g.repo, "branch", "--list", "nano/tk-1"); !strings.Contains(got, "nano/tk-1") {
		t.Fatal("git 分支留着")
	}
}

// 验收没过：返工把输出的最后 40 行放进分支的下一个安全边界；返工两次后受阻，问派活的人；名单里的人续上再给两次，记在工单上。
func TestTicketReworkThenBlockedThenRenewed(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "for i in $(seq 1 60); do echo 行$i; done; exit 1")
	in := g.startWork(t, sid)
	g.editWorktree(t, "a.txt", "改了\n")
	g.a.mu.Lock()
	g.a.ticketReported("tk-1", commit.StatusReady, "改好了", "")
	g.a.mu.Unlock()
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	// 第一次没过：返工 1，分支接着跑，带着验收输出的最后 40 行
	in2 := g.k.expect(proto.TInput)
	if in2.SID != in.SID {
		t.Fatalf("返工在分支里：%s", in2.SID)
	}
	tk := g.tk("tk-1")
	if tk.Status != ticket.StatusWorking || tk.Rework != 1 || !strings.Contains(tk.CheckLog, "行60") || strings.Contains(tk.CheckLog, "行1\n") {
		t.Fatalf("返工 1：%+v", tk)
	}
	g.a.mu.Lock()
	var seen string
	for _, m := range g.a.store.History(in.SID) {
		seen += m.Content + "\n"
	}
	g.a.mu.Unlock()
	if !strings.Contains(seen, "[验收没过]") || !strings.Contains(seen, "行60") {
		t.Fatalf("返工消息带验收输出：%s", seen)
	}
	// 第二次没过：返工用完，受阻，问派活的人
	g.a.mu.Lock()
	g.a.ticketReported("tk-1", commit.StatusReady, "再改好了", "")
	g.a.mu.Unlock()
	drainSent(g.env)
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "返工用完受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	tk = g.tk("tk-1")
	if tk.BlockedBy != "rework" || tk.Rework != 2 || !strings.Contains(tk.Reason, "验收没过") {
		t.Fatalf("受阻：%+v", tk)
	}
	q := g.ch.wait(t, "继续改、放弃，还是先收下？")
	if !strings.Contains(q, "需要你回答") || !strings.Contains(q, "回复就行") {
		t.Fatalf("问派活的人：%q", q)
	}
	if v := findCommit(g.a.Commitments("", ""), commit.KindTicket, "tk-1"); v == nil || v.Phase != commit.Blocked || !strings.Contains(v.Why, "验收没过") {
		t.Fatalf("承诺列表里是受阻：%+v", v)
	}
	// 名单里的人 note：续上，再给两次返工；记在工单上（谁、什么时候）
	mention(g.env, "@机器人 继续改", "g200", "u2", "李四")
	g.k.expect(proto.TInput)
	if out, err := callTicket(g.env, sid, `{"action":"note","id":"tk-1","text":"继续改，注意手机端"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	tk = g.tk("tk-1")
	if tk.Status != ticket.StatusWorking || tk.Renewals != 1 || len(tk.Renewed) != 1 || tk.Renewed[0].By != "fake:u2" || tk.EffectiveReworkCap() != 4 || tk.BlockedBy != "" {
		t.Fatalf("续上：%+v", tk)
	}
	finishGroupRun(t, g.env, sid)
	if in3 := g.k.expect(proto.TInput); in3.SID != in.SID {
		t.Fatalf("回答送进分支：%s", in3.SID)
	}
}

// 续上最多 3 次，之后只能叫停或放弃；重启续上和主人续上是两个计数。
func TestTicketRenewalsAreBounded(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	_ = in
	for i := 0; i < ticket.MaxRenewals; i++ {
		g.a.mu.Lock()
		g.a.ticketBlock("tk-1", "rework", "验收没过", "", false)
		cur, _ := g.a.tickets.Get("tk-1")
		err := g.a.ticketDeliver(cur, "fake:u2", "再来")
		g.a.mu.Unlock()
		if err != nil {
			t.Fatalf("第 %d 次续上：%v", i+1, err)
		}
	}
	g.a.mu.Lock()
	g.a.ticketBlock("tk-1", "rework", "验收没过", "", false)
	cur, _ := g.a.tickets.Get("tk-1")
	err := g.a.ticketDeliver(cur, "fake:u2", "再来")
	g.a.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "只能叫停或放弃") {
		t.Fatalf("第 4 次要拒绝：%v", err)
	}
	if tk := g.tk("tk-1"); tk.Renewals != ticket.MaxRenewals || tk.Resumes != 0 {
		t.Fatalf("两个计数分开：%+v", tk)
	}
}

// 预算：每次模型调用结束时检查，超了停下这一轮、工单受阻；用量记进工单；续预算再给一份。
func TestTicketBudgetStopsTheRun(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	drainSent(g.env)
	g.a.mu.Lock()
	s := g.a.sessions[in.SID]
	s.run.usage.PromptTokens, s.run.usage.CompletionTokens = 4000, 500
	g.a.ticketBudgetCheck(s, s.run)
	g.a.mu.Unlock()
	if tk := g.tk("tk-1"); tk.Status != ticket.StatusWorking {
		t.Fatalf("没超：%+v", tk.Status)
	}
	g.a.mu.Lock()
	s.run.usage.PromptTokens = 5200
	g.a.ticketBudgetCheck(s, s.run)
	r := s.run
	g.a.mu.Unlock()
	tk := g.tk("tk-1")
	if tk.Status != ticket.StatusBlocked || tk.BlockedBy != "budget" || !strings.Contains(tk.Reason, "预算用完") || tk.Spent != 5700 {
		t.Fatalf("预算用完受阻、用量记上：%+v", tk)
	}
	if r != nil && !r.ticketSettled {
		t.Fatal("这一轮的用量只记一次")
	}
	g.ch.wait(t, "预算用完")
	// 续上：再给一份预算
	g.a.mu.Lock()
	cur, _ := g.a.tickets.Get("tk-1")
	err := g.a.ticketDeliver(cur, "fake:u2", "继续")
	g.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if tk := g.tk("tk-1"); tk.EffectiveCap() != 10000 || tk.Status != ticket.StatusWorking {
		t.Fatalf("再给一份预算：%+v", tk)
	}
}

// 没有 Landlock 的机器上工单 blocked、理由写明，不降级跑。
func TestTicketNeedsLandlock(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	g.a.SetHosts(noLandlockHosts{g.hosts})
	mention(g.env, "@机器人 修登录页", "g100", "u2", "李四")
	g.k.expect(proto.TInput)
	if _, err := callTicket(g.env, sid, `{"action":"create","title":"修登录页","goal":"x"}`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	tk := g.tk("tk-1")
	if tk.BlockedBy != "sandbox" || !strings.Contains(tk.Reason, "Landlock") || tk.Worktree != "" {
		t.Fatalf("没有沙箱就不开工：%+v", tk)
	}
	if _, err := os.Stat(g.r.worktree("tk-1")); !os.IsNotExist(err) {
		t.Fatal("不该建 worktree")
	}
	finishGroupRun(t, g.env, sid)
}

// 工单分支的工具：只给 Auth.Tools；每次调用再按它查；ask 模式的审批发到 agent 主会话的管理员窗口，不发到团队群。
func TestTicketBranchToolGateAndApprovals(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.a.mu.Lock()
	s := g.a.sessions[in.SID]
	_, offered, _, _ := g.a.contextParts(in.SID)
	for _, tl := range offered {
		if !ticketToolAllowed(g.tkLocked("tk-1"), tl.Function.Name) {
			t.Errorf("给了授权之外的工具 %s", tl.Function.Name)
		}
	}
	for _, name := range []string{"Message", "SendFile", "Session", "Schedule", "Hook", "FollowUp", "Agenda", "Ticket", "Gateway", "GatewayAdmin", "Knowledge"} {
		if is, allow, _ := g.a.ticketPreCheck(s, name); !is || allow {
			t.Errorf("%s 即使被调用也要拒绝：%v %v", name, is, allow)
		}
	}
	for _, name := range []string{"Shell", "Read", "Write", "Edit", "Grep", "Report"} {
		if is, allow, _ := g.a.ticketPreCheck(s, name); !is || !allow {
			t.Errorf("%s 在授权里：%v %v", name, is, allow)
		}
	}
	// 审批：主会话的管理员窗口有人时发到那里，不是团队群
	g.a.eps["fake:admin-dm"] = &epRec{Session: "main", Who: "fake:u1"}
	outs := g.a.approvalOuts(s)
	g.a.mu.Unlock()
	if len(outs) != 1 || outs[0].ep.key() != "fake:admin-dm" {
		t.Fatalf("审批发到管理员窗口：%+v", outs)
	}
	// 提权请求被拒：身份不是管理员
	g.a.mu.Lock()
	if g.a.isAdmin(s.run.from) {
		t.Fatal("提权要拒绝")
	}
	g.a.mu.Unlock()
}

func (g *gitEnv) tkLocked(id string) ticket.Ticket {
	t, _ := g.a.tickets.Get(id)
	return t
}

// 重启后：working 的接着跑（重启续上最多 3 次，再被打断就受阻）；不重复建 worktree、不重复提交；在等人回答的不替人续。
func TestTicketResumeAfterRestart(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK}) // 这一轮没汇报就结束了 → 受阻（不是重启）
	eventually(t, "没汇报受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := g.tk("tk-1"); tk.BlockedBy != "silent" {
		t.Fatalf("没汇报：%+v", tk)
	}
	// 直接把工单改回 working，模拟“重启时这一轮在跑”
	g.a.mu.Lock()
	g.a.tickets.Update("tk-1", func(t *ticket.Ticket) { t.Status, t.BlockedBy, t.Reason = ticket.StatusWorking, "", "" })
	g.a.mu.Unlock()
	head := gitRun(t, g.repo, "rev-parse", "nano/tk-1")
	for i := 1; i <= ticket.MaxResumes; i++ {
		g.a.resumeWorking("tk-1")
		g.k.expect(proto.TInput) // 重新排了一条“接着做”
		if tk := g.tk("tk-1"); tk.Resumes != i || tk.Renewals != 0 {
			t.Fatalf("第 %d 次重启续上：%+v", i, tk)
		}
		g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
		waitIdle(t, g.env, in.SID)
		g.a.mu.Lock()
		g.a.tickets.Update("tk-1", func(t *ticket.Ticket) { t.Status, t.BlockedBy, t.Reason = ticket.StatusWorking, "", "" })
		g.a.mu.Unlock()
	}
	g.a.resumeWorking("tk-1")
	if tk := g.tk("tk-1"); tk.Status != ticket.StatusBlocked || tk.BlockedBy != "interrupted" || !strings.Contains(tk.Reason, "多次被打断") {
		t.Fatalf("第 4 次被打断就受阻：%+v", tk)
	}
	// 没有重复建 worktree，也没有多出提交
	if n := strings.Count(gitRun(t, g.repo, "worktree", "list"), "\n") + 1; n != 2 {
		t.Fatalf("worktree 个数：%d", n)
	}
	if gitRun(t, g.repo, "rev-parse", "nano/tk-1") != head {
		t.Fatal("没有多出提交")
	}
	// 重做开工：worktree 和分支已经在了，接着用
	g.a.mu.Lock()
	g.a.tickets.Update("tk-1", func(t *ticket.Ticket) { t.Status, t.Worktree, t.BlockedBy, t.Reason = ticket.StatusOpen, "", "", "" })
	g.a.mu.Unlock()
	g.a.driveTicketStart("tk-1")
	if tk := g.tk("tk-1"); tk.Worktree != g.r.worktree("tk-1") || tk.Base != g.t0.Base && tk.Base == "" {
		t.Fatalf("重做开工：%+v", tk)
	}
	if n := strings.Count(gitRun(t, g.repo, "worktree", "list"), "\n") + 1; n != 2 {
		t.Fatalf("重做之后 worktree 个数：%d", n)
	}
}

// 从另一个进程接着：验收时已经有带 Nano-Ticket 行的提交，重跑验收不会再提交。
func TestTicketCheckingRedoDoesNotCommitTwice(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	g.editWorktree(t, "a.txt", "一\n二\n")
	g.a.mu.Lock()
	g.a.ticketReported("tk-1", commit.StatusReady, "好了", "")
	g.a.mu.Unlock()
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	eventually(t, "ready", func() bool { return g.tk("tk-1").Status == ticket.StatusReady })
	first := g.tk("tk-1").Commit
	// 模拟验收到一半被重启打断：状态回到 checking，再来一遍
	g.a.mu.Lock()
	g.a.tickets.Update("tk-1", func(t *ticket.Ticket) { t.Status, t.Commit, t.WantCheck = ticket.StatusChecking, "", true })
	g.a.mu.Unlock()
	g.a.acceptTicket("tk-1")
	tk := g.tk("tk-1")
	if tk.Status != ticket.StatusReady || tk.Commit != first {
		t.Fatalf("重做收养同一个提交：%s vs %s（%s）", tk.Commit, first, tk.Status)
	}
	if n := gitRun(t, g.repo, "rev-list", "--count", tk.Base+"..nano/tk-1"); n != "1" {
		t.Fatalf("只有一个提交：%s", n)
	}
	_ = sid
}

// 叫停：停掉干活的分支、删 worktree、去掉派生工作目录；其他工单不受影响。
func TestTicketCancelWrapsUp(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	if err := g.a.CancelTicket("tk-1", "不做了"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worktree 清理", func() bool { _, err := os.Stat(g.r.worktree("tk-1")); return os.IsNotExist(err) })
	g.a.mu.Lock()
	br, _ := g.a.store.Branch(in.SID)
	ws := g.a.findWorkspace("w-tk-1")
	g.a.mu.Unlock()
	if br.Status != session.BranchArchived || ws != nil {
		t.Fatalf("分支归档、派生工作目录去掉：%s %v", br.Status, ws)
	}
	if v := findCommit(g.a.Commitments("", ""), commit.KindTicket, "tk-1"); v == nil || v.Phase != commit.Stopped {
		t.Fatalf("stopped：%+v", v)
	}
	_ = fmt.Sprint
}

// noLandlockHosts 是机器没有 Landlock 的主机表。
type noLandlockHosts struct{ inner *fakeHosts }

func (h noLandlockHosts) Host(name string) (host.Host, host.Info, bool) {
	hh, info, ok := h.inner.Host(name)
	info.Landlock = 0
	return hh, info, ok
}
func (h noLandlockHosts) Infos() []host.Info { return h.inner.Infos() }

// 前置检查（内核每次工具调用前问网关）：工单分支里不在 Auth.Tools 里的工具被拒，在里面的放行。
func TestTicketPreToolCheckGate(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	check := func(name string) proto.Msg {
		id := "c-" + name
		g.a.mu.Lock()
		s := g.a.sessions[in.SID]
		s.run.callArgs[id] = proto.Call{ID: id, Name: name, Args: []byte(`{}`)}
		g.a.preToolCheck(s, &proto.Msg{ID: id, RequestID: id, Name: name, RunID: s.run.runID, TurnID: s.run.turnID})
		g.a.mu.Unlock()
		return g.k.expect(proto.THookResult)
	}
	if res := check("Message"); res.OK == nil || *res.OK || !strings.Contains(res.Detail, "没有工具 Message") {
		t.Fatalf("Message 要拒绝：%+v", res)
	}
	if res := check("Shell"); res.OK == nil || !*res.OK {
		t.Fatalf("Shell 在授权里：%+v", res)
	}
}

// 问题怎么回答：名单里的人 @agent 回复，agent 用 Ticket note 转进工单，同时记作回答；主人也能在等我处理里回答，送进分支。
func TestTicketQuestionAnsweredByNoteOrByThePendingList(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	ask := func(q string) commit.CommitRec {
		g.a.mu.Lock()
		defer g.a.mu.Unlock()
		r, err := g.a.commits.Append(commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindTicket, ID: "tk-1", Agent: "default", Status: commit.StatusBlocked, Text: "要选方案", Question: q})
		if err != nil {
			t.Fatal(err)
		}
		g.a.ticketPush("tk-1", "blocked", "要选方案", q)
		return r
	}
	r := ask("用方案 A 还是 B？")
	g.ch.wait(t, "问：用方案 A 还是 B？")
	if p := pendingOf(g.env, commit.KindTicket, "tk-1"); p == nil || p.Question != "用方案 A 还是 B？" {
		t.Fatalf("问题在等我处理里：%+v", p)
	}
	// 名单里的人回复：note 同时记作回答，问题不再算
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK}) // 分支这一轮结束（汇报过了，所以不算没汇报）
	waitIdle(t, g.env, in.SID)
	mention(g.env, "@机器人 用 A", "g300", "u2", "李四")
	g.k.expect(proto.TInput)
	if _, err := callTicket(g.env, sid, `{"action":"note","id":"tk-1","text":"用 A"}`); err != nil {
		t.Fatal(err)
	}
	g.a.mu.Lock()
	ans, ok := g.a.commits.Answer(commit.KindTicket, "tk-1", fmt.Sprintf("r:%d", r.Seq))
	g.a.mu.Unlock()
	if !ok || ans.Answer != "用 A" || ans.By != "fake:u2" {
		t.Fatalf("note 同时记作回答：%+v %v", ans, ok)
	}
	if p := pendingOf(g.env, commit.KindTicket, "tk-1"); p != nil && p.Question != "" {
		t.Fatalf("回答过的问题不再列：%+v", p)
	}
	finishGroupRun(t, g.env, sid)
	if in2 := g.k.expect(proto.TInput); in2.SID != in.SID {
		t.Fatalf("补充送进分支：%s", in2.SID)
	}
	// 主人在等我处理里回答另一个问题：送进分支
	g.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, g.env, in.SID)
	r2 := ask("要不要加测试？")
	if err := g.a.PendingAnswer(commit.KindTicket, "tk-1", fmt.Sprintf("r:%d", r2.Seq), "要", "web:admin"); err != nil {
		t.Fatal(err)
	}
	if in3 := g.k.expect(proto.TInput); in3.SID != in.SID {
		t.Fatalf("回答送进分支：%s", in3.SID)
	}
}
