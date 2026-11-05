package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/session"
)

// Report：谁有它、汇报的是哪件事、四种拒绝、写不进记录、重启后还在。

func reportArgs(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

// lifeRunOnAgenda 让 life 会话为一个到点的约定跑起来，返回这一轮的会话和约定的 ID。
func lifeRunOnAgenda(t *testing.T, e *env) (*sess, string) {
	t.Helper()
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	item := addAgendaCall(t, e, `{"action":"add","title":"整理文档","kind":"commitment","when":"15:10"}`)
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	if in.SID != lifeSID("default") {
		t.Fatalf("应该在 life 会话里跑：%s", in.SID)
	}
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.sessions[in.SID], item.ID
}

// finishRun 把 life 会话这一轮走完，别让它在测试结束后还在写数据目录。
func finishRun(t *testing.T, e *env, s *sess) {
	t.Helper()
	sid := s.sid
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

func call(e *env, s *sess, args map[string]any) (string, error) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.reportCall(s, reportArgs(args))
}

func TestReportOfferedOnlyToWorkingSessions(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	task := schedule.Task{ID: "t1"}
	for sid, want := range map[string]bool{
		mainSID:                           false, // 普通聊天：在原对话里直接回复就是汇报
		"fake:c1":                         false,
		lifeSID("default"):                true,
		cronSID(task):                     true,
		hookSID("default", "d1"):          true,
		e.a.roomMemberSID("default", "r"): false, // 群聊
		dreamSID("default"):               false,
	} {
		if _, ok := e.a.reportSpec(sid); ok != want {
			t.Errorf("%s：Report 提供=%v，应为 %v", sid, ok, want)
		}
	}
	// 真的进了这几类会话的工具表（走 contextParts）
	for _, sid := range []string{lifeSID("default"), cronSID(task), hookSID("default", "d1")} {
		_, tools, _, _ := e.a.contextParts(sid)
		found := false
		for _, tl := range tools {
			found = found || tl.Function.Name == "Report"
		}
		if !found {
			t.Errorf("%s 的工具表里没有 Report", sid)
		}
	}
	_, tools, _, _ := e.a.contextParts(mainSID)
	for _, tl := range tools {
		if tl.Function.Name == "Report" {
			t.Error("主会话不该有 Report")
		}
	}
}

func TestReportSubjectIsDerivedFromTheRun(t *testing.T) {
	e := setup(t)
	s, agendaID := lifeRunOnAgenda(t, e)
	out, err := call(e, s, map[string]any{"status": "progress", "text": "开始了"})
	if err != nil || !strings.Contains(out, "不会发给任何人") {
		t.Fatalf("progress：%q %v", out, err)
	}
	e.a.mu.Lock()
	r, ok := e.a.commits.Latest(commit.KindAgenda, agendaID)
	e.a.mu.Unlock()
	if !ok || r.Status != "progress" || r.Agent != "default" || r.Run != s.run.runID || r.Text != "开始了" {
		t.Fatalf("记在这件约定上：%+v", r)
	}
	// 汇报进了承诺列表
	if v := findCommit(e.a.Commitments("default", ""), commit.KindAgenda, agendaID); v == nil || v.Report == nil || v.Report.Text != "开始了" {
		t.Fatalf("列表里：%+v", v)
	}
	// 意图：没有议程项、有意图的那一轮
	e.a.mu.Lock()
	s.mind.Trigger.AgendaID, s.mind.Trigger.IntentionID = "", "in-9"
	_, err = e.a.reportCall(s, reportArgs(map[string]any{"status": "waiting", "text": "在等"}))
	_, got := e.a.commits.Latest(commit.KindIntention, "in-9")
	s.mind.Trigger.AgendaID, s.mind.Trigger.IntentionID = agendaID, ""
	e.a.mu.Unlock()
	if err != nil || !got {
		t.Fatalf("意图：%v %v", got, err)
	}
	// 分支汇报的是分支自己；定时任务汇报这个任务；Hook 汇报这条规则（跑在哪都算 Hook）
	e.a.mu.Lock()
	cron := e.a.sessOf(cronSID(schedule.Task{ID: "t1"}))
	cron.run = &run{runID: "rc"}
	cron.scheduled = &schedule.Task{ID: "t1", Agent: "default"}
	if _, err := e.a.reportCall(cron, reportArgs(map[string]any{"status": "progress", "text": "x"})); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.a.commits.Latest(commit.KindSchedule, "t1"); !ok {
		t.Fatal("定时任务的汇报要记在任务上")
	}
	cron.run, cron.scheduled = nil, nil
	hs := e.a.sessOf(hookSID("default", "d1")) // Hook 的这一轮（不管跑在哪个会话里）记在规则上
	hs.run = &run{runID: "rh", hook: &hookInvocation{work: hooks.Execution{Delivery: eventlog.Delivery{HookID: "hook_1", AgentID: "default"}}}}
	if _, err := e.a.reportCall(hs, reportArgs(map[string]any{"status": "progress", "text": "x"})); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.a.commits.Latest(commit.KindHook, "hook_1"); !ok {
		t.Fatal("Hook 的汇报要记在规则上")
	}
	hs.run = nil
	e.a.mu.Unlock()
	finishRun(t, e, s)
}

func TestReportRejections(t *testing.T) {
	e := setup(t)
	s, agendaID := lifeRunOnAgenda(t, e)
	reject := func(name string, args map[string]any, want string) {
		t.Helper()
		_, err := call(e, s, args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s：应该拒绝并说到 %q，得到 %v", name, want, err)
		}
	}
	// 四种拒绝
	reject("没有凭据的 ready", map[string]any{"status": "ready", "text": "好了"}, "evidence")
	reject("凭据不是本轮成功的工具结果", map[string]any{"status": "ready", "text": "好了", "evidence": []string{"ev-nope"}}, "不是本轮已成功")
	reject("没有 text 的 blocked", map[string]any{"status": "blocked"}, "text")
	reject("没有 text 的 failed", map[string]any{"status": "failed"}, "text")
	reject("没有 text 的 not_needed", map[string]any{"status": "not_needed"}, "text")
	reject("question 配 ready", map[string]any{"status": "ready", "text": "好了", "question": "好吗？", "evidence": []string{"x"}}, "只能和 blocked")
	reject("不认识的 status", map[string]any{"status": "done"}, "status")
	reject("text 太长", map[string]any{"status": "progress", "text": strings.Repeat("字", 501)}, "最多 500")
	reject("question 太长", map[string]any{"status": "blocked", "text": "x", "question": strings.Repeat("字", 201)}, "最多 200")
	e.a.mu.Lock()
	if _, ok := e.a.commits.Latest(commit.KindAgenda, agendaID); ok {
		t.Fatal("被拒绝的汇报不能进记录")
	}
	// 自己安排的检查：不对应任何承诺
	s.mind.Trigger.AgendaID, s.mind.Trigger.IntentionID = "", ""
	e.a.mu.Unlock()
	reject("自己安排的检查", map[string]any{"status": "progress", "text": "x"}, "不对应任何承诺")
	// 真实成功的工具结果可以作凭据；Report 自己不行
	e.a.mu.Lock()
	s.mind.Trigger.AgendaID = agendaID
	s.run.toolEvidence = map[string]autonomy.ToolEvidence{
		"ev-1": {ID: "ev-1", Name: "WebFetch", OK: true, Verified: true},
		"ev-2": {ID: "ev-2", Name: "WebFetch", OK: false, Verified: true},
		"ev-3": {ID: "ev-3", Name: "Report", OK: true, Verified: true},
	}
	e.a.mu.Unlock()
	reject("失败的工具结果", map[string]any{"status": "ready", "text": "好了", "evidence": []string{"ev-2"}}, "不是本轮已成功")
	reject("Report 不能给自己作凭据", map[string]any{"status": "ready", "text": "好了", "evidence": []string{"ev-3"}}, "不是本轮已成功")
	if _, err := call(e, s, map[string]any{"status": "ready", "text": "整理好了", "evidence": []string{"ev-1"}}); err != nil {
		t.Fatal("有凭据的 ready 可以：", err)
	}
	e.a.mu.Lock()
	r, _ := e.a.commits.Latest(commit.KindAgenda, agendaID)
	e.a.mu.Unlock()
	if r.Status != "ready" || len(r.Evidence) != 1 || r.Evidence[0] != "ev-1:WebFetch" {
		t.Fatalf("凭据记下 id 和工具名：%+v", r)
	}
	// 带问题的 blocked 可以问（life 这一轮之后主人可以回答）
	if _, err := call(e, s, map[string]any{"status": "blocked", "text": "两份重复", "question": "留哪一份？"}); err != nil {
		t.Fatal(err)
	}
	// 一轮最多 4 次（上面已经成功了 3 次：ready、blocked 和下一条之前的…数一下）
	for i := s.run.reports; i < 4; i++ {
		if _, err := call(e, s, map[string]any{"status": "progress", "text": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	reject("第 5 次", map[string]any{"status": "progress", "text": "x"}, "已经汇报 4 次")
	finishRun(t, e, s)
}

// Hook 和定时任务这一轮结束后没有人接着问：带 question 的拒绝，只汇报状态。
func TestReportQuestionRejectedWhereNobodyCanAnswer(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	cron := e.a.sessOf(cronSID(schedule.Task{ID: "t1"}))
	cron.run, cron.scheduled = &run{runID: "rc"}, &schedule.Task{ID: "t1", Agent: "default"}
	hook := e.a.sessOf(hookSID("default", "d1"))
	hook.run = &run{runID: "rh", hook: &hookInvocation{work: hooks.Execution{Delivery: eventlog.Delivery{HookID: "hook_1", AgentID: "default"}}}}
	for name, s := range map[string]*sess{"定时任务": cron, "Hook": hook} {
		_, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "blocked", "text": "卡住了", "question": "怎么办？"}))
		if err == nil || !strings.Contains(err.Error(), "不能带 question") {
			t.Errorf("%s：带问题的要拒绝：%v", name, err)
		}
		if _, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "blocked", "text": "卡住了"})); err != nil {
			t.Errorf("%s：只汇报状态可以：%v", name, err)
		}
	}
	cron.run, cron.scheduled, hook.run = nil, nil, nil
}

// 写不进记录：返回错误给模型，健康条目 commitments 变 degraded，记录里没有这一条。
func TestReportWriteFailureReturnsErrorAndDegradesHealth(t *testing.T) {
	e := setup(t)
	s, agendaID := lifeRunOnAgenda(t, e)
	e.a.mu.Lock()
	e.a.commits.Close() // 之后所有追加都失败
	e.a.mu.Unlock()
	_, err := call(e, s, map[string]any{"status": "progress", "text": "开始了"})
	if err == nil || !strings.Contains(err.Error(), "没有记下来") {
		t.Fatalf("要返回错误：%v", err)
	}
	e.a.mu.Lock()
	_, ok := e.a.commits.Latest(commit.KindAgenda, agendaID)
	reports := s.run.reports
	e.a.mu.Unlock()
	if ok || reports != 0 {
		t.Fatalf("没记下来就不算：%v %d", ok, reports)
	}
	degraded := false
	for _, h := range e.a.health.Snapshot() {
		if h.Name == "commitments" && h.State == health.Degraded {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("健康条目 commitments 要变 degraded：%+v", e.a.health.Snapshot())
	}
	finishRun(t, e, s)
}

// 重启后最近一条汇报还在：在同一个数据目录上重新起一个 App，用 Commitments 查到，连 openCommits 的路径和叠加一起测到。
func TestReportSurvivesRestart(t *testing.T) {
	e := setup(t)
	s, agendaID := lifeRunOnAgenda(t, e)
	if _, err := call(e, s, map[string]any{"status": "progress", "text": "第一条"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(e, s, map[string]any{"status": "blocked", "text": "卡住了", "question": "怎么办？"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(e, s, map[string]any{"status": "progress", "text": "又看了一眼"}); err != nil {
		t.Fatal(err)
	}
	finishRun(t, e, s)
	e.a.mu.Lock()
	e.a.commits.Close() // 旧的网关停下，记录留在磁盘上
	e.a.mu.Unlock()
	a2 := importApp(t, e.dir, "", "") // 同一个数据目录上重新起一个 App
	v := findCommit(a2.Commitments("default", ""), commit.KindAgenda, agendaID)
	if v == nil || v.Report == nil || v.Report.Text != "又看了一眼" {
		t.Fatalf("重启后最近一条汇报还在：%+v", v)
	}
	if v.Phase != commit.Blocked || v.Why != "等你回答：怎么办？" || v.Mark != "r:2" {
		t.Fatalf("重启后没处理的问题还在，progress 没顶掉它：%+v", v)
	}
}

// 没用 Report 时消息、投递和存储都不变：不会凭空多出承诺记录文件的内容。
func TestNoReportLeavesStorageUntouched(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	_ = store
	e.say("你好")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if size := e.a.commits.Size(); size != 0 {
		t.Fatalf("没有汇报，记录文件应该是空的：%d 字节", size)
	}
	if _, ok := e.a.sessions[mainSID]; !ok {
		t.Fatal("聊天照常")
	}
	_ = agents.DefaultID
}

// 分支里调一个真实的工具：模型看得到它的凭据，用这个 id Report ready 成功（不是往 toolEvidence 里塞的）。
func TestReportReadyWithRealReceiptInBranch(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "来源会话的上下文"}); err != nil {
		t.Fatal(err)
	}
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "查竞品", RequestID: "report-ready", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.SessionBranchAction("continue", branch.BranchSID, `!call echo {"a":1}`, "fake:u1"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	if in.SID != branch.BranchSID {
		t.Fatalf("这一轮在分支里：%s", in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	id := turn.Calls[0].ID
	e.k.send(proto.Msg{T: proto.TResult, SID: in.SID, ID: id, OK: proto.Bool(true), Content: "竞品有三家"})
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	// 模型这一次的请求里看得到凭据回执（分支没有 Agenda，靠 Report 才给）
	msgs := e.llm.lastReq().Messages
	last := msgs[len(msgs)-1].Content
	m := regexp.MustCompile(`\[本轮验收凭据\] \{"id":"([^"]+)"`).FindStringSubmatch(last)
	if m == nil || m[1] != id {
		t.Fatalf("模型看不到凭据回执：%q", last)
	}
	e.a.mu.Lock()
	s := e.a.sessions[branch.BranchSID]
	out, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "ready", "text": "竞品有三家", "evidence": []string{m[1]}}))
	r, ok := e.a.commits.Latest(commit.KindBranch, branch.BranchSID)
	e.a.mu.Unlock()
	if err != nil || !strings.Contains(out, "ready") || !ok || r.Status != "ready" || len(r.Evidence) != 1 || r.Evidence[0] != id+":echo" {
		t.Fatalf("用这个 id 汇报 ready：%q %v %+v", out, err, r)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
}
