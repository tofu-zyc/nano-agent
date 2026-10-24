package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
)

// 会话和聊天窗口
//
// 会话是一段对话：上下文、工作目录、权限模式、思考强度、人格。聊天窗口（代码里叫端点）是
// 终端、和 Telegram 机器人的某个私聊、和飞书机器人的某个私聊。每个窗口接在一个会话上，一个会话
// 可以同时接好几个窗口：从哪个窗口说话都是同一段上下文，终端、飞书和 Telegram 自动同步
// 用户消息与回复。网页能看到所有会话，也能往任何一个会话里发消息。
//
// 窗口第一次发消息时决定接到哪里：管理员接到主会话，其他人各自一个会话（会话 ID 就是窗口 ID），
// 互相看不到。管理员可以用 /session 切换，或者在网页里调整。接线存在 data/endpoints.json。

const mainSID = "main"

// endpoint 是一个聊天窗口：渠道名（Channel.Name()，比如 tg、feishu、cli）加平台的聊天 ID。
type endpoint struct{ ch, chat string }

func (e endpoint) key() string { return e.ch + ":" + e.chat }

func parseEndpoint(key string) endpoint {
	ch, chat, _ := strings.Cut(key, ":")
	return endpoint{ch, chat}
}

// epRec 是记下来的一个窗口
type epRec struct {
	Show    string `json:"show,omitempty"`
	Name    string `json:"name,omitempty"` // 聊天的名字（私聊对方的名字）
	Who     string `json:"who,omitempty"`  // 最近在这个窗口说话的人的身份，网页里设管理员用
	Session string `json:"session"`        // 接着的会话

	// Seen 是这个窗口在各个会话里看到了哪里（历史里的位置），用来补上没看到的对话（recap.go）
	Seen map[string]int `json:"seen,omitempty"`
}

// EndpointInfo 是一个聊天窗口，网页用。
type EndpointInfo struct {
	Show    string `json:"show"`
	Key     string `json:"key"`
	Channel string `json:"channel"` // 拓扑图里的渠道：cli、telegram、feishu
	Name    string `json:"name"`
	Who     string `json:"who,omitempty"` // 在这个窗口说话的人
	Admin   bool   `json:"admin"`         // 说话的人是管理员
	Session string `json:"session"`
}

// chanName 把渠道名（身份和窗口 ID 的前缀）换成拓扑图里的渠道名
func chanName(prefix string) string {
	if prefix == "tg" {
		return "telegram"
	}
	return prefix
}

var sourceLabel = map[string]string{"cli": "命令行", "web": "网页", "tg": "Telegram", "feishu": "飞书"}

// sourceOf 是消息来源的名字，写在用户消息的时间戳里，模型就知道用户是从哪里说的话
func sourceOf(prefix string) string {
	if l, ok := sourceLabel[prefix]; ok {
		return l
	}
	return prefix
}

func (a *App) epFile() string { return filepath.Join(a.store.Dir(), "endpoints.json") }

func (a *App) loadEndpoints() {
	a.eps = map[string]*epRec{}
	b, err := os.ReadFile(a.epFile())
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &a.eps); err != nil {
		a.log.Warn("读取聊天窗口的接线失败，全部按默认重新接", "file", a.epFile(), "err", err)
		a.eps = map[string]*epRec{}
	}
	for k, r := range a.eps {
		if r == nil || r.Session == "" {
			delete(a.eps, k)
		}
	}
}

func (a *App) saveEndpoints() error {
	b, _ := json.MarshalIndent(a.eps, "", "  ")
	tmp := a.epFile() + ".tmp"
	err := os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, a.epFile())
	}
	if err != nil {
		a.log.Warn("保存聊天窗口的接线失败", "err", err)
	}
	return err
}

// migrate 升级旧版本的数据：以前命令行有自己的会话 cli:local，现在命令行接在主会话上，
// 把它的历史和设置搬过去。
func (a *App) migrate() {
	const old = "cli:local"
	if a.store.Exists(mainSID) || !a.store.Exists(old) {
		return
	}
	if err := a.store.Rename(old, mainSID); err != nil {
		a.log.Warn("把旧的命令行会话搬到主会话失败", "err", err)
		return
	}
	if st := a.store.Settings(mainSID); st.Title == "命令行" {
		st.Title = ""
		a.store.SaveSettings(mainSID, st)
	}
	a.log.Info("旧的命令行会话 cli:local 已经并入主会话 main")
}

// boundSession 是窗口现在接着的会话，还没接过时返回空。网页的消息自己带着会话。调用方持有锁。
func (a *App) boundSession(ep endpoint, m channel.Message) string {
	if ep.ch == "web" {
		if m.Session != "" {
			return m.Session
		}
		return mainSID
	}
	if r := a.eps[ep.key()]; r != nil {
		return r.Session
	}
	return ""
}

// attach 返回窗口接着的会话；第一次见到的窗口按说话的人决定接到哪里。调用方持有锁。
func (a *App) attach(ep endpoint, m channel.Message, who string) string {
	if ep.ch == "web" {
		// 网页能在任何会话里说话，说过话就算网页也接着这个会话（主会话一直接着网页）
		sid := a.boundSession(ep, m)
		if st := a.store.Settings(sid); sid != mainSID && !st.Web {
			st.Web = true
			if err := a.store.SaveSettings(sid, st); err != nil {
				a.log.Warn("保存会话设置失败", "sid", sid, "err", err)
			}
			a.publishSession(sid)
		}
		return sid
	}
	key := ep.key()
	r := a.eps[key]
	changed := false
	// 这个渠道（机器人）归哪个 agent：窗口接到的会话跟着它，一个机器人只属于一个 agent
	agentID := a.agentForChannel(ep.ch)
	base := key
	if a.isAdmin(who) {
		base = mainSID
	}
	want := agents.SessionFor(agentID, base)
	if r == nil {
		r = &epRec{Session: want}
		a.eps[key] = r
		changed = true
		a.log.Info("新的聊天窗口", "endpoint", key, "session", r.Session)
		// 接到的会话里已经有对话了（比如管理员的新窗口接到主会话）：说一声，附上最近的对话
		if ts, _ := a.turnsFrom(r.Session, 0); len(ts) > 0 {
			lead := fmt.Sprintf("这个窗口接在会话「%s」上。/history 查看历史，/sync 补上其他窗口的对话。", a.sessionInfo(r.Session).Title)
			if r.Session == mainSID {
				lead = "这个窗口接在主会话上，在这里说话和终端、网页是同一段上下文。/history 查看历史，/sync 补上其他窗口的对话。"
			}
			a.introduce(ep, r.Session, lead)
		} else {
			a.markSeen(ep, r.Session, a.histEnd(r.Session), false)
		}
	}
	if agents.SessionAgent(r.Session) != agentID {
		// 机器人换绑了 agent：这个窗口跟着换到新 agent 的会话
		a.log.Info("机器人换绑了 agent，聊天窗口跟着换会话", "endpoint", key, "from", r.Session, "to", want)
		r.Session = want
		changed = true
	}
	if m.ChatName != "" && r.Name != m.ChatName {
		r.Name, changed = m.ChatName, true
	}
	if r.Who != who {
		r.Who, changed = who, true
	}
	if changed {
		a.saveEndpoints()
		a.publishSession(r.Session)
	}
	return r.Session
}

// bind 把窗口改接到会话 sid。调用方持有锁。
func (a *App) bind(key, sid string) {
	r := a.eps[key]
	if r == nil || r.Session == sid {
		return
	}
	old := r.Session
	r.Session = sid
	a.saveEndpoints()
	a.log.Info("聊天窗口改接到别的会话", "endpoint", key, "from", old, "to", sid)
	a.publishSession(old)
	a.publishSession(sid)
}

func (a *App) publishSession(sid string) {
	a.publish(events.Event{T: "session", SID: sid, Data: a.sessionInfo(sid)})
}

// endpointsOf 是接在会话 sid 上的窗口。网页放在最后：主会话一直接着网页（网页用户是管理员，
// 管理员的窗口默认都接在主会话上），其他会话在网页里新建或者说过话才算。
func (a *App) endpointsOf(sid string) []EndpointInfo {
	out := []EndpointInfo{}
	for key, r := range a.eps {
		if r.Session == sid {
			ep := parseEndpoint(key)
			out = append(out, EndpointInfo{Show: a.showOf(ep), Key: key, Channel: chanName(ep.ch), Name: r.Name, Who: r.Who,
				Admin: ep.ch == "cli" || a.isAdmin(r.Who), Session: sid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if sid == mainSID || a.store.Settings(sid).Web {
		out = append(out, EndpointInfo{Show: "详细", Key: "web", Channel: "web", Who: "web:admin", Admin: true, Session: sid})
	}
	return out
}

// sessionIDs 是所有会话：主会话、磁盘上的、内存里的、有窗口接着的。
func (a *App) sessionIDs() []string {
	seen := map[string]bool{mainSID: true}
	out := []string{mainSID}
	add := func(sid string) {
		if !seen[sid] {
			seen[sid] = true
			out = append(out, sid)
		}
	}
	for _, sid := range a.store.List() {
		add(sid)
	}
	for sid := range a.sessions {
		add(sid)
	}
	for _, r := range a.eps {
		add(r.Session)
	}
	return out
}

func (a *App) hasSession(sid string) bool {
	if sid == mainSID || a.sessions[sid] != nil || a.store.Exists(sid) {
		return true
	}
	for _, r := range a.eps {
		if r.Session == sid {
			return true
		}
	}
	return false
}

// orderedSessions 是 /session 列出的顺序：主会话第一，其他按最近活跃
func (a *App) orderedSessions() []SessionInfo {
	var list []SessionInfo
	for _, sid := range a.sessionIDs() {
		list = append(list, a.sessionInfo(sid))
	}
	sort.SliceStable(list, func(i, j int) bool {
		if (list[i].SID == mainSID) != (list[j].SID == mainSID) {
			return list[i].SID == mainSID
		}
		return list[i].LastAt > list[j].LastAt
	})
	return list
}

// newSession 新建一个会话。web 表示是在网页里新建的（网页接着它）。
func (a *App) newSession(title string, web bool) (string, error) {
	return a.newSessionIn(agents.DefaultID, title, web)
}

// newSessionIn 在某个 agent 下新建一个会话。
func (a *App) newSessionIn(agentID, title string, web bool) (string, error) {
	var sid string
	for sid == "" || a.hasSession(sid) {
		b := make([]byte, 3)
		rand.Read(b)
		sid = agents.SessionFor(agentID, "s-"+hex.EncodeToString(b))
	}
	title = strings.TrimSpace(title)
	if title == "" {
		t := a.now().In(a.loc)
		title = fmt.Sprintf("新会话 %s", t.Format("01-02 15:04"))
	}
	if err := a.store.SaveSettings(sid, session.Settings{Title: title, Web: web}); err != nil {
		return "", err
	}
	a.publishSession(sid)
	return sid, nil
}

// ---- 网页用 ----

// HasSession 判断有没有这个会话。
func (a *App) HasSession(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasSession(sid)
}

// NewSession 新建一个会话（默认 agent 下的）。
func (a *App) NewSession(title string) (SessionInfo, error) {
	return a.NewSessionIn(agents.DefaultID, title)
}

// NewSessionIn 在某个 agent 下新建一个会话。
func (a *App) NewSessionIn(agentID, title string) (SessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sid, err := a.newSessionIn(agentID, title, true)
	if err != nil {
		return SessionInfo{}, err
	}
	return a.sessionInfo(sid), nil
}

// DeleteSession 删除一个会话的历史和设置。接着它的窗口下次说话时按默认规则重新接。
func (a *App) DeleteSession(sid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid == mainSID {
		return errors.New("主会话不能删除，可以清空它的上下文")
	}
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	if s := a.sessions[sid]; s != nil && (s.run != nil || len(s.queue) > 0) {
		return errors.New("这个会话有任务在跑，先停止再删除")
	}
	if err := a.store.Delete(sid); err != nil {
		return err
	}
	delete(a.sessions, sid)
	changed := false
	for key, r := range a.eps {
		if r.Session == sid {
			delete(a.eps, key)
			changed = true
		}
	}
	if changed {
		a.saveEndpoints()
	}
	a.log.Info("删除会话", "sid", sid)
	a.publish(events.Event{T: "session_deleted", SID: sid})
	return nil
}

// SetEndpoint 把聊天窗口 key 改接到会话 sid，并在那个窗口里说一声。
func (a *App) SetEndpoint(key, sid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.eps[key]
	if r == nil {
		return errors.New("没有这个聊天窗口")
	}
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	if r.Session == sid {
		return nil
	}
	a.bind(key, sid)
	a.introduce(parseEndpoint(key), sid, fmt.Sprintf("这个窗口已经改接到会话「%s」，之后在这里说的话都进这个会话。", a.sessionInfo(sid).Title))
	return nil
}

// EndpointWho 是在聊天窗口 key 里说话的人。
func (a *App) EndpointWho(key string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.eps[key]
	if r == nil {
		return "", errors.New("没有这个聊天窗口")
	}
	if r.Who == "" {
		return "", errors.New("还不知道这个窗口里是谁在说话，先让他发一条消息")
	}
	return r.Who, nil
}

// WelcomeAdmin 在窗口里的人刚被设为管理员之后调用：把窗口接到主会话（管理员的窗口默认都在那里），
// 并告诉他。
func (a *App) WelcomeAdmin(key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.eps[key]
	if r == nil {
		return errors.New("没有这个聊天窗口")
	}
	a.publishSession(mainSID) // 窗口的管理员标记变了
	if r.Session == mainSID {
		a.deliver(parseEndpoint(key), "你现在是管理员了。发 /session 可以查看和切换所有会话。", "")
		return nil
	}
	a.bind(key, mainSID)
	a.introduce(parseEndpoint(key), mainSID, "你现在是管理员了。这个窗口已经接到主会话，和终端、网页是同一段上下文；发 /session 可以查看和切换所有会话。")
	return nil
}

// ---- /session 命令 ----

const sessionUsage = "用 /session <编号> 切换，/session new [名字] 新建一个会话并切换过去"

func (a *App) cmdSession(c in, arg string) {
	if arg == "" {
		a.answer(c, a.describeSessions(c))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能切换会话。"+a.adminHint(c))
		return
	}
	if c.ep.ch == "web" {
		a.answer(c, "网页里在会话列表里直接选就行")
		return
	}
	var sid string
	if sub, rest, _ := strings.Cut(arg, " "); sub == "new" {
		var err error
		if sid, err = a.newSession(rest, false); err != nil {
			a.answer(c, "新建会话失败："+err.Error())
			return
		}
	} else if sid = a.findSession(arg); sid == "" {
		a.answer(c, "没有这个会话。"+sessionUsage+"，/session 查看所有会话")
		return
	}
	if sid == c.s.sid {
		a.answer(c, "这个窗口已经接在这个会话上了")
		return
	}
	a.bind(c.ep.key(), sid)
	info := a.sessionInfo(sid)
	a.introduce(c.ep, sid, fmt.Sprintf("已切换到会话「%s」（%d 条消息），之后在这里说的话都进这个会话。", info.Title, info.Messages))
}

// findSession 按 /session 列表里的编号、会话 ID 或者名字找会话
func (a *App) findSession(arg string) string {
	list := a.orderedSessions()
	if n, err := strconv.Atoi(strings.TrimPrefix(arg, "#")); err == nil {
		if n >= 1 && n <= len(list) {
			return list[n-1].SID
		}
		return ""
	}
	var byTitle []string
	for _, info := range list {
		if info.SID == arg {
			return arg
		}
		if info.Title == arg {
			byTitle = append(byTitle, info.SID)
		}
	}
	if len(byTitle) == 1 {
		return byTitle[0]
	}
	return ""
}

func (a *App) describeSessions(c in) string {
	cur := a.sessionInfo(c.s.sid)
	if !a.isAdmin(c.who) {
		return fmt.Sprintf("当前会话：%s（%d 条消息）\n%s", cur.Title, cur.Messages, a.adminHint(c))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "这个窗口接在会话「%s」上。所有会话：\n", cur.Title)
	for i, info := range a.orderedSessions() {
		fmt.Fprintf(&b, "%d. %s · %d 条消息", i+1, info.Title, info.Messages)
		var where []string
		for _, e := range info.Endpoints {
			where = append(where, endpointLabel(e))
		}
		if len(where) > 0 {
			b.WriteString(" · " + strings.Join(where, "、"))
		}
		if info.SID == c.s.sid {
			b.WriteString("  ← 当前")
		}
		b.WriteString("\n")
	}
	b.WriteString(sessionUsage)
	return b.String()
}

// adminHint 告诉不是管理员的人为什么看不到、切不了别的会话，以及怎么成为管理员
func (a *App) adminHint(c in) string {
	return fmt.Sprintf("你现在不是管理员（你的身份：%s），只能用自己的这个会话，看不到也切不到别的会话。\n"+
		"如果这是你自己的机器人：打开网页管理界面，点拓扑图上的「%s」，在你的名字旁边点「设为管理员」，这个窗口就会接到主会话。"+
		"也可以把这个身份加到配置的 [access] admins 里，然后发 /session 1 切到主会话。", c.who, sourceOf(c.ep.ch))
}

func endpointLabel(e EndpointInfo) string {
	l := sourceOf(parseEndpoint(e.Key).ch)
	if e.Name != "" && !slices.Contains([]string{"cli", "web"}, e.Channel) {
		l += "（" + e.Name + "）"
	}
	return l
}
