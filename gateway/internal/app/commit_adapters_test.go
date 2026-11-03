package app

import (
	"testing"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/session"
)

// 六个适配函数：给出各自的记录，推出的事实和阶段都对。

const nowU = 1_000_000

func phaseOf(c commitItem) (string, string) { return commit.Phase(c.Facts) }

func wantPhase(t *testing.T, name string, c commitItem, phase, why string) {
	t.Helper()
	if p, w := phaseOf(c); p != phase || (why != "" && w != why) {
		t.Fatalf("%s：阶段 %s / %q，应为 %s / %q（事实 %+v）", name, p, w, phase, why, c.Facts)
	}
}

func TestAgendaCommit(t *testing.T) {
	in := &autonomy.Intention{ID: "in-1", Why: "怕忘了", Blocked: "等预算恢复"}
	base := autonomy.AgendaItem{ID: "ag-1", Title: "发邮件", Kind: autonomy.KindCommitment, At: nowU + 600, Source: "chat:main",
		Status: autonomy.StatusPlanned, Criteria: "邮件发出去", Mode: "ask", Workspaces: []string{"proj"}, Minutes: 10, IntentionID: "in-1"}
	c := agendaCommit(base, in, false, "default", nowU)
	if c.Contract.Goal != "发邮件：怕忘了" || c.Contract.By != "default" || c.Contract.Scene != "chat:main" || c.Contract.Done != "邮件发出去" ||
		len(c.Contract.ReportTo) != 1 || c.Contract.ReportTo[0] != "@default/life" || c.Contract.Scope == "" || c.Contract.Stop == "" {
		t.Fatalf("契约：%+v", c.Contract)
	}
	wantPhase(t, "没到时间", c, commit.Waiting, "等到某个时间")
	due := base
	due.At = nowU - 1
	wantPhase(t, "到期但意图在等预算", agendaCommit(due, in, false, "default", nowU), commit.Waiting, "等预算恢复")
	wantPhase(t, "到期", agendaCommit(due, nil, false, "default", nowU), commit.Waiting, "到期了，马上开始")
	wantPhase(t, "自主活动暂停", agendaCommit(base, in, true, "default", nowU), commit.Paused, "")
	run := base
	run.Status = autonomy.StatusInProgress
	wantPhase(t, "在跑", agendaCommit(run, in, false, "default", nowU), commit.Running, "")
	done := base
	done.Status, done.Result, done.Updated = autonomy.StatusDone, "邮件已发出，收到回执", nowU
	wantPhase(t, "完成", agendaCommit(done, in, true, "default", nowU), commit.Done, "邮件已发出，收到回执")
	if agendaCommit(done, in, false, "default", nowU).EndedAt != nowU {
		t.Fatal("结束时间")
	}
	done.Result = ""
	wantPhase(t, "完成但没有结果", agendaCommit(done, in, false, "default", nowU), commit.Blocked, "说做完了，但没有证据")
	cancelled := base
	cancelled.Status, cancelled.Result = autonomy.StatusCancelled, "对方说不用了"
	cancelled.EndedBy = autonomy.EndedByOwner
	wantPhase(t, "主人取消", agendaCommit(cancelled, in, false, "default", nowU), commit.Stopped, "对方说不用了")
	cancelled.EndedBy = autonomy.EndedByAgent
	wantPhase(t, "agent 取消", agendaCommit(cancelled, in, false, "default", nowU), commit.Dropped, "对方说不用了")
	cancelled.Result = ""
	wantPhase(t, "agent 取消没有理由", agendaCommit(cancelled, in, false, "default", nowU), commit.Dropped, "取消了")
	skipped := base
	skipped.Status, skipped.Result = autonomy.StatusSkipped, "今天不适合"
	wantPhase(t, "跳过", agendaCommit(skipped, in, false, "default", nowU), commit.Dropped, "今天不适合")
}

func TestIntentionCommit(t *testing.T) {
	in := autonomy.Intention{ID: "in-1", Title: "整理照片", Why: "想留念", SourceEvent: "ev-1", State: autonomy.IntentionAccepted, NextAt: nowU + 60, BudgetTokens: 5000}
	// 还有没完成的议程项：只列议程项
	if _, ok := intentionCommit(in, []autonomy.AgendaItem{{ID: "a", IntentionID: "in-1", Status: autonomy.StatusPlanned}}, false, "default", nowU); ok {
		t.Fatal("有未完成的议程项就不列意图")
	}
	if _, ok := intentionCommit(in, []autonomy.AgendaItem{{ID: "a", IntentionID: "other", Status: autonomy.StatusPlanned}}, false, "default", nowU); !ok {
		t.Fatal("别的意图的议程项不算")
	}
	c, _ := intentionCommit(in, nil, false, "default", nowU)
	if c.Contract.Goal != "整理照片：想留念" || c.Contract.Scene != "ev-1" || c.Contract.Done != "有证据的执行结果" || c.Contract.Stop == "" {
		t.Fatalf("契约：%+v", c.Contract)
	}
	wantPhase(t, "等", c, commit.Waiting, "等到某个时间")
	for state, want := range map[string]string{autonomy.IntentionRunning: commit.Running, autonomy.IntentionPaused: commit.Paused} {
		x := in
		x.State = state
		c, _ := intentionCommit(x, nil, false, "default", nowU)
		wantPhase(t, state, c, want, "")
	}
	paused, _ := intentionCommit(in, nil, true, "default", nowU)
	wantPhase(t, "自主活动暂停", paused, commit.Paused, "")
	x := in
	x.State, x.Evidence = autonomy.IntentionDone, "文件都归好了"
	c, _ = intentionCommit(x, []autonomy.AgendaItem{{IntentionID: "in-1", Status: autonomy.StatusDone}}, false, "default", nowU)
	wantPhase(t, "完成", c, commit.Done, "文件都归好了")
	x.State, x.Blocked = autonomy.IntentionFailed, "磁盘满了"
	c, _ = intentionCommit(x, nil, false, "default", nowU)
	wantPhase(t, "失败", c, commit.Failed, "磁盘满了")
	x.State = autonomy.IntentionCancelled
	c, _ = intentionCommit(x, []autonomy.AgendaItem{{IntentionID: "in-1", Status: autonomy.StatusCancelled, Result: "不做了", EndedBy: autonomy.EndedByOwner}}, false, "default", nowU)
	wantPhase(t, "主人取消", c, commit.Stopped, "不做了")
	c, _ = intentionCommit(x, []autonomy.AgendaItem{{IntentionID: "in-1", Status: autonomy.StatusCancelled, Result: "没必要", EndedBy: autonomy.EndedByAgent}}, false, "default", nowU)
	wantPhase(t, "agent 取消", c, commit.Dropped, "没必要")
}

func TestFollowUpCommit(t *testing.T) {
	f := autonomy.FollowUp{At: nowU + 300, Note: "问问面试怎么样"}
	c := followUpCommit("tg:42", f, "default", nowU)
	if c.ID != "tg:42" || c.Contract.Goal != "问问面试怎么样" || c.Contract.Scene != "tg:42" || c.Contract.ReportTo[0] != "tg:42" || c.Contract.Stop != "一次" {
		t.Fatalf("契约：%+v", c)
	}
	wantPhase(t, "等", c, commit.Waiting, "等到某个时间")
	f.At = nowU - 5
	wantPhase(t, "到点", followUpCommit("tg:42", f, "default", nowU), commit.Waiting, "到期了，马上开始")
	// 回访不随自主活动暂停：暂停时照常发，所以不设 Paused
	if got := followUpCommit("tg:42", f, "default", nowU); got.Facts.Paused {
		t.Fatal("回访不该因为自主活动暂停而 Paused")
	}
}

func TestScheduleCommit(t *testing.T) {
	task := schedule.Task{ID: "t1", Name: "每天看新闻", When: "every 24h", Agent: "default", CreatedBy: "web:me", CreatorMode: "edit", Mode: "ask",
		Workspaces: []string{"proj"}, Deliver: []string{"tg:1"}, Enabled: true, Next: time.Unix(nowU+3600, 0), Source: "chat:main"}
	c := scheduleCommit(task, nowU)
	if c.Contract.Goal != "每天看新闻" || c.Contract.By != "web:me" || c.Contract.Done != "每次跑完的结果" || c.Contract.ReportTo[0] != "tg:1" ||
		c.Contract.Stop != "超时 30m" {
		t.Fatalf("契约：%+v", c.Contract)
	}
	wantPhase(t, "等", c, commit.Waiting, "等到某个时间")
	silent := task
	silent.Silent = true
	if got := scheduleCommit(silent, nowU).Contract.ReportTo; len(got) != 0 {
		t.Fatalf("Silent 时汇报去处为空：%v", got)
	}
	run := task
	run.Running = true
	wantPhase(t, "在跑", scheduleCommit(run, nowU), commit.Running, "")
	off := task
	off.Enabled = false
	wantPhase(t, "停用", scheduleCommit(off, nowU), commit.Paused, "")
	bad := task
	bad.Last = &schedule.Result{At: time.Unix(nowU-100, 0), OK: false, Summary: "接口超时"}
	cb := scheduleCommit(bad, nowU)
	wantPhase(t, "上一次失败", cb, commit.Blocked, "上一次失败：接口超时")
	if cb.Facts.Mark != "last:999900" {
		t.Fatalf("Mark：%q", cb.Facts.Mark)
	}
	once := task
	once.When, once.Enabled, once.Timeout = "at 2026-11-02 09:00", false, "10m"
	once.Last = &schedule.Result{At: time.Unix(nowU-100, 0), OK: true, Summary: "发了 3 条摘要"}
	co := scheduleCommit(once, nowU)
	wantPhase(t, "一次性任务跑完", co, commit.Done, "发了 3 条摘要")
	if co.Contract.Stop != "超时 10m；一次性的，跑一次" || co.EndedAt != nowU-100 {
		t.Fatalf("%+v", co)
	}
	once.Last.OK, once.Last.Summary = false, "没有在线的内核"
	wantPhase(t, "一次性任务失败", scheduleCommit(once, nowU), commit.Failed, "没有在线的内核")
	once.Enabled = true // 还没跑
	once.Last = nil
	wantPhase(t, "一次性任务没跑", scheduleCommit(once, nowU), commit.Waiting, "")
}

func TestHookCommit(t *testing.T) {
	rule := eventlog.RuleRecord{ID: "hook_1", AgentID: "default", Creator: "web:me", Enabled: true}
	spec := hooks.Spec{Name: "构建失败提醒", Deliver: "tg:1", Retries: 2, DeadlineAt: (nowU + 7200) * 1000, // 毫秒

		Handler: hooks.Handler{Type: "agent", TokenBudget: 3000}, Scope: hooks.Scope{SID: "chat:main", Mode: "ask", Workspaces: []string{"proj"}, Tools: []string{"WebSearch"}}}
	c := hookCommit(rule, spec, nil, false, nowU, time.UTC)
	if c.Contract.Goal != "构建失败提醒" || c.Contract.By != "web:me" || c.Contract.Scene != "chat:main" || c.Contract.Done != "每次投递成功" ||
		c.Contract.ReportTo[0] != "tg:1" || c.Contract.Stop == "" || c.Title != "构建失败提醒" {
		t.Fatalf("契约：%+v", c.Contract)
	}
	wantPhase(t, "等到截止时间", c, commit.Waiting, "等到某个时间")
	spec2 := spec
	spec2.DeadlineAt = 0
	wantPhase(t, "等事件", hookCommit(rule, spec2, nil, false, nowU, time.UTC), commit.Waiting, "等事件")
	wantPhase(t, "有投递在跑", hookCommit(rule, spec2, nil, true, nowU, time.UTC), commit.Running, "")
	off := rule
	off.Enabled = false
	wantPhase(t, "停用", hookCommit(off, spec2, nil, false, nowU, time.UTC), commit.Paused, "")
	failed := &eventlog.Delivery{ID: "hd_1", Status: "failed", Error: "目标会话没了", UpdatedMS: 5000}
	cf := hookCommit(rule, spec2, failed, false, nowU, time.UTC)
	wantPhase(t, "最近一次失败", cf, commit.Blocked, "最近一次投递失败：目标会话没了")
	if cf.Facts.Mark != "d:hd_1" {
		t.Fatalf("Mark：%q", cf.Facts.Mark)
	}
	unsure := &eventlog.Delivery{ID: "hd_2", Status: "uncertain", Error: "超时", UpdatedMS: 6000}
	wantPhase(t, "结果不明", hookCommit(rule, spec2, unsure, false, nowU, time.UTC), commit.Blocked, "最近一次投递的结果不明：超时")
	ok := &eventlog.Delivery{ID: "hd_3", Status: "succeeded", Result: "已提醒", UpdatedMS: 7000}
	wantPhase(t, "成功了还在等下一次", hookCommit(rule, spec2, ok, false, nowU, time.UTC), commit.Waiting, "等事件")
	once := off
	once.Once = true
	co := hookCommit(once, spec2, ok, false, nowU, time.UTC)
	wantPhase(t, "Once 成功后停用", co, commit.Done, "已提醒")
	if co.EndedAt != 7 {
		t.Fatalf("结束时间：%d", co.EndedAt)
	}
	wantPhase(t, "Once 停用但没成功", hookCommit(once, spec2, failed, false, nowU, time.UTC), commit.Paused, "")
	silent := rule
	silent.Silent = true
	if got := hookCommit(silent, spec, nil, false, nowU, time.UTC).Contract.ReportTo; len(got) != 0 {
		t.Fatalf("Silent 时汇报去处为空：%v", got)
	}
}

func TestBranchCommit(t *testing.T) {
	m := session.BranchMeta{SID: "main/b1", ParentSID: "main", OwnerAgent: "default", CreatedBy: "web:me", Intent: "查一下竞品", Title: "竞品",
		Status: session.BranchReady, Delivery: session.BranchDeliverySilent, CreatedAt: nowU - 50, ProgressAt: nowU - 10, Progress: "完成：这一轮做了「查竞品」"}
	c := branchCommit(m, false, false, nowU)
	if c.Contract.Goal != "查一下竞品" || c.Contract.By != "web:me" || c.Contract.Scene != "main" || c.Contract.Done != "主人引入结论或归档" || len(c.Contract.ReportTo) != 0 {
		t.Fatalf("契约：%+v", c.Contract)
	}
	wantPhase(t, "等开始", c, commit.Waiting, "等开始")
	m.Title, m.Intent = "竞品", ""
	if branchCommit(m, false, false, nowU).Contract.Goal != "竞品" {
		t.Fatal("没有 Intent 就用 Title")
	}
	m.Intent = "查一下竞品"
	m.Status = session.BranchPreparing
	wantPhase(t, "准备中", branchCommit(m, false, false, nowU), commit.Waiting, "等源会话的安全边界")
	m.Status = session.BranchPendingBoundary
	wantPhase(t, "等边界", branchCommit(m, false, false, nowU), commit.Waiting, "等源会话的安全边界")
	for _, st := range []string{session.BranchRunning, session.BranchQueued} {
		m.Status = st
		wantPhase(t, st, branchCommit(m, false, false, nowU), commit.Running, "")
	}
	m.Status = session.BranchReady
	wantPhase(t, "在排队", branchCommit(m, true, false, nowU), commit.Running, "")
	m.Status = session.BranchPaused
	wantPhase(t, "暂停", branchCommit(m, false, false, nowU), commit.Paused, "")
	m.Status, m.Note = session.BranchNeedsAttention, "要选一个方案"
	cb := branchCommit(m, false, false, nowU)
	wantPhase(t, "要处理", cb, commit.Blocked, "要选一个方案")
	if cb.Facts.Mark != "p:999990" {
		t.Fatalf("Mark：%q", cb.Facts.Mark)
	}
	m.Status, m.Note = session.BranchCompleted, ""
	cr := branchCommit(m, false, false, nowU)
	wantPhase(t, "做完一轮等人看", cr, commit.Ready, "完成：这一轮做了「查竞品」")
	if cr.Facts.Mark != "p:999990" {
		t.Fatalf("Mark：%q", cr.Facts.Mark)
	}
	wantPhase(t, "自主活动分出的不算就绪", branchCommit(m, false, true, nowU), commit.Waiting, "等事件")
	m.ReportedAt = nowU - 5 // 做完之后主人引入过结论
	wantPhase(t, "引入过结论", branchCommit(m, false, false, nowU), commit.Waiting, "等事件")
	m.ReportedAt = nowU - 20 // 引入之后又做了一轮
	wantPhase(t, "引入之后又做了一轮", branchCommit(m, false, false, nowU), commit.Ready, "")
	for st, why := range map[string]string{session.BranchArchived: "主人归档了", session.BranchCancelled: "取消了"} {
		m.Status, m.LastActive = st, nowU-3
		c := branchCommit(m, false, false, nowU)
		wantPhase(t, st, c, commit.Stopped, why)
		if c.EndedAt != nowU-3 {
			t.Fatalf("结束时间：%d", c.EndedAt)
		}
	}
}

// 主人取消日程没写理由：不能变成受阻，也不能留着 Retry 留下的旧原因。
func TestOwnerCancelWithoutReasonIsStoppedNotBlocked(t *testing.T) {
	now := time.Unix(nowU, 0)
	st := autonomy.New()
	item, _ := st.AddAgenda(autonomy.AgendaItem{Title: "发邮件", Kind: autonomy.KindCommitment, At: nowU + 600}, now)
	st.Retry(autonomy.Trigger{AgendaID: item.ID}, "还没有配置模型", now, 30*time.Minute) // 留下一条旧原因
	if err := st.CancelAgenda(item.ID, "", autonomy.EndedByOwner, now); err != nil {
		t.Fatal(err)
	}
	got, _ := st.AgendaByID(item.ID)
	c := agendaCommit(got, nil, false, "default", nowU)
	wantPhase(t, "主人取消没写理由", c, commit.Stopped, "主人取消了")
	// 意图也一样
	in := autonomy.Intention{ID: "in-1", Title: "整理", State: autonomy.IntentionCancelled}
	ci, _ := intentionCommit(in, []autonomy.AgendaItem{{IntentionID: "in-1", Status: autonomy.StatusCancelled, EndedBy: autonomy.EndedByOwner}}, false, "default", nowU)
	wantPhase(t, "意图：主人取消没写理由", ci, commit.Stopped, "主人取消了")
	// 升级前的记录没有 EndedBy：算无需处理，没有 Result 就写“取消了”，不算受阻
	legacy := autonomy.AgendaItem{ID: "ag-old", Title: "老的", Status: autonomy.StatusCancelled}
	wantPhase(t, "升级前取消", agendaCommit(legacy, nil, false, "default", nowU), commit.Dropped, "取消了")
	ci, _ = intentionCommit(in, []autonomy.AgendaItem{{IntentionID: "in-1", Status: autonomy.StatusCancelled}}, false, "default", nowU)
	wantPhase(t, "意图：升级前取消", ci, commit.Dropped, "取消了")
	// agent 取消不写理由（工具会拦，别的路径也可能）：清掉旧原因，算无需处理
	st2 := autonomy.New()
	it2, _ := st2.AddAgenda(autonomy.AgendaItem{Title: "x", Kind: autonomy.KindPlan, At: nowU}, now)
	st2.Retry(autonomy.Trigger{AgendaID: it2.ID}, "没有在线的内核", now, 5*time.Minute)
	st2.CancelAgenda(it2.ID, "", autonomy.EndedByAgent, now)
	g2, _ := st2.AgendaByID(it2.ID)
	if g2.Result != "" {
		t.Fatalf("旧原因不该留着：%q", g2.Result)
	}
}

// Hook 的 DeadlineAt 是毫秒：换成秒才能和 Now 比；触发过之后不再算到期；停止条件按时区写。
func TestHookDeadlineIsMilliseconds(t *testing.T) {
	rule := eventlog.RuleRecord{ID: "hook_1", AgentID: "default", Creator: "web:me", Enabled: true}
	deadlineMS := int64((nowU + 7200) * 1000)
	spec := hooks.Spec{Name: "到点提醒", Handler: hooks.Handler{Type: "notify"}, DeadlineAt: deadlineMS}
	c := hookCommit(rule, spec, nil, false, nowU, time.UTC)
	if c.Facts.Due != nowU+7200 {
		t.Fatalf("Due 要换成秒：%d", c.Facts.Due)
	}
	wantPhase(t, "没到点", c, commit.Waiting, "等到某个时间")
	if want := "到 " + time.UnixMilli(deadlineMS).UTC().Format("01-02 15:04") + " 为止"; c.Contract.Stop != want {
		t.Fatalf("停止条件：%q，应为 %q", c.Contract.Stop, want)
	}
	loc := time.FixedZone("x", 8*3600)
	if got := hookCommit(rule, spec, nil, false, nowU, loc).Contract.Stop; got == c.Contract.Stop {
		t.Fatalf("停止条件要按 loc 写：%q", got)
	}
	// 这个时间的事件触发过（规则在 DeadlineAt 之后有投递）：不再是要到期的事
	fired := &eventlog.Delivery{ID: "hd_1", Status: "succeeded", UpdatedMS: deadlineMS + 5}
	if got := hookCommit(rule, spec, fired, false, deadlineMS/1000+60, time.UTC); got.Facts.Due != 0 {
		t.Fatalf("触发过就不设 Due：%d", got.Facts.Due)
	}
	// 投递在 DeadlineAt 之前（别的事件）：还在等这个时间
	early := &eventlog.Delivery{ID: "hd_2", Status: "succeeded", UpdatedMS: deadlineMS - 1000}
	if got := hookCommit(rule, spec, early, false, nowU, time.UTC); got.Facts.Due != nowU+7200 {
		t.Fatalf("早先的投递不算：%d", got.Facts.Due)
	}
}

// Once 规则已经被事件占用、那次投递最终失败或取消：它不会再触发，算失败；结果不明仍然是受阻。
func TestHookOnceClaimedAndFailed(t *testing.T) {
	rule := eventlog.RuleRecord{ID: "hook_1", AgentID: "default", Creator: "web:me", Once: true, Enabled: false, ClaimedBy: "ev_1"}
	spec := hooks.Spec{Name: "只提醒一次", Handler: hooks.Handler{Type: "notify"}}
	failed := &eventlog.Delivery{ID: "hd_1", Status: "failed", Error: "目标会话没了", UpdatedMS: 9000}
	c := hookCommit(rule, spec, failed, false, nowU, time.UTC)
	wantPhase(t, "Once 失败", c, commit.Failed, "目标会话没了")
	if c.EndedAt != 9 {
		t.Fatalf("结束时间：%d", c.EndedAt)
	}
	cancelled := &eventlog.Delivery{ID: "hd_2", Status: "cancelled", UpdatedMS: 9000}
	wantPhase(t, "Once 取消", hookCommit(rule, spec, cancelled, false, nowU, time.UTC), commit.Failed, "投递被取消")
	unsure := &eventlog.Delivery{ID: "hd_3", Status: "uncertain", Error: "超时", UpdatedMS: 9000}
	wantPhase(t, "Once 结果不明仍是受阻", hookCommit(rule, spec, unsure, false, nowU, time.UTC), commit.Blocked, "最近一次投递的结果不明：超时")
	// 还没被事件占用的 Once 规则：照旧在等
	free := rule
	free.ClaimedBy, free.Enabled = "", true
	wantPhase(t, "Once 还在等", hookCommit(free, spec, nil, false, nowU, time.UTC), commit.Waiting, "等事件")
}

// 升级前引入过结论的分支没有 ReportedAt：Progress 是引入结论时写的那句，也算引入过。
func TestBranchIntroducedBeforeUpgradeIsNotReady(t *testing.T) {
	m := session.BranchMeta{SID: "main/b1", ParentSID: "main", OwnerAgent: "default", CreatedBy: "web:me", Intent: "查竞品",
		Status: session.BranchCompleted, ProgressAt: nowU - 10, Progress: "已把结论带回来源会话：竞品有三家"}
	wantPhase(t, "升级前引入过", branchCommit(m, false, false, nowU), commit.Waiting, "等事件")
	m.Progress = "完成：这一轮做了「查竞品」"
	wantPhase(t, "没引入过", branchCommit(m, false, false, nowU), commit.Ready, "")
	m.Progress, m.ReportedAt = "已把结论带回来源会话：竞品有三家", nowU-20 // 新记录以 ReportedAt 为准：引入之后又做了一轮
	m.Progress = "完成：又做了一轮"
	wantPhase(t, "引入之后又做了一轮", branchCommit(m, false, false, nowU), commit.Ready, "")
}

// 契约的汇报去处：report 和 attention 的分支都汇报到父会话，silent 只留在自己的记录里。
func TestBranchContractReportTo(t *testing.T) {
	for delivery, want := range map[string]int{session.BranchDeliverySilent: 0, session.BranchDeliveryReport: 1, session.BranchDeliveryAttention: 1} {
		m := session.BranchMeta{SID: "b1", ParentSID: "main", OwnerAgent: "default", CreatedBy: "web:me", Title: "t", Status: session.BranchReady, Delivery: delivery}
		got := branchCommit(m, false, false, 1000).Contract.ReportTo
		if len(got) != want || (want == 1 && got[0] != "main") {
			t.Errorf("%s 的汇报去处：%v", delivery, got)
		}
	}
}
