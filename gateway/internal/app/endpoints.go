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

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
)

// 会话和聊天窗口
//
// 会话是一段对话：上下文、工作目录、权限模式、思考强度、人格。聊天窗口（代码里叫端点）是
// 终端、和 Telegram 机器人的某个私聊、和飞书机器人的某个私聊。每个窗口接在一个会话上，一个会话
// 可以同时接好几个窗口：从哪个窗口说话都是同一段上下文，回复发回说话的那个窗口（命令行如果也接着
// 这个会话，会同步显示）。网页能看到所有会话，也能往任何一个会话里发消息。
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
	Name    string `json:"name,omitempty"` // 聊天的名字（私聊对方的名字）
	Session string `json:"session"`        // 接着的会话
}

// EndpointInfo 是一个聊天窗口，网页用。
type EndpointInfo struct {
	Key     string `json:"key"`
	Channel string `json:"channel"` // 拓扑图里的渠道：cli、telegram、feishu
	Name    string `json:"name"`
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

func (a *App) saveEndpoints() {
	b, _ := json.MarshalIndent(a.eps, "", "  ")
	tmp := a.epFile() + ".tmp"
	err := os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, a.epFile())
	}
	if err != nil {
		a.log.Warn("保存聊天窗口的接线失败", "err", err)
	}
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
	if r == nil {
		r = &epRec{Session: key}
		if a.isAdmin(who) {
			r.Session = mainSID
		}
		a.eps[key] = r
		changed = true
		a.log.Info("新的聊天窗口", "endpoint", key, "session", r.Session)
	}
	if m.ChatName != "" && r.Name != m.ChatName {
		r.Name, changed = m.ChatName, true
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
			out = append(out, EndpointInfo{Key: key, Channel: chanName(ep.ch), Name: r.Name, Session: sid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if sid == mainSID || a.store.Settings(sid).Web {
		out = append(out, EndpointInfo{Key: "web", Channel: "web", Session: sid})
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
	var sid string
	for sid == "" || a.hasSession(sid) {
		b := make([]byte, 3)
		rand.Read(b)
		sid = "s-" + hex.EncodeToString(b)
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

// NewSession 新建一个会话。
func (a *App) NewSession(title string) (SessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sid, err := a.newSession(title, true)
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
	a.deliver(parseEndpoint(key), fmt.Sprintf("这个窗口已经改接到会话「%s」，之后在这里说的话都进这个会话", a.sessionInfo(sid).Title), "")
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
		a.answer(c, "只有管理员能切换会话")
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
	a.tell(sid, c.ep, fmt.Sprintf("已切换到会话「%s」（%d 条消息），之后在这里说的话都进这个会话", info.Title, info.Messages))
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
		return fmt.Sprintf("当前会话：%s（%d 条消息）", cur.Title, cur.Messages)
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

func endpointLabel(e EndpointInfo) string {
	l := sourceOf(parseEndpoint(e.Key).ch)
	if e.Name != "" && !slices.Contains([]string{"cli", "web"}, e.Channel) {
		l += "（" + e.Name + "）"
	}
	return l
}
