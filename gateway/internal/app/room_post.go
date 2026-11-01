package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/session"
)

// 内部群里的每一句话都走 roomPost：记进群记录，发给听得到的成员，再按发言规则决定叫谁说话。

// 句子的种类。
const (
	lineSay     = "say"     // 说话（人或 agent）
	lineNarrate = "narrate" // 旁白
	lineCue     = "cue"     // 导演只对一个成员说
)

const narratorName = "旁白"

type roomLine struct {
	From    string // agent ID；人和旁白为空
	Name    string // 显示名；旁白为“旁白”
	Kind    string // lineSay | lineNarrate | lineCue
	Channel string // 私下频道 ID；空表示公开
	To      string // cue 的对象
	Text    string
}

type postOpt struct {
	asUser bool                     // 记进群记录时算“用户消息”（Message 工具的老做法），否则 agent 的话算助手消息
	wake   func(member string) bool // 听到之后要不要叫这个成员说话；回合制的模式会按规则忽略它
}

// recordText 是群记录里的一行：`[名字] …`、`[#频道][名字] …`、`[旁白] …`、`[导演→名字] …`。
func (a *App) recordText(room rooms.Room, l roomLine) string {
	switch l.Kind {
	case lineNarrate:
		if c, ok := room.ChannelByID(l.Channel); ok {
			return "[#" + c.Name + "][" + narratorName + "] " + l.Text
		}
		return "[" + narratorName + "] " + l.Text
	case lineCue:
		return "[导演→" + a.agentDisplay(l.To) + "] " + l.Text
	}
	name := l.Name
	if name == "" {
		name = "有人"
	}
	if c, ok := room.ChannelByID(l.Channel); ok {
		return "[#" + c.Name + "][" + name + "] " + l.Text
	}
	return "[" + name + "] " + l.Text
}

func (a *App) agentDisplay(id string) string {
	if ag, ok := a.agentByID(id); ok {
		return ag.Name
	}
	return id
}

// hears 这个成员听不听得到这一句：公开的全体都收；频道里的话只有频道成员收；cue 只有对象收。
func hears(room rooms.Room, l roomLine, member string) bool {
	switch {
	case l.Kind == lineCue:
		return member == l.To
	case l.Channel != "":
		c, ok := room.ChannelByID(l.Channel)
		return ok && slicesContains(c.Members, member)
	}
	return true
}

func slicesContains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// memberText 是成员会话里收到的那一行：公开的和群记录一样；cue 写作 `[导演] …`。
func (a *App) memberText(room rooms.Room, l roomLine) string {
	if l.Kind == lineCue {
		return "[导演] " + l.Text
	}
	return a.recordText(room, l)
}

// roomPost 发一句话。调用方持有锁。
func (a *App) roomPost(room rooms.Room, l roomLine, o postOpt) {
	l.Text = strings.TrimSpace(l.Text)
	if l.Text == "" {
		return
	}
	// 群记录收下每一句
	role, agent := "user", ""
	if l.Kind == lineSay && l.From != "" && !o.asUser {
		role, agent = "assistant", l.From
	}
	log := roomLogSID(room.ID)
	if err := a.store.Append(log, session.Msg{Role: role, Content: a.recordText(room, l), Agent: agent}); err != nil {
		a.log.Warn("保存房间记录失败", "room", room.ID, "err", err)
	}
	if l.From != "" {
		a.publish(events.Event{T: "say", SID: log, Text: l.Text})
	}
	// 回合制按规则叫人，@ 和叫名字不触发；轮流也只在公开频道里轮
	wake := o.wake
	switch room.Mode {
	case scene.ModeDirector:
		wake = nil
	case scene.ModeTurns:
		if l.Channel == "" {
			wake = nil
		}
	}
	// 成员会话只收它听得到的；说话的 agent 自己不再收一遍
	for _, member := range room.Members {
		if member == l.From || !hears(room, l, member) {
			continue
		}
		sid := a.roomMemberSID(member, room.ID)
		st := a.store.Settings(sid)
		if !st.Group {
			st.Group = true
			if err := a.store.SaveSettings(sid, st); err != nil {
				a.log.Warn("保存房间成员会话失败", "sid", sid, "err", err)
			}
		}
		if err := a.store.Append(sid, session.Msg{Role: "user", Content: a.memberText(room, l)}); err != nil {
			a.log.Warn("保存房间消息失败", "sid", sid, "err", err)
			continue
		}
		s := a.sessOf(sid)
		s.groupFresh = true
		s.groupPending++
		s.groupLast = a.now()
		s.groupEp = endpoint{ch: "room", chat: room.ID}
		// 暂停了：消息照样进成员的会话（它们还能听到），只是不叫人回
		if room.Paused || room.MutedAgent(member) || wake == nil || l.Kind == lineCue {
			continue
		}
		if wake(member) {
			a.roomTrigger(s, member, room, l.Channel)
		}
	}
	a.publish(events.Event{T: "rooms"})
	// 人或旁白在公开频道说了一句：轮流模式从这里开始转
	if l.From == "" && l.Kind != lineCue && l.Channel == "" && room.Mode == scene.ModeTurns {
		a.roomTurnStart(room)
	}
}

// roomHumanSay 是人在群里说的话（公开）。群记录由 roomPost 记。调用方持有锁。
func (a *App) roomHumanSay(room rooms.Room, name, text string) {
	if name == "" {
		name = "有人"
	}
	a.roomPost(room, roomLine{Name: name, Kind: lineSay, Text: text}, postOpt{
		wake: func(m string) bool { return a.roomShouldSpeak(room, m, strings.TrimSpace(text)) }})
}

// roomAgentSay 是 agent 在房间里说了一句（channel 为空表示公开）：记进群记录、转给听得到的成员、看看要不要暂停。
// 调用方持有锁。
func (a *App) roomAgentSay(room rooms.Room, member, text, channelID string) {
	text = strings.TrimSpace(text)
	if isSilence(text) {
		return
	}
	a.roomPost(room, roomLine{From: member, Name: a.agentDisplay(member), Kind: lineSay, Channel: channelID, Text: text}, postOpt{
		wake: func(m string) bool {
			// 在群里 @ 了或者叫到别的成员：叫醒它（连续发言、来回轮数的暂停规则兜底）
			other, ok := a.agentByID(m)
			return ok && (strings.Contains(text, "@"+other.Name) || groupMentionsName(text, other.Name))
		}})
	a.signalRoomWaiters(room.ID, member, text)
	if !room.Turn() { // 回合制由轮数、预算和导演控制，不用防刷屏阈值
		a.roomGuard(room, member)
	}
}

// roomTrigger 让某个成员在房间里说一句（走群聊那一套：先攒 2 秒）；channelID 不空表示在那个频道里回。
// 调用方持有锁。
func (a *App) roomTrigger(s *sess, member string, room rooms.Room, channelID string) {
	if s.groupTimer != nil {
		return
	}
	// 两次发言之间留一点间隔
	rule := a.groupRuleOf(s.sid)
	if !s.groupSpokeAt.IsZero() && a.now().Sub(s.groupSpokeAt) < rule.MinGap {
		return
	}
	sid := s.sid
	s.groupTimer = time.AfterFunc(2*time.Second, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.sessions[sid] != s {
			return
		}
		s.groupTimer = nil
		s.groupFresh = false
		s.groupProactive, s.groupObserve = false, nil
		a.roomStart(s, member, room, a.roomChannelHint(room, channelID), channelID)
	})
}

// roomStart 立刻让成员在群里说一句：hint 只放进这一次请求，channelID 是回复要去的频道。调用方持有锁。
func (a *App) roomStart(s *sess, member string, room rooms.Room, hint, channelID string) {
	s.roomHint, s.roomChan = hint, channelID
	ep := endpoint{ch: "room", chat: room.ID}
	s.queue = append(s.queue, queued{ep: ep, who: "room:" + member, trigger: true,
		m: channel.Message{ChatID: room.ID, Group: true}})
	a.startRun(s)
}

// roomChannelHint 是在频道里说话时请求末尾的提示：这一句在哪个频道、谁听得到。
func (a *App) roomChannelHint(room rooms.Room, channelID string) string {
	c, ok := room.ChannelByID(channelID)
	if !ok {
		return ""
	}
	var names []string
	for _, m := range c.Members {
		names = append(names, a.agentDisplay(m))
	}
	return fmt.Sprintf("[系统] 这一句要说在私下频道「%s」里，只有 %s 听得到。", c.Name, strings.Join(names, "、"))
}

// RoomSay 是网页上往群里说一句（POST /api/rooms/{id}/say）：as 是“旁白”时记作旁白，否则用这个名字
// （空就用 defaultName）；channelID 空表示公开。
func (a *App) RoomSay(id, text, as, channelID, defaultName string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return errors.New("房间列表还没准备好")
	}
	room, ok := a.roomStore.Get(id)
	if !ok {
		return errors.New("没有这个群")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("话是空的")
	}
	if channelID != "" {
		if _, ok := room.ChannelByID(channelID); !ok {
			return fmt.Errorf("没有这个频道：%s", channelID)
		}
	}
	as = strings.TrimSpace(as)
	if as == narratorName {
		a.roomPost(room, roomLine{Name: narratorName, Kind: lineNarrate, Channel: channelID, Text: text}, postOpt{})
		a.publish(events.Event{T: "say", SID: roomLogSID(room.ID), Text: "[" + narratorName + "] " + text})
		return nil
	}
	if as == "" {
		as = defaultName
	}
	if as == "" {
		as = "有人"
	}
	a.roomHumanLine(room.ID)
	a.publish(events.Event{T: "message", SID: roomLogSID(room.ID), Name: as, Text: text})
	a.roomPost(room, roomLine{Name: as, Kind: lineSay, Channel: channelID, Text: text}, postOpt{
		wake: func(m string) bool { return a.roomShouldSpeak(room, m, text) }})
	return nil
}
