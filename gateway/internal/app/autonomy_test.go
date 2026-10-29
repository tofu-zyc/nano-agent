package app

// 自主活动的集成测试（模拟时间 + 假模型 + 假内核，走真实的模型请求和工具执行链）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
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

func TestAutonomyChatDoesNotCreateBeliefsEventsOrWake(t *testing.T) {
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
	if n != 0 || len(st.Beliefs) != 0 {
		t.Fatalf("聊天不应产生心智待办或认识: %d / %+v", n, st.Beliefs)
	}
	if next != 0 {
		t.Fatalf("聊天不应安排检查: %v", time.Unix(next, 0))
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

func TestAutonomySetNumberFromWebAndCommand(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC))
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	st := e.a.mind("default")
	now := e.a.now()
	idea, err := st.AddIdea(autonomy.Idea{Text: "半夜整理日志", Interest: 1, Novelty: 1, CostMinutes: 15, Source: "测试"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	intention, err := st.AddIntention(autonomy.Intention{Title: "把文档补完", Priority: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	// 网页 / API
	view, err := e.a.AutonomyAction("default", AutonomyActionReq{Action: "set_number", ID: idea.ID, Field: "interest", Value: 5})
	if err != nil || view.Ideas[0].Interest != 5 {
		t.Fatal(view, err)
	}
	if _, err := e.a.AutonomyAction("default", AutonomyActionReq{Action: "set_number", ID: idea.ID, Field: "interest", Value: 9}); err == nil {
		t.Fatal("越界数值没有被拒绝")
	}
	// 聊天命令
	e.say("/autonomy set " + intentID(intention) + " priority=4")
	e.ch.wait(t, "的 priority 改成 4")
	e.a.mu.Lock()
	priority := 0
	for _, in := range e.a.mind("default").ActiveIntentions() {
		if in.ID == intentID(intention) {
			priority = in.Priority
		}
	}
	e.a.mu.Unlock()
	if priority != 4 {
		t.Fatalf("命令没有改上优先级：%d", priority)
	}
	// 访客不能改
	e.say("/autonomy set "+idea.ID+" interest=1", from("u2", "李四"))
	e.ch.wait(t, "只有管理员")
	e.a.mu.Lock()
	interest := e.a.mind("default").OpenIdeas(e.a.now())[0].Interest
	e.a.mu.Unlock()
	if interest != 5 {
		t.Fatalf("访客改动了数值：%d", interest)
	}
}

func intentID(in *autonomy.Intention) string { return in.ID }

func TestTopicBranchIsReusedForTheSameTopic(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC))
	e.a.mu.Lock()
	st := e.a.mind("default")
	intention, err := st.AddIntention(autonomy.Intention{Title: "研究日志格式", Priority: 3}, e.a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "看看日志格式"}); err != nil {
		t.Fatal(err)
	}
	s := e.a.sessOf(lifeSID("default"))
	s.run = &run{runID: "r-life", from: "life:default", inner: true}
	s.mind = &mindRun{AgentID: "default", Trigger: autonomy.Trigger{Kind: autonomy.EventSelfCheck, IntentionID: intention.ID}}
	ag := e.a.agentOf(s.sid)
	e.a.mu.Unlock()
	fork := func(request string) string {
		t.Helper()
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		text, ok := e.a.handleSessionTool(s, &proto.Msg{Args: json.RawMessage(`{"action":"fork","source_sid":"` + mainSID + `","title":"研究日志","request_id":"` + request + `"}`)})
		if !ok {
			t.Fatalf("fork 失败：%s", text)
		}
		return text
	}
	fork("topic-1")
	e.a.mu.Lock()
	branch, ok := st.BranchOfTopic("in:" + intention.ID)
	e.a.mu.Unlock()
	if !ok || branch == "" {
		t.Fatal("分叉没有记到话题上")
	}
	if children := e.a.store.Children(mainSID); len(children) != 1 {
		t.Fatalf("第一次分叉数：%d", len(children))
	}
	// 同一话题再来：复用已有分支，不再新建
	again := fork("topic-2")
	if !strings.Contains(again, branch) || !strings.Contains(again, "已经有分支") {
		t.Fatalf("没有复用已有分支：%s", again)
	}
	if children := e.a.store.Children(mainSID); len(children) != 1 {
		t.Fatalf("同一话题又建了一个分支：%d", len(children))
	}
	// 下一次醒来在说明里看得到这个分支
	e.a.mu.Lock()
	note := e.a.mindNote(ag, st, s.mind.Trigger, e.a.now(), false)
	e.a.mu.Unlock()
	if !strings.Contains(note, branch) {
		t.Fatalf("内心说明没有提到已有分支：%s", note)
	}
}

func TestBranchRunTokensCountIntoTheActivityBudget(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC))
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "种子"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "预算", RequestID: "budget-1", By: "fake:u1"})
	if err != nil || res.BranchSID == "" {
		t.Fatal(res, err)
	}
	e.a.mu.Lock()
	before := e.a.mind("default").Budget.Tokens
	e.a.branchDone(&sess{sid: res.BranchSID}, &run{usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20}})
	after := e.a.mind("default").Budget.Tokens
	e.a.mu.Unlock()
	if after-before != 120 {
		t.Fatalf("分支 token 没有计进自主预算：%d → %d", before, after)
	}
}

func TestAutonomousBranchLimitsAndDepth(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC))
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "种子"}); err != nil {
		t.Fatal(err)
	}
	// 这三层都是 agent 自己分出来的：只有自己创建的分支它才能继续
	first, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, CallerSID: lifeSID("default"), Title: "第一层", RequestID: "depth-1", By: "life:default"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.a.SessionFork(SessionForkRequest{SourceSID: first.BranchSID, CallerSID: first.BranchSID, Title: "第二层", RequestID: "depth-2", By: "life:default"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := e.a.SessionFork(SessionForkRequest{SourceSID: second.BranchSID, CallerSID: second.BranchSID, Title: "第三层", RequestID: "depth-3", By: "life:default"})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	depth := e.a.branchDepth(third.BranchSID)
	e.a.mu.Unlock()
	if depth != 3 {
		t.Fatalf("分支层数：%d", depth)
	}
	// 已经三层，agent 自己再分一层就被拒绝
	e.a.mu.Lock()
	_, err = e.a.sessionForkLocked(SessionForkRequest{SourceSID: third.BranchSID, CallerSID: third.BranchSID, Title: "第四层", RequestID: "depth-4", By: "life:default"})
	e.a.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "最多 3 层") {
		t.Fatalf("自动分叉深度没有被拦住：%v", err)
	}
	// 已经有一个自主分支在跑：agent 自己再开一个被拒绝，主人手动继续不受限
	e.a.mu.Lock()
	e.a.sessOf(first.BranchSID).run = &run{runID: "r-busy", from: "life:default", inner: true}
	_, err = e.a.sessionBranchActionLocked("continue", second.BranchSID, "接着做", "life:default")
	e.a.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("自主分支并发没有被拦住：%v", err)
	}
	e.k.none()
	if _, err := e.a.SessionBranchAction("continue", second.BranchSID, "主人让你接着做", "fake:u1"); err != nil {
		t.Fatalf("用户手动继续被拦了：%v", err)
	}
}

func TestBranchReportComesBackAsASourcedMessageOnly(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	setNow(e, time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "帮我看看日志格式"}); err != nil {
		t.Fatal(err)
	}
	if err := e.a.store.Append(mainSID, session.Msg{Role: "assistant", Content: "我先记下这件事"}); err != nil {
		t.Fatal(err)
	}
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "日志格式", RequestID: "report-1", By: "fake:u1"})
	if err != nil || branch.BranchSID == "" {
		t.Fatal(branch, err)
	}
	// 分支里有一句结论
	if err := e.a.store.Append(branch.BranchSID, session.Msg{Role: "assistant", Content: "结论：日志按天轮转，保留 7 天。"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	before := len(e.a.store.History(mainSID))
	e.a.mu.Unlock()
	e.say("/branch report " + branch.BranchSID)
	e.ch.wait(t, "下一个安全边界")
	// 父会话历史只多了这条报告消息，分支历史没有被合并进来
	e.a.mu.Lock()
	after := e.a.store.History(mainSID)
	e.a.mu.Unlock()
	if len(after) != before+1 {
		t.Fatalf("父会话历史条数：%d → %d", before, len(after))
	}
	report := after[len(after)-1]
	for _, want := range []string{"[分支报告 " + branch.BranchSID, "来源会话：" + mainSID, "分叉点：第 ", "结论：日志按天轮转", "没有合并"} {
		if !strings.Contains(report.Content, want) {
			t.Fatalf("报告缺少 %q：%s", want, report.Content)
		}
	}
	if strings.Contains(report.Content, "我先记下这件事") {
		t.Fatal("分支继承的历史被合并进来了")
	}
	// 同一个结论不重复带回
	e.say("/branch report " + branch.BranchSID)
	e.ch.wait(t, "已经带回来过")
	e.a.mu.Lock()
	again := len(e.a.store.History(mainSID))
	e.a.mu.Unlock()
	if again != len(after) {
		t.Fatalf("重复带回了：%d → %d", len(after), again)
	}
}

func TestActivityLedgerSplitsExplorationAndManual(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.a.lifeTick()
	e.k.none()
	setNow(e, base.Add(21*time.Minute))
	e.a.lifeTick()
	e.k.expect(proto.TInput)
	sid := lifeSID("default")
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	if err := e.a.Wake("default"); err != nil {
		t.Fatal(err)
	}
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	e.a.mu.Lock()
	rows := e.a.mind("default").LedgerToday(e.a.now(), e.a.loc)
	e.a.mu.Unlock()
	by := map[string]autonomy.LedgerUsage{}
	for _, row := range rows {
		by[row.Source] = row
	}
	if by[autonomy.LedgerExploration].Starts != 1 {
		t.Fatalf("探索没有记进账本：%+v", rows)
	}
	if by[autonomy.LedgerManual].Starts != 1 {
		t.Fatalf("手动叫醒没有记进账本：%+v", rows)
	}
	if by[autonomy.LedgerManual].Reserved != 0 || by[autonomy.LedgerExploration].Reserved != 0 {
		t.Fatalf("结算之后还留着预留：%+v", rows)
	}
	if by[autonomy.LedgerCommitment].Starts != 0 || by[autonomy.LedgerHook].Starts != 0 {
		t.Fatalf("别的来源被一起记了：%+v", rows)
	}
	// /autonomy 上分别显示每个来源，不再只给一个总数
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	e.say("/autonomy")
	e.ch.wait(t, "约定 0 · Hook 0 · 手动 1")
}
