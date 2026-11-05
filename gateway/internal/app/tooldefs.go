package app

import (
	"encoding/json"
	"slices"
	"sort"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
)

// gwTool 是网关自己提供的一个工具。内核的工具（Read、Shell 等）和 MCP 工具不在这里。
// 哪类会话提供哪些工具写在 contextParts、toolsFor 里，但定义只从这张表取（toolDef）。
type gwTool struct {
	name string
	// spec 生成给模型的定义；ok 为 false 表示现在不提供（没配搜索、没有技能）。
	spec func(a *App, sid string) (proto.ToolSpec, bool)
	// call 是网关的执行入口。调用方持有锁；耗时的在里面自己起 goroutine。
	call func(a *App, s *sess, m *proto.Msg, reply func(bool, string))
	// state：走 gateway:tools 状态租约。
	state bool
	// started：受理后直接报“开始”；否则先报“排队”（有外部 I/O 的工具）。
	started bool
	// admin 不空：非管理员在排队前就被拒绝，内容是这句话。空表示处理器自己判断（见各 call）。
	admin string
	// scene：能出现在群聊和场景的工具表里（没设群聊白名单时的默认范围，也是网页上可选的范围）。
	scene bool
	// stage：戏里也能用（场内的事不进记忆，所以只有联网查资料的两个）。
	stage bool
}

func fixedSpec(f func() proto.ToolSpec) func(*App, string) (proto.ToolSpec, bool) {
	return func(*App, string) (proto.ToolSpec, bool) { return f(), true }
}

func gatewaySpec(name string) func(*App, string) (proto.ToolSpec, bool) {
	return func(a *App, _ string) (proto.ToolSpec, bool) {
		for _, t := range a.gatewayTools() {
			if t.Name == name {
				return t, true
			}
		}
		return proto.ToolSpec{}, false
	}
}

func searchSpec(name string) func(*App, string) (proto.ToolSpec, bool) {
	return func(a *App, _ string) (proto.ToolSpec, bool) {
		for _, t := range a.searchTools() {
			if t.Name == name {
				return t, true
			}
		}
		return proto.ToolSpec{}, false
	}
}

const adminOnlyMind = "只有管理员和 agent 自己的自主活动能修改日程、想法和心智状态"

// callOut 把“返回文本和错误”的处理器接成 reply。
func callOut(reply func(bool, string), out string, err error) {
	if err != nil {
		reply(false, err.Error())
	} else {
		reply(true, out)
	}
}

func rawOrEmpty(m *proto.Msg) string {
	if raw := string(m.Args); raw != "" {
		return raw
	}
	return "{}"
}

// 用 init 填表：处理器间接引用 contextParts，直接初始化会有循环。
var gwTools []gwTool

func init() {
	gwTools = []gwTool{
		{name: "WebSearch", spec: searchSpec("WebSearch"), scene: true, stage: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.webTool(s, m, reply) }},
		{name: "WebFetch", spec: searchSpec("WebFetch"), scene: true, stage: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.webTool(s, m, reply) }},
		{name: "Hook", spec: fixedSpec(hookTool), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.hookToolCall(s, m, reply) }},
		{name: "Job", spec: fixedSpec(jobTool), started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.jobToolCall(s, m, reply) }},
		{name: "Session", spec: fixedSpec(sessionTool), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				// 会话分叉在网关执行：不碰文件、不需要工作目录
				text, ok := a.handleSessionTool(s, m)
				reply(ok, text)
			}},
		{name: "Schedule", spec: fixedSpec(scheduleTool), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				var args ScheduleRequest
				if err := json.Unmarshal(m.Args, &args); err != nil {
					reply(false, "定时任务参数格式不正确")
					return
				}
				result, err := a.scheduleAction(s.sid, s.run.from, args)
				if err != nil {
					reply(false, err.Error())
					return
				}
				b, _ := json.Marshal(result)
				reply(true, string(b))
			}},
		{name: "Message", spec: gatewaySpec("Message"), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				// 管理员让它发，或者拟人型在内心活动里自己想找人（内心活动里有自己的规矩，见 messageChat）
				if !a.isAdmin(s.run.from) && !isLifeSID(s.sid) {
					reply(false, "只有管理员能用 Message 给群和别的 agent 发消息")
					return
				}
				go a.messageTool(s.sid, rawOrEmpty(m), reply)
			}},
		{name: "SendFile", spec: fixedSpec(sendFileTool), state: true, started: true, scene: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.sendFileCall(s, m, reply) }},
		{name: "FollowUp", spec: fixedSpec(followUpTool), state: true, started: true, scene: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.followUpCall(s.sid, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Agenda", spec: fixedSpec(agendaTool), state: true, started: true, admin: adminOnlyMind,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.agendaCall(s.sid, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Report", spec: func(a *App, sid string) (proto.ToolSpec, bool) { return a.reportSpec(sid) }, state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.reportCall(s, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Ticket", spec: func(a *App, sid string) (proto.ToolSpec, bool) { return a.ticketSpec(sid) }, state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.ticketCall(s, m.Args)
				callOut(reply, out, err)
			}},
		{name: "External", spec: func(a *App, sid string) (proto.ToolSpec, bool) { return a.externalSpec(sid) }, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.externalCall(s, m, reply) }},
		{name: "Idea", spec: fixedSpec(ideaTool), state: true, started: true, admin: adminOnlyMind,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.ideaCall(s.sid, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Mind", spec: fixedSpec(mindTool), state: true, started: true, admin: adminOnlyMind,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.mindCall(s.sid, s.run.runID, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Diary", spec: fixedSpec(diaryTool), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.diaryCall(s.sid, m.Args)
				callOut(reply, out, err)
			}},
		{name: knowledgeLabel, spec: fixedSpec(knowledgeTool), state: true, started: true, scene: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				out, err := a.knowledgeCall(s.sid, s.run.from, m.Args)
				callOut(reply, out, err)
			}},
		{name: "Skill", spec: func(a *App, sid string) (proto.ToolSpec, bool) { return a.skillSpec(sid) }, state: true, started: true, scene: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) { a.skillTool(s.run, s.sid, m, reply) }},
		{name: "Gateway", spec: gatewaySpec("Gateway"), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				if !a.isAdmin(s.run.from) {
					reply(false, "只有管理员能用网关管理工具")
					return
				}
				go a.gatewayQuery(s.sid, rawOrEmpty(m), reply)
			}},
		{name: "GatewayAdmin", spec: gatewaySpec("GatewayAdmin"), state: true, started: true,
			call: func(a *App, s *sess, m *proto.Msg, reply func(bool, string)) {
				if !a.isAdmin(s.run.from) {
					reply(false, "只有管理员能用网关管理工具")
					return
				}
				go a.gatewayAdminCall(s.sid, s.run.from, rawOrEmpty(m), reply)
			}},
	}
}

// toolDef 按名字找网关工具；不是网关工具（内核工具、MCP 工具）返回 nil。
func toolDef(name string) *gwTool {
	for i := range gwTools {
		if gwTools[i].name == name {
			return &gwTools[i]
		}
	}
	return nil
}

// toolOfName 是会话 sid 现在能拿到的某个网关工具的定义；现在不提供时 ok 为 false。调用方持有锁。
func (a *App) toolOfName(sid, name string) (llm.Tool, bool) {
	t := toolDef(name)
	if t == nil {
		return llm.Tool{}, false
	}
	spec, ok := t.spec(a, sid)
	if !ok {
		return llm.Tool{}, false
	}
	return toolOf(spec), true
}

// toolsNamed 按名字取现在提供的网关工具的定义，不提供的跳过。调用方持有锁。
func (a *App) toolsNamed(sid string, names ...string) []llm.Tool {
	var out []llm.Tool
	for _, name := range names {
		if t, ok := a.toolOfName(sid, name); ok {
			out = append(out, t)
		}
	}
	return out
}

// sceneToolNames 是群聊和场景工具表的可选范围，也是没设群聊白名单时的默认值。
func sceneToolNames() []string {
	var out []string
	for _, t := range gwTools {
		if t.scene {
			out = append(out, t.name)
		}
	}
	return out
}

// stageToolNames 是戏里可选的工具（登记表里 stage 为真的）。
func stageToolNames() []string {
	var out []string
	for _, t := range gwTools {
		if t.stage {
			out = append(out, t.name)
		}
	}
	return out
}

// gatewayToolNames 是表里全部网关工具的名字。
func gatewayToolNames() []string {
	out := make([]string, 0, len(gwTools))
	for _, t := range gwTools {
		out = append(out, t.name)
	}
	sort.Strings(out)
	return out
}

// unofferedCall 是模型提出的调用里第一个不在这一轮工具表里的工具名；都在返回空。
func unofferedCall(calls []llm.ToolCall, offered []llm.Tool) string {
	for _, c := range calls {
		if !slices.ContainsFunc(offered, func(t llm.Tool) bool { return t.Function.Name == c.Function.Name }) {
			return c.Function.Name
		}
	}
	return ""
}

// SceneToolNames 是场景里可选的工具（登记表里 scene 为真的），网页用它列选项。
func (a *App) SceneToolNames() []string { return sceneToolNames() }

// StageToolNames 是戏里可选的工具，网页开了 stage 的群用它列选项。
func (a *App) StageToolNames() []string { return stageToolNames() }
