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
