package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/session"
)

// ---- 会话切换 agent 的测试（v0.7.3）----

func strPtr(s string) *string { return &s }

// withModels 在测试环境里接上两个模型：e/m1（默认）、e/m2（小鲍用），并让所有模型请求走同一个
// recLLM，这样能检查真正发出去的请求（系统提示、模型、思考强度、工具）。
func withModels(t *testing.T, e *env) *models.Store {
	t.Helper()
	doc := `{
  "endpoints": [{"id":"e","name":"测试接口","kind":"openai","base_url":"http://127.0.0.1:1"}],
  "models": [
    {"id":"e/m1","endpoint":"e","name":"m1","label":"默认模型","context_window":32000,"thinking":"low","thinking_levels":["low","high"]},
    {"id":"e/m2","endpoint":"e","name":"m2","label":"小鲍的模型","context_window":64000,"thinking":"high","thinking_levels":["low","high"]}
  ],
  "default": "e/m1",
  "seeded": true
}`
	if err := os.WriteFile(filepath.Join(e.dir, "data", "models.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := models.Open(filepath.Join(e.dir, "data"), &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := st.Find("e/m2"); !ok {
		t.Fatal("测试模型没有读进来")
	}
	e.a.SetModels(st, func(config.LLM) (llm.Client, error) { return e.llm, nil })
	return st
}

// addAgent 建一个 agent，写一份人格文件。
func addAgent(t *testing.T, e *env, store *agents.Store, ag agents.Agent, persona string) {
	t.Helper()
	if err := store.Create(ag); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dir, "agents", ag.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if persona != "" {
		if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte(persona), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// waitRunDone 等一个会话的这一轮彻底结束（回复可能还在发送队列里，这里只关心调度状态）。
func waitRunDone(t *testing.T, a *App, sid string) {
	t.Helper()
	for i := 0; i < 600; i++ {
		a.mu.Lock()
		running := a.sessions[sid] != nil && a.sessions[sid].run != nil
		a.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("会话 %s 的一轮没有结束", sid)
}

// runTurn 跑完一轮：内核要模型、模型回复、内核结束，返回这次真正发给模型的请求。
func runTurn(t *testing.T, e *env, sid string) llm.Request {
	t.Helper()
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	req := e.llm.lastReq()
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitRunDone(t, e.a, sid)
	return req
}

func TestSwitchAgentOnMainKeepsSessionAndNextTurnUsesNewIdentity(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	steps := 3
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍", Model: "e/m2", Thinking: "high",
		Tools: []string{"echo"}, MaxSteps: &steps}, "你是小鲍，说话简短。")

	// 第一轮：默认 agent「你」
	e.say("第一句")
	in0 := e.k.expect(proto.TInput)
	if in0.SID != mainSID || in0.MaxSteps == 3 {
		t.Fatalf("默认 agent 的 input 不对: %+v", in0)
	}
	req0 := runTurn(t, e, mainSID)
	if req0.Messages[0].Content != "你是测试机器人" {
		t.Fatalf("第一轮的系统提示不对: %q", req0.Messages[0].Content)
	}
	before := len(e.a.store.History(mainSID))

	// 切换 main → bob：会话 ID、历史都不变
	agentID := "bob"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &agentID}); err != nil {
		t.Fatal(err)
	}
	info, ok := e.a.Session(mainSID)
	if !ok || info.SID != mainSID || info.Agent != "bob" || info.RouteAgent != agents.DefaultID {
		t.Fatalf("切换后的会话信息不对: %+v", info)
	}
	if !info.CanSwitchAgent || info.AgentFixed || info.ModelUsed != "e/m2" {
		t.Fatalf("切换后应该还能切换、身份不固定、用 bob 的模型: %+v", info)
	}
	if got := len(e.a.store.History(mainSID)); got != before {
		t.Fatalf("切换不该动历史：%d → %d", before, got)
	}
	st := e.a.store.Settings(mainSID)
	if st.Agent != "bob" || st.Handoff == "" {
		t.Fatalf("切换没有写进会话设置: %+v", st)
	}
	if len(st.AgentSegments) != 1 || st.AgentSegments[0].Agent != "bob" || st.AgentSegments[0].Position != before {
		t.Fatalf("切换边界不对: %+v", st.AgentSegments)
	}
	// 重启（重新从磁盘读设置）以后选择还在
	reopened, err := session.New(filepath.Join(e.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Settings(mainSID).Agent; got != "bob" {
		t.Fatalf("重载后选择丢了: %q", got)
	}

	// bob 的长期记忆：切换后这一轮要按 bob 的记忆构造（不是默认 agent 的）
	if err := os.MkdirAll(filepath.Join(e.dir, "agents", "bob", "knowledge"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "agents", "bob", "knowledge", "note.md"),
		[]byte("---\ntitle: 小鲍知道的事\ntype: 知识\n---\n测试暗号是青龙。\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 第二轮：真正发给模型的请求要用 bob 的人格、模型默认思考强度、工具白名单和上限
	e.say("第二句")
	in1 := e.k.expect(proto.TInput)
	if in1.SID != mainSID || in1.MaxSteps != 3 {
		t.Fatalf("切换后的 input 应该用 bob 的轮数上限: %+v", in1)
	}
	req1 := runTurn(t, e, mainSID)
	if got := req1.Messages[0].Content; got != "你是小鲍，说话简短。" {
		t.Fatalf("第二轮的系统提示不是 bob 的人格: %q", got)
	}
	if req1.Thinking != "high" {
		t.Fatalf("应该用 bob 的思考强度 high，实际 %q", req1.Thinking)
	}
	if len(req1.Tools) != 1 || req1.Tools[0].Function.Name != "echo" {
		t.Fatalf("应该只剩 bob 白名单里的 echo: %+v", req1.Tools)
	}
	// 交接说明只放一次，放在这一轮的用户消息前（不改系统提示词）
	user := lastUser(req1.Messages)
	if !strings.Contains(user, "换成了你") || !strings.Contains(user, "保留着之前的聊天记录") {
		t.Fatalf("第二轮没有交接说明: %q", user)
	}
	if !strings.Contains(user, "小鲍知道的事") || !strings.Contains(user, "[记忆]") {
		t.Fatalf("第二轮没有按 bob 的记忆构造上下文: %q", user)
	}
	info2, _ := e.a.Session(mainSID)
	if info2.ModelUsed != "e/m2" || info2.Thinking != "high" {
		t.Fatalf("会话实际用量应该是 bob 的模型和思考强度: %+v", info2)
	}
	if st := e.a.store.Settings(mainSID); st.Handoff != "" {
		t.Fatalf("交接说明用过后应该清掉: %q", st.Handoff)
	}

	// 历史里每条 assistant 消息都记着来源 agent，老消息按前缀算默认 agent
	h := e.a.store.Archive(mainSID)
	var sources []string
	for i, m := range h {
		if m.Role == "assistant" {
			sources = append(sources, m.Agent)
			if i < before && m.Agent != agents.DefaultID {
				t.Fatalf("切换前的回复来源不对: %+v", m)
			}
		}
	}
	if len(sources) < 2 || sources[0] != agents.DefaultID || sources[len(sources)-1] != "bob" {
		t.Fatalf("历史消息的来源不对: %v", sources)
	}
	if got := e.a.msgAgent(mainSID, 0, session.Msg{Role: "assistant"}); got != agents.DefaultID {
		t.Fatalf("没有 Agent 字段的老消息应该按前缀推断成默认 agent，实际 %q", got)
	}

	// 切回去：人格和模型跟着回去，历史还在
	def := agents.DefaultID
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &def}); err != nil {
		t.Fatal(err)
	}
	e.say("第三句")
	e.k.expect(proto.TInput)
	req2 := runTurn(t, e, mainSID)
	if req2.Messages[0].Content != "你是测试机器人" || req2.Thinking != "low" {
		t.Fatalf("切回去以后应该是默认 agent 的人格和思考强度: %q / %q", req2.Messages[0].Content, req2.Thinking)
	}
}

func TestSwitchAgentBlockedWhileRunningOrCompacting(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")
	want := "bob"

	e.say("你好")
	in := e.k.expect(proto.TInput)
	if in.SID != mainSID {
		t.Fatalf("input: %+v", in)
	}
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &want}); err == nil || !strings.Contains(err.Error(), "正在处理") {
		t.Fatalf("运行中应该拒绝切换，实际 %v", err)
	}
	if got := e.a.store.Settings(mainSID).Agent; got != "" {
		t.Fatalf("拒绝切换不能留下设置: %q", got)
	}
	// 排队的消息也算忙：先结束这一轮，再试排队的情况
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.say("排队的")
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &want}); err == nil || !strings.Contains(err.Error(), "正在处理") {
		t.Fatalf("队列非空时应该拒绝切换，实际 %v", err)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	// 这一轮结束时自动开始排队的那条：先收到它的 input，再喂模型、结束
	if q := e.k.expect(proto.TInput); q.SID != mainSID {
		t.Fatalf("排队的那条开了一轮: %+v", q)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	waitRunDone(t, e.a, mainSID)

	// 正在整理上下文
	e.a.mu.Lock()
	e.a.sessOf(mainSID).compacting = true
	e.a.mu.Unlock()
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &want}); err == nil || !strings.Contains(err.Error(), "整理上下文") {
		t.Fatalf("整理上下文时应该拒绝切换，实际 %v", err)
	}
	e.a.mu.Lock()
	e.a.sessOf(mainSID).compacting = false
	e.a.mu.Unlock()
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &want}); err != nil {
		t.Fatalf("空闲时应该能切换: %v", err)
	}
}

func TestSwitchAgentPatchIsAtomic(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")

	// 同一份 patch 里 agent 合法、模型不存在：不能切成 bob，也不能改名字
	agentID, title := "bob", "新名字"
	err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &agentID, Model: strPtr("e/nope"), Title: &title})
	if err == nil || !strings.Contains(err.Error(), "没有模型") {
		t.Fatalf("应该报没有这个模型: %v", err)
	}
	st := e.a.store.Settings(mainSID)
	if st.Agent != "" || st.Handoff != "" || st.Title == title {
		t.Fatalf("失败的 patch 不能留下任何修改: %+v", st)
	}
	if info, _ := e.a.Session(mainSID); info.Agent != agents.DefaultID {
		t.Fatalf("失败的 patch 不能换 agent: %+v", info)
	}

	// 工作目录验证失败同样不能留下部分修改（以前会先保存 workspaces）
	e.a.mu.Lock()
	e.a.wss = append(e.a.wss, Workspace{ID: "w1", Host: "test", Path: "/tmp/w1"})
	e.a.mu.Unlock()
	ws := []string{"w1", "missing"}
	err = e.a.UpdateSession(mainSID, SessionPatch{Workspaces: &ws, Title: &title})
	if err == nil || !strings.Contains(err.Error(), "没有这个工作目录") {
		t.Fatalf("应该报没有这个工作目录: %v", err)
	}
	st = e.a.store.Settings(mainSID)
	if st.WorkspacesSet || st.Title == title {
		t.Fatalf("工作目录校验失败不能留下部分修改: %+v", st)
	}

	// 思考强度不在新 agent 模型的可选值里：整份 patch 失败
	bad := "max"
	err = e.a.UpdateSession(mainSID, SessionPatch{Agent: &agentID, Thinking: &bad})
	if err == nil || !strings.Contains(err.Error(), "思考强度") {
		t.Fatalf("应该报思考强度不对: %v", err)
	}
	if e.a.store.Settings(mainSID).Agent != "" {
		t.Fatal("思考强度失败不能换 agent")
	}

	// 幂等的重复提交：已经是 bob 再提交 bob 不算切换，不写新的交接说明
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &agentID}); err != nil {
		t.Fatal(err)
	}
	first := e.a.store.Settings(mainSID).Handoff
	if first == "" {
		t.Fatal("第一次切换应该有交接说明")
	}
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &agentID}); err != nil {
		t.Fatal(err)
	}
	st = e.a.store.Settings(mainSID)
	if st.Handoff != first || len(st.AgentSegments) != 1 {
		t.Fatalf("重复提交同一个 agent 不该重复切换: %+v", st)
	}
}

func TestInternalSessionsKeepFixedAgent(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍", Style: agents.StyleLife}, "你是小鲍")

	for _, sid := range []string{lifeSID("bob"), dreamSID("bob"), "@bob/cron:t1"} {
		if err := e.a.store.SaveSettings(sid, session.Settings{Title: "内部会话"}); err != nil {
			t.Fatal(err)
		}
		info, ok := e.a.Session(sid)
		if !ok || info.Agent != "bob" || !info.AgentFixed || info.CanSwitchAgent {
			t.Fatalf("%s 应该是固定的 bob: %+v", sid, info)
		}
		def := agents.DefaultID
		if err := e.a.UpdateSession(sid, SessionPatch{Agent: &def}); err == nil {
			t.Fatalf("%s 不该能切换 agent", sid)
		}
		if got := e.a.agentOf(sid).ID; got != "bob" {
			t.Fatalf("%s 的身份被改了: %q", sid, got)
		}
	}
}

func TestDeleteAgentTakesOverChosenSessions(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")
	bob := "bob"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &bob}); err != nil {
		t.Fatal(err)
	}
	// 一个在 bob 名下、明确交给默认 agent 的会话；一个没明确选过的 bob 会话
	def := agents.DefaultID
	e.a.mu.Lock()
	sidChosen, err := e.a.newSessionIn("bob", "明确转交", true)
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.UpdateSession(sidChosen, SessionPatch{Agent: &def}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	sidImplicit, err := e.a.newSessionIn("bob", "还归 bob", true)
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if err := e.a.DeleteAgent("bob"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("bob"); ok {
		t.Fatal("bob 还在")
	}
	info, ok := e.a.Session(mainSID)
	if !ok || info.Agent != agents.DefaultID || info.RouteAgent != agents.DefaultID {
		t.Fatalf("main 应该交给默认 agent 并且还在: %+v", info)
	}
	st := e.a.store.Settings(mainSID)
	if st.Agent != agents.DefaultID || st.Handoff == "" {
		t.Fatalf("main 的交接记录不对: %+v", st)
	}
	if info, ok := e.a.Session(sidChosen); !ok || info.Agent != agents.DefaultID {
		t.Fatalf("明确转交过的会话应该还在、归默认 agent: %+v", info)
	}
	snap := e.a.Snapshot()
	found := map[string]bool{}
	for _, s := range snap.Sessions {
		found[s.SID] = true
	}
	if !found[mainSID] || !found[sidChosen] {
		t.Fatalf("main 和转交过的会话都应该可见: %v", found)
	}
	if found[sidImplicit] {
		t.Fatalf("还归已删除 bob 的会话不该列出来（重新建同 ID 的 agent 才会再出现）: %v", found)
	}
}

func TestBotEndpointKeepsExplicitAgentChoice(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")

	// 窗口接在 @bob/main，但会话被明确交给了默认 agent：下一条消息不能把窗口改接回 main
	if err := e.a.store.SaveSettings("@bob/main", session.Settings{Title: "bob 的 main", Agent: agents.DefaultID}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: "@bob/main", Who: "fake:u1"}
	e.a.mu.Unlock()
	e.say("你好")
	in := e.k.expect(proto.TInput)
	if in.SID != "@bob/main" {
		t.Fatalf("明确选了 agent 的会话被机器人改接走了: %+v", in)
	}
	e.a.mu.Lock()
	got := e.a.eps["fake:c1"].Session
	e.a.mu.Unlock()
	if got != "@bob/main" {
		t.Fatalf("窗口接线被改回了 %q", got)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: "@bob/main"})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: "@bob/main", Status: proto.StatusOK})
	waitRunDone(t, e.a, "@bob/main")

	// 没有明确选过的窗口，机器人换绑时照旧跟着换会话
	if err := e.a.store.SaveSettings("@bob/other", session.Settings{Title: "bob 的另一个会话"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.eps["fake:c2"] = &epRec{Session: "@bob/other", Who: "fake:u2"}
	e.a.mu.Unlock()
	e.a.OnMessage(e.ch, channel.Message{ChatID: "c2", SenderID: "u2", SenderName: "李四", Text: "你好", MsgID: "m2"})
	e.a.mu.Lock()
	got = e.a.eps["fake:c2"].Session
	e.a.mu.Unlock()
	if got != "fake:c2" {
		t.Fatalf("没选过 agent 的窗口应该跟着机器人换到默认 agent 的会话 fake:c2，实际 %q", got)
	}
}
func TestScheduleOwnerFollowsSessionAtCreationOnly(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")
	bob := "bob"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &bob}); err != nil {
		t.Fatal(err)
	}
	made, err := e.a.ScheduleAction(mainSID, "fake:u1", ScheduleRequest{Action: "create", Name: "每日看看", When: "every 1h", Prompt: "看看有什么新消息"})
	if err != nil {
		t.Fatal(err)
	}
	task := made.(schedule.Task)
	if task.Agent != "bob" {
		t.Fatalf("新任务应该归当前执行 agent bob，实际 %q", task.Agent)
	}
	if got := cronSID(task); got != "@bob/cron:"+task.ID {
		t.Fatalf("任务会话 ID 不对: %q", got)
	}
	// 聊天切回默认 agent：已有任务不跟着转交
	def := agents.DefaultID
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &def}); err != nil {
		t.Fatal(err)
	}
	upd, err := e.a.ScheduleAction(mainSID, "fake:u1", ScheduleRequest{Action: "update", ID: task.ID, Name: "改个名字"})
	if err != nil {
		t.Fatal(err)
	}
	if got := upd.(schedule.Task).Agent; got != "bob" {
		t.Fatalf("已有任务不该被聊天里的切换改动，实际 %q", got)
	}
}

func TestLifeOwnershipFollowsCurrentAgent(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍", Style: agents.StyleLife}, "你是小鲍")
	bob := "bob"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &bob}); err != nil {
		t.Fatal(err)
	}
	if !e.a.isOwnerSID("bob", mainSID) {
		t.Fatal("切换后 bob 应该把 main 认成主人会话")
	}
	if e.a.isOwnerSID(agents.DefaultID, mainSID) {
		t.Fatal("切换后默认 agent 不该再把 main 认成主人会话")
	}
	bobAgent, _ := store.Get("bob")
	targets := e.a.lifeTargets(bobAgent)
	foundMain := false
	for _, tg := range targets {
		if tg.To == "chat:"+mainSID {
			foundMain = true
			if tg.Name != "主人" {
				t.Fatalf("main 应该显示成主人: %+v", tg)
			}
		}
	}
	if !foundMain {
		t.Fatalf("bob 的 life 应该能找到 main: %+v", targets)
	}
	defAgent, _ := store.Get(agents.DefaultID)
	for _, tg := range e.a.lifeTargets(defAgent) {
		if tg.To == "chat:"+mainSID {
			t.Fatalf("默认 agent 不该再主动找 main: %+v", tg)
		}
	}

	// bob 的内心活动能把切换过来的 main 当主人，主动发消息
	e.a.mu.Lock()
	out, err := e.a.messageChat(lifeSID("bob"), mainSID, "我来问问你")
	e.a.mu.Unlock()
	if err != nil || !strings.Contains(out, "发出去了") {
		t.Fatalf("切换后 bob 应该能给 main 发消息: %q %v", out, err)
	}

	// 旧 agent 约好的回访不能越过归属：切换后到点直接作废
	ls := e.a.lifeOf("bob")
	ls.FollowUps[mainSID] = followUp{At: e.a.now().Add(-time.Minute).Unix(), Note: "问问他面试怎么样"}
	def := agents.DefaultID
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &def}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.followUpTick(bobAgent, ls, e.a.now())
	e.a.mu.Unlock()
	if _, ok := ls.FollowUps[mainSID]; ok {
		t.Fatal("旧 agent 的回访没有作废")
	}
}

func TestResetAndArchiveKeepAgentChoice(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	withModels(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")
	bob := "bob"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Agent: &bob}); err != nil {
		t.Fatal(err)
	}
	e.say("会记下来的话")
	e.k.expect(proto.TInput)
	runTurn(t, e, mainSID)

	if err := e.a.ResetSession(mainSID); err != nil {
		t.Fatal(err)
	}
	st := e.a.store.Settings(mainSID)
	if st.Agent != "bob" {
		t.Fatalf("清空上下文不该丢掉选择: %+v", st)
	}
	if st.Handoff != "" || len(st.AgentSegments) != 0 {
		t.Fatalf("清空上下文以后交接说明和分段应该清掉: %+v", st)
	}
	if got := e.a.agentOf(mainSID).ID; got != "bob" {
		t.Fatalf("清空后执行身份应该还是 bob，实际 %q", got)
	}
	if n := len(e.a.store.History(mainSID)); n != 0 {
		t.Fatalf("上下文应该清空了: %d", n)
	}

	// 再切一次、归档（reset 归档到 archives），选择仍然保留
	if err := e.a.UpdateSession(mainSID, SessionPatch{Title: strPtr("改名")}); err != nil {
		t.Fatal(err)
	}
	if got := e.a.store.Settings(mainSID).Agent; got != "bob" {
		t.Fatalf("改名不该丢选择: %q", got)
	}
	if err := e.a.ResetSession(mainSID); err != nil {
		t.Fatal(err)
	}
	if got := e.a.store.Settings(mainSID).Agent; got != "bob" {
		t.Fatalf("归档重置不该丢选择: %q", got)
	}
}
