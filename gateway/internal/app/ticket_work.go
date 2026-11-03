package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/ticket"
)

// 工单的执行（设计 §16.2）：建 worktree → 从派活的会话分出干活的分支 → 分支 Report ready 并结束这一轮 →
// 宿主验收、提交 → 交付到本地分支。分支以 ticket:<编号> 的身份跑：权限模式是 Auth.Mode，工具是 Auth.Tools，
// 不能提权；派活的人不一定是管理员，按他的身份分支什么工具都用不了。

const ticketIdentityPrefix = "ticket:"

// ticketIDOfIdentity 是 ticket:<编号> 里的编号；不是这种身份返回空。
func ticketIDOfIdentity(from string) string {
	id, _ := strings.CutPrefix(from, ticketIdentityPrefix)
	if id == from {
		return ""
	}
	return id
}

// ticketIdentity 认 ticket:<编号> 这种发起人：工单还在、没结束才算。调用方持有锁。
func (a *App) ticketIdentity(from string) (ticket.Ticket, bool) {
	id := ticketIDOfIdentity(from)
	if id == "" || a.tickets == nil {
		return ticket.Ticket{}, false
	}
	t, ok := a.tickets.Get(id)
	if !ok || ticket.IsEnded(t.Status) {
		return ticket.Ticket{}, false
	}
	return t, true
}

// ticketOfSession 是这个会话作为工单的分支时对应的工单（不管结没结束）。调用方持有锁。
func (a *App) ticketOfSession(sid string) (ticket.Ticket, bool) {
	if a.tickets == nil {
		return ticket.Ticket{}, false
	}
	m, ok := a.store.Branch(sid)
	if !ok {
		return ticket.Ticket{}, false
	}
	id := ticketIDOfIdentity(m.CreatedBy)
	if id == "" {
		return ticket.Ticket{}, false
	}
	return a.tickets.Get(id)
}

// ticketToolAllowed 是工单分支这次调用的这个工具在不在 Auth.Tools 里。
func ticketToolAllowed(t ticket.Ticket, name string) bool {
	for _, n := range t.Auth.Tools {
		if n == name {
			return true
		}
	}
	return false
}

// syncTicketWorkspaces 让派生工作目录和工单对上：还没结束、有 worktree 的工单各有一个，结束了的去掉。调用方持有锁。
func (a *App) syncTicketWorkspaces() {
	if a.tickets == nil {
		return
	}
	want := map[string]Workspace{}
	for _, t := range a.tickets.List() {
		if ticket.IsEnded(t.Status) || t.Worktree == "" {
			continue
		}
		repo := a.findWorkspace(t.Auth.Workspace)
		if repo == nil {
			continue
		}
		root := ticketRepo{machine: repo.Host, root: repo.Path}
		w := Workspace{ID: "w-" + t.ID, Host: repo.Host, Path: t.Worktree, OS: repo.OS, SandboxMode: repo.SandboxMode, Ticket: t.ID, ExtraRO: []string{root.gitDir()}}
		if t.GitDir != "" {
			w.ExtraRW = []string{t.GitDir}
		}
		want[w.ID] = w
	}
	kept := a.wss[:0]
	for _, w := range a.wss {
		if w.Ticket == "" {
			kept = append(kept, w)
		}
	}
	a.wss = kept
	for _, t := range a.tickets.List() {
		if w, ok := want["w-"+t.ID]; ok {
			a.wss = append(a.wss, w)
		}
	}
}

// ticketKickoff 是干活的分支的第一条消息：目标、验收命令、规矩。
func ticketKickoff(t ticket.Ticket) string {
	check := "没有验收命令，只看有没有改动"
	if strings.TrimSpace(t.Check) != "" {
		check = "验收命令：" + t.Check
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[工单 %s「%s」] 你在一个独立的 git worktree 里工作（已经挂成工作目录），不要动别的地方。\n目标：%s\n%s。\n", t.ID, t.Title, t.Goal, check)
	if s := strings.TrimSpace(t.Auth.Source.Text); s != "" {
		fmt.Fprintf(&b, "派活的原话（开头）：%s\n", s)
	}
	for _, n := range t.Notes {
		fmt.Fprintf(&b, "补充（%s）：%s\n", n.By, n.Text)
	}
	b.WriteString("规矩：不要提交或推送——提交由系统在验收通过后做；改完用 Report ready 汇报，附上你跑过的测试；卡住了用 Report blocked 问一个具体问题；这件事不用做了用 Report not_needed 写理由。\n")
	b.WriteString("沙箱读不了 ~/.gitconfig，用 git 前先 export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null XDG_CONFIG_HOME=/nonexistent。")
	return b.String()
}

// ticketBlock 把工单记成受阻（宿主记下的），理由写明。调用方持有锁；push 为真时发回原消息。
func (a *App) ticketBlock(id, by, reason, question string, push bool) {
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) {
		t.Status, t.BlockedBy, t.Reason, t.WantCheck = ticket.StatusBlocked, by, reason, false
	}); err != nil {
		a.ticketHealth(err)
		return
	}
	a.ticketHealth(nil)
	a.publishCommitments()
	if push {
		a.ticketPush(id, "blocked", reason, question)
	}
}

// ---- 开工 ----

// startTicket 在后台让一张刚建好的工单开工（建 worktree、分出分支）。重启后也由 resumeTickets 调用，每一步都可以重做。
// 调用方持有锁。
func (a *App) startTicket(id string) {
	if a.ticketBusy == nil {
		a.ticketBusy = map[string]bool{}
	}
	if a.ticketBusy[id] || a.ctx.Err() != nil {
		return
	}
	a.ticketBusy[id] = true
	go func() {
		defer func() { a.mu.Lock(); delete(a.ticketBusy, id); a.mu.Unlock() }()
		a.driveTicketStart(id)
	}()
}

func (a *App) driveTicketStart(id string) {
	a.mu.Lock()
	t, ok := a.tickets.Get(id)
	if !ok || t.Status != ticket.StatusOpen {
		a.mu.Unlock()
		return
	}
	repo, err := a.ticketRepoOf(t)
	var landlock int
	var why string
	if err == nil {
		_, landlock, why = a.hostFor(repo.machine)
		if why != "" {
			err = errors.New("仓库所在的机器现在用不了：" + why)
		} else if landlock == 0 {
			err = errors.New("仓库所在的机器没有 Landlock 沙箱：工单的分支必须在沙箱里跑，不降级")
		}
	}
	if err != nil {
		by := "sandbox"
		if why != "" {
			by = "error"
		}
		a.ticketBlock(id, by, err.Error(), "", true)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	base, gitDir, err := a.createWorktree(a.ctx, t, repo)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.ticketBlock(id, "error", err.Error(), "", true)
		return
	}
	t, ok = a.tickets.Get(id)
	if !ok || ticket.IsEnded(t.Status) { // 建的过程中被叫停了：清掉
		go a.cleanupTicket(t)
		return
	}
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) {
		t.Worktree, t.Branch, t.Base, t.GitDir = repo.worktree(id), ticketBranch(id), base, gitDir
	}); err != nil {
		a.ticketHealth(err)
		return
	}
	a.syncTicketWorkspaces()
	t, _ = a.tickets.Get(id)
	res, err := a.sessionForkLocked(SessionForkRequest{SourceSID: t.Auth.Scene, Title: "工单 " + t.ID + "「" + t.Title + "」", Prompt: ticketKickoff(t),
		Start: true, Delivery: session.BranchDeliverySilent, RequestID: "ticket-" + t.ID, By: ticketIdentityPrefix + t.ID, host: true})
	if err != nil {
		a.ticketBlock(id, "error", "分不出干活的分支："+err.Error(), "", true)
		return
	}
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) { t.Status, t.RunSID = ticket.StatusWorking, res.BranchSID }); err != nil {
		a.ticketHealth(err)
		return
	}
	a.publishCommitments()
}

// prepareTicketBranch 在工单的分支第一次开跑之前，把它的会话设置成工单要的样子：不是群聊，
// 工作目录只有工单的 worktree，权限模式是 Auth.Mode。调用方持有锁。
func (a *App) prepareTicketBranch(sid string) {
	t, ok := a.ticketOfSession(sid)
	if !ok {
		return
	}
	st := a.store.Settings(sid)
	st.Group, st.Scene = false, nil
	st.Workspaces, st.WorkspacesSet = []string{"w-" + t.ID}, true
	st.Mode = t.Auth.Mode
	if err := a.store.SaveSettings(sid, st); err != nil {
		a.log.Warn("保存工单分支的设置失败", "sid", sid, "err", err)
	}
}

// ---- 分支的这一轮结束了 / 预算 ----

// ticketBudgetCheck 在每次模型调用结束时检查预算：已用加上这一轮用的超过现在的上限，就停下这一轮、工单受阻。调用方持有锁。
func (a *App) ticketBudgetCheck(s *sess, r *run) {
	t, ok := a.ticketIdentity(r.from)
	if !ok || t.Status == ticket.StatusBlocked {
		return
	}
	used := t.Spent + r.usage.PromptTokens + r.usage.CompletionTokens
	if used <= t.EffectiveCap() {
		return
	}
	a.settleTicketSpend(t.ID, r)
	a.ticketBlock(t.ID, "budget", fmt.Sprintf("预算用完了（已用 %d token，上限 %d）", used, t.EffectiveCap()), "继续给一份预算，还是放弃？", true)
	if s.run == r {
		r.dropInbox = true
		a.cancelRun(s)
	}
}

// settleTicketSpend 把这一轮用的 token 记进工单（每轮只记一次）。调用方持有锁。
func (a *App) settleTicketSpend(id string, r *run) {
	if r.ticketSettled {
		return
	}
	r.ticketSettled = true
	n := r.usage.PromptTokens + r.usage.CompletionTokens
	if n <= 0 {
		return
	}
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) { t.Spent += n }); err != nil {
		a.ticketHealth(err)
	}
}

// ticketRunEnded 是工单分支的一轮结束了（branchDone 调用）：记用量，看有没有要验收的、有没有汇报。调用方持有锁。
func (a *App) ticketRunEnded(s *sess, r *run) {
	t, ok := a.ticketOfSession(s.sid)
	if !ok || ticket.IsEnded(t.Status) {
		return
	}
	a.settleTicketSpend(t.ID, r)
	t, _ = a.tickets.Get(t.ID)
	if t.Status == ticket.StatusBlocked || t.Status == ticket.StatusPaused {
		return
	}
	switch {
	case r.outcome != "" && r.outcome != proto.StatusOK && r.outcome != proto.StatusCancelled:
		a.ticketBlock(t.ID, "error", "这一轮出错了（"+r.outcome+"）："+clip(strings.TrimSpace(r.final), 200), "", true)
	case t.WantCheck:
		go a.acceptTicket(t.ID)
	case r.reports == 0:
		a.ticketBlock(t.ID, "silent", "干活的分支这一轮结束了，但没有用 Report 汇报结果或问题", "", true)
	}
}

// ---- 验收、提交、返工 ----

// acceptTicket 是验收：分支 Report ready 并结束这一轮后，宿主在 worktree 里跑验收命令；通过且有改动就提交，
// 否则返工，返工用完受阻。验收结果是证据，不采信分支的自述。
func (a *App) acceptTicket(id string) {
	a.mu.Lock()
	if a.ticketBusy == nil {
		a.ticketBusy = map[string]bool{}
	}
	if a.ticketBusy[id] {
		a.mu.Unlock()
		return
	}
	a.ticketBusy[id] = true
	t, ok := a.tickets.Get(id)
	if !ok || ticket.IsEnded(t.Status) || t.Status == ticket.StatusBlocked {
		delete(a.ticketBusy, id)
		a.mu.Unlock()
		return
	}
	repo, err := a.ticketRepoOf(t)
	if err == nil {
		_, err = a.tickets.Update(id, func(t *ticket.Ticket) { t.Status, t.WantCheck = ticket.StatusChecking, true })
	}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.ticketBusy, id); a.mu.Unlock() }()
	if err != nil {
		a.mu.Lock()
		a.ticketBlock(id, "error", err.Error(), "", true)
		a.mu.Unlock()
		return
	}
	check, err := a.runCheck(a.ctx, t, repo)
	var commitRes commitResult
	if err == nil && check.Passed {
		commitRes, err = a.commitWork(a.ctx, t, repo, a.agentDisplayName(t.Agent))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.tickets.Get(id); !ok || ticket.IsEnded(cur.Status) || cur.Status == ticket.StatusBlocked {
		return // 验收的过程中被叫停或受阻了
	}
	switch {
	case err != nil:
		a.ticketBlock(id, "error", "验收时出错："+err.Error(), "", true)
	case !check.Passed || !commitRes.Changed:
		log := check.Log
		if check.Passed {
			log = "验收命令通过了，但是没有任何改动"
		}
		a.reworkTicket(t, log)
	default:
		if _, err := a.tickets.Update(id, func(t *ticket.Ticket) {
			t.Status, t.WantCheck, t.Commit, t.CheckLog = ticket.StatusReady, false, commitRes.Commit, check.Log
		}); err != nil {
			a.ticketHealth(err)
			return
		}
		a.publishCommitments()
		a.ticketPush(id, "ready", deliveryText(t, commitRes), "")
	}
}

func (a *App) agentDisplayName(id string) string {
	if ag, ok := a.agentByID(id); ok && ag.Name != "" {
		return ag.Name
	}
	return id
}

// deliveryText 是交付发回原消息的话。
func deliveryText(t ticket.Ticket, c commitResult) string {
	short := c.Commit
	if len(short) > 8 {
		short = short[:8]
	}
	check := "验收命令 " + t.Check + " 通过"
	if strings.TrimSpace(t.Check) == "" {
		check = "没有验收命令，只看了改动"
	}
	summary := c.Summary
	if summary == "" {
		summary = "有改动"
	}
	return fmt.Sprintf("改好了：%s。%s，提交 %s 在本地分支 %s。", summary, check, short, ticketBranch(t.ID))
}

// reworkTicket 是验收没过：返工加一，把验收输出的最后 40 行放进分支的下一个安全边界接着跑；
// 返工用完（到上限还没过）受阻，问派活的人。调用方持有锁。
func (a *App) reworkTicket(t ticket.Ticket, log string) {
	cap := t.EffectiveReworkCap()
	n := t.Rework + 1
	if n >= cap {
		if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
			t.Rework, t.CheckLog = n, log
		}); err != nil {
			a.ticketHealth(err)
		}
		a.ticketBlock(t.ID, "rework", fmt.Sprintf("验收没过（返工 %d 次了）：%s", n, clip(tailLines(log, 8), 600)), "继续改、放弃，还是先收下？", true)
		return
	}
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
		t.Status, t.WantCheck, t.Rework, t.CheckLog = ticket.StatusWorking, false, n, log
	}); err != nil {
		a.ticketHealth(err)
		return
	}
	msg := fmt.Sprintf("[验收没过] 验收命令 %s 的输出（最后 40 行）：\n%s\n请改好后再用 Report ready 汇报。这是第 %d 次返工（最多 %d 次）。", orNone(t.Check), log, n, cap)
	if err := a.startBranchRunLocked(t.RunSID, msg, ticketIdentityPrefix+t.ID); err != nil {
		a.ticketBlock(t.ID, "error", "没法让分支接着改："+err.Error(), "", true)
	}
	a.publishCommitments()
}

// ---- 收下、叫停、清理 ----

// acceptTicketAction 是名单里的人收下一个就绪的工单：工单 done，删掉 worktree。
// 因为返工用完而受阻的，“先收下”：在后台把现在的改动提交下来再收下。调用方持有锁（这里不放锁）。
func (a *App) acceptTicketAction(t ticket.Ticket, by string) (string, error) {
	switch {
	case t.Status == ticket.StatusReady:
		return a.finishAccept(t)
	case t.Status == ticket.StatusBlocked && t.BlockedBy == "rework":
		if a.ticketBusy == nil {
			a.ticketBusy = map[string]bool{}
		}
		if a.ticketBusy[t.ID] {
			return "", errors.New("这张工单正在处理，稍后再试")
		}
		repo, err := a.ticketRepoOf(t)
		if err != nil {
			return "", err
		}
		a.ticketBusy[t.ID] = true
		go func() {
			defer func() { a.mu.Lock(); delete(a.ticketBusy, t.ID); a.mu.Unlock() }()
			c, err := a.commitWork(a.ctx, t, repo, a.agentDisplayName(t.Agent))
			a.mu.Lock()
			defer a.mu.Unlock()
			cur, ok := a.tickets.Get(t.ID)
			if !ok || ticket.IsEnded(cur.Status) {
				return
			}
			switch {
			case err != nil:
				a.ticketPush(t.ID, "blocked", "先收下没成功："+err.Error(), "")
			case !c.Changed:
				a.ticketPush(t.ID, "blocked", "没有任何改动，没什么可收下的", "")
			default:
				if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) { t.Commit = c.Commit }); err != nil {
					a.ticketHealth(err)
					return
				}
				cur, _ = a.tickets.Get(t.ID)
				if msg, err := a.finishAccept(cur); err == nil {
					a.ticketPush(t.ID, "ready", msg, "")
				}
			}
		}()
		return "好，先把现在的改动提交下来再收下，稍后发回结果。", nil
	}
	return "", fmt.Errorf("工单还没有就绪（现在是 %s）", t.Status)
}

// finishAccept 收下：done、收尾。调用方持有锁。
func (a *App) finishAccept(t ticket.Ticket) (string, error) {
	reason := fmt.Sprintf("收下了：提交 %s，验收结果 %s", t.Commit, clip(strings.TrimSpace(t.CheckLog), 100))
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
		t.Status, t.Reason, t.WantCheck, t.BlockedBy = ticket.StatusDone, reason, false, ""
	}); err != nil {
		a.ticketHealth(err)
		return "", fmt.Errorf("没有记下来（%v）", err)
	}
	a.ticketHealth(nil)
	a.publishCommitments()
	done, _ := a.tickets.Get(t.ID)
	a.wrapUpTicket(done)
	return fmt.Sprintf("已收下 %s：提交 %s 在本地分支 %s。", t.ID, t.Commit, ticketBranch(t.ID)), nil
}

// wrapUpTicket 在工单结束（收下、叫停、失败）后收尾：停掉干活的分支、去掉派生工作目录、删 worktree。调用方持有锁。
func (a *App) wrapUpTicket(t ticket.Ticket) {
	if t.RunSID != "" {
		if s := a.sessions[t.RunSID]; s != nil {
			if s.run != nil {
				s.run.dropInbox = true
				a.cancelRun(s)
			}
			s.queue = nil
		}
		_ = a.store.SetBranch(t.RunSID, func(m *session.BranchMeta) {
			if m.Status != session.BranchArchived {
				m.Status = session.BranchArchived
				m.Progress = clip("工单结束了："+t.Status, 200)
				m.ProgressAt = a.now().Unix()
			}
		})
	}
	a.syncTicketWorkspaces()
	go a.cleanupTicket(t)
}

// cleanupTicket 删 worktree（幂等）。不持有锁。
func (a *App) cleanupTicket(t ticket.Ticket) {
	if t.Worktree == "" {
		return
	}
	a.mu.Lock()
	repo, err := a.ticketRepoOf(t)
	a.mu.Unlock()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, ticketGitTimeout)
	defer cancel()
	if err := a.removeWorktree(ctx, t, repo); err != nil {
		a.log.Warn("清理工单的 worktree 失败", "ticket", t.ID, "err", err)
	}
}

// ---- 从受阻里出来、补充、回答 ----

// ticketDeliver 把一句话（回答或补充）送进工单的分支：工单受阻时先续上（最多 MaxRenewals 次，记下谁、什么时候、续的什么），
// 回到 working；工单在跑就排到分支的下一个安全边界。调用方持有锁。
func (a *App) ticketDeliver(t ticket.Ticket, by, body string) error {
	if t.RunSID == "" {
		return errors.New("工单还没有干活的分支")
	}
	if t.Status == ticket.StatusBlocked {
		if t.Renewals >= ticket.MaxRenewals {
			return fmt.Errorf("这张工单已经续了 %d 次，不能再续了；只能叫停或放弃", ticket.MaxRenewals)
		}
		what := map[string]string{"rework": "再给 2 次返工", "budget": "再给一份预算", "interrupted": "重启中断后续上", "sandbox": "沙箱问题后续上", "error": "出错后续上", "silent": "没汇报后续上"}[t.BlockedBy]
		if what == "" {
			what = "续上"
		}
		if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
			t.Renewals++
			t.Renewed = append(t.Renewed, ticket.Renewal{By: by, At: a.now().Unix(), What: what})
			t.Status, t.Reason, t.Resumes = ticket.StatusWorking, "", 0
			switch t.BlockedBy {
			case "rework":
				t.ReworkCap = t.Rework + ticket.MaxRework
			case "budget":
				t.Cap = t.EffectiveCap() + t.Budget
			}
			t.BlockedBy = ""
		}); err != nil {
			a.ticketHealth(err)
			return fmt.Errorf("没有记下来（%v）", err)
		}
		a.publishCommitments()
	}
	if err := a.startBranchRunLocked(t.RunSID, body, ticketIdentityPrefix+t.ID); err != nil {
		return err
	}
	a.publish(events.Event{T: "branch", SID: t.RunSID, Kind: "answer"})
	return nil
}

// answerPendingQuestion 工单有还没回答的问题时，名单里的人的补充同时记作回答（先记再生效）。调用方持有锁。
func (a *App) answerPendingQuestion(t ticket.Ticket, by, text string) {
	if a.commits == nil {
		return
	}
	n, ok := a.commits.Notice(commit.KindTicket, t.ID)
	if !ok || n.Question == "" {
		return
	}
	mark := fmt.Sprintf("r:%d", n.Seq)
	if a.commits.Seen(commit.KindTicket, t.ID, mark) {
		return
	}
	if _, err := a.commits.Append(commit.CommitRec{Type: commit.TypeAnswer, Kind: commit.KindTicket, ID: t.ID, Mark: mark, By: by, Answer: text}); err != nil {
		a.commitHealth(err)
		return
	}
	a.publishCommitments()
}

// ---- 重启后接着跑 ----

// resumeTickets 重启后让没结束的工单接着跑，每一步都可以重做：open 的重新开工（worktree、分支已经在了就接着用）；
// working 的重新排一条“接着做”（被重启打断而续上的次数单独记，最多 MaxResumes 次，再被打断就受阻）；
// checking 的重跑验收。调用方不持有锁。
func (a *App) resumeTickets() {
	a.mu.Lock()
	if a.tickets == nil {
		a.mu.Unlock()
		return
	}
	var open, working, checking []string
	for _, t := range a.tickets.List() {
		switch t.Status {
		case ticket.StatusOpen:
			open = append(open, t.ID)
		case ticket.StatusWorking:
			working = append(working, t.ID)
		case ticket.StatusChecking:
			checking = append(checking, t.ID)
		}
	}
	a.mu.Unlock()
	a.mu.Lock()
	for _, id := range open {
		a.startTicket(id)
	}
	a.mu.Unlock()
	for _, id := range checking {
		go a.acceptTicket(id)
	}
	for _, id := range working {
		a.resumeWorking(id)
	}
}

func (a *App) resumeWorking(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tickets.Get(id)
	if !ok || t.Status != ticket.StatusWorking || t.RunSID == "" {
		return
	}
	if s := a.sessions[t.RunSID]; s != nil && (s.run != nil || len(s.queue) > 0) {
		return // 已经在跑
	}
	if a.commits != nil {
		if n, ok := a.commits.Notice(commit.KindTicket, id); ok && (n.Status == commit.StatusBlocked || n.Status == commit.StatusFailed) &&
			!a.commits.Seen(commit.KindTicket, id, fmt.Sprintf("r:%d", n.Seq)) {
			return // 在等人回答：不替人续
		}
	}
	if t.Resumes >= ticket.MaxResumes {
		a.ticketBlock(id, "interrupted", "多次被打断（网关重启了 "+fmt.Sprint(t.Resumes)+" 次，每次都没做完）", "", true)
		return
	}
	if _, err := a.tickets.Update(id, func(t *ticket.Ticket) { t.Resumes++ }); err != nil { // 先记再续
		a.ticketHealth(err)
		return
	}
	msg := "[接着做] 网关重启过，上一轮被打断了。先看一下 worktree 现在的状态（git status、git diff），接着把工单做完，改完用 Report ready 汇报。"
	if err := a.startBranchRunLocked(t.RunSID, msg, ticketIdentityPrefix+id); err != nil {
		a.log.Warn("工单接着跑失败", "ticket", id, "err", err)
	}
}

var _ = time.Second

// isTicketRun 是这一轮的发起人是工单的分支（ticket:<编号>，工单还没结束）。调用方持有锁。
func (a *App) isTicketRun(from string) bool {
	_, ok := a.ticketIdentity(from)
	return ok
}

// ticketLoop 等有内核在线，把没结束的工单接着跑（一次）。
func (a *App) ticketLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			online := a.preferred() != ""
			a.mu.Unlock()
			if online {
				a.resumeTickets()
				return
			}
		}
	}
}

// ticketPreCheck 是工单分支每次工具调用前的检查：不在 Auth.Tools 里的一律拒绝，不管模型有没有看到它。
// 返回是不是工单的分支，以及允许与否。调用方持有锁。
func (a *App) ticketPreCheck(s *sess, name string) (isTicket, allow bool, detail string) {
	t, ok := a.ticketIdentity(s.run.from)
	if !ok {
		return false, false, ""
	}
	if !ticketToolAllowed(t, name) {
		return true, false, "工单 " + t.ID + " 的授权里没有工具 " + name
	}
	return true, true, ""
}

func (a *App) isTicketBranch(sid string) bool {
	_, ok := a.ticketOfSession(sid)
	return ok
}

// ticketReported 是工单的分支调了 Report：ready 记下“要验收”（等这一轮结束再跑，这一轮可能还在往 worktree 里写）；
// blocked 带着问题发回原消息；failed、not_needed 结束工单并发回原消息。调用方持有锁。
func (a *App) ticketReported(id, status, text, question string) {
	switch status {
	case commit.StatusReady:
		if _, err := a.tickets.Update(id, func(t *ticket.Ticket) {
			if t.Status == ticket.StatusWorking {
				t.WantCheck = true
			}
		}); err != nil {
			a.ticketHealth(err)
		}
	case commit.StatusBlocked:
		a.ticketPush(id, "blocked", text, question)
	case commit.StatusFailed, commit.StatusNotNeeded:
		st, push := ticket.StatusFailed, "failed"
		if status == commit.StatusNotNeeded {
			st, push = ticket.StatusDropped, "dropped"
		}
		if _, err := a.tickets.Update(id, func(t *ticket.Ticket) { t.Status, t.Reason, t.WantCheck = st, text, false }); err != nil {
			a.ticketHealth(err)
			return
		}
		a.ticketPush(id, push, text, "")
		if t, ok := a.tickets.Get(id); ok {
			a.wrapUpTicket(t)
		}
		a.publishCommitments()
	}
}
