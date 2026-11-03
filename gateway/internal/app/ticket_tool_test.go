package app

import (
	"context"
	"strings"
	"testing"

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
