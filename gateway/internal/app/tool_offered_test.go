package app

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"path/filepath"
)

// proposeModel 第一次回复就提出一个调用（工具名由测试定），之后给最终回答；记录每次请求给出的工具。
type proposeModel struct {
	mu    sync.Mutex
	name  string
	args  string
	n     int
	tools [][]llm.Tool
}

func (m *proposeModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.tools = append(m.tools, req.Tools)
	if m.n == 1 {
		return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: m.name, Arguments: m.args}}}}, nil
	}
	return &llm.Result{Reason: llm.ReasonFinal, Content: "好"}, nil
}

const pingText = "不该被发出去的话"

// 模型提出这一轮没提供的工具：整批不接受、不交给内核、处理器不执行，历史里留未接受提案。
func expectUnoffered(t *testing.T, e *env, in proto.Msg, name string) {
	t.Helper()
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonError || !strings.Contains(turn.Detail, "这一轮没有提供工具 "+name) || len(turn.Calls) != 0 {
		t.Fatalf("应整批不接受：%+v", turn)
	}
	e.k.none() // 没有后续的执行
	var proposal bool
	for _, m := range e.a.store.History(in.SID) {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("未接受的调用进了正式历史：%+v", m)
		}
		if m.Kind == "unaccepted_tool_proposal" && strings.Contains(m.Reason, name) && len(m.ProposedCalls) == 1 {
			proposal = true
		}
	}
	if !proposal {
		t.Fatalf("历史里没有未接受提案：%+v", e.a.store.History(in.SID))
	}
	e.a.mu.Lock()
	if s := e.a.sessions[in.SID]; s != nil && s.run != nil && len(s.run.callArgs) != 0 {
		t.Fatalf("调用被登记了：%+v", s.run.callArgs)
	}
	e.a.mu.Unlock()
	select {
	case got := <-e.ch.sent:
		if strings.Contains(got, pingText) {
			t.Fatalf("处理器执行了，消息发出去了：%q", got)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

const messageArgs = `{"to":"chat:main","text":"` + pingText + `"}`

// 外部群：管理员点名，群聊只提供了 SendFile 之类，模型却提出 Message。
func TestUnofferedToolExternalGroupAdmin(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&proposeModel{name: "Message", args: messageArgs})
	e.sid = "fake:c1"
	e.say("@bot 去告诉主会话", group(true))
	in := e.k.expect(proto.TInput)
	expectUnoffered(t, e, in, "Message")
}

// 内部群成员。
func TestUnofferedToolRoomMember(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.SetLLM(&proposeModel{name: "Message", args: messageArgs})
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	room, err := st.Create(rooms.Room{Name: "产品组", Members: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	e.a.OnMessage(e.a.Web(), channel.Message{ChatID: "local", SenderID: "u1", SenderName: "张三",
		Text: "@小纳 去告诉主会话", Session: roomLogSID(room.ID)})
	in := e.k.expect(proto.TInput)
	if in.SID != e.a.roomMemberSID("default", room.ID) {
		t.Fatalf("应该由成员会话跑：%+v", in)
	}
	expectUnoffered(t, e, in, "Message")
}

// 访客（没有工作目录，只有聊天）。
func TestUnofferedToolGuest(t *testing.T) {
	e := setup(t)
	m := &proposeModel{name: "Message", args: messageArgs}
	e.a.SetLLM(m)
	e.sid = "fake:c1"
	e.say("你好", from("u2", "李四"))
	in := e.k.expect(proto.TInput)
	expectUnoffered(t, e, in, "Message")
}

// 整理记忆：只有 Diary 和 Knowledge。
func TestUnofferedToolDream(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.SetLLM(&proposeModel{name: "Message", args: messageArgs})
	fill(t, e, mainSID, 6, e.a.now().Add(-time.Hour))
	if err := e.a.Dream("default"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	expectUnoffered(t, e, in, "Message")
}

// 提供了的工具照常接受（反向对照）：整理记忆里提出 Diary。
func TestOfferedToolStillAccepted(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.SetLLM(&proposeModel{name: "Diary", args: `{}`})
	fill(t, e, mainSID, 6, e.a.now().Add(-time.Hour))
	if err := e.a.Dream("default"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonToolUse || len(turn.Calls) != 1 || turn.Calls[0].Name != "Diary" {
		t.Fatalf("提供了的工具应该接受：%+v", turn)
	}
}

// 每个快照角色提供的工具，要么在登记表里，要么是内核工具或 MCP 工具；toolNames 包含表里的全部工具。
func TestOfferedToolsAreRegistered(t *testing.T) {
	for _, role := range surfaceRoles() {
		t.Run(role.name, func(t *testing.T) {
			e := setup(t)
			sid := role.build(t, e)
			e.a.mu.Lock()
			defer e.a.mu.Unlock()
			_, tools, _, _ := e.a.contextParts(sid)
			for _, tl := range tools {
				name := tl.Function.Name
				if toolDef(name) == nil && name != "echo" && !strings.Contains(name, "__") { // echo 是测试内核的工具
					t.Errorf("工具 %s 既不在登记表里，也不是内核或 MCP 工具", name)
				}
			}
		})
	}
	e := setup(t)
	e.a.mu.Lock()
	names := e.a.toolNames()
	e.a.mu.Unlock()
	for _, name := range gatewayToolNames() {
		if !slices.Contains(names, name) {
			t.Errorf("toolNames 缺 %s", name)
		}
	}
}

// offerNodeTools 让测试内核（只带 echo）再提供几个内核工具：模型替身提出的工具，这一轮得真的提供过。
func offerNodeTools(e *env, names ...string) {
	specs := []proto.ToolSpec{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for _, n := range names {
		specs = append(specs, proto.ToolSpec{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	e.a.mu.Lock()
	e.a.nodes["test"].setTools(specs)
	e.a.mu.Unlock()
}

// addSkill 放一个技能，让 Skill 工具这一轮被提供（没有任何技能时它不出现）。
func addSkill(t *testing.T, e *env, name string) {
	t.Helper()
	dir := filepath.Join(e.a.cfg.Skills.Dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: 测试用\n---\n正文"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.refreshSkills()
	e.a.mu.Unlock()
}
