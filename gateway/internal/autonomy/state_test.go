package autonomy

import (
	"strings"
	"testing"
	"time"
)

var testLoc = time.UTC

func at(hh, mm int) time.Time { return time.Date(2026, 10, 26, hh, mm, 0, 0, testLoc) }

func TestEventDedupeAndProcessing(t *testing.T) {
	s := New()
	now := at(10, 0)
	first := Event{Kind: EventConversation, Dedupe: "conv:main", SID: "main", Text: "你好"}
	if !s.AddEvent(first, now, 10*time.Minute) {
		t.Fatal("第一条事件应该进去")
	}
	if s.AddEvent(Event{Kind: EventConversation, Dedupe: "conv:main", SID: "main", Text: "又说了一句"}, now.Add(2*time.Minute), 10*time.Minute) {
		t.Fatal("窗口内同一个 dedupe 应该合并")
	}
	// 窗口外允许再进（新的聊天）
	if !s.AddEvent(Event{Kind: EventConversation, Dedupe: "conv:main", SID: "main"}, now.Add(11*time.Minute), 10*time.Minute) {
		t.Fatal("过了合并窗口应该能再进")
	}
	events := s.UnprocessedEvents(10)
	if len(events) != 2 {
		t.Fatalf("应该有两条没处理的事件: %+v", events)
	}
	s.MarkProcessed([]string{events[0].ID})
	if left := s.UnprocessedEvents(10); len(left) != 1 || left[0].ID != events[1].ID {
		t.Fatalf("处理后剩一条: %+v", left)
	}
	if s.Revision == 0 {
		t.Fatal("修改应该增加 revision")
	}
}

func TestDuePriorityAndCommitmentOutOfHours(t *testing.T) {
	s := New()
	now := at(23, 50) // 拟人型的睡觉时间
	later := now.Add(10 * time.Minute).Unix()
	if _, err := s.AddAgenda(AgendaItem{Title: "看论文", Kind: KindExploration, At: now.Add(5 * time.Minute).Unix(), Minutes: 20, Movable: true}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgenda(AgendaItem{Title: "提醒主人吃药", Kind: KindCommitment, At: later, Minutes: 5}, now); err != nil {
		t.Fatal(err)
	}
	due := s.Due(now.Add(20 * time.Minute))
	if len(due) != 2 || due[0].Kind != KindCommitment {
		t.Fatalf("约定应该排在最前: %+v", due)
	}
	// 约定不受作息限制；探索要等醒着
	if ok, why := due[0].CanStart(StartOptions{Awake: false, BudgetLeft: 1000, Wakes: 100, MaxWakes: 8}); !ok {
		t.Fatalf("约定应该能在睡着时做: %s", why)
	}
	if ok, why := due[1].CanStart(StartOptions{Awake: false, BudgetLeft: 1000}); ok || !strings.Contains(why, "睡着") {
		t.Fatalf("探索睡着时不该开始: %v %q", ok, why)
	}
	// 最近有人找过（旧版的 20 分钟门槛）不再影响判断：CanStart 里根本没有这个条件
	if ok, _ := due[1].CanStart(StartOptions{Awake: true, BudgetLeft: 1000, Wakes: 1, MaxWakes: 8}); !ok {
		t.Fatal("醒着、有预算就该能做，不要求空闲 20 分钟")
	}
	// 每天的自主次数只限制探索
	if ok, why := due[1].CanStart(StartOptions{Awake: true, BudgetLeft: 1000, Wakes: 8, MaxWakes: 8}); ok || !strings.Contains(why, "醒够") {
		t.Fatalf("探索应该受每天次数限制: %v %q", ok, why)
	}
	if ok, _ := due[0].CanStart(StartOptions{Awake: true, BudgetLeft: 1000, Wakes: 8, MaxWakes: 8}); !ok {
		t.Fatal("约定不该被每天次数挡住")
	}
	// 预算用完谁都不能开始
	if ok, why := due[0].CanStart(StartOptions{Awake: true, BudgetLeft: 0}); ok || !strings.Contains(why, "预算") {
		t.Fatalf("预算用完不能开始: %v %q", ok, why)
	}
	// 暂停立即生效
	if ok, why := due[0].CanStart(StartOptions{Paused: true, Awake: true, BudgetLeft: 1000}); ok || !strings.Contains(why, "暂停") {
		t.Fatalf("暂停应该挡住: %v %q", ok, why)
	}
}

func TestFinishRequiresRealToolResult(t *testing.T) {
	s := New()
	now := at(15, 0)
	ag, _ := s.AddAgenda(AgendaItem{Title: "查一下面试结果", Kind: KindPlan, At: now.Unix()}, now)
	in, _ := s.AddIntention(Intention{Title: "帮主人查面试结果", Priority: 4}, now)
	ag.IntentionID = in.ID
	s.MarkStarted(Trigger{Kind: KindPlan, AgendaID: ag.ID, IntentionID: in.ID}, now)
	if !s.Paused && s.Agenda[0].Status != StatusInProgress || s.Intentions[0].State != IntentionRunning {
		t.Fatalf("开始后状态不对: %+v %+v", s.Agenda[0], s.Intentions[0])
	}
	// 只写了一段计划、没调工具：不算完成，往后挪
	s.Finish(Execution{AgendaID: ag.ID, IntentionID: in.ID, Started: now.Unix(), OK: true, UsedTools: false,
		Summary: "我打算等会儿查"}, now.Add(time.Minute), 30*time.Minute)
	if s.Agenda[0].Status != StatusPlanned || s.Agenda[0].At != now.Add(31*time.Minute).Unix() {
		t.Fatalf("没真正执行不该算完成: %+v", s.Agenda[0])
	}
	if s.Intentions[0].State != IntentionWaiting || !strings.Contains(s.Intentions[0].Blocked, "没有真正执行") {
		t.Fatalf("意图应该等下次: %+v", s.Intentions[0])
	}
	// 真的调了工具、正常结束：完成并记下证据
	now2 := now.Add(32 * time.Minute)
	s.MarkStarted(Trigger{Kind: KindPlan, AgendaID: ag.ID, IntentionID: in.ID}, now2)
	s.Finish(Execution{AgendaID: ag.ID, IntentionID: in.ID, Started: now2.Unix(), OK: true, UsedTools: true, Verified: true,
		Summary: "查到了：下周三二面", Tokens: 1200}, now2.Add(2*time.Minute), 30*time.Minute)
	if s.Agenda[0].Status != StatusDone || !strings.Contains(s.Agenda[0].Result, "下周三") {
		t.Fatalf("有真实结果应该完成: %+v", s.Agenda[0])
	}
	if s.Intentions[0].State != IntentionDone || s.Intentions[0].Evidence == "" {
		t.Fatalf("意图应该完成并留下证据: %+v", s.Intentions[0])
	}
	// 失败：不是完成，安排重试
	ag2, _ := s.AddAgenda(AgendaItem{Title: "发消息问问", Kind: KindCommitment, At: now.Unix()}, now)
	s.MarkStarted(Trigger{Kind: KindCommitment, AgendaID: ag2.ID}, now)
	s.Finish(Execution{AgendaID: ag2.ID, Started: now.Unix(), OK: false, Summary: "网络断了"}, now, 10*time.Minute)
	if s.Agenda[1].Status != StatusPlanned || s.Agenda[1].At != now.Add(10*time.Minute).Unix() {
		t.Fatalf("失败要安排重试: %+v", s.Agenda[1])
	}
	if s.Executions[len(s.Executions)-1].OK {
		t.Fatal("执行记录应该是失败的")
	}
}

func TestCheckpointRecoveryAndUnverifiedCompletion(t *testing.T) {
	s := New()
	now := at(15, 0)
	ag, _ := s.AddAgenda(AgendaItem{Title: "写报告", At: now.Unix(), Criteria: "文件存在且核对过内容"}, now)
	id := ag.ID
	if err := s.SetCheckpoint(id, "第一段已写；下一步补数据；不要重发邮件", now); err != nil {
		t.Fatal(err)
	}
	s.MarkStarted(Trigger{AgendaID: id}, now)
	s.Finish(Execution{AgendaID: id, OK: true, UsedTools: true, Summary: "只是查询了日程"}, now, time.Minute)
	if s.Agenda[0].Status == StatusDone {
		t.Fatal("调过工具不能替代验收")
	}
	s.MarkStarted(Trigger{AgendaID: id}, now.Add(time.Minute))
	if !s.Recover(now.Add(2*time.Minute), time.Minute) {
		t.Fatal("应当恢复未结束任务")
	}
	if !strings.Contains(s.BriefForModel(now, time.UTC), "不要重发邮件") {
		t.Fatal("重启后没有保留给下一轮的检查点")
	}
	if err := s.CancelAgenda(id, "用户取消", now); err != nil {
		t.Fatal(err)
	}
	s.Finish(Execution{AgendaID: id, OK: true, UsedTools: true, Verified: true, Summary: "迟到的完成"}, now, time.Minute)
	if s.Agenda[0].Status != StatusCancelled {
		t.Fatal("迟到的结果覆盖了用户取消")
	}
}

func TestIdeasNeedSourceAndBecomeScheduled(t *testing.T) {
	s := New()
	now := at(9, 0)
	if _, err := s.AddIdea(Idea{Text: "比较两种解释"}, now, 7*24*time.Hour); err == nil {
		t.Fatal("没有来源的想法不该收")
	}
	x, err := s.AddIdea(Idea{Text: "比较两种解释", Source: "聊到 transformer 的两种说法", Interest: 4}, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if x.Interest != 4 || x.CostMinutes != 15 || x.Status != IdeaCandidate {
		t.Fatalf("想法默认值不对: %+v", x)
	}
	if _, err := s.AddIdea(Idea{Text: "比较两种解释", Source: "重复"}, now, 7*24*time.Hour); err == nil {
		t.Fatal("重复的想法应该挡住")
	}
	// 推迟
	if err := s.DeferIdea(x.ID, now.Add(time.Hour).Unix(), now); err != nil {
		t.Fatal(err)
	}
	if len(s.OpenIdeas(now)) != 0 {
		t.Fatal("推迟的想法不该出现在候选里")
	}
	if len(s.OpenIdeas(now.Add(2*time.Hour))) != 1 {
		t.Fatal("到时间以后应该回到候选")
	}
	// 排进日程：变成意图 + 可挪动的探索时间块
	in, ag, err := s.ScheduleIdea(x.ID, now.Add(3*time.Hour).Unix(), 20, now)
	if err != nil || in == nil || ag == nil {
		t.Fatalf("排期失败: %v", err)
	}
	if ag.Kind != KindExploration || !ag.Movable || ag.IntentionID != in.ID {
		t.Fatalf("探索块不对: %+v", ag)
	}
	if x2, _ := s.IdeaByID(x.ID); x2.Status != IdeaScheduled || x2.IntentionID != in.ID {
		t.Fatalf("想法状态不对: %+v", x2)
	}
	if err := s.DropIdea(x.ID, "", now); err == nil {
		t.Fatal("已排期的想法不能直接丢")
	}
	// 挪时间
	if err := s.MoveAgenda(ag.ID, now.Add(4*time.Hour).Unix(), now); err != nil {
		t.Fatal(err)
	}
	// 取消
	if err := s.CancelAgenda(ag.ID, "今天没空", now); err != nil {
		t.Fatal(err)
	}
	if s.Intentions[0].State != IntentionCancelled {
		t.Fatalf("取消最后一个议程项，意图也该取消: %+v", s.Intentions[0])
	}
}

func TestRecoverDoesNotReplayUnknownExecution(t *testing.T) {
	s := New()
	now := at(12, 0)
	ag, _ := s.AddAgenda(AgendaItem{Title: "发邮件", Kind: KindCommitment, At: now.Unix()}, now)
	in, _ := s.AddIntention(Intention{Title: "发邮件", Priority: 5}, now)
	ag.IntentionID = in.ID
	s.MarkStarted(Trigger{Kind: KindCommitment, AgendaID: ag.ID, IntentionID: in.ID}, now)
	if !s.Recover(now.Add(time.Minute), 5*time.Minute) {
		t.Fatal("应该发现重启前没结束的执行")
	}
	if s.Agenda[0].Status != StatusPlanned || s.Agenda[0].At != now.Add(6*time.Minute).Unix() {
		t.Fatalf("议程项应该重新确认而不是重放: %+v", s.Agenda[0])
	}
	if s.Intentions[0].State != IntentionWaiting || !strings.Contains(s.Intentions[0].Blocked, "结果未知") {
		t.Fatalf("意图应该等重新确认: %+v", s.Intentions[0])
	}
	// 恢复一次就够，再调不会把时间继续往后推
	before := s.Agenda[0].At
	if s.Recover(now.Add(2*time.Minute), 5*time.Minute) {
		t.Fatal("已经恢复过就不该再改")
	}
	if s.Agenda[0].At != before {
		t.Fatal("时间被重复推后了")
	}
}

func TestBudgetAndDayRollover(t *testing.T) {
	s := New()
	now := at(10, 0)
	if !s.RollDay(now, testLoc) {
		t.Fatal("第一次应该记下日期")
	}
	s.Spend(300)
	if s.BudgetLeft(1000) != 700 {
		t.Fatalf("预算剩多少不对: %d", s.BudgetLeft(1000))
	}
	if s.BudgetLeft(0) != -1 {
		t.Fatal("没设上限应该是 -1")
	}
	next := now.AddDate(0, 0, 1)
	if !s.RollDay(next, testLoc) || s.Budget.Tokens != 0 || s.BudgetLeft(1000) != 1000 {
		t.Fatalf("换天要清空用量: %+v", s.Budget)
	}
}

func TestNextTimeAndIDs(t *testing.T) {
	s := New()
	now := at(8, 0)
	if _, ok := s.NextDueTime(); ok {
		t.Fatal("空状态不该有下次时间")
	}
	a, _ := s.AddAgenda(AgendaItem{Title: "a", At: now.Add(2 * time.Hour).Unix()}, now)
	b, _ := s.AddAgenda(AgendaItem{Title: "b", At: now.Add(time.Hour).Unix()}, now)
	if a.ID == b.ID || a.ID == "" {
		t.Fatalf("ID 应该稳定且不重复: %q %q", a.ID, b.ID)
	}
	next, ok := s.NextDueTime()
	if !ok || !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("下次时间不对: %v %v", next, ok)
	}
	if s.Next(now) != nil {
		t.Fatal("还没到点不该醒")
	}
	s.SetNextCheck(now.Add(30 * time.Minute))
	trig := s.Next(now.Add(31 * time.Minute))
	if trig == nil || trig.Kind != EventSelfCheck {
		t.Fatalf("到自己的检查时间应该醒: %+v", trig)
	}
	// 约定优先于探索
	c, _ := s.AddAgenda(AgendaItem{Title: "约好九点", Kind: KindCommitment, At: now.Unix()}, now)
	s.Agenda = append(s.Agenda, AgendaItem{})
	s.Agenda = s.Agenda[:len(s.Agenda)-1]
	_ = c
	due := s.Due(now.Add(31 * time.Minute))
	if len(due) == 0 || due[0].Kind != KindCommitment {
		t.Fatalf("约定应该最先: %+v", due)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, testLoc)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"16:30", time.Date(2026, 10, 26, 16, 30, 0, 0, testLoc)},
		{"09:00", time.Date(2026, 10, 27, 9, 0, 0, 0, testLoc)},
		{"明天 09:00", time.Date(2026, 10, 27, 9, 0, 0, 0, testLoc)},
		{"+30m", now.Add(30 * time.Minute)},
		{"2h", now.Add(2 * time.Hour)},
		{"2026-10-26 20:00", time.Date(2026, 10, 26, 20, 0, 0, 0, testLoc)},
		{"10-27 08:00", time.Date(2026, 10, 27, 8, 0, 0, 0, testLoc)},
	}
	for _, c := range cases {
		got, err := ParseWhen(c.in, now, testLoc)
		if err != nil || !got.Equal(c.want) {
			t.Fatalf("%q → %v %v（期望 %v）", c.in, got, err, c.want)
		}
	}
	if _, err := ParseWhen("到时候", now, testLoc); err == nil {
		t.Fatal("认不出的时间应该报错")
	}
	if _, err := ParseWhen("-5m", now, testLoc); err == nil {
		t.Fatal("过去的时间应该报错")
	}
}

func TestImportLifeOnce(t *testing.T) {
	s := New()
	now := at(10, 0)
	recent := []LifeNote{{At: now.Add(-time.Hour).Unix(), Text: "上午看了一篇讲猫的文章"}}
	if !s.ImportLife(recent, now) {
		t.Fatal("第一次应该导入")
	}
	if s.ImportLife(recent, now) {
		t.Fatal("不能重复导入")
	}
	found := false
	for _, b := range s.Beliefs {
		if strings.Contains(b.Value, "讲猫") {
			found = true
		}
	}
	if !found {
		t.Fatalf("旧近况没有变成认识: %+v", s.Beliefs)
	}
}

func TestInterestsNeedSourceAndCanBeRemoved(t *testing.T) {
	s := New()
	now := at(10, 0)
	if _, err := s.AddInterest("读科幻", "", now); err == nil {
		t.Fatal("没有来源的兴趣不该收")
	}
	x, err := s.AddInterest("读科幻", "看完三体之后想到的", now)
	if err != nil || x.Source == "" {
		t.Fatalf("兴趣没记上: %+v %v", x, err)
	}
	if _, err := s.AddInterest("读科幻", "重复", now); err == nil {
		t.Fatal("同一个兴趣不该重复记")
	}
	if !s.RemoveInterest("读科幻", now) {
		t.Fatal("兴趣应该能删掉")
	}
	if s.RemoveInterest("读科幻", now) {
		t.Fatal("删过以后再删应该返回 false")
	}
}

func TestSetNumberValidatesAndChangesThePick(t *testing.T) {
	s := New()
	now := at(10, 0)
	low, err := s.AddIdea(Idea{Text: "低分想法", Interest: 1, Novelty: 1, CostMinutes: 10, Source: "test"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	high, err := s.AddIdea(Idea{Text: "高分想法", Interest: 3, Novelty: 3, CostMinutes: 10, Source: "test"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.OpenIdeas(now); len(got) != 2 || got[0].ID != high.ID {
		t.Fatalf("初始排序：%+v", got)
	}
	for _, bad := range []struct {
		id, field string
		value     float64
	}{{low.ID, "interest", 6}, {low.ID, "interest", 0}, {low.ID, "interest", 4.5}, {low.ID, "novelty", 9},
		{low.ID, "cost_minutes", 999}, {"i-999", "interest", 3}, {high.ID, "priority", 3}} {
		if err := s.SetNumber(bad.id, bad.field, bad.value, now); err == nil {
			t.Fatalf("%s=%v 应该被拒绝", bad.field, bad.value)
		}
	}
	if err := s.SetNumber(low.ID, "interest", 5, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNumber(low.ID, "novelty", 5, now); err != nil {
		t.Fatal(err)
	}
	if got := s.OpenIdeas(now); got[0].ID != low.ID || got[0].Interest != 5 || got[0].Novelty != 5 {
		t.Fatalf("改数值没有影响挑选：%+v", got)
	}
	first, err := s.AddIntention(Intention{Title: "甲", Priority: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddIntention(Intention{Title: "乙", Priority: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveIntentions(); got[0].ID != second.ID {
		t.Fatalf("初始优先级顺序：%+v", got)
	}
	if err := s.SetNumber(first.ID, "priority", 5, now); err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveIntentions(); got[0].ID != first.ID || got[0].Priority != 5 {
		t.Fatalf("改优先级没有影响挑选：%+v", got)
	}
	if err := s.SetNumber(first.ID, "progress", 101, now); err == nil {
		t.Fatal("进度越界应该被拒绝")
	}
	if err := s.SetNumber(first.ID, "progress", 60, now); err != nil {
		t.Fatal(err)
	}
	found := Intention{}
	for _, in := range s.Intentions {
		if in.ID == first.ID {
			found = in
		}
	}
	if found.Progress != 60 {
		t.Fatalf("进度没改上：%+v", found)
	}
	s.SetBelief("部署在哪", "本机", "测试", 0.9, now)
	if err := s.SetNumber("部署在哪", "confidence", 1.5, now); err == nil {
		t.Fatal("置信度越界应该被拒绝")
	}
	if err := s.SetNumber("部署在哪", "confidence", 0.2, now); err != nil || s.Beliefs[0].Confidence != 0.2 {
		t.Fatal(err, s.Beliefs)
	}
	if err := s.SetNumber("没有这条", "confidence", 0.5, now); err == nil {
		t.Fatal("不存在的认识应该被拒绝")
	}
}

func TestLedgerRecordsSourcesReservesAndRollsDays(t *testing.T) {
	s := New()
	loc := time.UTC
	day := at(10, 0)
	s.ReserveLedger(LedgerExploration, day, loc)
	if got := s.LedgerStarts(LedgerExploration, day, loc); got != 1 {
		t.Fatalf("预留没有被算进额度：%d", got)
	}
	s.ReleaseLedger(LedgerExploration, day, loc)
	if got := s.LedgerStarts(LedgerExploration, day, loc); got != 0 {
		t.Fatalf("启动失败没有退回预留：%d", got)
	}
	s.ReserveLedger(LedgerExploration, day, loc)
	s.SettleLedger(LedgerExploration, "r1", 120, day, loc)
	by := map[string]LedgerUsage{}
	for _, row := range s.LedgerToday(day, loc) {
		by[row.Source] = row
	}
	if by[LedgerExploration].Starts != 1 || by[LedgerExploration].Tokens != 120 || by[LedgerExploration].Reserved != 0 || len(by[LedgerExploration].Runs) != 1 {
		t.Fatalf("探索账本：%+v", by[LedgerExploration])
	}
	if by[LedgerCommitment].Starts != 0 || by[LedgerHook].Starts != 0 || by[LedgerManual].Starts != 0 {
		t.Fatalf("其他来源被一起记了：%+v", by)
	}
	s.SettleLedger(LedgerManual, "r2", 50, day, loc)
	s.SettleLedger(LedgerHook, "r3", 30, day, loc)
	by = map[string]LedgerUsage{}
	for _, row := range s.LedgerToday(day, loc) {
		by[row.Source] = row
	}
	if by[LedgerManual].Tokens != 50 || by[LedgerHook].Tokens != 30 {
		t.Fatalf("来源没有分开记：%+v", by)
	}
	// 换一天各自记，不互相覆盖
	next := day.AddDate(0, 0, 1)
	s.SettleLedger(LedgerExploration, "r4", 10, next, loc)
	if s.LedgerStarts(LedgerExploration, day, loc) != 1 || s.LedgerStarts(LedgerExploration, next, loc) != 1 {
		t.Fatalf("换天结算不对：%d / %d", s.LedgerStarts(LedgerExploration, day, loc), s.LedgerStarts(LedgerExploration, next, loc))
	}
	// 账本有界：只保留最近 14 天
	for i := 0; i < 20; i++ {
		s.SettleLedger(LedgerExploration, "", 1, day.AddDate(0, 0, i), loc)
	}
	days := map[string]bool{}
	for _, row := range s.Ledger {
		days[row.Day] = true
	}
	if len(days) > maxLedgerDays {
		t.Fatalf("账本没有裁剪：%d 天", len(days))
	}
	// 旧 life.json 的次数只导一次
	fresh := New()
	fresh.SeedLedger(LedgerExploration, 3, 300, day, loc)
	fresh.SeedLedger(LedgerExploration, 3, 300, day, loc)
	if row := fresh.LedgerToday(day, loc)[0]; row.Starts != 3 || row.Tokens != 300 {
		t.Fatalf("迁移导重了：%+v", row)
	}
}
