package app

// 群聊（阶段 7）。
//
// 群里每条消息都先记进历史，但不触发模型（只是“听着”）。被 @、回复了机器人的消息、或者叫到
// agent 的名字时回一句；群里安静下来、额度又够的时候可以自己插一句，模型不想说话就只回 [不回复]。
// 群里的回复短、口语，不发思考、工具过程和用量；工具只有 agent 的群聊白名单（默认只有搜索）。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/session"
)

// defaultGroupGuide 是没写 agents/<id>/group.md 时用的群聊守则。
const defaultGroupGuide = `这是群聊，不是私聊：
- 像群里的一个人那样说话：口语、简短，一般一到两句。
- 不用标题、列表、代码块；不说客套话，不说“作为一个 AI”。
- 不提工具、执行过程、上下文和用量。
- 不知道、不相关、或者别人在聊别的事，就别说话，只回 [不回复]。
- 一次只说一件事，别把群里的话都接一遍。`

// groupRule 是会话所属 agent 的群聊规则。
type groupRule struct {
	Eagerness string        // off / low / normal / high
	PerHour   int           // 一小时内最多主动发言几次
	Quiet     time.Duration // 群里安静多久以后才可能插话
	MinGap    time.Duration // 两次发言之间至少隔多久
	MaxLen    int           // 一条回复最多多少字
	Parts     int           // 最多拆成几条发
	Decisions int           // 一小时内最多判断几次（判断本身也是一次模型调用）
	ThinkGap  time.Duration // 两次判断之间至少隔多久
}

// groupPerHour 把积极度换成每小时的主动发言上限。
func groupPerHour(v string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "关", "关闭", "不":
		return "off", 0
	case "high", "高":
		return "high", 15
	case "normal", "medium", "中":
		return "normal", 6
	}
	return "low", 2
}

// groupRuleOf 是会话所属 agent 的群聊规则。调用方持有锁。
func (a *App) groupRuleOf(sid string) groupRule {
	ag := a.agentOf(sid)
	eag, per := groupPerHour(pickString(ag.GroupEagerness, "low"))
	// “要不要说话”的判断本身也要花一次模型调用：除了说出口的额度，判断也单独限一下
	r := groupRule{Eagerness: eag, PerHour: per, Quiet: 20 * time.Second, MinGap: 10 * time.Second, MaxLen: 300, Parts: 3,
		Decisions: max(2, per*3), ThinkGap: time.Minute}
	if d := parseLimitDuration(pickString(ag.GroupQuiet, "")); d > 0 {
		r.Quiet = d
	}
	if d := parseLimitDuration(pickString(ag.GroupMinGap, "")); d > 0 {
		r.MinGap = d
	}
	if ag.GroupMaxLen != nil && *ag.GroupMaxLen > 0 {
		r.MaxLen = *ag.GroupMaxLen
	}
	if ag.GroupParts != nil && *ag.GroupParts > 0 && *ag.GroupParts <= 5 {
		r.Parts = *ag.GroupParts
	}
	return r
}

// groupGuide 是 agent 的群聊守则：agents/<id>/group.md，没有就用内置的。调用方持有锁。
func (a *App) groupGuide(agentID string) string {
	if a.personaAgent != nil {
		if t, _ := a.personaAgent.File(filepath.Join(agentID, "group.md")); strings.TrimSpace(t) != "" {
			return t
		}
	}
	return defaultGroupGuide
}

// isGroupSession 是会话属于某个群聊。调用方持有锁。
func (a *App) isGroupSession(sid string) bool { return a.store.Settings(sid).Group }

// groupLine 是历史里的一行群消息：[15:08][张三] 你好
func (a *App) groupLine(m channel.Message) string {
	name := strings.TrimSpace(m.SenderName)
	if name == "" {
		name = "某人"
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && len(m.Images) > 0 {
		text = fmt.Sprintf("（%d 张图片）", len(m.Images))
	}
	if text == "" {
		return ""
	}
	return fmt.Sprintf("[%s][%s] %s", a.now().In(a.loc).Format("15:04"), name, text)
}

// listenGroup 把一条群消息记进历史（不触发模型）。调用方持有锁。
func (a *App) listenGroup(s *sess, ep endpoint, m channel.Message, who string) {
	line := a.groupLine(m)
	if line == "" {
		return
	}
	// 记下群里谁是谁（名字 → 身份），整理记忆时把群友的画像和身份对上
	if name := strings.TrimSpace(m.SenderName); name != "" && who != "" {
		if st := a.store.Settings(s.sid); st.Members[name] != who {
			if st.Members == nil {
				st.Members = map[string]string{}
			}
			if len(st.Members) < 500 {
				st.Members[name] = who
				if err := a.store.SaveSettings(s.sid, st); err != nil {
					a.log.Warn("保存群成员失败", "sid", s.sid, "err", err)
				}
			}
		}
	}
	if err := a.store.Append(s.sid, session.Msg{Role: "user", Content: line}); err != nil {
		a.log.Warn("保存群消息失败", "sid", s.sid, "err", err)
	}
	s.groupFresh = true
	s.groupLast = a.now()
	s.groupEp = ep
	a.publish(events.Event{T: "message", SID: s.sid, Name: m.SenderName, Text: strings.TrimSpace(m.Text),
		Data: map[string]any{"via": sourceOf(ep.ch), "channel": chanName(ep.ch), "group": true}})
}

// groupMentionsName 是消息里叫了 agent 的名字。
func groupMentionsName(text, name string) bool {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 {
		return false
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(name))
}

// groupReply 排一次回复：等 2 秒，把紧接着的几条一起交给模型。调用方持有锁。
func (a *App) groupReply(s *sess, ep endpoint, who string) {
	if s.groupTimer != nil {
		return // 已经排上了，后面的消息都进了历史，等这一次一起看
	}
	sid, key := s.sid, ep.key()
	s.groupTimer = time.AfterFunc(2*time.Second, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.sessions[sid] != s {
			return
		}
		s.groupTimer = nil
		s.groupFresh = false
		s.groupProactive = false
		ep2 := parseEndpoint(key)
		s.queue = append(s.queue, queued{ep: ep2, who: who, trigger: true, m: channel.Message{ChatID: ep2.chat, Group: true}})
		a.startRun(s)
	})
}

// groupLoop 每秒看一遍群会话：安静够了、有新消息、额度够，就让 agent 决定要不要插一句。
func (a *App) groupLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.groupTick()
		}
	}
}

// groupTick 是一轮检查，测试里可以直接调。
// groupMayThink 判断现在能不能做一次“要不要说话”的判断。判断本身也是一次模型调用，
// 所以除了说出口的额度，判断也单独限：两次至少隔 ThinkGap，一小时内最多 Decisions 次。
// 调用方持有锁。
func (a *App) groupMayThink(s *sess, rule groupRule, now time.Time) bool {
	if rule.Decisions <= 0 {
		return false
	}
	if !s.groupThoughtAt.IsZero() && now.Sub(s.groupThoughtAt) < rule.ThinkGap {
		return false
	}
	s.groupThoughts = cleanupGroupSpeaks(s.groupThoughts, now)
	return len(s.groupThoughts) < rule.Decisions
}

func (a *App) groupTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for _, s := range a.sessions {
		if s == nil || s.groupEp.chat == "" || !a.isGroupSession(s.sid) {
			continue
		}
		if s.run != nil || len(s.queue) > 0 || s.compacting || s.groupTimer != nil || !s.groupFresh {
			continue
		}
		// 房间里的成员：点名模式下永远不主动开口，只有自由模式交给这里按额度插话
		if room, ok := a.roomOfSession(s.sid); ok {
			if s.sid == roomLogSID(room.ID) || room.Paused || room.Mode != rooms.ModeFree || room.MutedAgent(roomSender(s.sid)) {
				continue
			}
		}
		rule := a.groupRuleOf(s.sid)
		if rule.PerHour <= 0 {
			continue
		}
		if now.Sub(s.groupLast) < rule.Quiet {
			continue
		}
		if !s.groupSpokeAt.IsZero() && now.Sub(s.groupSpokeAt) < rule.MinGap {
			continue
		}
		s.groupSpeaks = cleanupGroupSpeaks(s.groupSpeaks, now)
		if len(s.groupSpeaks) >= rule.PerHour {
			continue
		}
		if !a.groupMayThink(s, rule, now) {
			continue
		}
		s.groupThoughtAt = now
		s.groupThoughts = append(s.groupThoughts, now)
		s.groupFresh = false
		s.groupProactive = true
		s.queue = append(s.queue, queued{ep: s.groupEp, who: "group", trigger: true,
			m: channel.Message{ChatID: s.groupEp.chat, Group: true}})
		a.startRun(s)
	}
}

// groupTools 是群聊里能用的工具：agent 的群聊白名单，没设就只有搜索和技能。
func (a *App) groupTools(sid string, all []llm.Tool) []llm.Tool {
	allow := a.agentOf(sid).GroupTools
	if len(allow) == 0 {
		allow = []string{"WebSearch", "WebFetch", "Skill", knowledgeLabel}
	}
	out := make([]llm.Tool, 0, len(allow))
	for _, t := range all {
		for _, name := range allow {
			if t.Function.Name == name {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// sendGroup 用人的方式发群消息：按空行拆成最多 N 条，每条不超过长度上限，中间停一会儿。
// 模型只回 [不回复] 就什么都不发。调用方持有锁。
func (a *App) sendGroup(s *sess, ep endpoint, text string) {
	text = strings.TrimSpace(text)
	if text == "" || (strings.Contains(text, "[不回复]") && len([]rune(text)) <= 10) {
		return
	}
	rule := a.groupRuleOf(s.sid)
	parts := groupParts(text, rule.Parts, rule.MaxLen)
	if len(parts) == 0 {
		return
	}
	s.groupSpokeAt = a.now()
	s.groupSpeaks = append(s.groupSpeaks, s.groupSpokeAt)
	if len(s.groupSpeaks) > 50 {
		s.groupSpeaks = append([]time.Time(nil), s.groupSpeaks[len(s.groupSpeaks)-50:]...)
	}
	go func() {
		for i, p := range parts {
			if i > 0 {
				// 像人一样停一下，最多 3 秒
				d := time.Duration(len([]rune(p))) * 20 * time.Millisecond
				if d > 3*time.Second {
					d = 3 * time.Second
				}
				if d < 300*time.Millisecond {
					d = 300 * time.Millisecond
				}
				select {
				case <-time.After(d):
				case <-a.ctx.Done():
					return
				}
			}
			ch := a.chans[ep.ch]
			if ch == nil {
				return
			}
			ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
			if err := ch.Send(ctx, ep.chat, p, ""); err != nil {
				a.log.Warn("群消息发送失败", "channel", ep.ch, "err", err)
			}
			cancel()
		}
	}()
}

// groupParts 把回复拆成几条：先按空行拆，一条太长就在句号处截断，最多 maxParts 条、每条最多 limit 字。
func groupParts(text string, maxParts, limit int) []string {
	if limit <= 0 {
		limit = 300
	}
	if maxParts <= 0 {
		maxParts = 1
	}
	var out []string
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.TrimSpace(block)
		for block != "" && len(out) < maxParts {
			if len([]rune(block)) <= limit {
				out = append(out, block)
				break
			}
			runes := []rune(block)
			cut := limit
			for i := limit - 1; i > limit/2; i-- {
				if strings.ContainsRune("。！？!?\n", runes[i]) {
					cut = i + 1
					break
				}
			}
			out = append(out, strings.TrimSpace(string(runes[:cut])))
			block = strings.TrimSpace(string(runes[cut:]))
		}
		if len(out) >= maxParts {
			break
		}
	}
	return out
}

// cleanupGroupSpeaks 丢掉一小时以前的主动发言记录。
func cleanupGroupSpeaks(speaks []time.Time, now time.Time) []time.Time {
	out := makeslice(speaks)
	for _, at := range speaks {
		if now.Sub(at) < time.Hour {
			out = append(out, at)
		}
	}
	return out
}

func makeslice(in []time.Time) []time.Time { return make([]time.Time, 0, len(in)) }
