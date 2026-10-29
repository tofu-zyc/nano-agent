package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

func TestLifeExecutionToolsAndCreatorScope(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	startDesktopHelper(t, e)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	work := filepath.Join(e.dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	e.say("/dir add " + work)
	e.ch.wait(t, "挂上了")
	e.say("/mode ask")
	e.ch.wait(t, "已切换")
	e.k.send(proto.Msg{T: proto.TTools, Tools: []proto.ToolSpec{
		{Name: "Read"}, {Name: "Write"}, {Name: "Shell"},
	}})
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Tools) == 3 })
	item := addAgendaCall(t, e, `{"action":"add","title":"写文件并检查","when":"15:10","criteria":"文件内容正确"}`)
	if !item.ScopeSet || item.Mode != proto.ModeAsk || len(item.Workspaces) != 1 {
		t.Fatalf("没有保留创建者范围: %+v", item)
	}
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	if in.Mode != proto.ModeAsk || !in.Elevate || len(in.Workspaces) != 1 || in.Workspaces[0].Dirs[0] != work {
		t.Fatalf("自主执行没有沿用目录/审批模式: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	req := e.llm.lastReq()
	for _, name := range []string{"Read", "Write", "Shell", "Agenda", "Mind", "helper__get_window_state"} {
		if !hasTool(req.Tools, name) {
			t.Errorf("自主执行缺少 %s: %v", name, toolNames(req.Tools))
		}
	}
	if !strings.Contains(req.Messages[0].Content, "桌面操作") || !strings.Contains(lastUser(req.Messages), work) {
		t.Fatal("缺少桌面指引或真实执行环境说明")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
}

func TestAgendaEvidenceAndInterruptedCommit(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	item := addAgendaCall(t, e, `{"action":"add","title":"写出结果","when":"+10m"}`)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	s := e.a.sessOf(mainSID)
	r := &run{from: "fake:u1", callArgs: map[string]proto.Call{
		"plan": {Name: "Agenda"}, "bad": {Name: "Shell"}, "good": {Name: "Read"}, "unknown-effect": {Name: "desktop__type_text"},
	}}
	r.unverifiedTools = map[string]bool{"unknown-effect": true}
	s.run = r
	defer func() { s.run = nil }()
	for _, x := range []struct {
		id string
		ok bool
	}{{"plan", true}, {"bad", false}, {"good", true}, {"unknown-effect", true}} {
		e.a.recordToolEvidence(r, &proto.Msg{ID: x.id, OK: proto.Bool(x.ok), Content: "真实结果"})
	}
	for _, ids := range [][]string{nil, {"invented"}, {"plan"}, {"bad"}, {"unknown-effect"}} {
		if err := e.a.stageAgendaCompletion(mainSID, item.ID, "做完了", ids); err == nil {
			t.Fatalf("不该接受证据 %v", ids)
		}
	}
	if err := e.a.stageAgendaCompletion(mainSID, item.ID, "读取到正确的内容", []string{"good"}); err != nil {
		t.Fatal(err)
	}
	listing, err := e.a.agendaCall(mainSID, []byte(`{"action":"list"}`))
	if err != nil || !strings.Contains(listing, `"id":"good"`) || strings.Contains(listing, `"id":"bad"`) || strings.Contains(listing, `"id":"unknown-effect"`) || strings.Contains(listing, `"id":"plan"`) {
		t.Fatalf("查询凭据应只返回本轮已确认的真实操作: %s, %v", listing, err)
	}
	e.a.commitAgendaResults(s, r, proto.StatusCancelled)
	st := e.a.mind("default")
	if got, _ := st.AgendaByID(item.ID); got.Status == autonomy.StatusDone {
		t.Fatal("中断不能提交完成")
	}
	e.a.commitAgendaResults(s, r, proto.StatusOK)
	if got, _ := st.AgendaByID(item.ID); got.Status != autonomy.StatusDone {
		t.Fatalf("真实成功证据应可验收: %+v", got)
	}
}

func TestAgendaReceiptsReachModelWithoutChangingHistory(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(fmt.Sprint(verified), func(t *testing.T) {
			e := setup(t)
			store, _ := withAgents(t, e)
			makeLife(t, e, store, nil)
			e.say(`!call echo {"a":1}`)
			e.k.expect(proto.TInput)
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
			turn := e.k.expect(proto.TTurn)
			id := turn.Calls[0].ID
			if !verified {
				e.a.mu.Lock()
				e.a.sessions[e.sid].run.unverifiedTools = map[string]bool{id: true}
				e.a.mu.Unlock()
			}
			e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, ID: id, OK: proto.Bool(true), Content: "真实应用输出"})
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
			e.k.expect(proto.TTurn)
			messages := e.llm.lastReq().Messages
			last := messages[len(messages)-1]
			if strings.Contains(last.Content, "[本轮验收凭据]") != verified || (verified && !strings.Contains(last.Content, `"id":"`+id+`"`)) {
				t.Fatalf("模型无法准确复制有效凭据，或拿到了未验证凭据: %+v", last)
			}
			history := e.a.store.History(e.sid)
			found := false
			for _, message := range history {
				if message.Role == "tool" && message.ToolCallID == id {
					found = true
					if message.Content != "真实应用输出" {
						t.Fatalf("请求视图修改了原始输出: %+v", message)
					}
				}
			}
			if !found {
				t.Fatal("没有保存原始工具结果")
			}
			e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
			waitIdle(t, e, e.sid)
		})
	}
}

func TestReceiptsDoNotReusePreviousTurnOrDuplicateIDs(t *testing.T) {
	r := &run{toolEvidence: map[string]autonomy.ToolEvidence{"same": {ID: "same", Name: "Read", OK: true, Verified: true}}}
	history := []session.Msg{{Role: "tool", ToolCallID: "same", Content: "上一轮"}, {Role: "user", Content: "新一轮"},
		{Role: "tool", ToolCallID: "same", Content: "本轮旧结果"}, {Role: "tool", ToolCallID: "same", Content: "本轮最新结果"}}
	view := evidenceHistory(r, history)
	for i := 0; i < 3; i++ {
		if view[i].Content != history[i].Content {
			t.Fatalf("旧结果不应带本次凭据: %v", view)
		}
	}
	if !strings.Contains(view[3].Content, "[本轮验收凭据]") {
		t.Fatal("最新真实结果应可引用")
	}
}

func TestGuestCannotCreateExecutableAgenda(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.a.mu.Lock()
	s := e.a.sessOf("fake:guest")
	s.run = &run{from: "fake:guest", noTools: true}
	_, tools, _, _ := e.a.contextParts(s.sid)
	for _, name := range []string{"Agenda", "Idea", "Mind"} {
		if hasTool(tools, name) {
			t.Errorf("访客不该拿到 %s", name)
		}
	}
	e.a.execTool("test", s, &proto.Msg{Name: "Agenda", ID: "guest", Args: []byte(`{"action":"add"}`)})
	s.run = nil
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK {
		t.Fatalf("访客伪造调用没有被拒绝: %+v", res)
	}
}

func TestExternalEventAdvancesCheckWithoutDuplicate(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	now := e.a.now()
	st := e.a.mind("default")
	st.SetNextCheck(now.Add(time.Hour))
	e.a.mindExternal("default", "schedule:demo", "发现新的资料")
	e.a.mindExternal("default", "schedule:demo", "发现新的资料")
	if len(st.Events) != 1 || st.NextCheck > now.Add(time.Minute).Unix() {
		t.Fatalf("外部结果没有合并并提前检查: %+v", st)
	}
}

func TestMCPExplicitUnverifiedEffectIsNotEvidence(t *testing.T) {
	for _, x := range []struct {
		value any
		want  bool
	}{
		{nil, true}, {[]string{"有效的查询结果"}, true},
		{map[string]any{"verified": false}, false},
		{map[string]any{"success": false}, false},
		{map[string]any{"effect": "unverifiable"}, false},
		{map[string]any{"status": "backgrounded", "ok": true}, false},
		{map[string]any{"status": "queued"}, false},
		{map[string]any{"status": "unknown"}, false},
		{map[string]any{"status": "cancelled"}, false},
		{map[string]any{"status": "succeeded"}, true},
		{map[string]any{"verified": true, "effect": "applied"}, true},
	} {
		if got := mcpOutcomeVerified(x.value); got != x.want {
			t.Errorf("%v: got %v want %v", x.value, got, x.want)
		}
	}
}

func TestAutonomyApprovalReturnsToCreator(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.say("先接到主会话")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.say("/mode ask")
	e.ch.wait(t, "已切换")
	addAgendaCall(t, e, `{"action":"add","title":"执行需要批准的工作","when":"15:10","kind":"commitment"}`)
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: in.SID, ID: "write", Name: "Write", Args: []byte(`{"path":"a.txt","content":"a"}`)})
	e.ch.wait(t, "需要审批")
	aps := e.a.Approvals()
	if len(aps) != 1 {
		t.Fatalf("审批数: %v", aps)
	}
	e.say("/approve " + fmt.Sprint(aps[0].N))
	approved := e.k.expect(proto.TApprove)
	if approved.OK == nil || !*approved.OK {
		t.Fatalf("无法在原管理员窗口审批自主任务: %+v", approved)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusCancelled})
	waitIdle(t, e, in.SID)
}

func TestAutonomyDoesNotFallbackToHomeForDeletedWorkspace(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	now := e.a.now()
	st := e.a.mind("default")
	item, err := st.AddAgenda(autonomy.AgendaItem{Title: "在原目录执行", At: now.Unix(), Kind: autonomy.KindCommitment,
		ScopeSet: true, Workspaces: []string{"deleted"}, Mode: proto.ModeAuto}, now)
	if err != nil {
		t.Fatal(err)
	}
	ag, _ := e.a.agentByID("default")
	err = e.a.startMindLocked(ag, st, autonomy.Trigger{AgendaID: item.ID, Kind: autonomy.KindCommitment}, false)
	if err == nil || !strings.Contains(err.Error(), "已删除") {
		t.Fatalf("不应自动转到 HOME: %v", err)
	}
	if s := e.a.sessions[lifeSID("default")]; s != nil && s.run != nil {
		t.Fatal("删除目录后仍启动了任务")
	}
}

func TestBackgroundReceiptNeverBecomesCompletionEvidence(t *testing.T) {
	for _, status := range []string{"backgrounded", "queued", "running", "unknown", "succeeded"} {
		t.Run(status, func(t *testing.T) {
			r := &run{turnID: 1, callArgs: map[string]proto.Call{"c": {ID: "c", Name: "Shell"}}}
			new(App).recordToolEvidence(r, &proto.Msg{ID: "c", TurnID: 1, OK: proto.Bool(true), Content: `{"status":"` + status + `","job_id":"j_test"}`})
			evidence := r.toolEvidence["c"]
			if evidence.Verified != (status == "succeeded") {
				t.Fatal(evidence)
			}
			if status != "succeeded" && !strings.Contains(r.experienceActions[0], "尚无完成凭据") {
				t.Fatal(r.experienceActions)
			}
		})
	}
}
