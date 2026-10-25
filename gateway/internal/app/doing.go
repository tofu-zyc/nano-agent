package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
)

// agent 现在在干嘛（/state、网页的 agent 节点）：睡着了、在做梦、在跟谁聊天、在冲浪、在摸鱼、在发呆……
// 全是从已有的状态推出来的：作息、哪个会话在跑、这一轮用了什么工具、多久没人找它。

// Doing 是 agent 现在的状态。
type Doing struct {
	State string `json:"state"` // dreaming 整理记忆 | chatting 聊天 | working 干活 | thinking 内心活动 | sleeping 睡着 | slacking 摸鱼 | idle 发呆 / 闲着
	Icon  string `json:"icon"`
	Text  string `json:"text"`  // 给人看的一句话（带着在跟谁聊）
	Brief string `json:"brief"` // 不带人名的说法（给访客看）
}

const (
	slackFor = 30 * time.Minute // 忙完这么久以内算“摸鱼”，再久算“发呆”
	boredFor = 3 * time.Hour    // 这么久没人找它：发呆发得久了（也更容易自己醒）
)

// agentDoing 推出 agent 现在在干嘛。viewer 是问的那个会话（在跟它聊时说“在跟你聊”），可以为空。调用方持有锁。
func (a *App) agentDoing(ag agents.Agent, viewer string) Doing {
	now := a.now().In(a.loc)
	asleep := ag.Life() && !a.awakeOf(ag).contains(now)
	if s := a.sessions[dreamSID(ag.ID)]; s != nil && s.run != nil {
		if asleep {
			return Doing{State: "dreaming", Icon: "🌙", Text: "在做梦（整理记忆）", Brief: "在做梦"}
		}
		return Doing{State: "dreaming", Icon: "🗂️", Text: "在整理记忆", Brief: "在整理记忆"}
	}

	// 在跟人聊、在干活：看它的哪些会话正在跑
	var chats, groups, jobs []string
	for sid, s := range a.sessions {
		if s == nil || s.run == nil || agents.SessionAgent(sid) != ag.ID || isInnerSID(sid) {
			continue
		}
		name := a.chatName(sid)
		switch {
		case sid == viewer:
			name = "你"
		case name == "":
			name = "别人"
		}
		switch {
		case isCronSID(sid):
			jobs = append(jobs, "「"+name+"」")
		case a.isGroupSession(sid):
			groups = append(groups, name)
		default:
			chats = append(chats, name)
		}
	}
	sort.Strings(chats)
	sort.Strings(groups)
	sort.Strings(jobs)
	if !ag.Life() && len(chats)+len(groups)+len(jobs) > 0 {
		return Doing{State: "working", Icon: "🛠️", Text: "在干活（" + strings.Join(append(append(chats, groups...), jobs...), "、") + "）", Brief: "在干活"}
	}
	if len(chats) > 0 {
		return Doing{State: "chatting", Icon: "💬", Text: "在跟" + strings.Join(chats, "、") + "聊天", Brief: "在跟人聊天"}
	}
	if len(groups) > 0 {
		return Doing{State: "chatting", Icon: "💬", Text: "在群「" + strings.Join(groups, "」「") + "」里说话", Brief: "在群里说话"}
	}
	if len(jobs) > 0 {
		return Doing{State: "working", Icon: "🛠️", Text: "在做定时任务" + strings.Join(jobs, ""), Brief: "在做定时任务"}
	}

	if !ag.Life() {
		return Doing{State: "idle", Icon: "☕", Text: "闲着，等人找它", Brief: "闲着"}
	}
	// 内心活动：按这一轮最近用的工具说它在干嘛
	if s := a.sessions[lifeSID(ag.ID)]; s != nil && s.run != nil {
		d := Doing{State: "thinking", Icon: "💭", Text: "在想事情"}
		switch s.run.tool {
		case "WebSearch", "WebFetch":
			d.Icon, d.Text = "🏄", "在上网冲浪"
		case "Message":
			d.Icon, d.Text = "📨", "在找人聊天"
		case "Knowledge":
			d.Icon, d.Text = "📝", "在翻记忆、记东西"
		}
		d.Brief = d.Text
		return d
	}
	w := a.awakeOf(ag)
	if asleep {
		d := Doing{State: "sleeping", Icon: "💤", Brief: "睡着了"}
		d.Text = "睡着了，" + clockDay(now, w.nextStart(now)) + "醒"
		return d
	}
	// 醒着、没事：刚忙完算摸鱼，久了算发呆
	last := a.agentLastActivity(ag.ID)
	if s := a.sessions[lifeSID(ag.ID)]; s != nil && s.lastActivity.After(last) {
		last = s.lastActivity
	}
	if last.IsZero() {
		last = now // 网关刚启动，还不知道它闲了多久
	}
	idle := now.Sub(last)
	d := Doing{State: "slacking", Icon: "🐟", Text: "在摸鱼（刚忙完）", Brief: "在摸鱼"}
	switch {
	case idle >= boredFor:
		d = Doing{State: "idle", Icon: "😶", Text: fmt.Sprintf("在发呆，%s没人找它了", spanText(idle)), Brief: "在发呆"}
	case idle >= slackFor:
		d = Doing{State: "idle", Icon: "😶", Text: "在发呆", Brief: "在发呆"}
	}
	if ls := a.lifeOf(ag.ID); ls.Next > 0 {
		if next := time.Unix(ls.Next, 0).In(a.loc); next.After(now) && w.contains(next) {
			d.Text += "，大约 " + clockDay(now, next) + "起来转转"
		}
	}
	return d
}

// clockDay 是几点：今天的只写时刻，别的日子带上“明天”或日期。
func clockDay(now, t time.Time) string {
	y1, m1, d1 := now.Date()
	y2, m2, d2 := t.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04") + " "
	case now.AddDate(0, 0, 1).Format("2006-01-02") == t.Format("2006-01-02"):
		return "明天 " + t.Format("15:04") + " "
	}
	return t.Format("1月2日 15:04") + " "
}

// spanText 是一段时间的说法：3 小时、2 天。
func spanText(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}

// cmdState 是 /state：所有 agent 现在在干嘛（当前这个排第一）。拟人型还说近况、今天醒了几次、在等谁回。
// 访客只看到当前 agent，也看不到它在跟谁聊。
func (a *App) cmdState(c in) {
	sid := ""
	if c.s != nil {
		sid = c.s.sid
	}
	cur := a.agentOf(sid)
	list := []agents.Agent{cur}
	admin := a.isAdmin(c.who)
	if admin && a.agentStore != nil {
		for _, ag := range a.agentStore.List() {
			if ag.ID != cur.ID {
				list = append(list, ag)
			}
		}
	}
	var b strings.Builder
	for i, ag := range list {
		if i > 0 {
			b.WriteString("\n\n")
		}
		d := a.agentDoing(ag, sid)
		text := d.Text
		if !admin {
			text = d.Brief
		}
		name := ag.Name
		if ag.Emoji != "" {
			name = ag.Emoji + " " + name
		}
		fmt.Fprintf(&b, "%s：%s %s", name, d.Icon, text)
		if !ag.Life() || !admin {
			continue
		}
		ls := a.lifeOf(ag.ID)
		if n := len(ls.Recent); n > 0 {
			b.WriteString("\n近况：" + ls.Recent[n-1].Text)
		}
		if ls.Day == a.now().In(a.loc).Format("2006-01-02") {
			fmt.Fprintf(&b, "\n今天自己醒了 %d 次，主动找了 %d 次人", ls.Wakes, ls.Reaches)
		}
		var waiting []string
		for s := range ls.Awaiting {
			waiting = append(waiting, a.chatName(s))
		}
		if len(waiting) > 0 {
			sort.Strings(waiting)
			b.WriteString("，在等" + strings.Join(waiting, "、") + "回它")
		}
	}
	if len(list) == 1 && cur.Life() && admin {
		b.WriteString("\n\n（/wake 叫醒它，网页的 agent 节点里能看它的内心）")
	}
	a.answer(c, b.String())
}
