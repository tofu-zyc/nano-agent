package app

// 网关内部的群聊（阶段 8）：网页上建"房间"，选哪些 agent 进去，agent 在群里说的话大家都能看到；
// 两个 agent 之间的私聊就是只有这两个成员的房间，第一次发消息时自动建。
//
// 聊天记录就是网关里的一个会话 room:<id>（网页的会话页直接能看、能发言），
// 每个成员 agent 在这个群里还有自己的会话 @<agent>/room:<id>，它在那边听和说。
// 说话方式沿用阶段 7 的群聊引擎：短、口语、不发思考，被叫到才说（或者自由模式按积极度插话）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/session"
)

// isRoomMemberSID 是 agent 在房间里旁听的会话（@<agent>/room:<id>）：内部用，不当普通会话显示。
func isRoomMemberSID(sid string) bool { return strings.Contains(sid, "/room:") }

// roomLogSID 是房间的聊天记录会话。
func roomLogSID(id string) string { return "room:" + id }

// roomMemberSID 是某个 agent 在这个房间里的会话。
// 一律带 @<agent>/ 前缀：默认 agent 也不能和房间的记录会话撞在一起。
func (a *App) roomMemberSID(agentID, roomID string) string {
	return "@" + agentID + "/" + roomLogSID(roomID)
}

// SetRooms 接上房间列表。
func (a *App) SetRooms(store *rooms.Store) {
	a.mu.Lock()
	a.roomStore = store
	a.mu.Unlock()
}

// roomOfSession 从会话 ID 找出房间：room:<id> 或 @<agent>/room:<id>。
func (a *App) roomOfSession(sid string) (rooms.Room, bool) {
	if a.roomStore == nil {
		return rooms.Room{}, false
	}
	i := strings.Index(sid, roomLogSID(""))
	if i < 0 {
		return rooms.Room{}, false
	}
	id := sid[i+len(roomLogSID("")):]
	if id == "" || strings.Contains(id, "/") {
		return rooms.Room{}, false
	}
	return a.roomStore.Get(id)
}

// roomSender 是房间会话属于哪个 agent（房间本身的记录会话返回默认 agent）。
func roomSender(sid string) string { return agents.SessionAgent(sid) }

// roomHumanSay 把人在群里说的话转给所有成员，并按需要叫它们说话。调用方持有锁。
func (a *App) roomHumanSay(room rooms.Room, name, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if name == "" {
		name = "有人"
	}
	line := fmt.Sprintf("[%s] %s", name, text)
	for _, member := range room.Members {
		sid := a.roomMemberSID(member, room.ID)
		st := a.store.Settings(sid)
		if !st.Group {
			st.Group = true
			if err := a.store.SaveSettings(sid, st); err != nil {
				a.log.Warn("保存房间成员会话失败", "sid", sid, "err", err)
			}
		}
		if err := a.store.Append(sid, session.Msg{Role: "user", Content: line}); err != nil {
			a.log.Warn("保存房间消息失败", "sid", sid, "err", err)
			continue
		}
		s := a.sessOf(sid)
		s.groupFresh = true
		s.groupPending++
		s.groupLast = a.now()
		s.groupEp = endpoint{ch: "room", chat: room.ID}
		// 暂停了：消息照样进成员的会话（它们还能听到），只是不叫人回
		if room.Paused || room.MutedAgent(member) {
			continue
		}
		if a.roomShouldSpeak(room, member, text) {
			a.roomTrigger(s, member, room)
		}
	}
	a.publish(events.Event{T: "rooms"})
}

// roomShouldSpeak 是这个人说的这句话，要不要叫某个 agent 说话。
func (a *App) roomShouldSpeak(room rooms.Room, member, text string) bool {
	ag, ok := a.agentByID(member)
	if !ok {
		return false
	}
	// 私聊（两个 agent 的房间）里一定回
	if room.IsDM() {
		return true
	}
	// 被 @、叫到名字
	if strings.Contains(text, "@"+ag.Name) || groupMentionsName(text, ag.Name) {
		return true
	}
	// 自由模式不在这里直接回：交给 groupTick 按安静时长和每小时额度决定
	return false
}

// roomTrigger 让某个成员在房间里说一句（走群聊那一套：先攒 2 秒）。调用方持有锁。
func (a *App) roomTrigger(s *sess, member string, room rooms.Room) {
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
		s.groupProactive = false
		ep := endpoint{ch: "room", chat: room.ID}
		s.queue = append(s.queue, queued{ep: ep, who: "room:" + member, trigger: true,
			m: channel.Message{ChatID: room.ID, Group: true}})
		a.startRun(s)
	})
}

// roomAgentSay 是 agent 在房间里说了一句：记进房间记录、转给其他成员、看看要不要暂停。调用方持有锁。
func (a *App) roomAgentSay(room rooms.Room, member, text string) {
	text = strings.TrimSpace(text)
	if isSilence(text) {
		return
	}
	ag, ok := a.agentByID(member)
	name := member
	if ok {
		name = ag.Name
	}
	line := fmt.Sprintf("[%s] %s", name, text)
	if err := a.store.Append(roomLogSID(room.ID), session.Msg{Role: "assistant", Content: line}); err != nil {
		a.log.Warn("保存房间记录失败", "room", room.ID, "err", err)
	}
	a.publish(events.Event{T: "say", SID: roomLogSID(room.ID), Text: text})
	// 转给其他成员（说话的这个不再收到自己说的话）
	for _, m := range room.Members {
		if m == member {
			continue
		}
		sid := a.roomMemberSID(m, room.ID)
		if err := a.store.Append(sid, session.Msg{Role: "user", Content: line}); err != nil {
			a.log.Warn("保存房间消息失败", "sid", sid, "err", err)
			continue
		}
		s := a.sessOf(sid)
		s.groupFresh = true
		s.groupPending++
		s.groupLast = a.now()
		s.groupEp = endpoint{ch: "room", chat: room.ID}
		// 在群里 @ 了或者叫到别的成员：叫醒它（连续发言、来回轮数的暂停规则兜底）
		if !room.Paused && !room.MutedAgent(m) {
			if other, ok := a.agentByID(m); ok && (strings.Contains(text, "@"+other.Name) || groupMentionsName(text, other.Name)) {
				a.roomTrigger(s, m, room)
			}
		}
	}
	a.signalRoomWaiters(room.ID, member, text)
	a.roomGuard(room, member)
}

// roomUsage 累加一个群今天的 token 用量，超过预算就暂停。调用方持有锁。
func (a *App) roomUsage(room rooms.Room, prompt, completion int) {
	if room.Budget <= 0 {
		return
	}
	st := a.roomState(room.ID)
	day := a.now().In(a.loc).Format("2006-01-02")
	if st.TokenDay != day {
		st.TokenDay, st.Tokens = day, 0
	}
	st.Tokens += prompt + completion
	if st.Tokens > room.Budget {
		a.pauseRoom(room, fmt.Sprintf("今天的 token 用量到了预算（%d），先暂停，明天或者你恢复", room.Budget))
	}
}

// roomGuard 防止几个 agent 在群里聊个不停。调用方持有锁。
func (a *App) roomGuard(room rooms.Room, speaker string) {
	st := a.roomState(room.ID)
	st.Tail = append(st.Tail, speaker)
	if len(st.Tail) > 12 {
		st.Tail = st.Tail[len(st.Tail)-12:]
	}
	// 连续 8 条都是 agent 说的（中间没有人说话）就暂停
	consecutive := 0
	for i := len(st.Tail) - 1; i >= 0; i-- {
		if st.Tail[i] == "human" {
			break
		}
		consecutive++
	}
	if consecutive >= 8 {
		a.pauseRoom(room, "群里连续说了 8 条，先暂停，等你说话")
		return
	}
	// 两个 agent 来回超过 6 轮（没有新的人）也暂停
	if len(st.Tail) >= 6 {
		tail := st.Tail[len(st.Tail)-6:]
		set := map[string]bool{}
		for _, x := range tail {
			set[x] = true
		}
		if len(set) == 2 && !set["human"] {
			a.pauseRoom(room, "两个 agent 来回说了 6 轮，先暂停，等你说话")
			return
		}
	}
}

type roomRun struct {
	Tail     []string // 最近的发言者（"human" 或者 agent ID）
	Pending  map[string][]chan string
	Tokens   int
	TokenDay string
}

// roomState 是房间的运行状态（只在内存里）。调用方持有锁。
func (a *App) roomState(id string) *roomRun {
	if a.roomRuns == nil {
		a.roomRuns = map[string]*roomRun{}
	}
	st := a.roomRuns[id]
	if st == nil {
		st = &roomRun{Pending: map[string][]chan string{}}
		a.roomRuns[id] = st
	}
	return st
}

// pauseRoom 暂停一个群，并在群记录里说一声。调用方持有锁。
func (a *App) pauseRoom(room rooms.Room, why string) {
	if room.Paused {
		return
	}
	room.Paused = true
	if err := a.roomStore.Update(room); err != nil {
		a.log.Warn("暂停房间失败", "room", room.ID, "err", err)
	}
	a.store.Append(roomLogSID(room.ID), session.Msg{Role: "user", Content: "[系统] " + why})
	a.publish(events.Event{T: "say", SID: roomLogSID(room.ID), Text: "[系统] " + why})
	a.publish(events.Event{T: "rooms"})
	a.log.Info("房间已暂停", "room", room.ID, "why", why)
}

// signalRoomWaiters 叫醒在等某个 agent 回话的调用（Message 工具的 wait）。调用方持有锁。
func (a *App) signalRoomWaiters(roomID, member, text string) {
	st := a.roomState(roomID)
	chans := st.Pending[member]
	if len(chans) == 0 {
		return
	}
	delete(st.Pending, member)
	for _, c := range chans {
		select {
		case c <- text:
		default:
		}
	}
}

// roomHumanLine 记一条"人说话了"，用来算连续发言。调用方持有锁。
func (a *App) roomHumanLine(roomID string) {
	st := a.roomState(roomID)
	st.Tail = append(st.Tail, "human")
	if len(st.Tail) > 12 {
		st.Tail = st.Tail[len(st.Tail)-12:]
	}
}

// ---- 网页和聊天里的房间管理 ----

// RoomInfo 是一个房间的概况。
type RoomInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Mode    string   `json:"mode"`
	Paused  bool     `json:"paused"`
	Muted   []string `json:"muted"`
	DM      bool     `json:"dm"`
	Budget  int      `json:"budget"` // 每天最多的 token（0 不限）
	SID     string   `json:"sid"`    // 记录会话（网页里打开它就能看聊天）
	Count   int      `json:"count"`
}

func (a *App) roomInfo(r rooms.Room) RoomInfo {
	out := RoomInfo{ID: r.ID, Name: r.Name, Members: r.Members, Mode: r.Mode, Paused: r.Paused, Muted: r.Muted,
		DM: r.IsDM(), Budget: r.Budget, SID: roomLogSID(r.ID)}
	if out.Members == nil {
		out.Members = []string{}
	}
	if out.Muted == nil {
		out.Muted = []string{}
	}
	out.Count = len(a.store.History(roomLogSID(r.ID)))
	return out
}

// Rooms 是所有房间（网页用）。
func (a *App) Rooms() []RoomInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []RoomInfo{}
	if a.roomStore == nil {
		return out
	}
	for _, r := range a.roomStore.List() {
		out = append(out, a.roomInfo(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RoomPatch 是网页上对一个房间的修改。
type RoomPatch struct {
	Name    *string   `json:"name"`
	Members *[]string `json:"members"`
	Mode    *string   `json:"mode"`
	Paused  *bool     `json:"paused"`
	Muted   *[]string `json:"muted"`
	Budget  *int      `json:"budget"`
	DM      *bool     `json:"dm"`
}

// CreateRoom 新建一个房间。
func (a *App) CreateRoom(p RoomPatch) (RoomInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return RoomInfo{}, errors.New("房间列表还没准备好")
	}
	r := rooms.Room{}
	if p.Name != nil {
		r.Name = *p.Name
	}
	if p.Members != nil {
		r.Members = *p.Members
	}
	if p.Mode != nil {
		r.Mode = *p.Mode
	}
	for _, m := range r.Members {
		if _, ok := a.agentByID(m); !ok {
			return RoomInfo{}, fmt.Errorf("没有这个 agent：%s", m)
		}
	}
	// 私聊：正好两个 agent，名字自动取
	if p.DM != nil && *p.DM {
		if len(r.Members) != 2 {
			return RoomInfo{}, errors.New("私聊正好两个人")
		}
		r.DM = r.Members
		r.Mode = rooms.ModeMention
		if strings.TrimSpace(r.Name) == "" {
			na, nb := r.Members[0], r.Members[1]
			if ag, ok := a.agentByID(na); ok {
				na = ag.Name
			}
			if ag, ok := a.agentByID(nb); ok {
				nb = ag.Name
			}
			r.Name = "@" + na + "+" + nb
		}
	}
	got, err := a.roomStore.Create(r)
	if err != nil {
		return RoomInfo{}, err
	}
	a.setRoomTitle(got.ID, got.Name)
	a.log.Info("新建房间", "id", got.ID, "name", got.Name, "members", len(got.Members))
	a.publish(events.Event{T: "rooms"})
	return a.roomInfo(got), nil
}

// UpdateRoom 改一个房间。
func (a *App) UpdateRoom(id string, p RoomPatch) (RoomInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return RoomInfo{}, errors.New("房间列表还没准备好")
	}
	r, ok := a.roomStore.Get(id)
	if !ok {
		return RoomInfo{}, errors.New("没有这个群")
	}
	if p.Name != nil {
		r.Name = *p.Name
	}
	if p.Members != nil {
		for _, m := range *p.Members {
			if _, ok := a.agentByID(m); !ok {
				return RoomInfo{}, fmt.Errorf("没有这个 agent：%s", m)
			}
		}
		r.Members = *p.Members
	}
	if p.Mode != nil {
		r.Mode = *p.Mode
	}
	if p.Paused != nil {
		r.Paused = *p.Paused
		if !r.Paused {
			// 恢复的时候清掉“进来就暂停”的记录
			st := a.roomState(r.ID)
			st.Tail = nil
		}
	}
	if p.Muted != nil {
		r.Muted = *p.Muted
	}
	if p.Budget != nil {
		if *p.Budget < 0 {
			return RoomInfo{}, errors.New("预算不能是负数")
		}
		r.Budget = *p.Budget
	}
	if err := a.roomStore.Update(r); err != nil {
		return RoomInfo{}, err
	}
	a.setRoomTitle(r.ID, r.Name)
	a.publish(events.Event{T: "rooms"})
	return a.roomInfo(r), nil
}

// setRoomTitle 让群记录那个会话在会话列表里显示群名。调用方持有锁。
func (a *App) setRoomTitle(id, name string) {
	sid := roomLogSID(id)
	st := a.store.Settings(sid)
	if st.Title == name {
		return
	}
	st.Title = name
	if err := a.store.SaveSettings(sid, st); err != nil {
		a.log.Warn("保存群会话标题失败", "sid", sid, "err", err)
	}
}

// DeleteRoom 删掉一个房间（聊天记录留着，可以在会话里找到）。
func (a *App) DeleteRoom(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return errors.New("房间列表还没准备好")
	}
	if err := a.roomStore.Delete(id); err != nil {
		return err
	}
	a.publish(events.Event{T: "rooms"})
	return nil
}

// ---- Message 工具：agent 主动给别的群/agent 发消息 ----

// messageTool 执行 Message：给房间或者某个 agent 发一条消息；wait 时等对方的回复。
func (a *App) messageTool(fromSID string, raw string, reply func(bool, string)) {
	var args struct {
		To   string `json:"to"`
		Text string `json:"text"`
		Wait bool   `json:"wait"`
	}
	_ = json.Unmarshal([]byte(raw), &args)
	to, text := strings.TrimSpace(args.To), strings.TrimSpace(args.Text)
	if to == "" || text == "" {
		reply(false, "to 和 text 都要有")
		return
	}
	a.mu.Lock()
	if target, ok := strings.CutPrefix(to, "chat:"); ok {
		out, err := a.messageChat(fromSID, strings.TrimSpace(target), text)
		a.mu.Unlock()
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if isLifeSID(fromSID) {
		if ag, ok := a.agentByID(agents.SessionAgent(fromSID)); ok && pickString(ag.LifeReach, "owner") == "off" && strings.HasPrefix(to, "agent:") {
			a.mu.Unlock()
			reply(false, "你设成了不主动找人")
			return
		}
	}
	sender := roomSender(fromSID)
	senderName := sender
	if ag, ok := a.agentByID(sender); ok {
		senderName = ag.Name
	}
	var target rooms.Room
	var targetMember string
	switch {
	case strings.HasPrefix(to, "room:") || (!strings.Contains(to, ":") && a.roomByName(strings.TrimPrefix(to, "room:")) != nil):
		name := strings.TrimPrefix(to, "room:")
		r := a.roomByName(strings.TrimPrefix(name, "#"))
		if r == nil {
			a.mu.Unlock()
			reply(false, "没有这个群："+name)
			return
		}
		target = *r
	case strings.HasPrefix(to, "agent:"):
		name := strings.TrimPrefix(to, "agent:")
		ag, ok := a.findAgent(name)
		if !ok {
			a.mu.Unlock()
			reply(false, "没有这个 agent："+name)
			return
		}
		r, err := a.dmRoom(sender, ag.ID)
		if err != nil {
			a.mu.Unlock()
			reply(false, err.Error())
			return
		}
		target, targetMember = r, ag.ID
	default:
		// 只写了名字：先找群，再找 agent
		if r := a.roomByName(to); r != nil {
			target = *r
		} else if ag, ok := a.findAgent(to); ok {
			r, err := a.dmRoom(sender, ag.ID)
			if err != nil {
				a.mu.Unlock()
				reply(false, err.Error())
				return
			}
			target, targetMember = r, ag.ID
		} else {
			a.mu.Unlock()
			reply(false, "找不到要发给谁："+to)
			return
		}
	}
	if target.Paused {
		a.mu.Unlock()
		reply(false, "这个群现在暂停了，等人恢复")
		return
	}
	line := fmt.Sprintf("[%s] %s", senderName, text)
	if err := a.store.Append(roomLogSID(target.ID), session.Msg{Role: "user", Content: line}); err != nil {
		a.mu.Unlock()
		reply(false, "保存消息失败："+err.Error())
		return
	}
	a.publish(events.Event{T: "say", SID: roomLogSID(target.ID), Text: text})
	// 转给成员；要等的那个 agent 记下来
	var wait chan string
	for _, m := range target.Members {
		if m == sender {
			continue
		}
		sid := a.roomMemberSID(m, target.ID)
		st := a.store.Settings(sid)
		if !st.Group {
			st.Group = true
			_ = a.store.SaveSettings(sid, st)
		}
		_ = a.store.Append(sid, session.Msg{Role: "user", Content: line})
		s := a.sessOf(sid)
		s.groupFresh = true
		s.groupLast = a.now()
		s.groupEp = endpoint{ch: "room", chat: target.ID}
		if target.MutedAgent(m) {
			continue
		}
		if targetMember != "" {
			if m == targetMember {
				a.roomTrigger(s, m, target)
			}
		} else if a.roomShouldSpeak(target, m, text) {
			a.roomTrigger(s, m, target)
		}
	}
	if args.Wait {
		if targetMember == "" {
			a.mu.Unlock()
			reply(false, "给整个群发消息时不能等某一个人的回复（wait 只能用在 agent 上）")
			return
		}
		wait = make(chan string, 1)
		st := a.roomState(target.ID)
		st.Pending[targetMember] = append(st.Pending[targetMember], wait)
	}
	a.mu.Unlock()
	if wait == nil {
		reply(true, "消息已经发进「"+target.Name+"」。")
		return
	}
	name := targetMember
	if ag, ok := a.agentByID(targetMember); ok {
		name = ag.Name
	}
	go func() {
		select {
		case got := <-wait:
			reply(true, name+" 回了："+got)
		case <-time.After(5 * time.Minute):
			reply(true, "等 "+name+" 回话超时（5 分钟），消息已经发到了。")
		case <-a.ctx.Done():
		}
	}()
}

// cmdRoom 是 /room：列出网关里的群，暂停 / 恢复某个群。
func (a *App) cmdRoom(c in, arg string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能看和管网关里的群")
		return
	}
	if a.roomStore == nil {
		a.answer(c, "还没有群")
		return
	}
	list := a.roomStore.List()
	if len(list) == 0 {
		a.answer(c, "还没有群。管理员可以在网页拓扑图的 Agent 列点「创建群」。")
		return
	}
	args := strings.Fields(arg)
	if len(args) == 0 {
		var b strings.Builder
		b.WriteString("网关里的群：")
		for _, r := range list {
			state := "在聊"
			if r.Paused {
				state = "暂停"
			}
			fmt.Fprintf(&b, "\n%s（%d 人 · %s）", r.Name, len(r.Members), state)
		}
		b.WriteString("\n/room <群名> pause 暂停、/room <群名> resume 恢复；聊天记录在会话「群名」里。")
		a.answer(c, b.String())
		return
	}
	name := args[0]
	r := a.roomByName(name)
	if r == nil {
		a.answer(c, "没有这个群："+name)
		return
	}
	action := ""
	if len(args) > 1 {
		action = strings.ToLower(args[1])
	}
	switch action {
	case "", "info":
		var names []string
		for _, m := range r.Members {
			n := m
			if ag, ok := a.agentByID(m); ok {
				n = ag.Name
			}
			names = append(names, n)
		}
		state, mode := "在聊", "点名"
		if r.Paused {
			state = "暂停"
		}
		if r.Mode == rooms.ModeFree {
			mode = "自由"
		}
		a.answer(c, fmt.Sprintf("群「%s」：%s · %s · 成员 %s", r.Name, state, mode, strings.Join(names, "、")))
	case "pause":
		room := *r
		room.Paused = true
		if err := a.roomStore.Update(room); err != nil {
			a.answer(c, "没暂停成："+err.Error())
			return
		}
		a.publish(events.Event{T: "rooms"})
		a.answer(c, "群「"+r.Name+"」已暂停，等你恢复。")
	case "resume":
		room := *r
		room.Paused = false
		if err := a.roomStore.Update(room); err != nil {
			a.answer(c, "没恢复成："+err.Error())
			return
		}
		a.roomState(room.ID).Tail = nil
		a.publish(events.Event{T: "rooms"})
		a.answer(c, "群「"+r.Name+"」恢复了。")
	default:
		a.answer(c, "不认识的用法：/room <群名> pause|resume")
	}
}

// roomByName 按名字（或 ID）找群。调用方持有锁。
func (a *App) roomByName(name string) *rooms.Room {
	if a.roomStore == nil {
		return nil
	}
	if r, ok := a.roomStore.Get(name); ok {
		return &r
	}
	if r, ok := a.roomStore.FindByName(name); ok {
		return &r
	}
	return nil
}

// findAgent 按 ID 或名字找 agent。调用方持有锁。
func (a *App) findAgent(name string) (agents.Agent, bool) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "@"))
	if a.agentStore == nil {
		return agents.Agent{}, false
	}
	if ag, ok := a.agentStore.Get(strings.ToLower(name)); ok {
		return ag, true
	}
	for _, ag := range a.agentStore.List() {
		if ag.Name == name {
			return ag, true
		}
	}
	return agents.Agent{}, false
}

// dmRoom 是两个 agent 的私聊房间，没有就建一个。调用方持有锁。
func (a *App) dmRoom(from, to string) (rooms.Room, error) {
	if from == to {
		return rooms.Room{}, errors.New("不能给自己发私聊")
	}
	if a.roomStore == nil {
		return rooms.Room{}, errors.New("房间列表还没准备好")
	}
	for _, r := range a.roomStore.List() {
		if !r.IsDM() || len(r.Members) != 2 {
			continue
		}
		if (r.Members[0] == from && r.Members[1] == to) || (r.Members[0] == to && r.Members[1] == from) {
			return r, nil
		}
	}
	na, nb := from, to
	if ag, ok := a.agentByID(from); ok {
		na = ag.Name
	}
	if ag, ok := a.agentByID(to); ok {
		nb = ag.Name
	}
	return a.roomStore.Create(rooms.Room{Name: "@" + na + "+" + nb, Members: []string{from, to}, Mode: rooms.ModeMention, DM: []string{from, to}})
}

// messageChat 把一条消息发到 agent 自己的某个私聊：记进那个会话的历史，发到对方最近说话的那个窗口。
// 内心活动里发的有规矩：找人范围、对方没回上一条不再找、每天次数、睡觉时间不打扰。调用方持有锁。
func (a *App) messageChat(fromSID, target, text string) (string, error) {
	// 发送者是这一轮的执行 agent（会话可能被切换过）；内部会话仍然按固定前缀
	agentID := a.execAgentID(fromSID)
	ag, ok := a.agentByID(agentID)
	if !ok {
		return "", errors.New("没有这个 agent")
	}
	if a.execAgentID(target) != agentID || !a.hasSession(target) {
		return "", fmt.Errorf("没有这个私聊：%s（只能发给你自己的私聊，内心活动醒来时列出来的那些）", target)
	}
	// 给拟人型自己的内心留一句话：不是对外发消息，放进 life 的历史里；它没在忙就顺手叫醒它。
	if isLifeSID(target) {
		if isLifeSID(fromSID) {
			return "", errors.New("不能给自己的内心留言")
		}
		sender := "主人"
		if !a.isOwnerSID(agentID, fromSID) {
			if title := a.sessionInfo(fromSID).Title; title != "" {
				sender = title
			} else {
				sender = "另一个会话"
			}
		}
		line := fmt.Sprintf("[%s 留了一句] %s", sender, text)
		if err := a.store.Append(target, session.Msg{Role: "user", Content: line}); err != nil {
			return "", fmt.Errorf("保存留言失败：%w", err)
		}
		a.publishSession(target)
		if s := a.sessions[target]; s == nil || (s.run == nil && len(s.queue) == 0 && !s.compacting) {
			if err := a.startLife(agentID, true); err != nil {
				a.log.Info("内心留言已保存，但它现在没法开始", "agent", agentID, "err", err)
			}
		}
		a.log.Info("给内心留言", "agent", agentID, "from", fromSID)
		return "留言放进它的内心了，它会把这句话当成一轮任务；正在想事情的话，下次醒来会看到。", nil
	}
	if !a.isPrivateChat(target) {
		return "", fmt.Errorf("没有这个私聊：%s（只能发给你自己的私聊，内心活动醒来时列出来的那些）", target)
	}
	fromLife := isLifeSID(fromSID)
	var ls *lifeState
	if fromLife {
		now := a.now().In(a.loc)
		ls = a.lifeOf(agentID)
		switch reach := pickString(ag.LifeReach, "owner"); {
		case reach == "off":
			return "", errors.New("你设成了不主动找人")
		case reach == "owner" && !a.isOwnerSID(agentID, target):
			return "", errors.New("你只能主动找主人（找人范围在 agent 设置里改）")
		}
		if at, waiting := ls.Awaiting[target]; waiting {
			return "", fmt.Errorf("你 %s 主动找过他，他还没回；等他回了再说", agoText(now, at))
		}
		if ls.Reaches >= lifeLevelOf(ag).reaches {
			return "", errors.New("今天主动找人的次数用完了")
		}
		if !a.awakeOf(ag).contains(now) {
			return "", errors.New("现在是睡觉时间，别打扰人")
		}
	}
	if s := a.sessions[target]; s != nil && (s.run != nil || len(s.queue) > 0) {
		return "", errors.New("那边正在聊着，等会儿再说")
	}
	if err := a.store.Append(target, session.Msg{Role: "assistant", Content: text, Agent: agentID}); err != nil {
		return "", fmt.Errorf("保存消息失败：%w", err)
	}
	a.publish(events.Event{T: "say", SID: target, Text: text})
	if fromLife {
		a.publish(events.Event{T: "reach", SID: target, Name: ag.Name, Text: cutRunes(text, 120)}) // 网页上弹一条“来找你了”
	}
	where := "网页上"
	if ep, ok := a.lastEndpoint(target); ok {
		a.deliver(ep, text, "")
		a.markSeen(ep, target, a.histEnd(target), true)
		where = chanName(ep.ch) + "里"
	}
	if fromLife {
		ls.Awaiting[target] = a.now().Unix()
		ls.Reaches++
		a.saveLife()
	}
	a.log.Info("主动发了私聊消息", "agent", agentID, "to", target, "from", fromSID)
	return "发出去了（" + where + "）。对方回了会在那个聊天里接着聊。", nil
}

// lastEndpoint 是会话最近有人说过话的那个窗口（不算网页）；都没记录时挑一个聊天软件的窗口。调用方持有锁。
func (a *App) lastEndpoint(sid string) (endpoint, bool) {
	best, bestAt, found := endpoint{}, int64(-1), false
	for key, r := range a.eps {
		if r.Session != sid {
			continue
		}
		ep := parseEndpoint(key)
		if ep.ch == "web" || a.chans[ep.ch] == nil {
			continue
		}
		at := r.Last
		if ep.ch == "cli" {
			at-- // 一样久的时候优先聊天软件
		}
		if !found || at > bestAt || at == bestAt && key < best.key() {
			best, bestAt, found = ep, at, true
		}
	}
	return best, found
}
