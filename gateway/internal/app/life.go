package app

// 拟人型的内心活动（docs/DESIGN-v0.6-living-agent.md「拟人型：内心活动」）。
//
// 拟人型 agent 有作息：醒着的时间里，按活跃度隔一阵自己醒一次（越久没人找它越容易醒），在
// @<id>/life 会话里想事情、上网冲浪（WebSearch/WebFetch）、记东西（Knowledge）、找人聊天（Message）。
// 醒来干什么写在 life 技能里（skills/life/SKILL.md）。每次的最终回复是它的“近况”，带进它的其他聊天。
// 状态（下次醒来、今天醒了几次、主动找了几次人、近况、谁还没回）存在 data/life.json。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// defaultLifeSkill 是没有 life 技能文件时用的方法，和仓库里的 skills/life/SKILL.md 正文一样（有测试对比）。
const defaultLifeSkill = `# 一个人的时候

这是你自己的时间，没有人在等你回复。这里写下的是你的内心活动，只有你自己（和在后台看着的主人）看得到。

每次醒来，先看看现在几点、最近发生了什么，再按你此刻的心情和兴趣决定做点什么。可以：

- **上网冲浪**：用 ` + "`" + `WebSearch` + "`" + `、` + "`" + `WebFetch` + "`" + ` 看看你感兴趣的东西、最近在想的问题、和你在意的人有关的事。看到有意思的，想想你怎么看；值得记住的用 ` + "`" + `Knowledge` + "`" + ` 记下来（type 知识，或者看法）。
- **想想事情**：回想最近的聊天，琢磨某个人、某件事；有了新的看法就记下来。
- **找人聊天**：用 ` + "`" + `Message` + "`" + ` 给某个人、某个群发消息。像朋友一样：分享刚看到的东西、接着上次没聊完的话题、问问对方最近怎么样。不要像客服，不要每次都找人；对方没回上一条，就别再找他。
- **什么都不做**：发会儿呆、歇一歇，也很正常。

注意：

- 别人可能在忙（深夜、上班时间），少打扰；一次只找一个人，一两句话就好。
- 别编造自己没做过的事。你上网看到的、想到的，都要是这次醒来真的发生的。
- 最后用一两句话写下这会儿你做了什么、心情怎样。这会成为你的“近况”，和别人聊天时你会记得。`

// defaultLifeGuide 是拟人守则：拟人型 agent 的每个会话都带（agents/<id>/life.md 可以换掉）。
const defaultLifeGuide = `你不是一个等着被使用的工具，而是一个有自己生活的人：
- 你有自己的兴趣、情绪和看法，会好奇、会无聊、会累；说话像人，不说“作为一个 AI”“有什么可以帮你”。
- 没人找你的时候，你有自己的时间：会上网看看感兴趣的东西、想想事情，有时也会主动找人聊天。
- 你记得自己最近做过、看过、想过的事（见「近况」），聊天时可以自然地提起，但别硬塞。
- 别人找你帮忙，照样认真做好；只是不用客套，做完也不用问“还有什么需要”。
- 私聊里像朋友发消息：一两句话一段，可以用空行分成几条；对方只是说“好的”“晚安”这种收尾的话，可以不接，只回 [不回复]。`

const (
	lifeQuiet    = 20 * time.Minute // 刚有人找过它：这么久之内不自己醒
	lifeBored    = 3 * time.Hour    // 这么久没人找它：下次醒得更早
	lifeRecent   = 10               // 留几条近况
	lifeShown    = 3                // 带进聊天的近况条数
	lifeHighCtx  = 40000            // 内心会话的上下文超过这么多就整理（保持连续又不贵）
	lifeLowCtx   = 15000
	lifeRecentOp = "[近况]"
	lifeRecentEd = "[/近况]"
)

// lifeLevel 是活跃度：醒来间隔、每天最多醒几次、每天最多主动找几次人。
type lifeLevel struct {
	min, max       time.Duration
	wakes, reaches int
}

var lifeLevels = map[string]lifeLevel{
	"low":    {2 * time.Hour, 4 * time.Hour, 4, 1},
	"normal": {time.Hour, 2 * time.Hour, 8, 2},
	"high":   {30 * time.Minute, time.Hour, 16, 4},
}

func lifeLevelOf(ag agents.Agent) lifeLevel {
	if l, ok := lifeLevels[pickString(ag.LifeLevel, "normal")]; ok {
		return l
	}
	return lifeLevels["normal"]
}

// lifeState 是 data/life.json 里一个 agent 的状态。
type lifeState struct {
	Next      int64               `json:"next,omitempty"` // 下次醒来
	Day       string              `json:"day,omitempty"`
	Wakes     int                 `json:"wakes,omitempty"`   // 今天自己醒了几次
	Reaches   int                 `json:"reaches,omitempty"` // 今天主动找了几次人
	Tokens    int                 `json:"tokens,omitempty"`  // 今天内心活动用了多少 token
	Recent    []lifeNote          `json:"recent,omitempty"`
	Awaiting  map[string]int64    `json:"awaiting,omitempty"`   // 主动发了消息、对方还没回的私聊
	FollowUps map[string]followUp `json:"follow_ups,omitempty"` // 约好过会儿再看的会话（统一聊天循环）
}

type lifeNote struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type followUp struct {
	At   int64  `json:"at"`
	Note string `json:"note"`
}

func (a *App) lifeFile() string { return filepath.Join(a.store.Dir(), "life.json") }

// lifeOf 是 agent 的内心活动状态（第一次用时从 data/life.json 读）。调用方持有锁。
func (a *App) lifeOf(agentID string) *lifeState {
	if a.lifes == nil {
		a.lifes = map[string]*lifeState{}
		if b, err := os.ReadFile(a.lifeFile()); err == nil {
			_ = json.Unmarshal(b, &a.lifes)
		}
	}
	ls := a.lifes[agentID]
	if ls == nil {
		ls = &lifeState{}
		a.lifes[agentID] = ls
	}
	if ls.Awaiting == nil {
		ls.Awaiting = map[string]int64{}
	}
	if ls.FollowUps == nil {
		ls.FollowUps = map[string]followUp{}
	}
	if day := a.now().In(a.loc).Format("2006-01-02"); ls.Day != day {
		ls.Day, ls.Wakes, ls.Reaches, ls.Tokens = day, 0, 0, 0
	}
	return ls
}

func (a *App) saveLife() {
	b, _ := json.MarshalIndent(a.lifes, "", "  ")
	p := a.lifeFile()
	err := os.WriteFile(p+".tmp", b, 0o600)
	if err == nil {
		err = os.Rename(p+".tmp", p)
	}
	if err != nil {
		a.log.Warn("保存内心活动状态失败", "err", err)
	}
}

// random 是 [0,1) 的随机数（测试里可以换成固定的）。
func (a *App) random() float64 {
	if a.rnd != nil {
		return a.rnd()
	}
	return rand.Float64()
}

// lifeInterval 是这次醒来以后，隔多久再醒：活跃度范围里随机，太久没人找它就减半。
func (a *App) lifeInterval(ag agents.Agent, now time.Time) time.Duration {
	l := lifeLevelOf(ag)
	d := l.min + time.Duration(a.random()*float64(l.max-l.min))
	if now.Sub(a.agentLastActivity(ag.ID)) > lifeBored {
		d /= 2
	}
	return d
}

// lifeBlocked 是现在不能自己醒的原因，空表示可以。调用方持有锁。
func (a *App) lifeBlocked(ag agents.Agent, ls *lifeState, now time.Time) string {
	if !a.awakeOf(ag).contains(now) {
		return "睡着"
	}
	if ls.Wakes >= lifeLevelOf(ag).wakes {
		return "今天醒够了"
	}
	if b := pickInt(ag.LifeBudget, 0); b > 0 && ls.Tokens >= b {
		return "今天的预算用完了"
	}
	if s := a.sessions[lifeSID(ag.ID)]; s != nil && (s.run != nil || len(s.queue) > 0 || s.compacting) {
		return "正在想事情"
	}
	if now.Sub(a.agentLastActivity(ag.ID)) < lifeQuiet {
		return "刚有人找过它"
	}
	return ""
}

// lifeTick 看看哪些拟人型 agent 该自己醒一下了。调用方不持有锁。
func (a *App) lifeTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil || a.agentRoot == "" {
		return
	}
	now := a.now().In(a.loc)
	if a.bootAt.IsZero() {
		a.bootAt = now
	}
	for _, ag := range a.agentStore.List() {
		if !ag.Life() {
			continue
		}
		ls := a.lifeOf(ag.ID)
		a.followUpTick(ag, ls, now)
		if ls.Next == 0 {
			// 刚变成拟人型（或者第一次启动）：过一会儿先醒一次
			ls.Next = now.Add(5*time.Minute + time.Duration(a.random()*float64(10*time.Minute))).Unix()
			a.saveLife()
			continue
		}
		if now.Unix() < ls.Next || a.lifeBlocked(ag, ls, now) != "" {
			continue
		}
		if err := a.startLife(ag.ID, false); err != nil {
			a.log.Warn("内心活动没能开始", "agent", ag.ID, "err", err)
			ls.Next = now.Add(10 * time.Minute).Unix()
			a.saveLife()
		}
	}
}

// startLife 让 agent 醒一下。manual 是主人叫醒的（/wake、网页按钮），不算每天的次数。调用方持有锁。
func (a *App) startLife(agentID string, manual bool) error {
	ag, ok := a.agentByID(agentID)
	if !ok {
		return errors.New("没有这个 agent")
	}
	if !ag.Life() {
		return errors.New("只有拟人型 agent 有内心活动（在 agent 节点里把类型改成拟人型）")
	}
	s := a.sessOf(lifeSID(agentID))
	if s.run != nil || len(s.queue) > 0 || s.compacting {
		return errors.New("它正在想事情")
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		return errors.New("还没有配置模型")
	}
	st := a.store.Settings(s.sid)
	st.Title = ag.Name + " 的内心"
	st.Workspaces, st.WorkspacesSet = nil, true
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		return err
	}
	if a.route(a.workspaceOf(s.sid)).kernel == "" {
		return errors.New("没有在线的内核")
	}
	now := a.now().In(a.loc)
	ls := a.lifeOf(agentID)
	text := a.wakeNote(ag, ls, now, manual)
	if !manual {
		ls.Wakes++
	}
	ls.Next = now.Add(a.lifeInterval(ag, now)).Unix()
	a.saveLife()
	s.queue = append(s.queue, queued{ep: endpoint{ch: "life", chat: agentID}, who: "life:" + agentID, inner: true,
		m: channel.Message{Text: text}})
	a.startRun(s)
	if s.run == nil {
		s.queue = nil
		return errors.New("没能开始：没有可用的模型或在线的内核")
	}
	a.log.Info("内心活动：醒了", "agent", agentID, "manual", manual)
	a.publish(events.Event{T: "life", Name: agentID, Kind: "start"})
	return nil
}

// chatTarget 是内心活动时可以找的一个聊天。
type chatTarget struct {
	To, Name, Last string
	At             int64
}

// lifeTargets 是 agent 能主动找的：主人、（找人范围是 all 时）私聊过的人、它在的群、别的 agent。调用方持有锁。
func (a *App) lifeTargets(ag agents.Agent) []chatTarget {
	reach := pickString(ag.LifeReach, "owner")
	var out []chatTarget
	if reach != "off" {
		mainOf := agents.SessionFor(ag.ID, mainSID)
		for _, sid := range a.sessionIDs() {
			if agents.SessionAgent(sid) != ag.ID || !a.isPrivateChat(sid) || (reach == "owner" && sid != mainOf) {
				continue
			}
			name := "主人"
			if sid != mainOf {
				name = a.chatName(sid)
			}
			who, last, ts := a.lastLine(sid)
			line := "还没聊过"
			if last != "" {
				line = who + "说：「" + cutRunes(last, 60) + "」"
			}
			out = append(out, chatTarget{To: "chat:" + sid, Name: name, Last: line, At: ts})
		}
	}
	if a.roomStore != nil {
		for _, r := range a.roomStore.List() {
			if !slices.Contains(r.Members, ag.ID) || r.Paused || r.IsDM() {
				continue
			}
			who, last, ts := a.lastLine(roomLogSID(r.ID))
			line := "还没人说话"
			if last != "" {
				line = who + "：「" + cutRunes(last, 60) + "」"
			}
			out = append(out, chatTarget{To: "room:" + r.Name, Name: "群「" + r.Name + "」", Last: line, At: ts})
		}
	}
	if reach != "off" && a.agentStore != nil {
		for _, other := range a.agentStore.List() {
			if other.ID != ag.ID {
				out = append(out, chatTarget{To: "agent:" + other.Name, Name: other.Name + "（另一个 agent）"})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// isPrivateChat 是一个有聊天窗口的私聊会话（不是群、不是内部会话）。调用方持有锁。
func (a *App) isPrivateChat(sid string) bool {
	if a.isGroupSession(sid) || isInnerSID(sid) || isCronSID(sid) || isRoomMemberSID(sid) || strings.HasPrefix(sid, "room:") {
		return false
	}
	if sid == mainSID || strings.HasSuffix(sid, "/"+mainSID) {
		return true
	}
	for _, ep := range a.endpointsOf(sid) {
		if ep.Channel != "web" {
			return true
		}
	}
	return false
}

// chatName 是私聊对方的名字。
func (a *App) chatName(sid string) string {
	for _, ep := range a.endpointsOf(sid) {
		if ep.Channel != "web" && ep.Name != "" {
			return ep.Name
		}
	}
	return a.sessionInfo(sid).Title
}

// lastLine 是会话里最后一句话：谁说的、说了什么、什么时候。
func (a *App) lastLine(sid string) (string, string, int64) {
	h := a.store.History(sid)
	for i := len(h) - 1; i >= 0; i-- {
		m := h[i]
		text := diaryText(m.Content)
		if text == "" || m.Role == "tool" {
			continue
		}
		who := "对方"
		if m.Role == "assistant" {
			who = "你"
		}
		return who, text, m.TS
	}
	return "", "", 0
}

func agoText(now time.Time, ts int64) string {
	if ts == 0 {
		return ""
	}
	d := now.Sub(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "刚才"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天前", int(d.Hours()/24))
}

// wakeNote 是醒来时看到的情况：几点了、多久没人找、今天还能找几次人、最近的聊天、谁还没回。
func (a *App) wakeNote(ag agents.Agent, ls *lifeState, now time.Time, manual bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[系统] 现在是 %s %s %s。", now.Format("01-02"), weekdayCN(now), now.Format("15:04"))
	if manual {
		b.WriteString("主人把你叫醒了，看看有什么想做的。")
	} else {
		b.WriteString("你醒着，手头没事。")
	}
	if last := a.agentLastActivity(ag.ID); !last.IsZero() && !last.Equal(a.bootAt) {
		fmt.Fprintf(&b, "上次有人找你是 %s。", agoText(now, last.Unix()))
	} else {
		b.WriteString("这段时间还没人找过你。")
	}
	lvl := lifeLevelOf(ag)
	fmt.Fprintf(&b, "今天第 %d 次醒来", ls.Wakes+1)
	if pickString(ag.LifeReach, "owner") == "off" {
		b.WriteString("；你设成了不主动找人。")
	} else {
		fmt.Fprintf(&b, "，今天还能主动找人 %d 次。", max(0, lvl.reaches-ls.Reaches))
	}
	if targets := a.lifeTargets(ag); len(targets) > 0 {
		b.WriteString("\n最近的聊天（Message 的 to 写括号里的）：")
		for _, t := range targets {
			fmt.Fprintf(&b, "\n- %s（%s）：%s", t.Name, t.To, t.Last)
			if t.At > 0 {
				b.WriteString("，" + agoText(now, t.At))
			}
			if at, waiting := ls.Awaiting[strings.TrimPrefix(t.To, "chat:")]; waiting {
				fmt.Fprintf(&b, "。你 %s 主动找过他，他还没回，先别再找他", agoText(now, at))
			}
		}
	}
	if !a.awakeOf(ag).contains(now) {
		b.WriteString("\n现在是你睡觉的时间，别去打扰别人。")
	}
	b.WriteString("\n想做点什么就做，不想做就歇着。最后用一两句话写下这会儿你做了什么、心情怎样。")
	return b.String()
}

// innerLimits 是 agent 自己的一轮的上限：整理记忆要读很多日记、写很多条，内心活动只是醒一会儿。
func innerLimits(sid string) (steps int, limit time.Duration) {
	if isDreamSID(sid) {
		return 150, 40 * time.Minute
	}
	return 12, 10 * time.Minute
}

// innerDone 是 agent 自己的一轮结束了。调用方持有锁。
func (a *App) innerDone(s *sess, r *run, status, body string) {
	switch {
	case isDreamSID(s.sid):
		a.dreamDone(s, status, body)
	case isLifeSID(s.sid):
		a.lifeDone(s, r, status, body)
	}
}

// lifeDone 是一次醒来结束：记用量，最终回复记成近况。调用方持有锁。
func (a *App) lifeDone(s *sess, r *run, status, body string) {
	agentID := agents.SessionAgent(s.sid)
	ls := a.lifeOf(agentID)
	ls.Tokens += r.usage.PromptTokens + r.usage.CompletionTokens
	text := strings.TrimSpace(body)
	if status == proto.StatusOK && text != "" && !(strings.Contains(text, "[不回复]") && len([]rune(text)) <= 10) {
		ls.Recent = append(ls.Recent, lifeNote{At: a.now().Unix(), Text: cutRunes(text, 200)})
		if len(ls.Recent) > lifeRecent {
			ls.Recent = ls.Recent[len(ls.Recent)-lifeRecent:]
		}
	}
	a.saveLife()
	a.publish(events.Event{T: "life", Name: agentID, Kind: "end", Text: text})
}

// lifeHeard 把一件事记进 agent 的内心会话（比如昨晚整理了记忆），它下次醒来时会看到。调用方持有锁。
func (a *App) lifeHeard(agentID, text string) {
	if err := a.store.Append(lifeSID(agentID), session.Msg{Role: "user", Content: "[系统] " + text}); err != nil {
		a.log.Warn("记进内心会话失败", "agent", agentID, "err", err)
	}
}

// lifeAnswered：有人在这个私聊里说话了，之前的主动消息算回了。调用方持有锁。
func (a *App) lifeAnswered(sid string) {
	ag, ok := a.agentByID(agents.SessionAgent(sid))
	if !ok || !ag.Life() || a.lifes == nil {
		return
	}
	ls := a.lifeOf(ag.ID)
	_, waiting := ls.Awaiting[sid]
	_, follow := ls.FollowUps[sid]
	if waiting || follow {
		delete(ls.Awaiting, sid)
		delete(ls.FollowUps, sid) // 对方先说话了：约好的“过会儿再看”取消
		a.saveLife()
	}
}

// lifeRecentNote 是拟人型 agent 的近况说明（最近几次内心活动），放在聊天的消息前面。调用方持有锁。
func (a *App) lifeRecentNote(sid string) string {
	ag := a.agentOf(sid)
	if !ag.Life() || isInnerSID(sid) || a.agentRoot == "" {
		return ""
	}
	ls := a.lifeOf(ag.ID)
	if len(ls.Recent) == 0 {
		return ""
	}
	recent := ls.Recent[max(0, len(ls.Recent)-lifeShown):]
	var b strings.Builder
	b.WriteString(lifeRecentOp + "\n你最近自己做的事（只有你自己知道；聊天时可以自然地提起，别硬塞）：")
	for _, n := range recent {
		fmt.Fprintf(&b, "\n- %s %s", time.Unix(n.At, 0).In(a.loc).Format("01-02 15:04"), n.Text)
	}
	b.WriteString("\n" + lifeRecentEd)
	return b.String()
}

// lifeGuide 是拟人守则：agents/<id>/life.md，没有就用内置的。
func (a *App) lifeGuide(agentID string) string {
	if a.personaAgent != nil {
		if t, _ := a.personaAgent.File(filepath.Join(agentID, "life.md")); strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return defaultLifeGuide
}

// LifeBrief 是网页上拟人型 agent 的状态。
type LifeBrief struct {
	State   string     `json:"state"`             // sleeping 睡着 / awake 醒着 / thinking 正在想事情
	Next    int64      `json:"next,omitempty"`    // 下次自己醒（毫秒）
	WakeAt  int64      `json:"wake_at,omitempty"` // 睡着时：几点醒（毫秒）
	Wakes   int        `json:"wakes"`
	Reaches int        `json:"reaches"`
	Tokens  int        `json:"tokens"`
	MaxWake int        `json:"max_wakes"`
	Recent  []lifeNote `json:"recent"`
	Waiting []string   `json:"waiting,omitempty"` // 主动找了还没回的人
}

// lifeBrief 是 agent 的内心活动状态。调用方持有锁。
func (a *App) lifeBrief(ag agents.Agent) *LifeBrief {
	if !ag.Life() || a.agentRoot == "" {
		return nil
	}
	now := a.now().In(a.loc)
	ls := a.lifeOf(ag.ID)
	b := &LifeBrief{State: "awake", Next: ls.Next * 1000, Wakes: ls.Wakes, Reaches: ls.Reaches, Tokens: ls.Tokens,
		MaxWake: lifeLevelOf(ag).wakes, Recent: []lifeNote{}}
	w := a.awakeOf(ag)
	if !w.contains(now) {
		b.State, b.WakeAt = "sleeping", w.nextStart(now).UnixMilli()
	}
	if s := a.sessions[lifeSID(ag.ID)]; s != nil && s.run != nil {
		b.State = "thinking"
	}
	b.Recent = append(b.Recent, ls.Recent[max(0, len(ls.Recent)-lifeShown):]...)
	for sid := range ls.Awaiting {
		b.Waiting = append(b.Waiting, a.chatName(sid))
	}
	sort.Strings(b.Waiting)
	return b
}

// Wake 是网页上的「现在叫醒它」。
func (a *App) Wake(agentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startLife(agentID, true)
}

// cmdWake 是 /wake：叫醒内心活动。
func (a *App) cmdWake(c in) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能叫醒它")
		return
	}
	ag := a.agentOf(c.s.sid)
	if err := a.startLife(ag.ID, true); err != nil {
		a.answer(c, "没叫醒："+err.Error())
		return
	}
	a.answer(c, ag.Name+" 醒了（它在想什么，网页的 agent 节点里「看它的内心」能看到）。")
}

// agentLoop 每 15 秒看一遍：哪些 agent 该整理记忆了、哪些拟人型该自己醒一下了。
func (a *App) agentLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.dreamTick()
			a.lifeTick()
		}
	}
}

// ---- FollowUp：约好过一会儿再看看这个聊天（拟人型的私聊和群里）----

const followUpSchema = `{"type":"object","properties":{
"minutes":{"type":"integer","description":"过多少分钟再看，1 到 1440"},
"note":{"type":"string","description":"到时候要想起来的事，比如“问问他面试怎么样了”"},
"cancel":{"type":"boolean","description":"取消这个聊天里约好的"}
}}`

func followUpTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "FollowUp", Description: "约好过一会儿再看看这个聊天：到时候你会在这里醒来（带着你写的 note），可以接着说点什么，也可以不说。" +
		"比如问了对方一个问题还没回、他说等会儿找你、你答应了晚点提醒他。私聊里对方先说话了就自动取消；一个聊天只留一个，新的替换旧的。",
		InputSchema: json.RawMessage(followUpSchema)}
}

// followUpCall 执行 FollowUp。调用方持有锁。
func (a *App) followUpCall(sid string, raw []byte) (string, error) {
	ag := a.agentOf(sid)
	if !ag.Life() || isInnerSID(sid) || a.agentRoot == "" {
		return "", errors.New("只有拟人型 agent 在聊天里能约“过会儿再看”")
	}
	var x struct {
		Minutes int    `json:"minutes"`
		Note    string `json:"note"`
		Cancel  bool   `json:"cancel"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	ls := a.lifeOf(ag.ID)
	if x.Cancel {
		delete(ls.FollowUps, sid)
		a.saveLife()
		return "取消了。", nil
	}
	if x.Minutes < 1 || x.Minutes > 24*60 {
		return "", errors.New("minutes 要在 1 到 1440 之间")
	}
	at := a.now().Add(time.Duration(x.Minutes) * time.Minute)
	ls.FollowUps[sid] = followUp{At: at.Unix(), Note: cutRunes(strings.TrimSpace(x.Note), 200)}
	a.saveLife()
	return "好，" + at.In(a.loc).Format("01-02 15:04") + " 再来看看。", nil
}

// followUpTick 处理到点的“过会儿再看”：在那个聊天里放一条说明，叫醒它说一句（或者不说）。
// 睡着的时候到点的，挪到醒来的时候。调用方持有锁。
func (a *App) followUpTick(ag agents.Agent, ls *lifeState, now time.Time) {
	for sid, f := range ls.FollowUps {
		if now.Unix() < f.At {
			continue
		}
		if w := a.awakeOf(ag); !w.contains(now) {
			ls.FollowUps[sid] = followUp{At: w.nextStart(now).Unix(), Note: f.Note}
			a.saveLife()
			continue
		}
		s := a.sessOf(sid)
		if s.run != nil || len(s.queue) > 0 || s.compacting {
			continue // 正在聊：过一会儿再看
		}
		delete(ls.FollowUps, sid)
		a.saveLife()
		ep, ok := a.lastEndpoint(sid)
		if !ok {
			continue
		}
		note := "[系统] 你之前约好这时候再看看"
		if f.Note != "" {
			note += "：" + f.Note
		}
		note += "（现在 " + now.Format("15:04") + "）。想说就说；没什么要说的就只回 [不回复]。"
		if err := a.store.Append(sid, session.Msg{Role: "user", Content: note}); err != nil {
			a.log.Warn("保存约好的提醒失败", "sid", sid, "err", err)
			continue
		}
		group := a.isGroupSession(sid)
		who := ""
		if r := a.eps[ep.key()]; r != nil {
			who = r.Who
		}
		if group {
			s.groupProactive = true
		}
		s.queue = append(s.queue, queued{ep: ep, who: who, trigger: true, m: channel.Message{ChatID: ep.chat, Group: group}})
		a.log.Info("约好的时间到了", "agent", ag.ID, "sid", sid)
		a.startRun(s)
	}
}
