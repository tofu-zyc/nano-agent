package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/ticket"
)

// 团队群里的 Ticket 工具（设计 §16.1）：名单、触发消息、授权记录、去重。

// teamGroup 建一个外部群并设上团队规则，名单里是 fake:u2。返回群会话 ID。
func teamGroup(t *testing.T, e *env, roster ...string) string {
	t.Helper()
	sid := extGroup(t, e)
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(roster) == 0 {
		roster = []string{"fake:u2"}
	}
	setScene(t, e, sid, &scene.Rules{Team: &scene.Team{Roster: roster, Workspace: ws.ID, Check: "make test", Budget: 500}})
	e.a.ticketNoStart = true // 这些测试只看工单和授权记录，不开工（开工有自己的测试）
	return sid
}

// mention 说一句点名的话（群里）。
func mention(e *env, text, id, uid, name string) {
	e.say(text, group(true), msgID(id), from(uid, name))
}

// callTicket 在这一轮里调 Ticket 工具。
func callTicket(e *env, sid string, args string) (string, error) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.ticketCall(e.a.sessions[sid], []byte(args))
}

func finishGroupRun(t *testing.T, e *env, sid string) {
	t.Helper()
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

func TestTicketCreateRecordsAuthorityFromTheTriggerMessage(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 登录页在手机上错位了，帮忙修一下", "g100", "u2", "李四")
	e.k.expect(proto.TInput)
	out, err := callTicket(e, sid, `{"action":"create","title":"修登录页","goal":"手机上错位"}`)
	if err != nil || !strings.Contains(out, "tk-1") || !strings.Contains(out, "确认一次") {
		t.Fatalf("建工单：%q %v", out, err)
	}
	tk, _ := e.a.tickets.Get("tk-1")
	au := tk.Auth
	if au.By != "fake:u2" || au.Scene != sid || au.Source.Channel != "fake" || au.Source.Chat != "c1" || au.Source.MsgID != "g100" ||
		!strings.Contains(au.Source.Text, "登录页在手机上错位了") || au.Workspace == "" || au.Mode == "" || au.At == 0 ||
		tk.Check != "make test" || tk.Budget != 500 || tk.Status != ticket.StatusOpen || tk.Agent != "default" {
		t.Fatalf("授权记录：%+v", tk)
	}
	// 同一条原消息重试：还是这一张
	out, err = callTicket(e, sid, `{"action":"create","title":"再建一张","goal":"x"}`)
	if err != nil || !strings.Contains(out, "已经建过工单 tk-1") || len(e.a.tickets.List()) != 1 {
		t.Fatalf("同一条消息只建一个：%q %v", out, err)
	}
	finishGroupRun(t, e, sid)
}

func TestTicketRefusedForPeopleNotOnTheRoster(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e) // 名单里只有 fake:u2
	mention(e, "@机器人 帮我改个东西", "g200", "u3", "王五")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"改东西","goal":"x"}`); err == nil || !strings.Contains(err.Error(), "名单") {
		t.Fatalf("不在名单里的人建不了：%v", err)
	}
	if n := len(e.a.tickets.List()); n != 0 {
		t.Fatalf("不该有工单：%d", n)
	}
	finishGroupRun(t, e, sid)
	// 管理员总算在内
	mention(e, "@机器人 改一下", "g201", "u1", "张三")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"改","goal":"x"}`); err != nil {
		t.Fatalf("管理员能派活：%v", err)
	}
	finishGroupRun(t, e, sid)
}

// 2 秒里 A（名单内）先点名、B（名单外）后点名：这一轮是 A 触发的，工单记在 A 名下。
func TestTicketBelongsToTheFirstMentionInTheBatch(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 A 要修登录", "a1", "u2", "李四")
	mention(e, "@机器人 B 也来插一句", "b1", "u3", "王五")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"x"}`); err != nil {
		t.Fatalf("A 在名单里：%v", err)
	}
	tk, _ := e.a.tickets.Get("tk-1")
	if tk.Auth.By != "fake:u2" || tk.Auth.Source.MsgID != "a1" {
		t.Fatalf("工单记在 A 名下：%+v", tk.Auth)
	}
	finishGroupRun(t, e, sid)
	// 反过来：B（名单外）先点名，工单建不了——权限按触发这一轮的人判
	mention(e, "@机器人 B 先说", "b2", "u3", "王五")
	mention(e, "@机器人 A 后说", "a2", "u2", "李四")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"x","goal":"x"}`); err == nil {
		t.Fatal("触发这一轮的是 B，建不了")
	}
	finishGroupRun(t, e, sid)
}

// 这一轮在跑时 C 又点名：不改变这一轮是谁派的。
func TestMentionDuringARunDoesNotChangeWhoDispatched(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e, "fake:u2", "fake:u3")
	mention(e, "@机器人 A 要修登录", "a1", "u2", "李四")
	e.k.expect(proto.TInput)
	mention(e, "@机器人 C 我也要", "c1", "u3", "王五")
	e.a.mu.Lock()
	who := e.a.sessions[sid].run.trig.who
	e.a.mu.Unlock()
	if who != "fake:u2" {
		t.Fatalf("这一轮还是 A 派的：%s", who)
	}
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"x"}`); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.a.tickets.Get("tk-1"); tk.Auth.By != "fake:u2" || tk.Auth.Source.MsgID != "a1" {
		t.Fatalf("工单记在 A 名下：%+v", tk.Auth)
	}
	finishGroupRun(t, e, sid)
}

// 触发消息不填进排队项的 MsgID：群里的回复仍然贴在最后一条消息上，和以前一样。
func TestGroupTriggerDoesNotFillTheQueuedMessageID(t *testing.T) {
	e := setup(t)
	sid := extGroup(t, e)
	e.say("@机器人 几点了", group(true), msgID("first"), from("u2", "李四"))
	e.a.mu.Lock()
	s := e.a.sessions[sid]
	e.a.mu.Unlock()
	e.k.expect(proto.TInput)
	e.a.mu.Lock()
	trig := s.run.trig
	e.a.mu.Unlock()
	if trig == nil || trig.msgID != "first" || trig.who != "fake:u2" {
		t.Fatalf("run 带着触发消息：%+v", trig)
	}
	finishGroupRun(t, e, sid)
}

func TestTicketToolOnlyInTeamGroups(t *testing.T) {
	e := setup(t)
	plain := extGroup(t, e)
	e.a.mu.Lock()
	_, toolsPlain, _, _ := e.a.contextParts(plain)
	_, toolsMain, _, _ := e.a.contextParts(mainSID)
	e.a.mu.Unlock()
	for _, tl := range append(toolsPlain, toolsMain...) {
		if tl.Function.Name == "Ticket" {
			t.Fatal("没设团队规则的群和私聊没有 Ticket 工具")
		}
	}
	e2 := setup(t)
	sid := teamGroup(t, e2)
	e2.a.mu.Lock()
	_, tools, _, _ := e2.a.contextParts(sid)
	e2.a.mu.Unlock()
	found := false
	for _, tl := range tools {
		found = found || tl.Function.Name == "Ticket"
	}
	if !found {
		t.Fatal("团队群有 Ticket 工具")
	}
	// 内部群不可能设团队；设了团队的群里 mode 固定点名
	e2.a.mu.Lock()
	sc, _ := e2.a.sceneOf(sid)
	e2.a.mu.Unlock()
	if sc.free() || !sc.Rules.Observe {
		t.Fatal("团队群不主动插话")
	}
}

func TestTicketActions(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 修登录", "g1", "u2", "李四")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"手机上错位"}`); err != nil {
		t.Fatal(err)
	}
	out, _ := callTicket(e, sid, `{"action":"list"}`)
	if !strings.Contains(out, "tk-1 · open · 修登录") {
		t.Fatalf("list：%q", out)
	}
	out, _ = callTicket(e, sid, `{"action":"show","id":"tk-1"}`)
	if !strings.Contains(out, "手机上错位") || !strings.Contains(out, "make test") {
		t.Fatalf("show：%q", out)
	}
	if _, err := callTicket(e, sid, `{"action":"show","id":"tk-9"}`); err == nil {
		t.Fatal("没有的工单")
	}
	if out, err := callTicket(e, sid, `{"action":"note","id":"tk-1","text":"再加一条：别动 CSS 变量"}`); err != nil || !strings.Contains(out, "记下了") {
		t.Fatalf("note：%q %v", out, err)
	}
	if tk, _ := e.a.tickets.Get("tk-1"); len(tk.Notes) != 1 || tk.Notes[0].By != "fake:u2" {
		t.Fatalf("补充记在工单上：%+v", tk.Notes)
	}
	// 5a 里工单到不了就绪：accept 只拒绝
	if _, err := callTicket(e, sid, `{"action":"accept","id":"tk-1"}`); err == nil || !strings.Contains(err.Error(), "还没有就绪") {
		t.Fatalf("accept 只拒绝：%v", err)
	}
	// cancel 要理由
	if _, err := callTicket(e, sid, `{"action":"cancel","id":"tk-1"}`); err == nil {
		t.Fatal("叫停要写理由")
	}
	if out, err := callTicket(e, sid, `{"action":"cancel","id":"tk-1","text":"不做了，改走别的方案"}`); err != nil || !strings.Contains(out, "已叫停") {
		t.Fatalf("cancel：%q %v", out, err)
	}
	if tk, _ := e.a.tickets.Get("tk-1"); tk.Status != ticket.StatusStopped || tk.Reason != "不做了，改走别的方案" {
		t.Fatalf("stopped 带理由：%+v", tk)
	}
	if _, err := callTicket(e, sid, `{"action":"cancel","id":"tk-1","text":"再来一次"}`); err == nil {
		t.Fatal("已经结束的不能再叫停")
	}
	if _, err := callTicket(e, sid, `{"action":"note","id":"tk-1","text":"x"}`); err == nil {
		t.Fatal("已经结束的不能再补充")
	}
	finishGroupRun(t, e, sid)
	// 非名单里的人不能 note、cancel、accept
	mention(e, "@机器人 取消它", "g2", "u3", "王五")
	e.k.expect(proto.TInput)
	for _, a := range []string{`{"action":"note","id":"tk-1","text":"x"}`, `{"action":"cancel","id":"tk-1","text":"x"}`, `{"action":"accept","id":"tk-1"}`} {
		if _, err := callTicket(e, sid, a); err == nil || !strings.Contains(err.Error(), "名单") {
			t.Errorf("%s 名单外的人不能做：%v", a, err)
		}
	}
	finishGroupRun(t, e, sid)
}

func TestTicketWriteFailureReturnsErrorAndDegradesHealth(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 修登录", "g1", "u2", "李四")
	e.k.expect(proto.TInput)
	e.a.mu.Lock()
	e.a.tickets.Close() // 之后的写入都失败
	e.a.mu.Unlock()
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"x"}`); err == nil || !strings.Contains(err.Error(), "没有记下来") {
		t.Fatalf("写不进去要把错误返回给模型：%v", err)
	}
	var st health.State
	for _, h := range e.a.health.Snapshot() {
		if h.Name == "tickets" {
			st = h.State
		}
	}
	if st != health.Degraded {
		t.Fatalf("健康条目 tickets 要变 degraded：%q", st)
	}
	finishGroupRun(t, e, sid)
}

func TestTicketsSurviveRestart(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 修登录", "g1", "u2", "李四")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"x"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, e, sid)
	e.a.mu.Lock()
	e.a.tickets.Close()
	e.a.mu.Unlock()
	a2 := newAppAt(t, e.dir, nil)
	if tk, ok := a2.tickets.Get("tk-1"); !ok || tk.Title != "修登录" || tk.Auth.By != "fake:u2" {
		t.Fatalf("重启后工单还在：%+v %v", tk, ok)
	}
}

// 工单进承诺列表：open 是等待；叫停后是 stopped 并有理由；重启后还在；也进“全部承诺”但 open 不进等我处理。
func TestTicketsAppearInCommitmentsAndSurviveRestart(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 修登录", "g1", "u2", "李四")
	e.k.expect(proto.TInput)
	if _, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"手机上错位"}`); err != nil {
		t.Fatal(err)
	}
	finishGroupRun(t, e, sid)
	v := findCommit(e.a.Commitments("", ""), commit.KindTicket, "tk-1")
	if v == nil || v.Phase != commit.Waiting || v.Why != "等开工" || v.Title != "修登录" || v.Contract.By != "fake:u2" {
		t.Fatalf("open 是等待：%+v", v)
	}
	if p := pendingOf(e, commit.KindTicket, "tk-1"); p != nil {
		t.Fatalf("等待中的不进等我处理：%+v", p)
	}
	// 回答：5a 里没有干活的分支
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindTicket, ID: "tk-1", Agent: "default", Status: commit.StatusBlocked, Text: "x", Question: "用哪个？"})
	if err := e.a.PendingAnswer(commit.KindTicket, "tk-1", fmt.Sprintf("r:%d", r.Seq), "第一个", "web:admin"); err == nil {
		t.Fatal("5a 里工单没有分支，拒绝回答")
	}
	// 主人在网页上叫停：要理由
	if err := e.a.CancelTicket("tk-1", "  "); err == nil {
		t.Fatal("叫停要写理由")
	}
	if err := e.a.CancelTicket("tk-9", "x"); err == nil {
		t.Fatal("没有这张工单")
	}
	if err := e.a.CancelTicket("tk-1", "需求取消了"); err != nil {
		t.Fatal(err)
	}
	v = findCommit(e.a.Commitments("", ""), commit.KindTicket, "tk-1")
	if v == nil || v.Phase != commit.Stopped || v.Why != "需求取消了" {
		t.Fatalf("叫停后是 stopped 并有理由：%+v", v)
	}
	if err := e.a.CancelTicket("tk-1", "再来"); err == nil {
		t.Fatal("已经结束的不能再叫停")
	}
	e.a.mu.Lock()
	e.a.tickets.Close()
	e.a.commits.Close()
	e.a.mu.Unlock()
	a2 := newAppAt(t, e.dir, nil)
	v = findCommit(a2.Commitments("", ""), commit.KindTicket, "tk-1")
	if v == nil || v.Phase != commit.Stopped {
		t.Fatalf("重启后工单还在：%+v", v)
	}
}

// 说话纪律：建工单、补充、叫停都不往群里发别的；通知只有结果和问题，作为原消息的回复，去重、每小时 3 条、结束后不发。
func TestTicketSpeechDiscipline(t *testing.T) {
	e := setup(t)
	sid := teamGroup(t, e)
	mention(e, "@机器人 修登录", "g1", "u2", "李四")
	e.k.expect(proto.TInput)
	drainSent(e)
	for _, a := range []string{`{"action":"create","title":"修登录","goal":"手机上错位"}`, `{"action":"note","id":"tk-1","text":"补一句"}`, `{"action":"list"}`} {
		if _, err := callTicket(e, sid, a); err != nil {
			t.Fatal(err)
		}
	}
	if sent := drainSent(e); len(sent) != 0 {
		t.Fatalf("工单的过程不往群里发：%v", sent)
	}
	push := func(state, text, q string) {
		e.a.mu.Lock()
		e.a.ticketPush("tk-1", state, text, q)
		e.a.mu.Unlock()
	}
	push("ready", "改好了：验收通过", "")
	got := e.ch.wait(t, "[工单 tk-1「修登录」· 就绪] 改好了")
	if got == "" {
		t.Fatal("没发")
	}
	if rs := e.ch.replies(); len(rs) == 0 || rs[len(rs)-1] != "g1" {
		t.Fatalf("作为原消息的回复：%v", rs)
	}
	push("ready", "改好了：验收通过", "") // 和上一条一样：不发
	push("blocked", "验收没过", "继续改还是放弃？")
	q := e.ch.wait(t, "需要你回答")
	e.a.mu.Lock()
	ag, _ := e.a.agentByID("default")
	e.a.mu.Unlock()
	if ag.Name == "" || !strings.Contains(q, "问：继续改还是放弃？") || !strings.Contains(q, "@"+ag.Name+" 回复就行") || strings.Contains(q, "/answer") {
		t.Fatalf("问题：%q", q)
	}
	push("ready", "第二版", "")
	push("ready", "第三版", "") // 这一小时的第 4 条：不发
	if sent := drainSent(e); len(sent) != 1 {
		t.Fatalf("去重和每小时 3 条之后只剩一条：%v", sent)
	}
	e.a.mu.Lock()
	n := len(e.a.commits.Notes(commit.KindTicket, "tk-1"))
	e.a.mu.Unlock()
	if n != 3 {
		t.Fatalf("先写 notify 记录：%d", n)
	}
	// 结束了的工单：问题和失败不再发
	if err := e.a.CancelTicket("tk-1", "不做了"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.commits.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	e.a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	e.a.mu.Unlock()
	push("blocked", "还有问题", "")
	if sent := drainSent(e); len(sent) != 0 {
		t.Fatalf("结束了的工单不发问题：%v", sent)
	}
	finishGroupRun(t, e, sid)
}

// 授权记录里的权限和工具说的是工单的分支，不是团队群自己的设置。
func TestTicketAuthorityIsAboutTheTicketBranch(t *testing.T) {
	create := func(e *env, sid string, id string) error {
		mention(e, "@机器人 修登录", id, "u2", "李四")
		e.k.expect(proto.TInput)
		_, err := callTicket(e, sid, `{"action":"create","title":"修登录","goal":"x"}`)
		finishGroupRun(t, e, sid)
		return err
	}
	// 群会话自己设成只读：Auth.Mode 仍是 agent 的默认模式
	e := setup(t)
	sid := teamGroup(t, e)
	e.a.mu.Lock()
	st := e.a.store.Settings(sid)
	st.Mode = "readonly"
	_ = e.a.store.SaveSettings(sid, st)
	want := e.a.agentModeDefault(sid)
	e.a.mu.Unlock()
	if err := create(e, sid, "g1"); err != nil {
		t.Fatal(err)
	}
	tk, _ := e.a.tickets.Get("tk-1")
	if tk.Auth.Mode != want || tk.Auth.Mode == "readonly" {
		t.Fatalf("Auth.Mode 是 agent 的默认模式 %q，不是群会话的只读：%q", want, tk.Auth.Mode)
	}
	// 工具：固定的工单工具集；有 Shell，没有对外发声和派生承诺的
	for _, must := range []string{"Read", "Write", "Edit", "Grep", "Shell", "Job", "Report", "WebSearch", "WebFetch", "Skill"} {
		if !slices.Contains(tk.Auth.Tools, must) {
			t.Errorf("工单工具里缺 %s：%v", must, tk.Auth.Tools)
		}
	}
	for _, not := range []string{"Message", "SendFile", "Session", "Schedule", "Hook", "FollowUp", "Agenda", "Ticket", "Idea", "Mind", "Diary", "Gateway", "GatewayAdmin", "Knowledge"} {
		if slices.Contains(tk.Auth.Tools, not) {
			t.Errorf("工单工具里不该有 %s", not)
		}
	}
	// 契约的范围显示新的值
	if v := findCommit(e.a.Commitments("", ""), commit.KindTicket, "tk-1"); v == nil || !strings.Contains(v.Contract.Scope, "Shell") || strings.Contains(v.Contract.Scope, "SendFile") {
		t.Fatalf("Scope：%+v", v)
	}

	// agent 的默认模式是 full：压成 auto
	e2 := setup(t)
	e2.a.cfg.Workspace.Mode = "full"
	sid2 := teamGroup(t, e2)
	if err := create(e2, sid2, "g1"); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e2.a.tickets.Get("tk-1"); tk.Auth.Mode != "auto" {
		t.Fatalf("full 压成 auto：%q", tk.Auth.Mode)
	}

	// agent 是只读的：接不了
	e3 := setup(t)
	e3.a.cfg.Workspace.Mode = "readonly"
	sid3 := teamGroup(t, e3)
	if err := create(e3, sid3, "g1"); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatalf("只读的 agent 接不了工单：%v", err)
	}
	if len(e3.a.tickets.List()) != 0 {
		t.Fatal("不该有工单")
	}

	// agent 的工具上限：取交集
	e4 := setup(t)
	store, _ := withAgents(t, e4)
	ag, _ := store.Get(agents.DefaultID)
	ag.Tools = []string{"Read", "Shell", "SendFile"}
	if err := store.Update(ag); err != nil {
		t.Fatal(err)
	}
	sid4 := teamGroup(t, e4)
	if err := create(e4, sid4, "g1"); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e4.a.tickets.Get("tk-1"); !slices.Equal(tk.Auth.Tools, []string{"Read", "Shell"}) {
		t.Fatalf("工具是固定集合和 agent 上限的交集：%v", tk.Auth.Tools)
	}
}
