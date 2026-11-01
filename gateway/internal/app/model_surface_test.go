package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/schedule"
)

// 模型面快照：每种角色的模型能看到的内容（系统提示和工具定义）有字节快照和体积上限。
// 内容变了就是缓存回归：先确认是有意的，工具定义变了还必须同步更新 prefixVer。
//
//	go test ./internal/app -run TestModelSurface -update
var updateSurface = flag.Bool("update", false, "更新 testdata/model-surface 下的金样（工具定义变了而 prefixVer 没变时拒绝更新）")

// 说明：定时任务（cron）和 Hook 没有真实的 run，快照是 contextParts 在该会话刚建好时算出的内容，
// 不是线上某一轮的内容。其余角色的状态都走真实入口布置。
// surfaceCap 是每个角色“系统提示加工具定义”的总字节上限：当时的值上浮 10%。
// 超了说明模型面膨胀了，先想想值不值，再来改这里。
var surfaceCap = map[string]int{
	"admin-workspace":      21662,
	"guest-none":           1125,
	"guest-readonly":       3601,
	"group-external":       1569,
	"companion-dm":         36228,
	"life":                 38779,
	"dream":                9967,
	"cron":                 9057,
	"hook":                 8846,
	"branch":               3601,
	"room-member":          6586,
	"room-member-scene":    0,
	"group-external-scene": 576,
}

type surfaceRole struct {
	name  string
	build func(t *testing.T, e *env) string // 返回要看的会话 ID，会话的状态已经布置好
}

func surfaceRoles() []surfaceRole {
	life := func(t *testing.T, e *env) {
		store, _ := withAgents(t, e)
		makeLife(t, e, store, nil)
	}
	return []surfaceRole{
		{"admin-workspace", func(t *testing.T, e *env) string {
			e.useDir(t, "work")
			return mainSID
		}},
		{"guest-none", func(t *testing.T, e *env) string {
			e.sid = "fake:c1" // 访客没有工作目录：不给工具，只能聊天
			e.say("你好", from("u2", "李四"))
			e.k.expect(proto.TInput)
			return e.sid
		}},
		{"guest-readonly", func(t *testing.T, e *env) string {
			e.a.cfg.Access.Guest = "readonly"
			e.sid = "fake:c1"
			e.useDir(t, "work")
			e.say("你好", from("u2", "李四"))
			e.k.expect(proto.TInput)
			return e.sid
		}},
		{"group-external", func(t *testing.T, e *env) string {
			e.sid = "fake:c1"
			e.say("大家好", group(true), from("u2", "李四"))
			e.k.expect(proto.TInput)
			return e.sid
		}},
		{"companion-dm", func(t *testing.T, e *env) string {
			life(t, e)
			return mainSID
		}},
		{"life", func(t *testing.T, e *env) string {
			life(t, e)
			return lifeSID(agents.DefaultID)
		}},
		{"dream", func(t *testing.T, e *env) string {
			life(t, e)
			return dreamSID(agents.DefaultID)
		}},
		{"cron", func(t *testing.T, e *env) string {
			withAgents(t, e)
			return cronSID(schedule.Task{ID: "t1"})
		}},
		{"hook", func(t *testing.T, e *env) string {
			withAgents(t, e)
			return hookSID(agents.DefaultID, "d1")
		}},
		{"branch", func(t *testing.T, e *env) string {
			e.say("第一问", msgID("m1"))
			e.k.expect(proto.TInput)
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
			e.k.expect(proto.TTurn)
			e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
			e.ch.wait(t, "收到：第一问")
			res, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "分支", RequestID: "req-1", By: "fake:u1"})
			if err != nil {
				t.Fatal(err)
			}
			return res.BranchSID
		}},
		// 走真实入口：CreateRoom 建群，再由人说一句话（roomHumanSay 把成员会话标成 Group）。
		// 群 ID 是随机的，快照里换成 r1。
		{"room-member", func(t *testing.T, e *env) string {
			withAgents(t, e)
			st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
			e.a.SetRooms(st)
			name, members := "测试群", []string{agents.DefaultID}
			info, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members})
			if err != nil {
				t.Fatal(err)
			}
			e.a.mu.Lock()
			room, _ := st.Get(info.ID)
			e.a.roomHumanSay(room, "李四", "大家好") // 没点名，不会触发模型
			e.a.mu.Unlock()
			return e.a.roomMemberSID(agents.DefaultID, info.ID)
		}},
		// 带场景说明、角色提示词和工具表的内部群成员
		{"room-member-scene", func(t *testing.T, e *env) string {
			withAgents(t, e)
			st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
			e.a.SetRooms(st)
			name, members := "谋杀之夜", []string{agents.DefaultID}
			roles := map[string]scene.Role{agents.DefaultID: {Prompt: "你是庄园的管家，知道所有人的秘密，但只在被问到时才说。"}}
			info, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &members, ScenePatch: ScenePatch{
				Brief: strptr("晚宴上有人被毒死，大家被困在庄园里。"), Roles: &roles, Tools: json.RawMessage(`["Knowledge"]`)}})
			if err != nil {
				t.Fatal(err)
			}
			e.a.mu.Lock()
			room, _ := st.Get(info.ID)
			e.a.roomHumanSay(room, "李四", "大家好")
			e.a.mu.Unlock()
			return e.a.roomMemberSID(agents.DefaultID, info.ID)
		}},
		// 带规则的外部群
		{"group-external-scene", func(t *testing.T, e *env) string {
			e.sid = "fake:c1"
			e.say("大家好", group(true), from("u2", "李四"))
			e.k.expect(proto.TInput)
			e.a.mu.Lock()
			defer e.a.mu.Unlock()
			st := e.a.store.Settings(e.sid)
			st.Scene = &scene.Rules{Brief: "这是读书会的群，聊书为主。", Tools: []string{},
				Roles: map[string]scene.Role{agents.DefaultID: {Prompt: "你是读书会的主持人。"}}}
			if err := e.a.store.SaveSettings(e.sid, st); err != nil {
				t.Fatal(err)
			}
			return e.sid
		}},
	}
}

// renderSurface 是金样的内容：prefixVer、字节数、系统提示全文、工具定义（按 contextParts 返回的顺序，固定缩进）。
func renderSurface(system string, tools []llm.Tool) (text string, toolsJSON string, total int) {
	b, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		panic(err)
	}
	toolsJSON = string(b)
	if len(tools) == 0 {
		toolsJSON = "[]"
	}
	total = len(system) + len(toolsJSON)
	text = fmt.Sprintf("prefixVer: %d\nsystem bytes: %d\ntools bytes: %d\ntotal bytes: %d\n\n== system ==\n%s\n\n== tools ==\n%s\n",
		prefixVer, len(system), len(toolsJSON), total, system, toolsJSON)
	return
}

// 金样里的 prefixVer 和工具定义那一段。
func parseSurface(golden string) (ver int, tools string) {
	if rest, ok := strings.CutPrefix(golden, "prefixVer: "); ok {
		ver, _ = strconv.Atoi(strings.SplitN(rest, "\n", 2)[0])
	}
	if i := strings.Index(golden, "\n== tools ==\n"); i >= 0 {
		tools = golden[i+len("\n== tools ==\n"):]
	}
	return
}

func TestModelSurface(t *testing.T) {
	for _, role := range surfaceRoles() {
		t.Run(role.name, func(t *testing.T) {
			e := setup(t)
			sid := role.build(t, e)
			e.a.mu.Lock()
			system, tools, _, _ := e.a.contextParts(sid)
			e.a.mu.Unlock()
			text, toolsJSON, total := renderSurface(system, tools)
			text = strings.ReplaceAll(text, e.dir, "<tmp>")
			toolsJSON = strings.ReplaceAll(toolsJSON, e.dir, "<tmp>")
			if i := strings.Index(sid, "room:"); i >= 0 { // 群 ID 随机，金样里统一叫 r1
				text = strings.ReplaceAll(text, sid[i+5:], "r1")
				toolsJSON = strings.ReplaceAll(toolsJSON, sid[i+5:], "r1")
			}

			// 同样的输入必须得到同样的字节（遍历 map 的顺序不固定之类要按缓存回归修复）
			e.a.mu.Lock()
			system2, tools2, _, _ := e.a.contextParts(sid)
			e.a.mu.Unlock()
			if again, _, _ := renderSurface(system2, tools2); strings.ReplaceAll(again, e.dir, "<tmp>") != text {
				t.Fatal("同样的输入两次得到的字节不同：这是缓存回归，不能在测试里绕过")
			}

			if limit := surfaceCap[role.name]; limit == 0 {
				t.Logf("%s total=%d（上限还没定：取 %d）", role.name, total, total+total/10)
			} else if total > limit {
				t.Fatalf("系统提示加工具定义共 %d 字节，超过上限 %d（当时的值上浮 10%%）。模型面膨胀了吗？", total, limit)
			}

			path := filepath.Join("testdata", "model-surface", role.name+".golden")
			old, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if string(old) == text {
				return
			}
			oldVer, oldTools := parseSurface(string(old))
			toolsChanged := len(old) > 0 && oldTools != toolsJSON+"\n"
			if toolsChanged && oldVer == prefixVer {
				t.Fatalf("工具定义的字节变了，但 prefixVer（%d）没变：旧会话的工具快照不会刷新，缓存和请求前缀会不一致。先更新 context.go 里的 prefixVer。带 -update 也不会写入。", prefixVer)
			}
			if !*updateSurface {
				t.Fatalf("模型能看到的内容和金样不同（%s）。确认是有意的之后：go test ./internal/app -run TestModelSurface -update，并检查 prefixVer", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("已更新 %s", path)
		})
	}
}
