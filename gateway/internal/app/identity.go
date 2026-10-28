package app

// 会话的身份：谁在执行，和会话 ID 的路由命名空间，是两件事。
//
// 以前“会话属于哪个 agent”只由会话 ID 的前缀决定（main、tg:123 属于默认 agent，@bob/xxx 属于 bob）。
// 现在网页里可以把一个已有会话交给别的 agent：Settings.Agent 记下“当前执行身份”，会话 ID、历史、
// 窗口接线都不变。
//
//   - 执行身份（execAgent）：人格、模型、工具、限制、记忆、交接说明用哪个 agent。本文件的 execAgentID。
//   - 路由命名空间（routeAgent）：会话 ID 里的前缀。机器人窗口重新接线、mainOf（哪个是主会话）、
//     @<agent>/cron、@<agent>/life 这些都继续按它算，绝不跟着切换走——否则用户切了 main 以后，
//     机器人的下一条消息会把窗口“改正”回原来的会话，表现为切换不生效。
//
// 内部会话（内心活动、整理记忆、定时任务、房间成员、房间记录）身份固定，不允许切换。

import (
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/session"
)

// maxAgentSegments 是最多保留多少段切换记录（旧的先丢；消息自己的 Agent 字段还在，够用了）。
const maxAgentSegments = 50

// routeAgentID 是会话 ID 的命名空间属于哪个 agent。路由（窗口接线、主会话、内部会话）按它算。
func (a *App) routeAgentID(sid string) string { return agents.SessionAgent(sid) }

// fixedAgentSID 是身份固定、不能切换 agent 的会话：agent 自己的内心活动、整理记忆、定时任务，
// 以及房间成员（@<agent>/room:<id>）和房间记录（room:<id>，没有单一 agent）。
func fixedAgentSID(sid string) bool {
	return sid == "" || isInnerSID(sid) || isCronSID(sid) || isRoomMemberSID(sid) || strings.HasPrefix(sid, "room:")
}

// agentExists 判断 agent 列表里有没有这个 ID。调用方持有锁。
func (a *App) agentExists(id string) bool {
	if a.agentStore == nil {
		return id == agents.DefaultID
	}
	_, ok := a.agentStore.Get(id)
	return ok
}

// execAgentID 是会话当前的执行身份。调用方持有锁。
//
//	显式选择（Settings.Agent）优先；被选中的 agent 删掉后回默认 agent（不按前缀猜，免得悄悄换回去）。
//	没有显式选择时按 ID 前缀推断；前缀的 agent 删掉后同样是默认 agent。
//	内部会话永远按 ID 前缀（固定身份）。
func (a *App) execAgentID(sid string) string {
	if sid == "" {
		return agents.DefaultID
	}
	if br, ok := a.store.Branch(sid); ok && br.OwnerAgent != "" {
		return br.OwnerAgent
	}
	if fixedAgentSID(sid) || a.agentStore == nil {
		return a.routeAgentID(sid)
	}
	if id := a.store.Settings(sid).Agent; id != "" {
		if a.agentExists(id) {
			return id
		}
		return agents.DefaultID
	}
	id := a.routeAgentID(sid)
	if a.agentExists(id) {
		return id
	}
	return agents.DefaultID
}

// execAgent 是会话当前的执行 agent。找不到时给出一个能显示的身份（不返回零值）。调用方持有锁。
func (a *App) execAgent(sid string) agents.Agent {
	return a.agentByIDOrFallback(a.execAgentID(sid))
}

// agentByIDOrFallback 按 ID 找 agent；没有 agent 列表或找不到时给一个带名字的替身。调用方持有锁。
func (a *App) agentByIDOrFallback(id string) agents.Agent {
	if a.agentStore == nil {
		if id == agents.DefaultID {
			return agents.Agent{ID: agents.DefaultID, Name: "小纳"}
		}
		return agents.Agent{ID: id, Name: id}
	}
	if ag, ok := a.agentStore.Get(id); ok {
		return ag
	}
	if ag, ok := a.agentStore.Get(agents.DefaultID); ok {
		return ag
	}
	return agents.Agent{ID: agents.DefaultID, Name: "小纳"}
}

// hasExplicitAgent 是这个会话被用户明确选过 agent（不是按 ID 前缀推断的），而且那个 agent 还在。
// 机器人换绑时窗口要不要跟着换会话，就看这个：用户手动选的不能被下一条消息改回去。调用方持有锁。
func (a *App) hasExplicitAgent(sid string) bool {
	id := a.store.Settings(sid).Agent
	return id != "" && a.agentExists(id)
}

// agentSwitchBlocked 是现在不能切换 agent 的原因，空表示可以。调用方持有锁。
func (a *App) agentSwitchBlocked(sid string) string {
	if a.isBranchSID(sid) {
		return "分支固定创建时的执行身份"
	}
	if fixedAgentSID(sid) {
		return "这是 agent 自己的内部会话（内心活动、整理记忆、定时任务或房间成员），身份是固定的"
	}
	if a.agentStore == nil {
		return "还没有准备好 agent 列表"
	}
	if s := a.sessions[sid]; s != nil {
		if s.compacting {
			return "正在整理上下文，等它结束再切换"
		}
		if s.run != nil {
			return "这个会话正在处理消息，等这一轮结束再切换"
		}
		if len(s.queue) > 0 {
			return "这个会话还有排队的消息，等它们处理完再切换"
		}
	}
	return ""
}

// handoffText 是切换 agent 后给新 agent 的交接说明：只放在下一条用户消息前，不改系统提示词。
func handoffText(oldAg, newAg agents.Agent, hasHistory bool) string {
	oldName := displayAgentName(oldAg)
	newName := displayAgentName(newAg)
	if !hasHistory {
		return "[系统] 这个会话现在由你（" + newName + "）来聊。"
	}
	return "[系统] 这个会话从「" + oldName + "」换成了你（" + newName + "）。保留着之前的聊天记录，" +
		"里面 assistant 的回复可能是对方或之前的 agent 说的，不代表你的看法；从现在起由你接着聊，" +
		"不需要重述过去，也不用为之前的话道歉。"
}

func displayAgentName(ag agents.Agent) string {
	if ag.ID == "" || ag.ID == ag.Name {
		return ag.Name
	}
	if ag.Name == "" {
		return ag.ID
	}
	return ag.Name + "（" + ag.ID + "）"
}

// recordAgentSwitch 把切换写进会话设置：显式选择、来源区间、交接说明，并清掉按旧身份构造的缓存。
// position 是切换时的历史绝对位置（含已经被整理掉的消息）。调用方持有锁。
func (a *App) recordAgentSwitch(sid string, st *session.Settings, oldAg, newAg agents.Agent, position int) {
	a.lifecycleExperienceFor(oldAg.ID, sid, "会话交给了 "+newAg.Name)
	st.Agent = newAg.ID
	st.AgentSegments = append(st.AgentSegments, session.AgentSegment{Agent: newAg.ID, At: a.now().Unix(), Position: position})
	if n := len(st.AgentSegments); n > maxAgentSegments {
		st.AgentSegments = append([]session.AgentSegment(nil), st.AgentSegments[n-maxAgentSegments:]...)
	}
	st.Handoff = handoffText(oldAg, newAg, position > 0)
	// 新身份：工具快照、环境说明、记忆/近况注入、想起来的记忆都按它重新构造
	st.Prefix, st.EnvNote, st.MemNote, st.LifeNote = nil, "", "", ""
	st.Recalled = nil
}

// msgAgent 是一条历史消息的来源 agent。index 是它在完整历史里的绝对位置（Archive 的下标）。
// 消息自己记着（v0.7.3 起写的）就用它；老消息按切换区间推断，再往前算会话 ID 前缀的 agent。
// 调用方持有锁。
func (a *App) msgAgent(sid string, index int, m session.Msg) string {
	if m.Agent != "" {
		return m.Agent
	}
	st := a.store.Settings(sid)
	for i := len(st.AgentSegments) - 1; i >= 0; i-- {
		if index >= st.AgentSegments[i].Position {
			return st.AgentSegments[i].Agent
		}
	}
	return a.routeAgentID(sid)
}

// clearAgentBoundState 清掉旧 agent 对这个会话留下的主动状态：约好的回访（FollowUp）、
// 主动发了还没回的标记。旧 agent 的回访不能在新 agent 的会话里继续执行。调用方持有锁。
func (a *App) clearAgentBoundState(sid, oldAgentID string) {
	if oldAgentID == "" || a.agentRoot == "" {
		return
	}
	ls := a.lifeOf(oldAgentID)
	changed := false
	if _, ok := ls.Awaiting[sid]; ok {
		delete(ls.Awaiting, sid)
		changed = true
	}
	if _, ok := ls.FollowUps[sid]; ok {
		delete(ls.FollowUps, sid)
		changed = true
	}
	if changed {
		a.saveLife()
	}
	// 群聊里已经排着的“要不要插一句”的触发按旧身份算的：换人了就不算数
	if s := a.sessions[sid]; s != nil {
		if s.groupTimer != nil {
			s.groupTimer.Stop()
			s.groupTimer = nil
		}
		s.groupFresh, s.groupProactive = false, false
	}
	// 旧 agent 的自主日程里和这个聊天有关的约定 / 复查也作废
	a.mindDropSession(sid, oldAgentID)
}

// followedByAgent 是 FollowUp/主动消息执行前的归属校验：这个会话现在还是当初那个 agent 的吗？
// 旧文件、恢复队列里带来的回调也要过这一关。调用方持有锁。
func (a *App) followedByAgent(sid, agentID string) bool {
	return a.agentExists(agentID) && a.execAgentID(sid) == agentID
}

// isOwnerSID 是这个会话是不是 agent 的“主人会话”：@<id>/main，或者默认的 main 被交给了它。
// 切换主会话以后，新 agent 的 life 也要能认出来；旧 agent 不能再往里面主动发消息。
func (a *App) isOwnerSID(agentID, sid string) bool {
	// 默认 agent 的 SessionFor 就是 "main"，但它也可能被交给了别人：main 必须看当前执行身份
	if sid == mainSID {
		return a.execAgentID(sid) == agentID
	}
	return sid == agents.SessionFor(agentID, mainSID)
}
