package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/session"
)

// 剧场（设计 §14.1）：隔离、场景记录、心里话、时间线接口。

func intptr(n int) *int { return &n }

func boolptr(b bool) *bool { return &b }

func openRooms(t *testing.T, e *env) *rooms.Store {
	t.Helper()
	st, err := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	return st
}

func stageRules(extra scene.Rules) scene.Rules {
	extra.Stage = true
	if extra.Mode == "" {
		extra.Mode = scene.ModeTurns
		if extra.Rounds == 0 {
			extra.Rounds = 2
		}
	}
	return extra
}

// timeline 是群的全部场景记录。
func timeline(t *testing.T, e *env, roomID string) []StageRec {
	t.Helper()
	items, _, err := e.a.StageTimeline(roomID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func kindsOf(recs []StageRec, kind string) []StageRec {
	var out []StageRec
	for _, r := range recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// 轮流跑 rounds 轮：每个成员按顺序走一遍。
func runRounds(t *testing.T, e *env, id string, rounds int, order ...string) {
	t.Helper()
	for i := 0; i < rounds; i++ {
		for _, who := range order {
			step(t, e, e.a.roomMemberSID(who, id))
		}
	}
	waitFor(t, func() bool { p, _ := paused(e, id); return p })
}

func TestStageRulesValidation(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st := openRooms(t, e)
	_ = st
	name, members := "戏", []string{agents.DefaultID}
	yes, no := true, false
	if _, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members, ScenePatch: ScenePatch{Inner: &yes}}); err == nil {
		t.Fatal("心里话要先开 stage")
	}
	if _, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members, ScenePatch: ScenePatch{Stage: &yes, Tools: json.RawMessage(`["Knowledge"]`)}}); err == nil {
		t.Fatal("戏里不能用 Knowledge")
	}
	info, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members, ScenePatch: ScenePatch{Stage: &yes, Inner: &yes, Tools: json.RawMessage(`["WebSearch"]`)}})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Stage || !info.Inner {
		t.Fatalf("规则没带上：%+v", info)
	}
	if _, err := e.a.UpdateRoom(info.ID, RoomPatch{ScenePatch: ScenePatch{Stage: &no}}); err == nil {
		t.Fatal("stage 建群以后不能改")
	}
	// inner 可以随时开关
	if _, err := e.a.UpdateRoom(info.ID, RoomPatch{ScenePatch: ScenePatch{Inner: &no}}); err != nil {
		t.Fatal(err)
	}
	// 两个 agent 的私聊不能是戏
	dm, two := true, []string{agents.DefaultID, "bob"}
	if _, err := e.a.CreateRoom(RoomPatch{Members: &two, DM: &dm, ScenePatch: ScenePatch{Stage: &yes}}); err == nil {
		t.Fatal("私聊不能是戏")
	}
}

// 不开 stage 的群：没有场景记录文件，也没有 stage 的任何痕迹。
func TestStageOffLeavesNoTrace(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, scene.Rules{Mode: scene.ModeTurns, Rounds: 1}, m)
	e.a.RoomSay(id, "开始", "张三", "", "我")
	runRounds(t, e, id, 1, "default", "bob", "cat")
	if _, _, err := e.a.StageTimeline(id, 0, 10); err == nil {
		t.Fatal("不是戏的群没有时间线")
	}
	e.a.mu.Lock()
	n := len(e.a.stages)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("不该为普通群建场景记录：%d", n)
	}
}

// 记录：每一句、每一轮、每次模型调用都有，Line 和群记录逐条对应，前缀只记一次全文。
func TestStageTimelineRecordsEverything(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, log := turnRoom(t, e, stageRules(scene.Rules{Order: []string{"default", "bob", "cat"}}), m)
	e.a.RoomSay(id, "夜深了", "旁白", "", "我")
	runRounds(t, e, id, 2, "default", "bob", "cat")

	recs := timeline(t, e, id)
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("序号要连续：第 %d 条是 %d", i+1, r.Seq)
		}
	}
	// 群记录逐条对应
	logLines := lines(e, log)
	lineRecs := kindsOf(recs, "line")
	if len(lineRecs) != len(logLines) {
		t.Fatalf("群记录 %d 条，场景记录里 %d 句\n%s", len(logLines), len(lineRecs), strings.Join(logLines, "\n"))
	}
	for i, r := range lineRecs {
		if r.Line != i+1 {
			t.Fatalf("Line 要从 1 连续：%d 是 %d", i+1, r.Line)
		}
		if !strings.HasSuffix(logLines[i], r.Text) {
			t.Fatalf("第 %d 句对不上：群记录 %q，场景记录 %q", i+1, logLines[i], r.Text)
		}
	}
	if lineRecs[0].Say != "narrate" || lineRecs[0].From != "" || lineRecs[0].Name != "旁白" {
		t.Fatalf("第一句是旁白：%+v", lineRecs[0])
	}
	if lineRecs[1].Say != "say" || lineRecs[1].From != "default" {
		t.Fatalf("第二句是小纳说的：%+v", lineRecs[1])
	}
	if last := lineRecs[len(lineRecs)-1]; last.Say != "system" {
		t.Fatalf("最后是暂停的系统提示：%+v", last)
	}
	// 每一轮：6 轮 run；每次调用：6 条 model，每个 run 一条
	runs, models := kindsOf(recs, "run"), kindsOf(recs, "model")
	if len(runs) != 6 || len(models) != 6 {
		t.Fatalf("run %d 条、model %d 条，应各 6 条", len(runs), len(models))
	}
	for i, r := range runs {
		if r.Status != "ok" || r.Said == 0 || r.Usage == nil || r.Usage.PromptTokens != 10 {
			t.Fatalf("run %d：%+v", i, r)
		}
		if r.Said > len(lineRecs) || lineRecs[r.Said-1].From != r.Member {
			t.Fatalf("run %d 说出口的那句对不上：%+v", i, r)
		}
		if models[i].Run != r.Run || models[i].Model == nil || models[i].Model.Out != r.Member+"说第"+string(rune('1'+i/3))+"句" {
			t.Fatalf("model %d：%+v", i, models[i].Model)
		}
	}
	// 前缀：三个成员三份，只记一次，时间线里只给哈希和长度，全文用接口取
	prefixes := kindsOf(recs, "prefix")
	if len(prefixes) != 3 {
		t.Fatalf("三个成员的前缀各记一次，实际 %d 条", len(prefixes))
	}
	for _, p := range prefixes {
		if p.Body != "" || p.Len == 0 || p.Hash == "" {
			t.Fatalf("prefix 在时间线里只给哈希和长度：%+v", p)
		}
		body, err := e.a.StagePrefix(id, p.Hash)
		if err != nil || len(body) != p.Len || hashHex([]byte(body)) != p.Hash {
			t.Fatalf("前缀全文：err=%v len=%d/%d", err, len(body), p.Len)
		}
		if !strings.Contains(body, "== tools ==") {
			t.Fatal("前缀全文要带工具定义")
		}
	}
	seen := map[string]bool{}
	for _, mr := range models {
		if !hasHash(prefixes, mr.Model.Prefix) || mr.Model.Input == "" || mr.Model.Reason != "final" {
			t.Fatalf("model 记录缺东西：%+v", mr.Model)
		}
		seen[mr.Model.Input] = true
	}
	// 增量取
	more, last, err := e.a.StageTimeline(id, uint64(len(recs)-2), 10)
	if err != nil || len(more) != 2 || last != uint64(len(recs)) {
		t.Fatalf("增量：%d 条 last=%d err=%v", len(more), last, err)
	}
}

func hasHash(ps []StageRec, h string) bool {
	for _, p := range ps {
		if p.Hash == h {
			return true
		}
	}
	return false
}

// Cursors：这一句送达之后每个成员会话的消息数，和会话的实际长度一致；频道里的话只有频道成员的会话多一条。
func TestStageCursorsMatchMemberSessions(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	actual := func() map[string]int {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		out := map[string]int{}
		for _, m := range room.Members {
			out[m] = e.a.histEnd(e.a.roomMemberSID(m, room.ID))
		}
		return out
	}
	lastCursors := func() map[string]int {
		cs := kindsOf(timeline(t, e, room.ID), "cursors")
		if len(cs) == 0 {
			t.Fatal("没有 cursors 记录")
		}
		return cs[len(cs)-1].Cursors
	}
	check := func(what string, wantGrew map[string]int, before map[string]int) {
		t.Helper()
		got, now := lastCursors(), actual()
		for m, n := range now {
			if got[m] != n {
				t.Fatalf("%s：%s 的 cursor %d，会话实际 %d", what, m, got[m], n)
			}
			if n-before[m] != wantGrew[m] {
				t.Fatalf("%s：%s 的会话多了 %d 条，应为 %d", what, m, n-before[m], wantGrew[m])
			}
		}
	}
	b0 := actual()
	if err := e.a.RoomSay(room.ID, "大家好", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	check("公开的话", map[string]int{"default": 1, "bob": 1, "cat": 1}, b0)
	b1 := actual()
	if err := e.a.RoomSay(room.ID, "只告诉你们", "张三", "s", "我"); err != nil {
		t.Fatal(err)
	}
	check("频道里的话", map[string]int{"default": 0, "bob": 1, "cat": 1}, b1)
	// cue：只有对象的会话多一条
	prev := lastCursors()
	if err := e.a.RoomCue(room.ID, "bob", "走到窗边", ""); err != nil {
		t.Fatal(err)
	}
	got := lastCursors()
	for _, m := range room.Members {
		want := prev[m]
		if m == "bob" {
			want++
		}
		if got[m] != want {
			t.Fatalf("cue：%s 的 cursor %d，应为 %d", m, got[m], want)
		}
	}
	step(t, e, e.a.roomMemberSID("bob", room.ID)) // 把被叫起来的这一轮走完
}

// 写不进去：这一句不发，群暂停，原因只写群记录，健康状态变化。
func TestStageWriteFailurePausesAndDoesNotSend(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	if err := e.a.RoomSay(room.ID, "第一句", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	st, _ := e.a.stageFor(roomLogSID(room.ID))
	st.j.Close() // 之后所有追加都失败
	e.a.mu.Unlock()
	if err := e.a.RoomSay(room.ID, "第二句", "张三", "", "我"); err != nil {
		t.Fatal(err)
	}
	if p, _ := paused(e, room.ID); !p {
		t.Fatal("写不进去要暂停这一场")
	}
	log := lines(e, roomLogSID(room.ID))
	if len(log) != 2 || log[0] != "[张三] 第一句" || !strings.HasPrefix(log[1], "[系统] 场景记录写不进去") {
		t.Fatalf("群记录里不该有第二句，只有暂停提示：%q", log)
	}
	for _, m := range room.Members {
		if has(e, e.a.roomMemberSID(m, room.ID), "[张三] 第二句") {
			t.Fatalf("%s 不该收到没记下来的话", m)
		}
	}
	degraded := false
	for _, h := range e.a.health.Snapshot() {
		if h.Name == "stage" && h.State != "ok" {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("健康状态 stage 要变：%+v", e.a.health.Snapshot())
	}
	// 场景记录里没有暂停提示，也没有第二句
	for _, r := range kindsOf(timeline(t, e, room.ID), "line") {
		if strings.Contains(r.Text, "第二句") || r.Say == "system" {
			t.Fatalf("场景记录里不该有：%+v", r)
		}
	}
}

// 导演的操作：暂停、恢复、禁言、改规则、清零都记成 op。
func TestStageRecordsDirectorOps(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	yes, no := true, false
	muted, brief := []string{"cat"}, "改了"
	for _, p := range []RoomPatch{{Paused: &yes}, {Paused: &no}, {Muted: &muted}, {ScenePatch: ScenePatch{Brief: &brief}}, {ScenePatch: ScenePatch{Reset: true}}} {
		if _, err := e.a.UpdateRoom(room.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	var ops []string
	for _, r := range kindsOf(timeline(t, e, room.ID), "op") {
		ops = append(ops, r.Op)
	}
	if strings.Join(ops, ",") != "pause,resume,mute,rules,reset" {
		t.Fatalf("操作记录：%v", ops)
	}
}

func TestStageInnerThoughts(t *testing.T) {
	e := setup(t)
	replies := map[string]string{
		"default": "<心里>他在撒谎</心里>我同意你的看法",
		"bob":     "<心里>这事和我无关</心里>[不回复]",
		"cat":     "没按格式，直接说话",
	}
	m := &scriptModel{reply: func(who string, n int) (string, error) { return replies[who], nil }}
	id, log := turnRoom(t, e, stageRules(scene.Rules{Inner: true, Order: []string{"default", "bob", "cat"}, Rounds: 1}), m)
	e.a.RoomSay(id, "是谁干的", "张三", "", "我")
	runRounds(t, e, id, 1, "default", "bob", "cat")

	got := strings.Join(lines(e, log), "\n")
	if strings.Contains(got, "撒谎") || strings.Contains(got, "心里") || strings.Contains(got, "无关") {
		t.Fatalf("群记录里不能有心里话：\n%s", got)
	}
	if !strings.Contains(got, "[小纳] 我同意你的看法") || !strings.Contains(got, "[小猫] 没按格式，直接说话") || strings.Contains(got, "[小鲍]") {
		t.Fatalf("说出口的部分：\n%s", got)
	}
	// 说话者自己的会话保留完整回复；别人的会话里没有心里话
	own := strings.Join(lines(e, e.a.roomMemberSID("default", id)), "\n")
	if !strings.Contains(own, "他在撒谎") && !historyHas(e, e.a.roomMemberSID("default", id), "<心里>他在撒谎</心里>我同意你的看法") {
		t.Fatalf("自己的会话要记得想过什么：\n%s", own)
	}
	for _, who := range []string{"bob", "cat"} {
		sid := e.a.roomMemberSID(who, id)
		if historyHas(e, sid, "他在撒谎") && who != "default" {
			t.Fatalf("%s 的会话里不该有别人的心里话", who)
		}
		for _, l := range lines(e, sid) {
			if strings.Contains(l, "撒谎") {
				t.Fatalf("%s 的会话里不该有别人的心里话：%q", who, l)
			}
		}
	}
	runs := kindsOf(timeline(t, e, id), "run")
	if len(runs) != 3 {
		t.Fatalf("三轮：%d", len(runs))
	}
	if runs[0].Inner != "他在撒谎" || runs[0].Said == 0 {
		t.Fatalf("小纳：%+v", runs[0])
	}
	if runs[1].Inner != "这事和我无关" || runs[1].Said != 0 {
		t.Fatalf("回 [不回复] 也要记心里话，且没说：%+v", runs[1])
	}
	if runs[2].Inner != "" || runs[2].Said == 0 {
		t.Fatalf("没按格式整段算说出口：%+v", runs[2])
	}
	// 系统前缀里有固定的说明
	e.a.mu.Lock()
	system, _, _, _ := e.a.contextParts(e.a.roomMemberSID("default", id))
	e.a.mu.Unlock()
	if !strings.Contains(system, innerGuide) {
		t.Fatal("系统前缀里要有心里话的说明")
	}
}

func TestSplitInner(t *testing.T) {
	long := strings.Repeat("长", 300)
	for _, c := range []struct{ in, inner, said string }{
		{"<心里>想</心里>说", "想", "说"},
		{"  <心里>想</心里>\n\n说\n第二行", "想", "说\n第二行"},
		{"<心里>想</心里>[不回复]", "想", "[不回复]"},
		{"<心里>想</心里>", "想", ""},
		{"没有格式", "", "没有格式"},
		{"说了<心里>想</心里>", "", "说了<心里>想</心里>"},
		{"<心里>没写完", "", "<心里>没写完"},
	} {
		inner, said := splitInner(c.in)
		if inner != c.inner || said != c.said {
			t.Errorf("%q → %q / %q，应为 %q / %q", c.in, inner, said, c.inner, c.said)
		}
	}
	if inner, _ := splitInner("<心里>" + long + "</心里>说"); len([]rune(inner)) != 200 {
		t.Errorf("心里话最多 200 字：%d", len([]rune(inner)))
	}
}

// 隔离：场内的事不进经历、不进整理记忆、请求里没有记忆说明；场外的 Message 进不来；工具只有联网的两个。
func TestStageIsolation(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	if err := os.WriteFile(filepath.Join(e.dir, "agents", "default", "self.md"), []byte("我叫小纳，喜欢看星星。"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, stageRules(scene.Rules{Order: []string{"default", "bob", "cat"}, Rounds: 1}), m)
	control, _ := e.a.CreateRoom(RoomPatch{Name: strptr("对照"), Members: &[]string{"default", "bob", "cat"},
		ScenePatch: ScenePatch{Mode: strptr("turns"), Rounds: intptr(1)}})
	for _, aid := range []string{"default", "bob", "cat"} {
		ag, _ := e.a.agentStore.Get(aid)
		ag.Style = agents.StyleLife
		if err := e.a.agentStore.Update(ag); err != nil {
			t.Fatal(err)
		}
	}
	e.a.RoomSay(id, "开始", "张三", "", "我")
	runRounds(t, e, id, 1, "default", "bob", "cat")
	e.a.RoomSay(control.ID, "开始", "张三", "", "我")
	runRounds(t, e, control.ID, 1, "default", "bob", "cat")

	for _, rid := range []string{id, control.ID} { // 暂停了的群不在聊天对象里：恢复了再看
		if _, err := e.a.UpdateRoom(rid, RoomPatch{Paused: boolptr(false)}); err != nil {
			t.Fatal(err)
		}
	}
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	// 经历：同样的一轮（有人问、有回答），戏里的不记，对照群的记
	for _, rid := range []string{id, control.ID} {
		sid := e.a.roomMemberSID("default", rid)
		e.a.recordRunExperience(e.a.sessOf(sid), &run{runID: "r-" + rid, asked: "你好吗", from: "admin"}, "ok", "我很好")
	}
	for _, aid := range []string{"default", "bob", "cat"} {
		inStage, inControl := false, false
		for _, en := range e.a.experienceOf(aid).Recent(knowledge.Viewer{Admin: true}, "", 100, e.a.now()) {
			inStage = inStage || strings.Contains(en.SID, "room:"+id)
			inControl = inControl || strings.Contains(en.SID, "room:"+control.ID)
		}
		if inStage {
			t.Errorf("%s：戏里的事不能进经历", aid)
		}
		if aid == "default" && !inControl {
			t.Error("对照：普通群的一轮要记进经历")
		}
		for _, src := range e.a.diarySources(aid) {
			if strings.Contains(src.SID, "room:"+id) {
				t.Errorf("%s：整理记忆不能读戏：%s", aid, src.SID)
			}
		}
	}
	// 请求里没有记忆说明
	sid := e.a.roomMemberSID("default", id)
	st := session.Settings{} // 空白的设置：什么都没放过
	dirty := false
	if got := e.a.memoryNotes(sid, "room:default", "开始", &st, &dirty); got != "" || dirty {
		t.Errorf("戏的请求里不该有记忆说明：%q", got)
	}
	csid := e.a.roomMemberSID("default", control.ID)
	cst := session.Settings{}
	if got := e.a.memoryNotes(csid, "room:default", "开始", &cst, &dirty); got == "" {
		t.Error("对照：普通群的请求有记忆说明")
	}
	// 工具：戏里没有设就一个都不给；对照有
	_, tools, _, _ := e.a.contextParts(sid)
	if len(tools) != 0 {
		t.Errorf("戏里 tools 不设就不给工具：%v", tools)
	}
	_, ctools, _, _ := e.a.contextParts(csid)
	if len(ctools) == 0 {
		t.Error("对照：普通群有默认工具")
	}
	// 内心活动的聊天对象里没有这场戏
	ag, _ := e.a.agentStore.Get("default")
	hasRoom := func(name string) bool {
		for _, tg := range e.a.lifeTargets(ag) {
			if strings.Contains(tg.To, name) {
				return true
			}
		}
		return false
	}
	if hasRoom("room:群") {
		t.Error("内心活动不能找到戏")
	}
	if !hasRoom("room:对照") {
		t.Error("对照：普通群在聊天对象里")
	}
}

// Message 工具不能发进戏里，谁发都拒绝；对照群可以。
func TestStageRejectsMessageTool(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	ok, msg := false, ""
	done := make(chan struct{})
	go func() {
		e.a.messageTool(e.a.roomMemberSID("bob", room.ID), `{"to":"room:群","text":"偷偷说一句"}`, func(o bool, m string) { ok, msg = o, m; close(done) })
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Message 没有回应")
	}
	if ok || !strings.Contains(msg, "只能在导演台说话") {
		t.Fatalf("应该被拒绝：%v %q", ok, msg)
	}
	if has(e, roomLogSID(room.ID), "[小鲍] 偷偷说一句") {
		t.Fatal("被拒绝的话不能进群记录")
	}
	if got := kindsOf(timeline(t, e, room.ID), "line"); len(got) != 0 {
		t.Fatalf("被拒绝的话不能进场景记录：%+v", got)
	}
}

var _ = errors.New

// ---- 重放驱动（只在测试里）----
//
// 按时间线，把人和旁白的话在同样的位置说出，模型调用按记录的输出回答，成员的轮次按 run 记录的顺序走完。

// replayModel 按成员依次给出记录里的回复。
func replayModel(recs []StageRec) *scriptModel {
	outs := map[string][]string{}
	for _, r := range kindsOf(recs, "model") {
		outs[r.Member] = append(outs[r.Member], r.Model.Out)
	}
	return &scriptModel{reply: func(who string, n int) (string, error) {
		if n > len(outs[who]) {
			return "", errors.New("记录里没有这么多次调用：" + who)
		}
		return outs[who][n-1], nil
	}}
}

// replayStage 在一个已经建好同样的群的环境里重放时间线。
func replayStage(t *testing.T, e *env, roomID string, recs []StageRec) {
	t.Helper()
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r.Kind != "line" || r.From != "" || r.Say == "system" {
			continue
		}
		switch r.Say {
		case "say", "narrate":
			as := r.Name
			if r.Say == "narrate" {
				as = "旁白"
			}
			if err := e.a.RoomSay(roomID, r.Text, as, r.Channel, "我"); err != nil {
				t.Fatal(err)
			}
		case "cue":
			if err := e.a.RoomCue(roomID, r.To, r.Text, r.Channel); err != nil {
				t.Fatal(err)
			}
		}
		// 到下一句人话之前，按 run 记录的顺序把成员的轮次走完
		for j := i + 1; j < len(recs); j++ {
			if recs[j].Kind == "line" && recs[j].From == "" && recs[j].Say != "system" {
				break
			}
			if recs[j].Kind == "run" {
				step(t, e, e.a.roomMemberSID(recs[j].Member, roomID))
			}
		}
	}
}

func TestStageReplayReproducesTheScene(t *testing.T) {
	rules := stageRules(scene.Rules{Order: []string{"cat", "default", "bob"}, Rounds: 2, Inner: true})
	play := func(replay []StageRec) (*env, string, []StageRec) {
		e := setup(t)
		var m *scriptModel
		if replay == nil {
			m = &scriptModel{reply: func(who string, n int) (string, error) {
				return "<心里>" + who + "在想第" + string(rune('0'+n)) + "件事</心里>" + who + "说第" + string(rune('0'+n)) + "句", nil
			}}
		} else {
			m = replayModel(replay)
		}
		id, _ := turnRoom(t, e, rules, m)
		if replay == nil {
			e.a.RoomSay(id, "先和你们几个说件事", "张三", "s", "我")
			e.a.RoomSay(id, "夜深了", "旁白", "", "我")
			runRounds(t, e, id, 2, "cat", "default", "bob")
		} else {
			replayStage(t, e, id, replay)
			waitFor(t, func() bool { p, _ := paused(e, id); return p })
		}
		return e, id, timeline(t, e, id)
	}
	e1, id1, rec1 := play(nil)
	e2, id2, rec2 := play(rec1)

	if a, b := roomLog(e1, roomLogSID(id1)), roomLog(e2, roomLogSID(id2)); a != b || a == "" {
		t.Fatalf("重放出来的群记录要逐字相同：\n--- 原场\n%s\n--- 重放\n%s", a, b)
	}
	m1, m2 := kindsOf(rec1, "model"), kindsOf(rec2, "model")
	if len(m1) != 6 || len(m2) != 6 {
		t.Fatalf("模型调用 %d / %d 次", len(m1), len(m2))
	}
	for i := range m1 {
		a, b := m1[i].Model, m2[i].Model
		if a.Input != b.Input || a.Prefix != b.Prefix || a.Out != b.Out || m1[i].Member != m2[i].Member {
			t.Fatalf("第 %d 次调用不一样：\n%+v\n%+v", i+1, a, b)
		}
	}
	// 心里话也一样
	r1, r2 := kindsOf(rec1, "run"), kindsOf(rec2, "run")
	for i := range r1 {
		if r1[i].Inner != r2[i].Inner || r1[i].Said != r2[i].Said || r1[i].Inner == "" {
			t.Fatalf("第 %d 轮：%+v / %+v", i+1, r1[i], r2[i])
		}
	}
}
