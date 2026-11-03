package app

import (
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/session"
)

// 承诺的适配函数（设计 §15.2）：每一种承诺从自己的记录推出契约和事实，阶段交给 commit.Phase。
// 这里只推“宿主自己记下的”部分；汇报（Report）和主人点过的“知道了”由 commitReports 统一叠上去。
// 适配函数是纯函数：给出各自的记录就得到结果，不碰 App。

// commitItem 是一件承诺：契约、事实和用来列表的几个字段。
type commitItem struct {
	Kind     string
	ID       string
	Agent    string
	Title    string
	Contract commit.Contract
	Facts    commit.Facts
	EndedAt  int64  // 结束的时间（秒）；没结束为 0。结束超过 30 天的不列
	At       int64  // 决定阶段的那条事实发生的时间（秒）：等我处理按它排，最新的在前
	Covered  string // 被汇报的就绪盖住的宿主就绪的 Mark：点“知道了”时一起记
	Link     string // 网页上打开它的链接
}

// 已经结束超过这么久的不列。
const commitKeep = 30 * 24 * time.Hour

func scopeText(mode string, workspaces, tools []string) string {
	var parts []string
	if mode != "" {
		parts = append(parts, "权限 "+mode)
	}
	if len(workspaces) > 0 {
		parts = append(parts, "目录 "+strings.Join(workspaces, "、"))
	}
	if len(tools) > 0 {
		parts = append(parts, "工具 "+strings.Join(tools, "、"))
	}
	if len(parts) == 0 {
		return "沿用默认范围"
	}
	return strings.Join(parts, "；")
}

func stopText(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "；")
}

// agendaCommit 是一个议程项。in 是它对应的意图（没有就是 nil）；paused 是这个 agent 的自主活动暂停了。
func agendaCommit(it autonomy.AgendaItem, in *autonomy.Intention, paused bool, agent string, now int64) commitItem {
	goal := it.Title
	if in != nil && in.Why != "" {
		goal += "：" + in.Why
	}
	c := commitItem{Kind: commit.KindAgenda, ID: it.ID, Agent: agent, Title: it.Title, Link: "#/agent/" + agent,
		Contract: commit.Contract{
			Goal: goal, By: agent, Scene: it.Source, Scope: scopeText(it.Mode, it.Workspaces, nil), Done: it.Criteria,
			ReportTo: []string{lifeSID(agent)},
			Stop:     stopText("每日自主预算和作息", minutesText(it.Minutes)),
		}}
	f := commit.Facts{Now: now}
	switch it.Status {
	case autonomy.StatusDone:
		f.Ended, f.Evidence = commit.Done, it.Result
	case autonomy.StatusCancelled:
		f.Ended, f.Reason = commit.Dropped, firstNonEmpty(it.Result, "取消了") // 升级前的记录没有 EndedBy：算无需处理
		if it.EndedBy == autonomy.EndedByOwner {
			f.Ended, f.Reason = commit.Stopped, firstNonEmpty(it.Result, "主人取消了")
		}
	case autonomy.StatusSkipped:
		f.Ended, f.Reason = commit.Dropped, it.Result
	case autonomy.StatusInProgress:
		f.Running = true
	case autonomy.StatusPlanned:
		f.Due = it.At
		if in != nil {
			f.Wait = in.Blocked
		}
	}
	f.Paused = paused && f.Ended == ""
	c.Facts = f
	if f.Ended != "" {
		c.EndedAt = it.Updated
	}
	return c
}

func minutesText(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("预计 %d 分钟", n)
}

// intentionCommit 是一个意图；它还有没完成的议程项时返回 false（那时只列议程项）。
// ags 是这个 agent 的全部议程项。
func intentionCommit(in autonomy.Intention, ags []autonomy.AgendaItem, paused bool, agent string, now int64) (commitItem, bool) {
	var last *autonomy.AgendaItem
	for i := range ags {
		a := &ags[i]
		if a.IntentionID != in.ID {
			continue
		}
		if a.Status == autonomy.StatusPlanned || a.Status == autonomy.StatusInProgress {
			return commitItem{}, false
		}
		if last == nil || a.Updated >= last.Updated {
			last = a
		}
	}
	goal := in.Title
	if in.Why != "" {
		goal += "：" + in.Why
	}
	budget := ""
	if in.BudgetTokens > 0 {
		budget = fmt.Sprintf("这个目标最多 %d token", in.BudgetTokens)
	}
	c := commitItem{Kind: commit.KindIntention, ID: in.ID, Agent: agent, Title: in.Title, Link: "#/agent/" + agent,
		Contract: commit.Contract{
			Goal: goal, By: agent, Scene: in.SourceEvent, Scope: "自主活动的默认范围", Done: "有证据的执行结果",
			ReportTo: []string{lifeSID(agent)}, Stop: stopText(budget, "每日自主预算"),
		}}
	f := commit.Facts{Now: now}
	switch in.State {
	case autonomy.IntentionDone:
		f.Ended, f.Evidence = commit.Done, in.Evidence
	case autonomy.IntentionFailed:
		f.Ended, f.Reason = commit.Failed, firstNonEmpty(in.Blocked, in.Evidence, "多次尝试没做成")
	case autonomy.IntentionCancelled:
		// 同议程项：主人取消的是叫停，自己取消的是无需处理；取消的理由和是谁取消的，看它最后一个议程项
		f.Ended, f.Reason = commit.Dropped, "取消了"
		if last != nil {
			f.Reason = firstNonEmpty(last.Result, "取消了")
			if last.EndedBy == autonomy.EndedByOwner {
				f.Ended, f.Reason = commit.Stopped, firstNonEmpty(last.Result, "主人取消了")
			}
		}
	case autonomy.IntentionRunning:
		f.Running = true
	case autonomy.IntentionPaused:
		f.Paused = true
	case autonomy.IntentionAccepted, autonomy.IntentionWaiting:
		f.Due, f.Wait = in.NextAt, in.Blocked
	}
	if paused && f.Ended == "" {
		f.Paused = true
	}
	c.Facts = f
	if f.Ended != "" {
		c.EndedAt = in.Updated
	}
	return c, true
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// followUpCommit 是一个约好过会儿再看的回访：sid 是那个聊天。回访不随自主活动暂停（autonomyAgent 在判断暂停之前就发了）。
func followUpCommit(sid string, f autonomy.FollowUp, agent string, now int64) commitItem {
	return commitItem{Kind: commit.KindFollowUp, ID: sid, Agent: agent, Title: f.Note, Link: "#/sessions/" + sid,
		Contract: commit.Contract{
			Goal: f.Note, By: agent, Scene: sid, Scope: "在那个聊天里说一句或不说", Done: "到点说一句或不说",
			ReportTo: []string{sid}, Stop: "一次",
		},
		Facts: commit.Facts{Now: now, Due: f.At}} // 暂停自主活动时回访照常发，所以不设 Paused
}

// oneShot 是只跑一次的定时任务（at 开头）。
func oneShot(t schedule.Task) bool { return strings.HasPrefix(strings.TrimSpace(t.When), "at ") }

// scheduleCommit 是一个定时任务。
func scheduleCommit(t schedule.Task, now int64) commitItem {
	timeout := t.Timeout
	if timeout == "" {
		timeout = "30m"
	}
	var report []string
	if !t.Silent {
		report = append(report, t.Deliver...)
	}
	once := ""
	if oneShot(t) {
		once = "一次性的，跑一次"
	}
	c := commitItem{Kind: commit.KindSchedule, ID: t.ID, Agent: t.Agent, Title: t.Name, Link: "#/schedules",
		Contract: commit.Contract{
			Goal: t.Name, By: t.CreatedBy, Scene: t.Source, Scope: scopeText(t.Mode, t.Workspaces, nil) + "；创建者上限 " + t.CreatorMode,
			Done: "每次跑完的结果", ReportTo: report, Stop: stopText("超时 "+timeout, once),
		}}
	f := commit.Facts{Now: now, Running: t.Running}
	ended := oneShot(t) && !t.Enabled && t.Last != nil && !t.Running
	switch {
	case ended && t.Last.OK:
		f.Ended, f.Evidence = commit.Done, t.Last.Summary
	case ended:
		f.Ended, f.Reason = commit.Failed, t.Last.Summary
	default:
		f.Paused = !t.Enabled
		if !t.Next.IsZero() {
			f.Due = t.Next.Unix()
		}
		if t.Last != nil && !t.Last.OK && !t.Running {
			f.Blocked, f.Mark = "上一次失败："+t.Last.Summary, fmt.Sprintf("last:%d", t.Last.At.Unix())
			c.At = t.Last.At.Unix()
		}
	}
	c.Facts = f
	if f.Ended != "" && t.Last != nil {
		c.EndedAt = t.Last.At.Unix()
	}
	return c
}

// hookCommit 是一条 Hook 规则（不是单次投递）。last 是最近一次投递（没有就是 nil），running 是有投递在跑。
func hookCommit(r eventlog.RuleRecord, spec hooks.Spec, last *eventlog.Delivery, running bool, now int64, loc *time.Location) commitItem {
	var report []string
	if spec.Deliver != "" && spec.Deliver != "none" && !r.Silent {
		report = []string{spec.Deliver}
	}
	stop := []string{}
	if spec.Handler.TokenBudget > 0 {
		stop = append(stop, fmt.Sprintf("每次最多 %d token", spec.Handler.TokenBudget))
	}
	if spec.Retries > 0 {
		stop = append(stop, fmt.Sprintf("失败最多重试 %d 次", spec.Retries))
	}
	if spec.DeadlineAt > 0 {
		stop = append(stop, "到 "+time.UnixMilli(spec.DeadlineAt).In(loc).Format("01-02 15:04")+" 为止") // DeadlineAt 是毫秒
	}
	if r.Once {
		stop = append(stop, "只触发一次")
	}
	c := commitItem{Kind: commit.KindHook, ID: r.ID, Agent: r.AgentID, Title: spec.Name, Link: "#/hooks/" + r.AgentID,
		Contract: commit.Contract{
			Goal: spec.Name, By: r.Creator, Scene: spec.Scope.SID, Scope: scopeText(spec.Scope.Mode, spec.Scope.Workspaces, spec.Scope.Tools),
			Done: "每次投递成功", ReportTo: report, Stop: stopText(stop...),
		}}
	f := commit.Facts{Now: now, Running: running}
	// DeadlineAt 是毫秒；这个时间的事件已经触发过（之后有投递）就不再是“要到期的事”
	if spec.DeadlineAt > 0 && (last == nil || last.UpdatedMS < spec.DeadlineAt) {
		f.Due = spec.DeadlineAt / 1000
	}
	switch {
	case r.Once && !r.Enabled && last != nil && last.Status == "succeeded":
		f.Ended, f.Evidence = commit.Done, firstNonEmpty(last.Result, "投递成功")
		c.EndedAt = last.UpdatedMS / 1000
	case r.Once && r.ClaimedBy != "" && last != nil && (last.Status == "failed" || last.Status == "cancelled"):
		// Once 规则已经被事件占用、那次投递最终失败或取消：它不会再触发了
		why := "投递失败"
		if last.Status == "cancelled" {
			why = "投递被取消"
		}
		f.Ended, f.Reason = commit.Failed, firstNonEmpty(last.Error, why)
		c.EndedAt = last.UpdatedMS / 1000
	default:
		// Once 规则被事件占用后会自己停用，那不是人停的
		f.Paused = !r.Enabled && !(r.Once && r.ClaimedBy != "")
		if last != nil && (last.Status == "failed" || last.Status == "uncertain") {
			why := "最近一次投递失败：" + last.Error
			if last.Status == "uncertain" {
				why = "最近一次投递的结果不明：" + last.Error
			}
			f.Blocked, f.Mark = why, "d:"+last.ID
			c.At = last.UpdatedMS / 1000
		}
	}
	c.Facts = f
	return c
}

// branchCommit 是一个分支。running 是它有一轮在跑或排队；autonomous 是自主活动（agent 自己）分出来的。
func branchCommit(m session.BranchMeta, running, autonomous bool, now int64) commitItem {
	goal := firstNonEmpty(m.Intent, m.Title)
	var report []string
	if m.Delivery == session.BranchDeliveryReport || m.Delivery == session.BranchDeliveryAttention {
		report = []string{m.ParentSID}
	}
	stop := ""
	if autonomous {
		stop = "自动分支最多 3 层，受当日自主预算限制"
	}
	c := commitItem{Kind: commit.KindBranch, ID: m.SID, Agent: m.OwnerAgent, Title: firstNonEmpty(m.Title, goal), Link: "#/sessions/" + m.SID,
		Contract: commit.Contract{
			Goal: goal, By: m.CreatedBy, Scene: m.ParentSID, Scope: "快照里的设置（模型、目录、权限）；归 " + m.OwnerAgent,
			Done: "主人引入结论或归档", ReportTo: report, Stop: stop,
		}}
	f := commit.Facts{Now: now, Running: running || m.Status == session.BranchRunning || m.Status == session.BranchQueued}
	switch m.Status {
	case session.BranchArchived:
		f.Ended, f.Reason = commit.Stopped, firstNonEmpty(m.Note, "主人归档了")
	case session.BranchCancelled:
		f.Ended, f.Reason = commit.Stopped, firstNonEmpty(m.Note, "取消了")
	case session.BranchPaused:
		f.Paused = true
	case session.BranchNeedsAttention:
		f.Blocked, f.Mark = firstNonEmpty(m.Note, m.Progress, "需要处理"), fmt.Sprintf("p:%d", m.ProgressAt)
		c.At = m.ProgressAt
	case session.BranchCompleted:
		// 主人建的分支做完一轮，之后没引入过结论：等人看。自主活动分出的不算，只看它的汇报
		if !autonomous && !branchIntroduced(m) {
			f.Ready, f.Mark = firstNonEmpty(m.Progress, "做完了一轮"), fmt.Sprintf("p:%d", m.ProgressAt)
			c.At = m.ProgressAt
		}
	case session.BranchPreparing, session.BranchPendingBoundary:
		f.Due, f.Wait = max(m.CreatedAt, 1), "等源会话的安全边界"
	case session.BranchReady:
		f.Due, f.Wait = max(m.CreatedAt, 1), "等开始"
	}
	c.Facts = f
	if f.Ended != "" {
		c.EndedAt = max(m.LastActive, m.ProgressAt, m.CreatedAt)
	}
	return c
}

// branchIntroduced 是主人在这一轮做完之后已经把结论带回来源会话。
// 升级前引入过结论的分支没有 ReportedAt：Progress 是引入结论时写的那句话，也算。
func branchIntroduced(m session.BranchMeta) bool {
	if m.ReportedAt > 0 {
		return m.ReportedAt >= m.ProgressAt
	}
	return strings.HasPrefix(m.Progress, "已把结论带回来源会话：")
}
