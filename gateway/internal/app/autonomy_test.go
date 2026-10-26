package app

// 自主活动的集成测试（模拟时间 + 假模型 + 假内核，走真实的模型请求和工具执行链）。

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/proto"
)

func addAgendaCall(t *testing.T, e *env, args string) autonomy.AgendaItem {
	t.Helper()
	e.a.mu.Lock()
	out, err := e.a.agendaCall(mainSID, []byte(args))
	var item autonomy.AgendaItem
	if err == nil {
		if open := e.a.mindOf("default").OpenAgenda(); len(open) > 0 {
			item = open[len(open)-1]
		}
	}
	e.a.mu.Unlock()
	if err != nil {
		t.Fatalf("agendaCall: %v", err)
	}
	if item.ID == "" {
		t.Fatalf("没有排上日程：%q", out)
	}
	return item
}

// 到点的约定：刚刚有人找过也要做（不再要求空闲 20 分钟），真的调了工具才算完成。
func TestAutonomyDueCommitmentUsesRealToolResult(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)

	// 刚刚有人找过它
	e.say("十分钟后提醒我吃药")
	e.k.expect(proto.TInput)
	e.roundTrip(t)

	// 约定 15:10；note 里带 !call 是让 mock 模型真的发起一次 echo 工具调用
	item := addAgendaCall(t, e, `{"action":"add","title":"提醒主人吃药","kind":"commitment","when":"15:10","note":"!call echo {}"}`)
	if item.Kind != autonomy.KindCommitment {
		t.Fatalf("应该是约定: %+v", item)
	}
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	sid := lifeSID("default")
	if in.SID != sid || in.MaxSteps == 12 {
		t.Fatalf("到点的约定应该在内心会话里跑一轮: %+v", in)
	}
	e.sid = sid
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonToolUse || len(turn.Calls) != 1 {
		t.Fatalf("应该真的去调工具: %+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: sid, ID: turn.Calls[0].ID, OK: proto.Bool(true), Content: "闹钟响了"})
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.a.mu.Lock()
	_, doneErr := e.a.agendaCall(sid, []byte(`{"action":"done","id":"`+item.ID+`","note":"查到了：闹钟响了","evidence":["`+turn.Calls[0].ID+`"]}`))
	e.a.mu.Unlock()
	if doneErr != nil {
		t.Fatal(doneErr)
	}
	req := e.llm.lastReq()
	if !strings.Contains(req.Messages[0].Content, "有自己生活的人") {
		t.Fatalf("自主活动应该用拟人型的人格: %q", req.Messages[0].Content)
	}
	if !strings.Contains(lastUser(req.Messages), "提醒主人吃药") || !strings.Contains(lastUser(req.Messages), "自主活动") {
		t.Fatalf("醒来时应该看到自己的日程: %q", lastUser(req.Messages))
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)

	e.a.mu.Lock()
	st := e.a.mindOf("default")
	ag, ok := st.AgendaByID(item.ID)
	execs := st.Executions
	e.a.mu.Unlock()
	if !ok || ag.Status != autonomy.StatusDone || !strings.Contains(ag.Result, "闹钟响了") {
		t.Fatalf("有真实工具结果才算完成: %+v", ag)
	}
	if len(execs) == 0 || !execs[len(execs)-1].OK || !execs[len(execs)-1].UsedTools {
		t.Fatalf("执行记录应该记着真的用了工具: %+v", execs)
	}
	// 完成后安排了下一次自己看
	e.a.mu.Lock()
	next := st.NextCheck
	e.a.mu.Unlock()
	if next <= base.Add(11*time.Minute).Unix() {
		t.Fatalf("做完以后应该安排下一次检查: %d", next)
	}
}

// 只写了一段计划、没调工具：不能把事标成完成，要往后挪。
func TestAutonomyNoFakeCompletionWithoutTools(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	item := addAgendaCall(t, e, `{"action":"add","title":"查一下资料","kind":"plan","when":"15:10"}`)
	e.llm.say("我打算等会儿再查")
	defer e.llm.say("")
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	sid := in.SID
	e.sid = sid
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)

	e.a.mu.Lock()
	st := e.a.mindOf("default")
	ag, _ := st.AgendaByID(item.ID)
	execs := st.Executions
	next := st.NextCheck
	e.a.mu.Unlock()
	if ag.Status != autonomy.StatusPlanned || ag.At <= base.Add(11*time.Minute).Unix() {
		t.Fatalf("没真正执行不能算完成，应该往后挪: %+v", ag)
	}
	if len(execs) == 0 || execs[len(execs)-1].UsedTools || !execs[len(execs)-1].OK {
		t.Fatalf("执行记录应该是“没调工具”: %+v", execs)
	}
	if next == 0 {
		t.Fatal("没做成也要安排下次")
	}
}

func TestAutonomyPauseStopsAndResumeContinues(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	addAgendaCall(t, e, `{"action":"add","title":"去查一件事","kind":"commitment","when":"15:10"}`)

	if _, err := e.a.AutonomyAction("default", AutonomyActionReq{Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	e.k.none()
	if _, err := e.a.AutonomyAction("default", AutonomyActionReq{Action: "resume"}); err != nil {
		t.Fatal(err)
	}
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	sid := in.SID
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

func TestAutonomyBudgetBlocksAndRecovers(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	budget := 100
	makeLife(t, e, store, func(ag *agents.Agent) { ag.LifeBudget = &budget })
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	addAgendaCall(t, e, `{"action":"add","title":"用掉今天预算","kind":"commitment","when":"15:10"}`)
	e.a.mu.Lock()
	e.a.mindOf("default").Spend(100)
	e.a.mu.Unlock()
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	e.k.none()
	e.a.mu.Lock()
	st := e.a.mindOf("default")
	blocked := st.NextCheck
	e.a.mu.Unlock()
	if blocked <= base.Add(11*time.Minute).Unix() {
		t.Fatal("预算用完以后应该安排下次再看")
	}
	// 提高上限以后，到点还能接着做
	e.a.mu.Lock()
	ag, _ := store.Get("default")
	more := 10000
	ag.LifeBudget = &more
	if err := store.Update(ag); err != nil {
		t.Fatal(err)
	}
	e.a.mindOf("default").SetNextCheck(time.Unix(blocked, 0))
	e.a.mu.Unlock()
	setNow(e, time.Unix(blocked, 0).Add(time.Second))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	sid := in.SID
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

// 进程重启：上次没结束的执行不能重放，要改成等重新确认。
func TestAutonomyRestartRecovery(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, now)

	e.a.mu.Lock()
	st := e.a.mindOf("default")
	item, err := st.AddAgenda(autonomy.AgendaItem{Title: "发邮件", Kind: autonomy.KindCommitment, At: now.Unix()}, now)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.AddIntention(autonomy.Intention{Title: "把邮件发出去", Priority: 5}, now)
	if err != nil {
		t.Fatal(err)
	}
	item.IntentionID = intent.ID
	st.MarkStarted(autonomy.Trigger{Kind: autonomy.KindCommitment, AgendaID: item.ID, IntentionID: intent.ID}, now)
	e.a.saveMinds()
	// 模拟重启：内存里的状态丢掉，从磁盘重读（恢复每个进程启动做一次）
	e.a.minds = map[string]*autonomy.State{}
	e.a.mindsLoaded = false
	e.a.mindsRecovered = map[string]bool{}
	st2 := e.a.mindOf("default")
	e.a.mu.Unlock()

	ag, ok := st2.AgendaByID(item.ID)
	if !ok || ag.Status != autonomy.StatusPlanned || ag.At != now.Add(5*time.Minute).Unix() {
		t.Fatalf("重启后应该重新确认而不是重放: %+v", ag)
	}
	in, _ := st2.IntentionByID(intent.ID)
	if in.State != autonomy.IntentionWaiting || !strings.Contains(in.Blocked, "结果未知") {
		t.Fatalf("意图应该等重新确认: %+v", in)
	}
}

func TestAutonomyChatEventsMergeAndDoNotWakeEachMessage(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.a.mu.Lock()
	e.a.mindHeard(mainSID, "fake:u1", "张三", "你好")
	e.a.mindHeard(mainSID, "fake:u1", "张三", "在吗")
	st := e.a.mindOf("default")
	n := len(st.UnprocessedEvents(10))
	next := st.NextCheck
	e.a.mu.Unlock()
	if n != 1 {
		t.Fatalf("同一个会话的高频消息应该合并成一条事件: %d", n)
	}
	if next != base.Add(10*time.Minute).Unix() {
		t.Fatalf("聊天事件应该把下一次检查放到 10 分钟后: %v", time.Unix(next, 0))
	}
	// 合并窗口里不会每条消息都调用模型
	setNow(e, base.Add(2*time.Minute))
	e.a.autonomyTick()
	e.k.none()
}

func TestAutonomyIdeaNeedsSourceAndSchedules(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC))

	e.a.mu.Lock()
	if _, err := e.a.ideaCall(mainSID, []byte(`{"action":"add","text":"比较两种解释"}`)); err == nil {
		e.a.mu.Unlock()
		t.Fatal("没有来源的想法不该收")
	}
	if _, err := e.a.ideaCall(mainSID, []byte(`{"action":"add","text":"比较两种解释","source":"刚和主人聊到 transformer","interest":4}`)); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	ideas := e.a.mindOf("default").OpenIdeas(e.a.now())
	if len(ideas) != 1 {
		e.a.mu.Unlock()
		t.Fatalf("想法没有记下来: %+v", ideas)
	}
	id := ideas[0].ID
	if _, err := e.a.ideaCall(mainSID, []byte(`{"action":"schedule","id":"`+id+`","when":"16:00","cost_minutes":20}`)); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	open := e.a.mindOf("default").OpenAgenda()
	e.a.mu.Unlock()
	if len(open) != 1 || open[0].Kind != autonomy.KindExploration || !open[0].Movable {
		t.Fatalf("想法应该变成可以挪动的探索时间: %+v", open)
	}
}

func TestMindToolUpdatesInterestsBeliefsAndReflections(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC))

	e.a.mu.Lock()
	out, err := e.a.mindCall(mainSID, []byte(`{"action":"view"}`))
	if err != nil || !strings.Contains(out, "长期兴趣：还没有") {
		e.a.mu.Unlock()
		t.Fatalf("初始心智状态不对: %q %v", out, err)
	}
	if _, err := e.a.mindCall(mainSID, []byte(`{"action":"interest_add","text":"读科幻"}`)); err == nil {
		e.a.mu.Unlock()
		t.Fatal("没有来源的兴趣不该收")
	}
	if _, err := e.a.mindCall(mainSID, []byte(`{"action":"interest_add","text":"读科幻","source":"看完三体之后想到的"}`)); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	if _, err := e.a.mindCall(mainSID, []byte(`{"action":"belief","subject":"主人的面试","value":"下周三二面","source":"主人刚才说的","confidence":0.9}`)); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	if _, err := e.a.mindCall(mainSID, []byte(`{"action":"reflect","text":"约定要当天记下来，不然容易忘","change":"以后先 Agenda add"}`)); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	out, err = e.a.mindCall(mainSID, []byte(`{"action":"view"}`))
	st := e.a.mindOf("default")
	view := st.View(e.a.now(), e.a.loc, 10)
	e.a.mu.Unlock()
	if err != nil || !strings.Contains(out, "读科幻") || !strings.Contains(out, "下周三二面") || !strings.Contains(out, "容易忘") {
		t.Fatalf("心智状态没有记住: %q %v", out, err)
	}
	if len(view.Self.Interests) != 1 || len(view.Beliefs) == 0 || len(view.Reflections) == 0 {
		t.Fatalf("View 里缺兴趣/认知/反思: %+v", view)
	}
}
