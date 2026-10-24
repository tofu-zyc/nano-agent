package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/llm"
)

// testAgent 是测试里新建 agent 的快捷写法。
func testAgent(id string) agents.Agent { return agents.Agent{ID: id, Name: id} }

// withAgents 给测试环境接上 agent 列表和机器人，并跑一次升级迁移。
func withAgents(t *testing.T, e *env) (*agents.Store, *agents.Bots) {
	t.Helper()
	store, err := agents.Open(filepath.Join(e.dir, "data", "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	bots, err := agents.OpenBots(filepath.Join(e.dir, "data", "bots.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetAgents(store, bots, filepath.Join(e.dir, "agents"))
	if err := e.a.MigrateAgents(); err != nil {
		t.Fatal(err)
	}
	return store, bots
}

func TestMigrateAgents(t *testing.T) {
	e := setup(t)
	store, bots := withAgents(t, e)

	def, ok := store.Get(agents.DefaultID)
	if !ok || def.Name != defaultAgentName {
		t.Fatalf("没有默认 agent: %+v", def)
	}
	// 老版本的 personas/default.md 复制到默认 agent 下面；原文件留着（升级失败退回旧版本还要用）
	if b, err := os.ReadFile(filepath.Join(e.dir, "personas", "default.md")); err != nil || string(b) != "你是测试机器人" {
		t.Fatalf("老的 personas/default.md 应该留着：%q %v", b, err)
	}
	data, err := os.ReadFile(filepath.Join(e.dir, "agents", agents.DefaultID, "persona.md"))
	if err != nil || string(data) != "你是测试机器人" {
		t.Fatalf("默认 agent 的人格没搬过来: %q %v", data, err)
	}
	// 内置的两个机器人，都绑在默认 agent 上
	for _, id := range []string{"tg", "feishu"} {
		b, ok := bots.Get(id)
		if !ok || !b.Legacy || b.Agent != agents.DefaultID {
			t.Fatalf("内置机器人 %s 不对: %+v", id, b)
		}
	}
	// 迁移只做一次：再跑一遍不会把人格文件写回去
	if err := e.a.MigrateAgents(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPersonaLookup(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	if err := store.Create(agents.Agent{ID: "bob", Name: "小鲍"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dir, "agents", "bob")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte("你是小鲍"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	text, path := e.a.personaFor("@bob/main")
	defText, _ := e.a.personaFor("main")
	// 会话专属人格优先
	if err := os.WriteFile(filepath.Join(e.dir, "personas", "bob-session.md"), []byte("会话专用"), 0o600); err != nil {
		t.Fatal(err)
	}
	override, _ := e.a.personaFor("bob-session")
	// bob 的会话用 bob 的人格，不加前缀的名字是别人的会话
	e.a.mu.Unlock()
	if text != "你是小鲍" || filepath.Base(filepath.Dir(path)) != "bob" {
		t.Fatalf("bob 的会话没用 bob 的人格: %q %q", text, path)
	}
	if defText != "你是测试机器人" {
		t.Fatalf("默认 agent 的人格不对: %q", defText)
	}
	if override != "会话专用" {
		t.Fatalf("会话专属人格应该优先: %q", override)
	}
}

func TestAgentOverrides(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	steps, mode := 7, "readonly"
	if err := store.Create(agents.Agent{ID: "bob", Name: "小鲍", MaxSteps: &steps, Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	l := e.a.limitsOf("@bob/main")
	global := e.a.limitsOf("main")
	agentMode := e.a.agentModeDefault("@bob/main")
	ws := e.a.workspaceOf("@bob/main")
	e.a.mu.Unlock()
	if l.Steps != 7 {
		t.Fatalf("agent 覆盖的工具调用轮数没用上: %d", l.Steps)
	}
	if global.Steps == 7 {
		t.Fatal("默认 agent 不该被 bob 的设置影响")
	}
	if agentMode != "readonly" || ws.mode != "readonly" {
		t.Fatalf("权限模式的覆盖没生效: %q %q", agentMode, ws.mode)
	}
	// 会话自己设过的优先于 agent
	e.a.mu.Lock()
	st := e.a.store.Settings("@bob/main")
	st.Mode = "auto"
	e.a.store.SaveSettings("@bob/main", st)
	ws2 := e.a.workspaceOf("@bob/main")
	e.a.mu.Unlock()
	if ws2.mode != "auto" {
		t.Fatalf("会话自己的模式应该优先: %q", ws2.mode)
	}
}

func TestAgentToolWhitelist(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	if err := store.Create(agents.Agent{ID: "bob", Name: "小鲍", Tools: []string{"Read"}}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	mk := func(names ...string) []llm.Tool {
		var out []llm.Tool
		for _, n := range names {
			out = append(out, llm.Tool{Type: "function", Function: llm.ToolFunction{Name: n}})
		}
		return out
	}
	tools := e.a.filterAgentTools("@bob/main", mk("Read", "Shell", "Grep"))
	all := e.a.filterAgentTools("main", mk("Read", "Shell"))
	e.a.mu.Unlock()
	if len(tools) != 1 || tools[0].Function.Name != "Read" {
		t.Fatalf("工具白名单没生效: %+v", tools)
	}
	if len(all) != 2 {
		t.Fatalf("没设白名单的 agent 应该拿到全部工具: %+v", all)
	}
}

func TestBotRoutesToAgent(t *testing.T) {
	e := setup(t)
	store, bots := withAgents(t, e)
	if err := store.Create(agents.Agent{ID: "bob", Name: "小鲍"}); err != nil {
		t.Fatal(err)
	}
	if err := bots.Bind("tg", "bob"); err != nil {
		t.Fatal(err)
	}
	tg := &fakeCh{name: "tg", sent: make(chan string, 10)}
	e.a.AddChannel(tg)
	// 把 tg:u1 加成管理员（终端、网页之外的用户要写在名单里）
	e.a.mu.Lock()
	e.a.cfg.Access.Admins = append(e.a.cfg.Access.Admins, "tg:u1")
	e.a.mu.Unlock()
	// 管理员：接到 bob 的主会话
	e.a.OnMessage(tg, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: "你好", MsgID: "m1"})
	e.a.mu.Lock()
	adminSession := e.a.eps["tg:c1"].Session
	e.a.mu.Unlock()
	if adminSession != "@bob/main" {
		t.Fatalf("绑到 bob 的机器人，管理员应该接 @bob/main，实际 %q", adminSession)
	}
	// 访客：自己的窗口 ID 加 agent 前缀
	e.a.OnMessage(tg, channel.Message{ChatID: "c2", SenderID: "u2", SenderName: "李四", Text: "你好", MsgID: "m2"})
	e.a.mu.Lock()
	guestSession := e.a.eps["tg:c2"].Session
	e.a.mu.Unlock()
	if guestSession != "@bob/tg:c2" {
		t.Fatalf("访客的会话应该是 @bob/tg:c2，实际 %q", guestSession)
	}
	// 换绑回默认 agent：窗口跟着换会话
	if err := bots.Bind("tg", agents.DefaultID); err != nil {
		t.Fatal(err)
	}
	e.a.OnMessage(tg, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: "还在吗", MsgID: "m3"})
	e.a.mu.Lock()
	moved := e.a.eps["tg:c1"].Session
	e.a.mu.Unlock()
	if moved != "main" {
		t.Fatalf("换绑以后应该回到默认 agent 的主会话，实际 %q", moved)
	}
}

func TestAgentCRUD(t *testing.T) {
	e := setup(t)
	_, _ = withAgents(t, e)
	name := "小鲍"
	info, err := e.a.CreateAgent(AgentPatch{Name: &name})
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	if info.ID != "xiao" && info.ID != "bob" && info.Name != "小鲍" {
		t.Fatalf("新建的 agent 不对: %+v", info)
	}
	// 名字为空
	if _, err := e.a.CreateAgent(AgentPatch{}); err == nil {
		t.Fatal("没有名字应该报错")
	}
	// 覆盖设置
	steps := float64(12)
	if _, err := e.a.UpdateAgent(info.ID, AgentPatch{Settings: map[string]any{"max_steps": steps}}); err != nil {
		t.Fatalf("改设置失败: %v", err)
	}
	got, _ := e.a.Agent(info.ID)
	if got.Settings[0].Key != "max_steps" || got.Settings[0].Value != 12 {
		t.Fatalf("覆盖的设置没存上: %+v", got.Settings[0])
	}
	// 恢复跟随全局
	if _, err := e.a.UpdateAgent(info.ID, AgentPatch{Settings: map[string]any{"max_steps": nil}}); err != nil {
		t.Fatalf("恢复默认失败: %v", err)
	}
	got, _ = e.a.Agent(info.ID)
	if got.Settings[0].Value != nil {
		t.Fatalf("应该恢复成跟随全局: %+v", got.Settings[0])
	}
	// 默认 agent 不能删
	if err := e.a.DeleteAgent(agents.DefaultID); err == nil {
		t.Fatal("默认 agent 不能删")
	}
	// 删 agent 的时候绑着它的机器人交给默认 agent，不用先一个个换绑
	e.a.mu.Lock()
	bots := e.a.botStore
	e.a.mu.Unlock()
	if err := bots.Bind("tg", info.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.a.DeleteAgent(info.ID); err != nil {
		t.Fatalf("删 agent 应该把机器人交给默认 agent: %v", err)
	}
	if b, ok := bots.Get("tg"); !ok || b.Agent != agents.DefaultID {
		t.Fatalf("机器人没交给默认 agent: %+v", b)
	}
}

func strp(s string) *string { return &s }

// 名字不能重复：按名字找 agent（@小纳）时会有歧义。
func TestAgentNameUnique(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	if _, err := e.a.CreateAgent(AgentPatch{Name: strp("小鲍")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.CreateAgent(AgentPatch{Name: strp("小鲍")}); err == nil {
		t.Fatal("同名的 agent 不该能建")
	} else if !strings.Contains(err.Error(), "已经有叫「小鲍」的 agent") {
		t.Fatalf("错误提示不清楚: %v", err)
	}
	// 改名撞上别人也不行
	other, err := e.a.CreateAgent(AgentPatch{Name: strp("阿强")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.UpdateAgent(other.ID, AgentPatch{Name: strp("小鲍")}); err == nil {
		t.Fatal("改名撞上同名的 agent 不该成功")
	}
}
