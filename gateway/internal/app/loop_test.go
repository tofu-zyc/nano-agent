package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/session"
)

// 统一聊天循环；见 docs/DESIGN-agent-runtime.md。

// 不调模型的门槛：问句更想接，@别人的不太想接，最近自己说得多更想接着说。
func TestGroupGate(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	now := e.a.now()
	p := func(lines ...string) float64 {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		sid := "fake:gate" + lines[0]
		for _, l := range lines {
			role := "user"
			if strings.HasPrefix(l, "我：") {
				role = "assistant"
			}
			e.a.store.Append(sid, session.Msg{Role: role, Content: l, TS: now.Unix()})
		}
		return e.a.groupGate(e.a.sessOf(sid), now)
	}
	plain := p("[15:08][张三] 今天天气不错")
	question := p("[15:08][张三] 今天有人去吃饭吗？")
	atOther := p("[15:08][张三] @李四 今天天气不错")
	engaged := p("[15:07][张三] 你说的那个", "我：对", "[15:08][张三] 那后来呢")
	if !(question > plain && atOther < plain && engaged > plain) {
		t.Fatalf("门槛的概率不对：普通 %.2f 问句 %.2f @别人 %.2f 正聊着 %.2f", plain, question, atOther, engaged)
	}
	if plain <= 0 || plain >= 1 {
		t.Fatalf("概率要在 0 和 1 之间：%v", plain)
	}
}

// 门槛没抽中：不调模型，等下一批新消息；连着几次不想说就越等越久。
func TestGroupGateAndBackoff(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC)
	setNow(e, base)
	e.say("随便聊聊", group(false))
	e.a.mu.Lock()
	e.a.rnd = func() float64 { return 0.999 }
	e.a.now = func() time.Time { return base.Add(time.Minute) }
	e.a.mu.Unlock()
	e.a.groupTick()
	e.k.none()
	e.a.mu.Lock()
	s := e.a.sessOf("fake:c1")
	fresh := s.groupFresh
	rule := e.a.groupRuleOf("fake:c1")
	// 退避：连着 5 次不说话，要等 15s×2³ = 2 分钟（比一分钟的最短间隔长）
	s.groupSilent, s.groupThoughtAt, s.groupPending = 5, base, 1
	early := e.a.groupMayThink(s, rule, base.Add(90*time.Second))
	late := e.a.groupMayThink(s, rule, base.Add(121*time.Second))
	s.groupPending = 6 // 新消息攒够了：不等
	busy := e.a.groupMayThink(s, rule, base.Add(61*time.Second))
	e.a.mu.Unlock()
	if fresh {
		t.Fatal("没抽中要等下一批新消息")
	}
	if early || !late || !busy {
		t.Fatalf("退避：90 秒 %v、121 秒 %v、攒够消息 %v", early, late, busy)
	}
}

// 拟人型的私聊：纯聊天的回复按空行分几条发；只回 [不回复] 就不接。工具型照旧一整条。
func TestLifeDM(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.say("第一段\n\n第二段")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	first := e.ch.wait(t, "（mock）")
	second := e.ch.wait(t, "第二段")
	if strings.Contains(first, "第二段") || second != "第二段" {
		t.Fatalf("应该分两条：%q / %q", first, second)
	}
	e.llm.say("[不回复]")
	e.say("晚安")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	select {
	case m := <-e.ch.sent:
		t.Fatalf("只回 [不回复] 就不该发：%q", m)
	case <-time.After(300 * time.Millisecond):
	}
	// 工具型：一整条
	e.llm.say("")
	e.a.mu.Lock()
	ag, _ := store.Get(agents.DefaultID)
	ag.Style = ""
	store.Update(ag)
	e.a.mu.Unlock()
	e.say("甲\n\n乙")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	if got := e.ch.wait(t, "（mock）"); !strings.Contains(got, "乙") {
		t.Fatalf("工具型应该一整条：%q", got)
	}
}

// 过会儿再看：到点在那个聊天里醒来；对方先说话就取消；睡着时挪到醒来。
func TestFollowUp(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.say("我去面试了")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.a.mu.Lock()
	_, tools, _, _ := e.a.contextParts(mainSID)
	out, err := e.a.followUpCall(mainSID, []byte(`{"minutes":30,"note":"问问面试怎么样"}`))
	e.a.lifeOf("default").Next = base.Add(10 * time.Hour).Unix() // 别让内心活动插进来
	e.a.mind("default").SetNextCheck(base.Add(10 * time.Hour))
	e.a.mu.Unlock()
	if err != nil || !strings.Contains(out, "15:30") {
		t.Fatalf("约：%q %v", out, err)
	}
	has := false
	for _, tl := range tools {
		has = has || tl.Function.Name == "FollowUp"
	}
	if !has {
		t.Fatal("拟人型的私聊里应该有 FollowUp")
	}
	setNow(e, base.Add(20*time.Minute))
	e.a.lifeTick()
	e.k.none()
	setNow(e, base.Add(31*time.Minute))
	e.a.lifeTick()
	in := e.k.expect(proto.TInput)
	if in.SID != mainSID || in.From != "fake:u1" {
		t.Fatalf("到点了在那个聊天里醒来（按主人的身份）：%+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	if last := e.llm.lastReq().Messages; !strings.Contains(findMsg(last, "约好这时候再看看"), "问问面试怎么样") {
		t.Fatal("醒来时要带着约好的事")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")
	// 对方先说话：取消
	e.a.mu.Lock()
	e.a.followUpCall(mainSID, []byte(`{"minutes":30,"note":"再问问"}`))
	e.a.mu.Unlock()
	e.say("面试过了！")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.a.mu.Lock()
	_, left := e.a.lifeOf("default").FollowUps[mainSID]
	e.a.mu.Unlock()
	if left {
		t.Fatal("对方先说话了，约好的应该取消")
	}
	// 睡着时到点：挪到醒来
	e.a.mu.Lock()
	e.a.lifeOf("default").FollowUps[mainSID] = followUp{At: time.Date(2026, 10, 21, 23, 40, 0, 0, time.UTC).Unix(), Note: "x"}
	e.a.mu.Unlock()
	setNow(e, time.Date(2026, 10, 21, 23, 45, 0, 0, time.UTC))
	e.a.lifeTick()
	e.k.none()
	e.a.mu.Lock()
	moved := e.a.lifeOf("default").FollowUps[mainSID].At
	e.a.mu.Unlock()
	if moved != time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("睡着时到点应该挪到 08:00：%v", time.Unix(moved, 0).UTC())
	}
	// 工具型不能约
	e.a.mu.Lock()
	ag, _ := store.Get(agents.DefaultID)
	ag.Style = ""
	store.Update(ag)
	_, err = e.a.followUpCall(mainSID, []byte(`{"minutes":5}`))
	e.a.mu.Unlock()
	if err == nil {
		t.Fatal("工具型不能约")
	}
}

// 网关里的群：agent 说话时 @ 了别的成员，叫醒它；说出口的计进每小时额度。
func TestRoomAgentMention(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, err := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	if err := e.a.agentStore.Create(agents.Agent{ID: "bob", Name: "小波"}); err != nil {
		t.Fatal(err)
	}
	room, err := st.Create(rooms.Room{Name: "产品组", Members: []string{"default", "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.roomAgentSay(room, "default", "@小波 你怎么看")
	e.a.mu.Unlock()
	in := e.k.expect(proto.TInput)
	if in.SID != e.a.roomMemberSID("bob", room.ID) {
		t.Fatalf("应该叫醒小波：%+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
	e.a.mu.Lock()
	spoke := len(e.a.sessOf(in.SID).groupSpeaks)
	e.a.mu.Unlock()
	if spoke != 1 {
		t.Fatalf("群里说出口的要计进额度：%d", spoke)
	}
	// 没叫到别人：不叫醒
	e.a.mu.Lock()
	e.a.roomAgentSay(room, "default", "今天天气不错")
	e.a.mu.Unlock()
	e.k.none()
}
