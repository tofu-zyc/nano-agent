package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
)

// scriptModel 按成员（从系统提示里的人格认）给出固定的回复；没有随机数。
type scriptModel struct {
	mu    sync.Mutex
	n     map[string]int
	reply func(who string, n int) (string, error)
	hints []string // 每次请求末尾那句只放在请求里的提示
}

func (m *scriptModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	who := "default"
	for _, name := range []string{"bob", "cat"} {
		if strings.Contains(req.Messages[0].Content, "你是"+map[string]string{"bob": "小鲍", "cat": "小猫"}[name]) {
			who = name
		}
	}
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[who]++
	last := req.Messages[len(req.Messages)-1].Content
	if strings.HasPrefix(last, "[系统]") {
		m.hints = append(m.hints, who+":"+last)
	}
	text, err := m.reply(who, m.n[who])
	if err != nil {
		return nil, err
	}
	return &llm.Result{Reason: llm.ReasonFinal, Content: text, Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 2}}, nil
}

func (m *scriptModel) hintLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.hints...)
}

// step 让内核走完一个成员的一轮，返回是哪个会话。
func step(t *testing.T, e *env, want string) {
	t.Helper()
	in := e.k.expect(proto.TInput)
	if in.SID != want {
		t.Fatalf("应该轮到 %s，实际 %s", want, in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	status := proto.StatusOK
	if turn.Reason == proto.ReasonError {
		status = proto.StatusError
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: status})
	waitRunDone(t, e.a, in.SID)
}

func say(who string, n int) (string, error) { return fmt.Sprintf("%s说第%d句", who, n), nil }

func turnRoom(t *testing.T, e *env, r scene.Rules, m *scriptModel) (string, string) {
	t.Helper()
	e.a.SetLLM(m)
	room, _ := threeRoom(t, e, r)
	return room.ID, roomLogSID(room.ID)
}

func roomLog(e *env, sid string) string { return strings.Join(lines(e, sid), "\n") }

func paused(e *env, id string) (bool, string) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	r, _ := e.a.roomStore.Get(id)
	return r.Paused, ""
}

// runScript 跑一场：顺序 cat、default、bob，两轮；返回群记录。
func runScript(t *testing.T) string {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, log := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 2, Order: []string{"cat", "default", "bob"}}, m)
	if err := e.a.RoomSay(id, "开始吧", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for _, who := range []string{"cat", "default", "bob"} {
			step(t, e, e.a.roomMemberSID(who, id))
		}
	}
	waitFor(t, func() bool { p, _ := paused(e, id); return p })
	e.k.none() // 到了轮数，不再叫人
	return roomLog(e, log)
}

// 轮流：顺序、计轮、到轮数暂停写明原因；同一个脚本跑两次，群记录逐字相同。
func TestRoomTurnsOrderRoundsAndDeterminism(t *testing.T) {
	first := runScript(t)
	for _, s := range []string{"[张三] 开始吧", "[小猫] cat说第1句", "[小鲍] bob说第1句", "[小猫] cat说第2句", "[小鲍] bob说第2句", "[系统] 这一场到了 2 轮"} {
		if !strings.Contains(first, s) {
			t.Fatalf("群记录里缺 %q：\n%s", s, first)
		}
	}
	// 顺序：cat 在 default 前，default 在 bob 前
	ci, di, bi := strings.Index(first, "cat说第1句"), strings.Index(first, "default说第1句"), strings.Index(first, "bob说第1句")
	if !(ci < di && di < bi) {
		t.Fatalf("顺序不对：\n%s", first)
	}
	if second := runScript(t); second != first {
		t.Fatalf("同样的脚本两次的群记录应该逐字相同：\n%s\n----\n%s", first, second)
	}
}

// 提示只放在请求里，不进历史；被禁言的跳过；人中途说话不打乱顺序。
func TestRoomTurnsHintMuteAndHumanInterjection(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 1}, m)
	muted := []string{"bob"}
	if _, err := e.a.UpdateRoom(id, RoomPatch{Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	if err := e.a.RoomSay(id, "开始", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("default", id))
	// default 说完之后、cat 开口之前，人插了一句：进历史，不打乱顺序
	if err := e.a.RoomSay(id, "我插一句", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("cat", id)) // bob 被禁言，跳过
	waitFor(t, func() bool { p, _ := paused(e, id); return p })
	hints := m.hintLog()
	if len(hints) != 2 || !strings.Contains(hints[0], "第 1 轮，轮到你。不想说就只回 [不回复]。") {
		t.Fatalf("提示：%v", hints)
	}
	for _, sid := range []string{e.a.roomMemberSID("default", id), e.a.roomMemberSID("cat", id)} {
		for _, l := range lines(e, sid) {
			if strings.Contains(l, "轮到你") {
				t.Fatalf("提示不能进历史：%q", l)
			}
		}
	}
	if !has(e, e.a.roomMemberSID("cat", id), "[张三] 我插一句") {
		t.Fatal("人中途说的话要进成员历史")
	}
	for _, l := range lines(e, e.a.roomMemberSID("bob", id)) {
		if strings.Contains(l, "bob说") {
			t.Fatal("被禁言的不该说话")
		}
	}
}

// @ 和叫名字在轮流时不插队。
func TestRoomTurnsMentionDoesNotJump(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 1, Order: []string{"default", "bob", "cat"}}, m)
	if err := e.a.RoomSay(id, "@小猫 你先说", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("default", id)) // 还是 default 先
}

// 一整轮所有人都回 [不回复]：停下来，不算暂停；下一句人话再开始。
func TestRoomTurnsAllSilentStopsWithoutPause(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: func(who string, n int) (string, error) { return "[不回复]", nil }}
	id, _ := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 5}, m)
	e.a.RoomSay(id, "有人吗", "张三", "", "我")
	for _, who := range []string{"default", "bob", "cat"} {
		step(t, e, e.a.roomMemberSID(who, id))
	}
	e.k.none()
	if p, _ := paused(e, id); p {
		t.Fatal("全员沉默只是停下来，不算暂停")
	}
	e.a.RoomSay(id, "再问一次", "张三", "", "我")
	in := e.k.expect(proto.TInput)
	if in.SID != e.a.roomMemberSID("default", id) {
		t.Fatalf("下一句人话要重新开始：%s", in.SID)
	}
}

// 出错算跳过；连续两次出错（跨轮也算）就暂停并写明原因。
func TestRoomTurnsErrorsSkipThenPause(t *testing.T) {
	e := setup(t)
	failing := map[string]bool{"default": true, "bob": true}
	m := &scriptModel{reply: func(who string, n int) (string, error) {
		if failing[who] {
			return "", errors.New("模型挂了")
		}
		return say(who, n)
	}}
	id, log := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 3, Order: []string{"default", "cat", "bob"}}, m)
	e.a.RoomSay(id, "开始", "张三", "", "我")
	step(t, e, e.a.roomMemberSID("default", id)) // 出错，跳过
	step(t, e, e.a.roomMemberSID("cat", id))     // 说话，清掉连续出错
	step(t, e, e.a.roomMemberSID("bob", id))     // 出错，但上一位说了话，不连续，继续
	if p, _ := paused(e, id); p {
		t.Fatal("不连续的出错不该暂停")
	}
	step(t, e, e.a.roomMemberSID("default", id)) // 第二轮开头又出错：和上一位的出错连起来（跨轮也算）
	waitFor(t, func() bool { p, _ := paused(e, id); return p })
	if !strings.Contains(roomLog(e, log), "连续两次出错") {
		t.Fatalf("要写明原因：\n%s", roomLog(e, log))
	}
}

// 轮数存进 scenes.json；重启后不续跑，下一句人话接着算。
func TestRoomTurnsPersistAndNoResumeAfterRestart(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 3, Order: []string{"default", "bob", "cat"}}, m)
	e.a.RoomSay(id, "开始", "张三", "", "我")
	step(t, e, e.a.roomMemberSID("default", id))
	// bob 已经被叫起来了：先让它进行中，再“重启”
	in := e.k.expect(proto.TInput)
	if in.SID != e.a.roomMemberSID("bob", id) {
		t.Fatalf("应该轮到 bob：%s", in.SID)
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "data", "runtime", "scenes.json"))
	if !strings.Contains(string(b), `"next": 2`) {
		t.Fatalf("下一位应该已经存盘（顺序里的第 2 位，已经叫了 bob）：%s", b)
	}
	a2 := newAppAt(t, e.dir, nil)
	a2.mu.Lock()
	u := a2.sceneUses[roomLogSID(id)]
	a2.mu.Unlock()
	if u == nil || u.Next != 2 || u.Round != 0 {
		t.Fatalf("重启后读回：%+v", u)
	}
	time.Sleep(200 * time.Millisecond)
	a2.mu.Lock()
	for sid, s := range a2.sessions {
		if s != nil && s.run != nil {
			t.Errorf("重启后不该有在跑的会话：%s", sid)
		}
	}
	a2.mu.Unlock()
}

// 导演：@、叫名字和主动插话都不触发；一次 cue 只叫一个；它正在说话时拒绝；cue 只有对象收到。
func TestRoomDirectorCue(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, log := turnRoom(t, e, scene.Rules{Mode: scene.ModeDirector}, m)
	e.a.RoomSay(id, "@小猫 你好", "张三", "", "我")
	e.a.mu.Lock()
	for _, who := range []string{"default", "bob", "cat"} {
		if s := e.a.sessOf(e.a.roomMemberSID(who, id)); s.groupTimer != nil {
			t.Errorf("导演模式下 @ 不该触发 %s", who)
		}
	}
	e.a.rnd = func() float64 { return 0 }
	base := e.a.now()
	e.a.now = func() time.Time { return base.Add(time.Hour) }
	e.a.mu.Unlock()
	e.a.groupTick()
	e.k.none() // 主动插话也不触发

	if err := e.a.RoomCue(id, "ghost", "x", ""); err == nil {
		t.Fatal("不在群里的人不能 cue")
	}
	if err := e.a.RoomCue(id, "cat", "去开门", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.a.RoomCue(id, "cat", "再去", ""); err == nil {
		t.Fatal("它正在说话时要拒绝")
	}
	in := e.k.expect(proto.TInput)
	if in.SID != e.a.roomMemberSID("cat", id) {
		t.Fatalf("一次 cue 只叫一个：%s", in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitRunDone(t, e.a, in.SID)
	e.k.none()
	if !has(e, log, "[导演→小猫] 去开门") || !has(e, e.a.roomMemberSID("cat", id), "[导演] 去开门") || !has(e, log, "[小猫] cat说第1句") {
		t.Fatalf("群记录：\n%s", roomLog(e, log))
	}
	for _, who := range []string{"default", "bob"} {
		for _, l := range lines(e, e.a.roomMemberSID(who, id)) {
			if strings.Contains(l, "去开门") {
				t.Fatalf("cue 只有对象收到，%s 也收到了", who)
			}
		}
	}
	if h := m.hintLog(); len(h) != 1 || !strings.Contains(h[0], "导演让你说一句") {
		t.Fatalf("请求末尾的提示：%v", h)
	}
}

// 频道：被叫到频道里的成员，回复回到那个频道；不在频道里的成员会话里没有频道里的话；cue 也可以指定频道。
func TestRoomChannelReplyStaysInChannel(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, log := turnRoom(t, e, scene.Rules{}, m)
	if err := e.a.RoomSay(id, "@小猫 你怎么看", "小鲍", "s", "我"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("cat", id)) // 2 秒后叫起来
	if !has(e, log, "[#密谈][小猫] cat说第1句") || !has(e, e.a.roomMemberSID("bob", id), "[#密谈][小猫] cat说第1句") {
		t.Fatalf("回复要回到频道：\n%s", roomLog(e, log))
	}
	for _, l := range lines(e, e.a.roomMemberSID("default", id)) {
		if strings.Contains(l, "密谈") || strings.Contains(l, "你怎么看") || strings.Contains(l, "cat说") {
			t.Fatalf("不在频道里的成员不该看到频道里的话：%q", l)
		}
	}
	if h := m.hintLog(); len(h) != 1 || !strings.Contains(h[0], "私下频道「密谈」") || !strings.Contains(h[0], "小鲍、小猫") {
		t.Fatalf("提示这一句在哪个频道、谁听得到：%v", h)
	}
	// cue 指定频道
	if err := e.a.RoomCue(id, "bob", "悄悄回一句", "s"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("bob", id))
	if !has(e, log, "[#密谈][小鲍] bob说第1句") || has(e, e.a.roomMemberSID("default", id), "[#密谈][小鲍] bob说第1句") {
		t.Fatalf("cue 指定的频道：\n%s", roomLog(e, log))
	}
	if err := e.a.RoomCue(id, "default", "x", "s"); err == nil {
		t.Fatal("不在频道里的成员不能被 cue 到频道")
	}
}

// 端到端（模型替身）：建群、设角色、轮流跑三轮、改成导演后 cue 一次，核对群记录。
func TestRoomTurnsThenDirectorEndToEnd(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	e.a.SetLLM(m)
	store, _ := withAgents(t, e)
	addAgent(t, e, store, testAgent("bob"), "你是小鲍")
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	name, members := "剧场", []string{"default", "bob"}
	rounds, mode := 3, scene.ModeTurns
	roles := map[string]scene.Role{"default": {Prompt: "你是侦探"}, "bob": {Prompt: "你是管家"}}
	info, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members, ScenePatch: ScenePatch{
		Mode: &mode, Rounds: &rounds, Roles: &roles, Brief: strptr("庄园里发生了命案")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.RoomSay(info.ID, "天黑了", "旁白", "", "我"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	system, _, _, _ := e.a.contextParts(e.a.roomMemberSID("bob", info.ID))
	e.a.mu.Unlock()
	if !strings.Contains(system, "[场景] 庄园里发生了命案") || !strings.Contains(system, "[你的角色] 你是管家") {
		t.Fatalf("场景和角色要进成员的前缀：\n%s", system)
	}
	for i := 0; i < 3; i++ {
		step(t, e, e.a.roomMemberSID("default", info.ID))
		step(t, e, e.a.roomMemberSID("bob", info.ID))
	}
	waitFor(t, func() bool { p, _ := paused(e, info.ID); return p })
	// 改成导演并恢复，cue 一次
	dir, resume := scene.ModeDirector, false
	if _, err := e.a.UpdateRoom(info.ID, RoomPatch{Paused: &resume, ScenePatch: ScenePatch{Mode: &dir, Reset: true}}); err != nil {
		t.Fatal(err)
	}
	if err := e.a.RoomCue(info.ID, "bob", "该你揭晓了", ""); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("bob", info.ID))
	got := roomLog(e, roomLogSID(info.ID))
	want := "[旁白] 天黑了\n[小纳] default说第1句\n[bob] bob说第1句\n[小纳] default说第2句\n[bob] bob说第2句\n[小纳] default说第3句\n[bob] bob说第3句\n[系统] 这一场到了 3 轮，先暂停，清零累计后再恢复\n[导演→bob] 该你揭晓了\n[bob] bob说第4句"
	if got != want {
		t.Fatalf("群记录：\n%s\n应为：\n%s", got, want)
	}
}

// 只剩一位能说话、模型一直出错：连续出错跨轮累计，叫两次就暂停，不会无限转下去。
func TestRoomTurnsErrorsAcrossRoundsPause(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: func(who string, n int) (string, error) { return "", errors.New("模型挂了") }}
	id, log := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, BudgetTotal: 100000, Order: []string{"default", "cat", "bob"}}, m)
	muted := []string{"cat", "bob"}
	if _, err := e.a.UpdateRoom(id, RoomPatch{Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	e.a.RoomSay(id, "开始", "张三", "", "我")
	// 同一位成员连着被叫：第一次出错的一轮结束时下一轮已经开始，不能等它“空闲”
	for i := 0; i < 2; i++ {
		in := e.k.expect(proto.TInput)
		e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
		e.k.expect(proto.TTurn)
		e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusError})
	}
	waitFor(t, func() bool { p, _ := paused(e, id); return p })
	e.k.none()
	if !strings.Contains(roomLog(e, log), "连续两次出错") {
		t.Fatalf("要写明原因：\n%s", roomLog(e, log))
	}
}

// 群在回复期间被用量暂停：先记 agent 的话，再写暂停；它 @ 的成员不该被叫起来。
func TestRoomPausedByUsageDoesNotWakeMentioned(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: func(who string, n int) (string, error) {
		if who == "bob" {
			return "@小猫 你怎么看", nil
		}
		return "我来说", nil
	}}
	e.a.SetLLM(m)
	room, _ := threeRoom(t, e, scene.Rules{Mode: scene.ModeMention, Budget: 5})
	log := roomLogSID(room.ID)
	if err := e.a.RoomSay(room.ID, "@小鲍 说说", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("bob", room.ID))
	waitFor(t, func() bool { p, _ := paused(e, room.ID); return p })
	time.Sleep(2500 * time.Millisecond) // 过了叫人的等待时间
	e.k.none()
	ls := lines(e, log)
	say, pause := -1, -1
	for i, l := range ls {
		if strings.HasPrefix(l, "[小鲍]") {
			say = i
		}
		if strings.HasPrefix(l, "[系统]") && strings.Contains(l, "先暂停") {
			pause = i
		}
	}
	if say < 0 || pause < 0 || say > pause {
		t.Fatalf("小鲍的话要在暂停提示前面：%q", ls)
	}
}
