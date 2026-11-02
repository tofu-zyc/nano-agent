package app

import (
	"context"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/autonomy"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
)

// extGroup 建一个外部群会话（fake:c1 里有人说了话），返回会话 ID。
func extGroup(t *testing.T, e *env) string {
	t.Helper()
	e.sid = "fake:c1"
	e.say("大家好", group(false), from("u2", "李四"))
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	for _, s := range e.a.sessions {
		if s != nil && e.a.store.Settings(s.sid).Group {
			return s.sid
		}
	}
	t.Fatal("没有外部群会话")
	return ""
}

func setScene(t *testing.T, e *env, sid string, r *scene.Rules) {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	st := e.a.store.Settings(sid)
	st.Scene = r
	if err := e.a.store.SaveSettings(sid, st); err != nil {
		t.Fatal(err)
	}
}

func strptr(s string) *string { return &s }

// 旧格式的 rooms.json（没有任何规则字段）读入后，生效规则等于现在的行为。
func TestOldRoomsJSONKeepsBehavior(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	path := filepath.Join(e.dir, "data", "rooms.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	old := `[{"id":"r1","name":"老群","members":["default"],"mode":"free","budget":500,"paused":true,"muted":["x"],"created":1}]`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := rooms.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	r, ok := st.Get("r1")
	if !ok || r.Mode != "free" || r.Budget != 500 || !r.Paused || r.Brief != "" || r.Tools != nil || len(r.Roles) != 0 {
		t.Fatalf("旧格式读入：%+v", r)
	}
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	sid := e.a.roomMemberSID("default", "r1")
	sc, ok := e.a.sceneOf(sid)
	if !ok || sc.Kind != scene.Room || !sc.free() {
		t.Fatalf("场景：%+v %v", sc, ok)
	}
	run, pp := sc.guard()
	if run != 8 || pp != 6 {
		t.Fatalf("防刷屏默认值：%d %d", run, pp)
	}
	if rule := e.a.groupRuleOf(sid); rule.Quiet != 20*time.Second || rule.MinGap != 10*time.Second || rule.MaxLen != 300 || rule.Parts != 3 {
		t.Fatalf("说话样式默认值：%+v", rule)
	}
	if e.a.sceneText(sid) != "" {
		t.Fatal("没设规则就没有场景内容")
	}
}

// 没设模式时：内部群点名，外部群自由。
func TestSceneDefaultMode(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	room, _ := st.Create(rooms.Room{Name: "群", Members: []string{"default"}})
	room.Mode = "" // 旧数据里没有 mode
	st.Update(room)
	ext := extGroup(t, e)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	rsc, _ := e.a.sceneOf(e.a.roomMemberSID("default", room.ID))
	esc, _ := e.a.sceneOf(ext)
	if rsc.Kind != scene.Room || rsc.free() || esc.Kind != scene.External || !esc.free() {
		t.Fatalf("默认模式：内部群 %v/%v 外部群 %v/%v", rsc.Kind, rsc.free(), esc.Kind, esc.free())
	}
	cur := e.a.store.Settings(ext)
	cur.Scene = &scene.Rules{Mode: scene.ModeMention}
	e.a.store.SaveSettings(ext, cur)
	if sc, _ := e.a.sceneOf(ext); sc.free() {
		t.Fatal("外部群设成 mention 就只在被点名时说话")
	}
}

// 生效顺序：积极度。
func TestSceneEagernessOrder(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	ext := extGroup(t, e)
	eager := func() string {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return e.a.groupRuleOf(ext).Eagerness
	}
	if got := eager(); got != "off" {
		t.Fatalf("工具型 agent 默认不主动插话：%s", got)
	}
	setScene(t, e, ext, &scene.Rules{Roles: map[string]scene.Role{agents.DefaultID: {Eagerness: "normal"}}})
	if got := eager(); got != "normal" {
		t.Fatalf("工具型 agent 在场景角色里明确设了才会插话：%s", got)
	}
	setScene(t, e, ext, nil)
	makeLife(t, e, store, nil)
	if got := eager(); got != "low" {
		t.Fatalf("拟人型默认 low：%s", got)
	}
	makeLife(t, e, store, func(a *agents.Agent) { a.GroupEagerness = strptr("high") })
	if got := eager(); got != "high" {
		t.Fatalf("agent 的群聊积极度：%s", got)
	}
	setScene(t, e, ext, &scene.Rules{Roles: map[string]scene.Role{agents.DefaultID: {Eagerness: "off"}}})
	if got := eager(); got != "off" {
		t.Fatalf("场景角色优先：%s", got)
	}
	setScene(t, e, ext, &scene.Rules{Roles: map[string]scene.Role{"别人": {Eagerness: "off"}}})
	if got := eager(); got != "high" {
		t.Fatalf("别的 agent 的角色不影响：%s", got)
	}
}

// 生效顺序：说话样式。
func TestSceneStyleOrder(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	ext := extGroup(t, e)
	makeLife(t, e, store, func(a *agents.Agent) {
		a.GroupQuiet = strptr("40s")
		n := 120
		a.GroupMaxLen = &n
	})
	rule := func() groupRule {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return e.a.groupRuleOf(ext)
	}
	if r := rule(); r.Quiet != 40*time.Second || r.MaxLen != 120 || r.MinGap != 10*time.Second || r.Parts != 3 {
		t.Fatalf("agent 的设置：%+v", r)
	}
	setScene(t, e, ext, &scene.Rules{Style: scene.Style{Quiet: "5s", Parts: 2}})
	if r := rule(); r.Quiet != 5*time.Second || r.MaxLen != 120 || r.Parts != 2 {
		t.Fatalf("场景只覆盖设了的项：%+v", r)
	}
}

// 生效顺序：工具。
func TestSceneToolOrder(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	ext := extGroup(t, e)
	names := func() []string {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		_, tools, _, _ := e.a.contextParts(ext)
		var out []string
		for _, tl := range tools {
			out = append(out, tl.Function.Name)
		}
		slices.Sort(out)
		return out
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s：%v，应为 %v", what, got, want)
		}
	}
	eq("默认表（去掉拿不到的）", names(), "Knowledge", "SendFile")
	addSkill(t, e, "s1")
	eq("有技能了", names(), "Knowledge", "SendFile", "Skill")
	setScene(t, e, ext, &scene.Rules{Tools: []string{}})
	eq("场景 []：不给工具", names())
	setScene(t, e, ext, &scene.Rules{Tools: []string{"Skill"}})
	eq("只有场景", names(), "Skill")
	e.a.mu.Lock()
	ag, _ := store.Get(agents.DefaultID)
	ag.GroupTools = []string{"SendFile", "Skill"}
	store.Update(ag)
	e.a.mu.Unlock()
	setScene(t, e, ext, nil)
	eq("agent 的群聊工具", names(), "SendFile", "Skill")
	setScene(t, e, ext, &scene.Rules{Tools: []string{"Skill", "WebSearch"}})
	eq("场景和 agent 取交集", names(), "Skill")
}

// 生效顺序：防刷屏。场景里设了阈值就按它暂停。
func TestSceneGuardOrder(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	room, _ := st.Create(rooms.Room{Name: "群", Members: []string{"default"}, Rules: scene.Rules{Guard: scene.Guard{Run: 3}}})
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	e.a.roomGuard(room, "default")
	e.a.roomGuard(room, "default")
	if r, _ := st.Get(room.ID); r.Paused {
		t.Fatal("两条不该暂停")
	}
	e.a.roomGuard(room, "default")
	if r, _ := st.Get(room.ID); !r.Paused {
		t.Fatal("连续 3 条（场景里设的）该暂停")
	}
	room2, _ := st.Create(rooms.Room{Name: "群2", Members: []string{"default"}})
	for i := 0; i < 7; i++ {
		e.a.roomGuard(room2, "default")
	}
	if r, _ := st.Get(room2.ID); r.Paused {
		t.Fatal("默认是 8 条，7 条不该暂停")
	}
	e.a.roomGuard(room2, "default")
	if r, _ := st.Get(room2.ID); !r.Paused {
		t.Fatal("默认 8 条该暂停")
	}
}

// 场景说明和角色提示词紧跟群聊守则；场景工具表之外的工具，模型提出也不执行。
func TestSceneTextAndToolGate(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	ext := extGroup(t, e)
	setScene(t, e, ext, &scene.Rules{Brief: "这是一场谋杀之夜", Roles: map[string]scene.Role{agents.DefaultID: {Prompt: "你是管家"}}})
	e.a.mu.Lock()
	system, _, _, _ := e.a.contextParts(ext)
	e.a.mu.Unlock()
	i, j, k := strings.Index(system, "这是群聊，不是私聊"), strings.Index(system, "[场景] 这是一场谋杀之夜"), strings.Index(system, "[你的角色] 你是管家")
	if i < 0 || j < i || k < j {
		t.Fatalf("顺序应是群聊守则、场景、角色：%d %d %d\n%s", i, j, k, system)
	}
	// 场景工具表里没有的工具（Message），模型提出也不执行
	e.a.SetLLM(&proposeModel{name: "Message", args: messageArgs})
	e.sid = "fake:c1"
	e.say("@bot 去告诉主会话", group(true), from("u1", "张三"))
	in := e.k.expect(proto.TInput)
	expectUnoffered(t, e, in, "Message")
}

func mkRoom(t *testing.T, e *env, r scene.Rules) (*rooms.Store, rooms.Room) {
	t.Helper()
	withAgents(t, e)
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	room, err := st.Create(rooms.Room{Name: "群", Members: []string{"default"}, Rules: r})
	if err != nil {
		t.Fatal(err)
	}
	return st, room
}

// 预算池：内部群超过当天或这一场的预算就暂停并写明原因；清零累计；重启前后一致；删群删条目。
func TestSceneBudgetRoom(t *testing.T) {
	e := setup(t)
	st, room := mkRoom(t, e, scene.Rules{Budget: 100, BudgetTotal: 1000})
	sid := e.a.roomMemberSID("default", room.ID)
	e.a.mu.Lock()
	e.a.sceneUsage(sid, 60)
	if r, _ := st.Get(room.ID); r.Paused {
		t.Fatal("60/100 不该暂停")
	}
	e.a.sceneUsage(e.a.roomMemberSID("default", room.ID), 50) // 同一个场景的别的会话也记进来
	r, _ := st.Get(room.ID)
	e.a.mu.Unlock()
	if !r.Paused {
		t.Fatal("110 > 100 该暂停")
	}
	var why string
	for _, m := range e.a.store.History(roomLogSID(room.ID)) {
		if strings.Contains(m.Content, "今天的 token 用量到了预算（100）") {
			why = m.Content
		}
	}
	if why == "" {
		t.Fatal("群记录里要写明原因")
	}

	// 重启前后累计一致
	e.a.mu.Lock()
	before := *e.a.sceneUses[roomLogSID(room.ID)]
	e.a.mu.Unlock()
	if before.DayTokens != 110 || before.Total != 110 || before.Since == 0 {
		t.Fatalf("用量：%+v", before)
	}
	a2 := newAppAt(t, e.dir, nil)
	a2.mu.Lock()
	after := *a2.sceneUses[roomLogSID(room.ID)]
	a2.mu.Unlock()
	if after != before {
		t.Fatalf("重启后：%+v，应为 %+v", after, before)
	}

	// 清零累计
	e.a.mu.Lock()
	e.a.sceneResetTotal(roomLogSID(room.ID))
	got := *e.a.sceneUses[roomLogSID(room.ID)]
	e.a.mu.Unlock()
	if got.Total != 0 || got.DayTokens != 110 {
		t.Fatalf("清零只清累计：%+v", got)
	}

	// 删群删条目
	if err := e.a.DeleteRoom(room.ID); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	_, left := e.a.sceneUses[roomLogSID(room.ID)]
	e.a.mu.Unlock()
	if left {
		t.Fatal("删群应删掉用量条目")
	}
}

func TestSceneBudgetTotalPausesRoom(t *testing.T) {
	e := setup(t)
	st, room := mkRoom(t, e, scene.Rules{BudgetTotal: 100})
	e.a.mu.Lock()
	e.a.sceneUsage(e.a.roomMemberSID("default", room.ID), 101)
	e.a.mu.Unlock()
	if r, _ := st.Get(room.ID); !r.Paused {
		t.Fatal("累计超了该暂停")
	}
}

// 外部群超额：当天不再调用模型，被点名时一天最多回一次固定说明；第二天恢复。
func TestSceneBudgetExternalGroup(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	ext := extGroup(t, e)
	setScene(t, e, ext, &scene.Rules{Budget: 100})
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 100)
	e.a.mu.Unlock()
	e.sid = "fake:c1"
	e.say("@bot 在吗", group(true), from("u2", "李四"), func(m *channel.Message) { m.MsgID = "m2" })
	e.ch.wait(t, "今天的额度用完了")
	e.k.none() // 没有调用模型
	e.say("@bot 还在吗", group(true), from("u2", "李四"), func(m *channel.Message) { m.MsgID = "m3" })
	e.k.none()
	select {
	case got := <-e.ch.sent:
		t.Fatalf("一天只回一次说明：%q", got)
	case <-time.After(200 * time.Millisecond):
	}
	setNow(e, e.a.now().Add(24*time.Hour))
	e.say("@bot 早", group(true), from("u2", "李四"), func(m *channel.Message) { m.MsgID = "m4" })
	e.k.expect(proto.TInput)
}

// observeModel：被“群里安静了”提醒触发的那一轮提出调用并带话；其余（被点名）照常回复。
type observeModel struct{}

func (observeModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	last := req.Messages[len(req.Messages)-1].Content
	if strings.Contains(last, "群里安静了一会儿") {
		return &llm.Result{Reason: llm.ReasonToolUse, Content: "本来想插的一句话",
			Calls: []llm.ToolCall{{ID: "o1", Type: "function", Function: llm.FunctionCall{Name: "SendFile", Arguments: `{"path":"a.txt"}`}}},
			Usage: llm.Usage{PromptTokens: 30, CompletionTokens: 7}}, nil
	}
	return &llm.Result{Reason: llm.ReasonFinal, Content: "被点名的回复", Usage: llm.Usage{PromptTokens: 20, CompletionTokens: 5}}, nil
}

func observeLines(t *testing.T, e *env) []ObserveRecord {
	t.Helper()
	return e.a.Observe("", 100, true)
}

func historyHas(e *env, sid, text string) bool {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	for _, m := range e.a.store.History(sid) {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

// observe 档，外部群：主动插话那一轮照常请求模型，没有发出、历史里没有、工具没执行，observe.jsonl 里有记录；被点名照常回复。
func TestObserveExternalGroup(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	e.a.SetLLM(observeModel{})
	sid := extGroup(t, e)
	makeLife(t, e, store, nil)
	e.a.mu.Lock()
	e.a.rnd = func() float64 { return 0 }
	base := e.a.now()
	e.a.now = func() time.Time { return base.Add(25 * time.Second) }
	e.a.mu.Unlock()
	setScene(t, e, sid, &scene.Rules{Observe: true})
	e.a.groupTick()
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonFinal || len(turn.Calls) != 0 {
		t.Fatalf("observe 的那一轮不能把工具调用交给内核：%+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	select {
	case got := <-e.ch.sent:
		t.Fatalf("observe 不该发出：%q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if historyHas(e, sid, "本来想插的一句话") {
		t.Fatal("回复不该写进会话历史")
	}
	for _, m := range e.a.store.History(sid) {
		if m.Role == "assistant" {
			t.Fatalf("历史里不该有助手消息：%+v", m)
		}
	}
	recs := observeLines(t, e)
	if len(recs) != 1 || recs[0].Say != "本来想插的一句话" || len(recs[0].Tools) != 1 || recs[0].Tools[0].Name != "SendFile" ||
		recs[0].Why.QuietSec < 25 || recs[0].Why.Pending != 1 || recs[0].PromptTokens != 30 || recs[0].Scene != sid {
		t.Fatalf("observe 记录：%+v", recs)
	}
	// 用量记进预算池
	e.a.mu.Lock()
	use := e.a.sceneUses[sid].Total
	e.a.mu.Unlock()
	if use != 37 {
		t.Fatalf("observe 的用量也要计入：%d", use)
	}
	// 被点名照常回复
	e.say("@bot 在吗", group(true), from("u2", "李四"), func(m *channel.Message) { m.MsgID = "m9" })
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	e.ch.wait(t, "被点名的回复")
}

// observe 档，内部群自由模式：不进群记录和成员历史。
func TestObserveRoomFree(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	e.a.SetLLM(observeModel{})
	makeLife(t, e, store, nil)
	st, room := mkRoom(t, e, scene.Rules{Mode: scene.ModeFree, Observe: true})
	_ = st
	member := e.a.roomMemberSID("default", room.ID)
	e.a.mu.Lock()
	r, _ := e.a.roomStore.Get(room.ID)
	e.a.roomHumanSay(r, "李四", "大家好")
	e.a.rnd = func() float64 { return 0 }
	base := e.a.now()
	e.a.now = func() time.Time { return base.Add(25 * time.Second) }
	e.a.mu.Unlock()
	e.a.groupTick()
	in := e.k.expect(proto.TInput)
	if in.SID != member {
		t.Fatalf("应该由成员会话跑：%q", in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: member})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: member, Status: proto.StatusOK})
	waitIdle(t, e, member)
	for _, sid := range []string{member, roomLogSID(room.ID)} {
		if historyHas(e, sid, "本来想插的一句话") {
			t.Fatalf("%s 的历史里不该有 observe 的回复", sid)
		}
	}
	recs := e.a.Observe("room:"+room.ID, 10, true)
	if len(recs) != 1 || recs[0].Say != "本来想插的一句话" {
		t.Fatalf("observe 记录：%+v", recs)
	}
}

// 只留最新的一半；默认不含“本来也不会说话”的判断。
func TestObserveTrimAndSilentFilter(t *testing.T) {
	e := setup(t)
	old := observeKeep
	observeKeep = 10
	defer func() { observeKeep = old }()
	e.a.mu.Lock()
	for i := 0; i < 11; i++ {
		e.a.observeAppend(ObserveRecord{At: int64(i), Scene: "s", Say: fmt.Sprint(i), Silent: i%2 == 0})
	}
	e.a.mu.Unlock()
	all := e.a.Observe("s", 100, true)
	if len(all) != 5 || all[0].Say != "10" || all[4].Say != "6" {
		t.Fatalf("超了只留最新的一半：%d %+v", len(all), all)
	}
	if loud := e.a.Observe("s", 100, false); len(loud) != 2 {
		t.Fatalf("默认不含 [不回复]：%+v", loud)
	}
	a2 := newAppAt(t, e.dir, nil)
	if got := a2.Observe("s", 100, true); len(got) != 5 {
		t.Fatalf("重启后读回：%d", len(got))
	}
}

// 接口：外部群的 scene 整块；内部群的规则字段（tools 区分不给、null、[]）。
func TestSceneAPIs(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	ext := extGroup(t, e)
	set := func(sid, body string) error {
		var p SessionPatch
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		return e.a.UpdateSession(sid, p)
	}
	if err := set(ext, `{"scene":{"brief":"读书会","tools":[],"style":{"quiet":"30s"}}}`); err != nil {
		t.Fatal(err)
	}
	info, _ := e.a.Session(ext)
	if info.Scene == nil || info.Scene.Brief != "读书会" || info.Scene.Tools == nil || len(info.Scene.Tools) != 0 || info.SceneUse == nil {
		t.Fatalf("会话信息：%+v", info.Scene)
	}
	for _, bad := range []string{`{"scene":{"mode":"turns"}}`, `{"scene":{"tools":["Shell"]}}`, `{"scene":{"guard":{"run":5}}}`, `{"scene":{"nope":1}}`} {
		if err := set(ext, bad); err == nil {
			t.Errorf("%s 应该被拒绝", bad)
		}
	}
	if info, _ := e.a.Session(ext); info.Scene.Brief != "读书会" {
		t.Fatal("被拒绝的修改不能留下半套")
	}
	if err := set(mainSID, `{"scene":{"brief":"x"}}`); err == nil {
		t.Fatal("私聊不能设群规则")
	}
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 50)
	e.a.mu.Unlock()
	if err := set(ext, `{"scene_reset":true}`); err != nil {
		t.Fatal(err)
	}
	if info, _ := e.a.Session(ext); info.SceneUse.Total != 0 || info.SceneUse.DayTokens != 50 {
		t.Fatalf("清零只清累计：%+v", info.SceneUse)
	}
	if err := set(ext, `{"scene":null}`); err != nil {
		t.Fatal(err)
	}
	if info, _ := e.a.Session(ext); info.Scene == nil || !info.Scene.IsZero() {
		t.Fatalf("null 清掉规则：%+v", info.Scene)
	}

	// 内部群：tools 不给 / null / []
	rs, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(rs)
	var create RoomPatch
	json.Unmarshal([]byte(`{"name":"群","members":["default"],"tools":["Skill"],"brief":"说明","budget":10}`), &create)
	ri, err := e.a.CreateRoom(create)
	if err != nil {
		t.Fatal(err)
	}
	if ri.Mode != "mention" || !slices.Equal(ri.Tools, []string{"Skill"}) || ri.Brief != "说明" || ri.Budget != 10 {
		t.Fatalf("建群：%+v", ri)
	}
	patch := func(body string) RoomInfo {
		var p RoomPatch
		json.Unmarshal([]byte(body), &p)
		got, err := e.a.UpdateRoom(ri.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := patch(`{"paused":false}`); !slices.Equal(got.Tools, []string{"Skill"}) {
		t.Fatalf("没给 tools 不动：%v", got.Tools)
	}
	if got := patch(`{"tools":[]}`); got.Tools == nil || len(got.Tools) != 0 {
		t.Fatalf("[] 不给工具：%v", got.Tools)
	}
	if got := patch(`{"tools":null}`); got.Tools != nil {
		t.Fatalf("null 沿用默认：%v", got.Tools)
	}
	if _, err := e.a.UpdateRoom(ri.ID, RoomPatch{ScenePatch: ScenePatch{Guard: &scene.Guard{Run: 1}}}); err == nil {
		t.Fatal("防刷屏阈值越界应该拒绝")
	}
}

// 外部群额度用完：拟人型约好的 FollowUp 到点也不调用模型，约定留着，第二天恢复后触发。
func TestFollowUpWaitsForSceneBudget(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	ext := extGroup(t, e)
	makeLife(t, e, store, nil)
	setScene(t, e, ext, &scene.Rules{Budget: 100})
	ag, _ := store.Get(agents.DefaultID)
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 100)
	st := e.a.mind("default")
	st.FollowUpMap()[ext] = autonomy.FollowUp{At: e.a.now().Add(-time.Minute).Unix(), Note: "问问他"}
	e.a.followUpTick(ag, st, e.a.now())
	_, kept := st.FollowUps[ext]
	e.a.mu.Unlock()
	e.k.none()
	if !kept {
		t.Fatal("额度用完：约定要留着")
	}
	setNow(e, e.a.now().Add(24*time.Hour))
	e.a.mu.Lock()
	e.a.followUpTick(ag, st, e.a.now())
	e.a.mu.Unlock()
	if in := e.k.expect(proto.TInput); in.SID != ext {
		t.Fatalf("第二天应该触发：%+v", in)
	}
}

// 删外部群会话时删掉它的用量条目。
func TestDeleteSessionForgetsSceneUse(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	ext := extGroup(t, e)
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 10)
	e.a.mu.Unlock()
	e.k.none()
	if err := e.a.DeleteSession(ext); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	_, left := e.a.sceneUses[ext]
	e.a.mu.Unlock()
	if left {
		t.Fatal("用量条目应该删掉")
	}
}
