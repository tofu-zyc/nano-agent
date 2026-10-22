// Package app 是网关的调度中心：聊天消息进来排队，交给内核开 run；内核要模型就调模型，
// 工具结果写进上下文，run 结束后把回复发回聊天平台。
//
// 所有状态由一把锁保护；模型请求在单独的协程里跑，回来后再拿锁。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

const helpText = `可用命令：
/whoami  查看你的身份、这个窗口和它接着的会话
/session 查看会话；管理员可以 /session <编号> 切换，/session new [名字] 新建
/history 查看这个会话最近的对话；/history <轮数> 看更多
/sync    补上其他窗口的聊天记录（同一会话内也会自动同步）
/dir     查看和切换工作目录（哪台机器上的哪个目录），切换需管理员
/node    查看机器和在线的内核
/stop    停止当前任务（任务进行中直接发「停」也行）
/undo    撤回最近一条消息：还没处理的直接拿掉，已经回复的连同回复从上下文里删掉（直接发「撤回」也行）
/reset   清空本会话的上下文
/mode    查看和切换权限模式：auto 自动、ask 审批、readonly 只读（切换需管理员）
/think   查看和切换思考强度（切换需管理员）
/approve <编号>  批准一次工具执行（需管理员）
/deny <编号>     拒绝一次工具执行（需管理员）`

type App struct {
	ctx     context.Context
	cfg     *config.Config
	llm     llm.Client
	store   *session.Store
	persona *persona.Loader
	hub     *kernel.Hub
	log     *slog.Logger
	bus     *events.Bus
	started time.Time

	// 渠道管理（channels.go），用单独的锁：停止渠道要等它退出，不能拿着 mu 等
	chMu  sync.Mutex
	runs  map[string]*chanRun
	chErr map[string]string
	cli   *channel.CLI
	web   *channel.Web

	now func() time.Time // 测试里可以换成固定时间
	loc *time.Location   // 时间戳用的时区

	mu           sync.Mutex
	groupNoticed map[string]bool            // 已经提醒过“只支持私聊”的群
	pendingLs    map[string]chan *proto.Msg // 等内核回复的列目录请求
	lsSeq        int
	chans        map[string]channel.Channel // 按名字找渠道：渠道重启后换成新的对象，会话照常能用
	outboxes     map[string]chan outItem
	eps          map[string]*epRec // 聊天窗口接在哪个会话上（endpoints.go）
	wss          []Workspace       // 工作目录列表（workspaces.go）
	hosts        Hosts             // 网关能直接执行工具的机器：本机、SSH 服务器（nodes.go）
	nodes        map[string]*node  // 在线的内核（nodes.go）
	localNode    string            // 最近一个和网关在同一台机器上的内核
	nodeSeq      int
	totals       Totals
	sessions     map[string]*sess
	approvals    map[int]*approval
	nextAppr     int
	runSeq       uint64
}

type sess struct {
	sid        string
	queue      []queued
	run        *run
	resetAfter bool     // run 结束后清空上下文（/reset 时正好有任务在跑）
	undoAfter  bool     // run 结束后撤回这一轮（/undo 时正好有任务在跑）
	recent     []msgPos // 最近处理过的平台消息在历史里的位置，平台上撤回消息时用
}

// queued 是排队等处理的一条消息
type queued struct {
	m   channel.Message
	ep  endpoint
	who string
}

// in 是收到一条命令时的情况：哪个会话、哪个窗口、谁说的
type in struct {
	s   *sess
	ep  endpoint
	who string
}

// out 是一轮处理要显示到的一个窗口
type out struct {
	ep   endpoint
	view channel.RunView // nil 表示这个窗口不能流式显示，回复在结束时整条发
}

type run struct {
	id         uint64
	node       string // 跑这一轮的内核
	from       string // 触发者身份，内核据此判断权限
	outs       []out  // 第一个是说话的窗口，后面是同步显示的窗口和中途来停止、清空的窗口
	final      string
	truncated  bool
	cancelLLM  context.CancelFunc // 非空表示有模型请求在路上
	stopTyping func()

	streamed bool // 回复正文已经通过 RunView 显示过了
	start    time.Time
	calls    int       // 这一轮请求了几次模型
	usage    llm.Usage // 这一轮的累计用量
	noTools  bool      // 没有工作目录：不给模型工具，只聊天

	// 异机模式（exec.go）：工具交给网关在 host 上执行，约束是这一轮开始时定下的
	gateway      bool
	host         string
	dirs, ro, rw []string
	write        bool
	execs        map[string]context.CancelFunc // 正在执行的工具调用
}

// views 是这一轮所有能流式显示的窗口
func (r *run) views() []channel.RunView {
	var vs []channel.RunView
	for _, o := range r.outs {
		if o.view != nil {
			vs = append(vs, o.view)
		}
	}
	return vs
}

// addOut 让这个窗口也收到这一轮的结束说明（中途从别的窗口停止、清空时）
func (r *run) addOut(ep endpoint) {
	for _, o := range r.outs {
		if o.ep == ep {
			return
		}
	}
	r.outs = append(r.outs, out{ep: ep})
}

type approval struct {
	sid, id, name, detail, from string
	at                          time.Time
}

type outItem struct {
	ch      channel.Channel
	chatID  string
	text    string
	replyTo string
}

func New(ctx context.Context, cfg *config.Config, client llm.Client, store *session.Store, p *persona.Loader, log *slog.Logger) *App {
	loc := time.Local
	if cfg.Session.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Session.Timezone); err == nil {
			loc = l
		}
	}
	a := &App{
		ctx: ctx, cfg: cfg, llm: client, store: store, persona: p, log: log,
		now: time.Now, loc: loc, bus: events.NewBus(), started: time.Now(),
		chans:        map[string]channel.Channel{},
		groupNoticed: map[string]bool{},
		pendingLs:    map[string]chan *proto.Msg{},
		outboxes:     map[string]chan outItem{},
		sessions:     map[string]*sess{},
		approvals:    map[int]*approval{},
		nodes:        map[string]*node{},
	}
	a.migrate()
	a.loadEndpoints()
	a.loadWorkspaces()
	return a
}

func (a *App) SetHub(h *kernel.Hub) { a.hub = h }

// Bus 是事件总线，网页界面从这里订阅。
func (a *App) Bus() *events.Bus { return a.bus }

func (a *App) publish(e events.Event) { a.bus.Publish(e) }

// AddChannel 登记一个聊天通道（同名的旧通道被替换），并保证有一个按顺序发送消息的协程。
func (a *App) AddChannel(ch channel.Channel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name := ch.Name()
	a.chans[name] = ch
	if _, ok := a.outboxes[name]; ok {
		return
	}
	q := make(chan outItem, 256)
	a.outboxes[name] = q
	go func() {
		for it := range q {
			ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
			if err := it.ch.Send(ctx, it.chatID, it.text, it.replyTo); err != nil {
				a.log.Warn("发送消息失败", "channel", name, "chat", it.chatID, "err", err)
			}
			cancel()
		}
	}()
}

// SetConfig 换上新的配置（网页里改了设置时调用）。模型客户端由调用方另外换。
func (a *App) SetConfig(cfg *config.Config) {
	loc := time.Local
	if cfg.Session.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Session.Timezone); err == nil {
			loc = l
		}
	}
	a.mu.Lock()
	a.cfg, a.loc = cfg, loc
	a.mu.Unlock()
}

// SetLLM 换上新的模型客户端。已经发出的请求不受影响。
func (a *App) SetLLM(c llm.Client) {
	a.mu.Lock()
	a.llm = c
	a.mu.Unlock()
}

// ---- 聊天消息 ----

var commands = map[string]bool{"help": true, "start": true, "sync": true, "whoami": true, "stop": true, "reset": true, "approve": true, "deny": true,
	"undo": true, "dir": true, "mode": true, "think": true, "session": true, "history": true, "node": true}

func parseCommand(text string) (cmd, arg string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	word, rest, _ := strings.Cut(text[1:], " ")
	word = strings.ToLower(word)
	if !commands[word] {
		return "", ""
	}
	return word, strings.TrimSpace(rest)
}

// OnMessage 是所有聊天通道共用的消息入口。
func (a *App) OnMessage(ch channel.Channel, m channel.Message) {
	ep := endpoint{ch: ch.Name(), chat: m.ChatID}
	who := ch.Name() + ":" + m.SenderID
	m.Text = strings.TrimSpace(m.Text)
	cmd, arg := parseCommand(m.Text)

	a.mu.Lock()
	defer a.mu.Unlock()
	if m.Group {
		// 群聊暂不支持：被 @ 时提醒一次，其余忽略
		if m.Mentioned && !a.groupNoticed[ep.key()] {
			a.groupNoticed[ep.key()] = true
			a.deliver(ep, "暂时只支持私聊，有事请私聊我", m.MsgID)
		}
		a.log.Debug("暂不支持群聊，忽略", "chat", ep.key())
		return
	}
	switch cmd {
	case "help":
		a.tell(a.boundSession(ep, m), ep, helpText)
		return
	case "whoami":
		sid := a.boundSession(ep, m)
		where := "还没有接到会话（发第一条消息时自动接上）"
		if sid != "" {
			where = a.sessionInfo(sid).Title + "（" + sid + "）"
		}
		a.tell(sid, ep, fmt.Sprintf("你的身份：%s\n这个窗口：%s\n接着的会话：%s", who, ep.key(), where))
		return
	}
	if !a.allowed(ep.key()) {
		a.log.Info("忽略不在白名单里的聊天", "chat", ep.key(), "from", who)
		return
	}
	if m.Action == channel.ActionRecall {
		// 撤回事件里没有说话的人：按窗口现在接着的会话处理，不改窗口的记录
		if r := a.eps[ep.key()]; r != nil && m.MsgID != "" {
			a.recall(in{s: a.sessOf(r.Session), ep: ep, who: r.Who}, m.MsgID)
		}
		return
	}
	c := in{s: a.sessOf(a.attach(ep, m, who)), ep: ep, who: who}
	if m.Action == channel.ActionStop {
		if c.s.run != nil || len(c.s.queue) > 0 {
			a.stop(c)
		}
		return
	}
	if cmd == "" {
		cmd = a.plainCommand(c.s, m.Text)
	}
	switch cmd {
	case "start":
		a.answer(c, helpText)
		return
	case "session":
		a.cmdSession(c, arg)
		return
	case "history":
		a.cmdHistory(c, arg)
		return
	case "sync":
		a.cmdHistory(c, arg)
		return
	case "node":
		a.cmdNode(c, arg)
		return
	case "stop":
		a.stop(c)
		return
	case "undo":
		a.undo(c)
		return
	case "reset":
		a.reset(c)
		return
	case "approve", "deny":
		a.decide(c, arg, cmd == "approve")
		return
	case "dir":
		a.cmdDir(c, arg)
		return
	case "mode":
		a.cmdMode(c, strings.ToLower(arg))
		return
	case "think":
		a.cmdThink(c, strings.ToLower(arg))
		return
	}
	if m.Text == "" {
		return
	}
	s := c.s
	a.publish(events.Event{T: "message", SID: s.sid, Name: m.SenderName, Text: m.Text,
		Data: map[string]any{"from": who, "via": sourceOf(ep.ch), "channel": chanName(ep.ch)}})
	s.queue = append(s.queue, queued{m: m, ep: ep, who: who})
	if s.run == nil {
		a.startRun(s)
	}
}

func (a *App) sessOf(sid string) *sess {
	s := a.sessions[sid]
	if s == nil {
		s = &sess{sid: sid}
		a.sessions[sid] = s
	}
	return s
}

func (a *App) allowed(key string) bool {
	if len(a.cfg.Access.AllowChats) == 0 || strings.HasPrefix(key, "cli:") || strings.HasPrefix(key, "web:") {
		return true
	}
	return slices.Contains(a.cfg.Access.AllowChats, key)
}

// deliver 把一条消息发到一个窗口。调用方持有锁。
func (a *App) deliver(ep endpoint, text, replyTo string) {
	ch := a.chans[ep.ch]
	if ch == nil {
		a.log.Warn("渠道已经不在了，消息没发出去", "chat", ep.key())
		return
	}
	select {
	case a.outboxes[ep.ch] <- outItem{ch: ch, chatID: ep.chat, text: text, replyTo: replyTo}:
	default:
		a.log.Warn("发送队列已满，丢弃一条消息", "chat", ep.key())
	}
}

// tell 回复一个窗口；sid 不为空时网页上也显示在这个会话里。调用方持有锁。
func (a *App) tell(sid string, ep endpoint, text string) {
	if sid != "" {
		a.publish(events.Event{T: "say", SID: sid, Text: text})
	}
	a.deliver(ep, text, "")
}

// answer 回复发命令的窗口。
func (a *App) answer(c in, text string) { a.tell(c.s.sid, c.ep, text) }

// notify 把和这一轮有关的消息（审批请求、内核提示）发到这一轮的所有窗口。调用方持有锁。
func (a *App) notify(s *sess, text string) {
	a.publish(events.Event{T: "say", SID: s.sid, Text: text})
	for _, o := range s.run.outs {
		a.deliver(o.ep, text, "")
	}
}

func (a *App) startTyping(ep endpoint) func() {
	t, ok := a.chans[ep.ch].(channel.Typer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		for {
			t.Typing(a.ctx, ep.chat)
			select {
			case <-done:
				return
			case <-a.ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// startRun 取出队首的消息（连同同一个人在同一个窗口紧接着发的几条）开一个 run。调用方持有锁。
func (a *App) startRun(s *sess) {
	if len(s.queue) == 0 || s.run != nil {
		return
	}
	head := s.queue[0]
	w := a.workspaceOf(s.sid)
	rt := a.route(w)
	node := rt.kernel
	if node == "" {
		const text = "（没有在线的内核，暂时没法处理消息）"
		a.publish(events.Event{T: "say", SID: s.sid, Text: text})
		var told []endpoint
		for _, q := range s.queue {
			if !slices.Contains(told, q.ep) {
				told = append(told, q.ep)
				a.deliver(q.ep, text, "")
			}
		}
		s.queue = nil
		return
	}
	n := 1
	for n < len(s.queue) && s.queue[n].ep == head.ep && s.queue[n].who == head.who {
		n++
	}
	batch := s.queue[:n]
	s.queue = slices.Clone(s.queue[n:])

	texts := make([]string, len(batch))
	for i, q := range batch {
		texts[i] = q.m.Text
	}
	text := strings.Join(texts, "\n")
	content := text
	if config.On(a.cfg.Session.Timestamps) {
		content = a.stamp(sourceOf(head.ep.ch)) + " " + content
	}
	// 工作目录、权限、机器变了：把新的环境说明放在这条消息前面，而不是改系统提示词（前缀缓存不失效）
	st := a.store.Settings(s.sid)
	dirty := false
	if !st.WorkspaceSet && w.ws != nil {
		st.Workspace, st.WorkspaceSet, dirty = w.ws.ID, true, true // 新会话第一次说话：定下默认的工作目录
	}
	note := envNote(w)
	if rt.note != "" {
		note = rt.note // 工作目录所在的机器用不了，只聊天
	}
	if note != st.EnvNote && note != "" {
		content = note + "\n" + content
		st.EnvNote, dirty = note, true
	}
	if dirty {
		if err := a.store.SaveSettings(s.sid, st); err != nil {
			a.log.Warn("保存会话设置失败", "sid", s.sid, "err", err)
		}
	}
	// 先补上这个窗口没看到的对话（别的窗口聊的），再开始这一轮
	a.catchUp(head.ep, s.sid)
	a.remember(s, batch, a.histEnd(s.sid))
	if err := a.store.Append(s.sid, session.Msg{Role: "user", Content: content}); err != nil {
		a.log.Warn("保存消息失败", "sid", s.sid, "err", err)
	}
	a.markSeen(head.ep, s.sid, a.histEnd(s.sid), false)

	a.runSeq++
	r := &run{id: a.runSeq, node: node, from: head.who, start: a.now(), outs: []out{{ep: head.ep}}, execs: map[string]context.CancelFunc{}}
	s.run = r
	if st, ok := a.chans[head.ep.ch].(channel.Streamer); ok {
		r.outs[0].view = st.OpenRun(head.ep.chat, "")
	}
	// 同一个会话的其他窗口里能同步显示的（命令行）：先显示这条消息，再显示回复过程
	via := sourceOf(head.ep.ch)
	if name := head.m.SenderName; name != "" && head.ep.ch != "web" && head.ep.ch != "cli" {
		via += " · " + name
	}
	for key, rec := range a.eps {
		ep := parseEndpoint(key)
		if rec.Session != s.sid || ep == head.ep {
			continue
		}
		if wt, ok := a.chans[ep.ch].(channel.Watcher); ok {
			a.catchUp(ep, s.sid)
			r.outs = append(r.outs, out{ep: ep, view: wt.OpenMirror(ep.chat, via, text)})
		}
	}
	r.stopTyping = a.startTyping(head.ep)
	mode := a.runMode(w, r.from)
	in := proto.Msg{T: proto.TInput, SID: s.sid, From: r.from, Mode: mode, Dirs: w.dirs, RO: w.ro, RW: w.rw}
	if w.ws == nil || rt.note != "" {
		// 没有工作目录，或者工作目录所在的机器用不了：不给模型工具，只聊天
		r.noTools = true
		in.Mode, in.Dirs, in.RO, in.RW = proto.ModeNone, nil, nil, nil
	} else if rt.gateway {
		// 异机模式：工具交给网关在工作目录所在的机器上执行
		in.Exec, in.Landlock = "gateway", rt.landlock
		r.gateway, r.host, r.dirs, r.ro, r.rw = true, w.ws.Host, w.dirs, w.ro, w.rw
		r.write = mode == proto.ModeAuto || mode == proto.ModeAsk
	}
	a.log.Info("开始处理", "sid", s.sid, "node", node, "gateway", rt.gateway, "from", r.from, "chat", head.ep.key(), "messages", n, "mode", in.Mode)
	a.totals.Runs++
	a.publish(events.Event{T: "run", SID: s.sid, Kind: "start", Text: content,
		Data: map[string]any{"from": r.from, "mode": in.Mode, "channel": chanName(head.ep.ch), "node": node}})
	if !a.hub.Send(node, in) {
		a.endRun(s, "（内核连接断开，本轮已中止）")
	}
}

// endRun 清理一个 run，可选地给这一轮的所有窗口发一条说明。调用方持有锁。
func (a *App) endRun(s *sess, note string) {
	r := s.run
	if r == nil {
		return
	}
	if r.cancelLLM != nil {
		r.cancelLLM()
	}
	for _, cancel := range r.execs {
		cancel() // 交给网关执行的工具调用
	}
	r.stopTyping()
	for _, v := range r.views() {
		v.Close()
	}
	s.run = nil
	for n, ap := range a.approvals {
		if ap.sid == s.sid {
			delete(a.approvals, n)
			a.publish(events.Event{T: "approval_done", SID: s.sid, N: n})
		}
	}
	if note != "" {
		a.publish(events.Event{T: "say", SID: s.sid, Text: note})
		for _, o := range r.outs {
			a.deliver(o.ep, note, "")
		}
	}
}

func (a *App) stop(c in) {
	s := c.s
	s.queue = nil
	if s.run == nil {
		a.answer(c, "当前没有进行中的任务")
		return
	}
	s.run.addOut(c.ep)
	a.cancelRun(s) // 内核会回 done(cancelled)
}

func (a *App) reset(c in) {
	s := c.s
	s.queue = nil
	if s.run != nil {
		s.resetAfter = true
		s.run.addOut(c.ep)
		a.cancelRun(s)
		return
	}
	if err := a.store.Reset(s.sid); err != nil {
		a.log.Warn("清空上下文失败", "sid", s.sid, "err", err)
	}
	a.answer(c, "上下文已清空")
}

func (a *App) decide(c in, arg string, ok bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	ap := a.approvals[n]
	if err != nil || ap == nil || ap.sid != c.s.sid {
		a.answer(c, "没有这个编号的待审批请求")
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能审批")
		return
	}
	a.hub.Send(c.s.run.node, proto.Msg{T: proto.TApprove, SID: ap.sid, ID: ap.id, OK: proto.Bool(ok), From: c.who})
}

// ---- 内核事件（kernel.Handler） ----

func (a *App) KernelMsg(node string, m *proto.Msg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.T == proto.TLsResult {
		if ch := a.pendingLs[m.ID]; ch != nil {
			ch <- m
		}
		return
	}
	if m.T == proto.TTools {
		a.toolsChanged(node, m)
		return
	}
	s := a.sessions[m.SID]
	if s == nil || s.run == nil || s.run.node != node {
		if m.T == proto.TModel { // 内核以为还有 run，告诉它结束
			a.hub.Send(node, proto.Msg{T: proto.TTurn, SID: m.SID, Reason: proto.ReasonError, Detail: "网关没有这个会话在这个内核上的进行中任务"})
		}
		a.log.Debug("忽略没有对应 run 的内核消息", "node", node, "t", m.T, "sid", m.SID)
		return
	}
	switch m.T {
	case proto.TModel:
		a.callModel(s)
	case proto.TExec:
		a.execTool(node, s, m)
	case proto.TSkillFiles:
		a.skillFiles(node, s, m)
	case proto.TExecCancel:
		if cancel := s.run.execs[m.ID]; cancel != nil {
			cancel()
		}
	case proto.TResult:
		content := m.Content
		if m.OK != nil && !*m.OK {
			content = "[执行失败] " + content
		}
		if err := a.store.Append(s.sid, session.Msg{Role: "tool", ToolCallID: m.ID, Content: content}); err != nil {
			a.log.Warn("保存工具结果失败", "sid", s.sid, "err", err)
		}
		for _, v := range s.run.views() {
			v.ToolResult(m.OK == nil || *m.OK, resultDetail(m.Content))
		}
		for n, ap := range a.approvals {
			if ap.sid == s.sid && ap.id == m.ID {
				delete(a.approvals, n)
				a.publish(events.Event{T: "approval_done", SID: s.sid, N: n})
			}
		}
		a.publish(events.Event{T: "tool_result", SID: s.sid, ID: m.ID, OK: events.Bool(m.OK == nil || *m.OK),
			Text: resultDetail(m.Content), Data: clip(m.Content, 4000)})
	case proto.TApproveReq:
		a.nextAppr++
		n := a.nextAppr
		desc := describeCall(m.Name, m.Args)
		a.approvals[n] = &approval{sid: s.sid, id: m.ID, name: m.Name, detail: desc, from: m.From, at: a.now()}
		a.publish(events.Event{T: "approval", SID: s.sid, N: n, ID: m.ID, Name: m.Name, Text: desc, Data: map[string]any{"from": m.From}})
		a.notify(s, fmt.Sprintf("⚠️ 需要审批 #%d\n%s\n发起人：%s\n管理员回复 /approve %d 批准，/deny %d 拒绝",
			n, desc, m.From, n, n))
	case proto.TNotice:
		a.notify(s, m.Text)
	case proto.TDone:
		a.finish(s, m.Status, m.Detail)
	default:
		a.log.Debug("忽略未知的内核消息", "t", m.T)
	}
}

func (a *App) finish(s *sess, status, detail string) {
	r := s.run
	// body 是回复正文，只发给没有流式显示过它的窗口；note 是给所有窗口的说明
	var body, note string
	switch status {
	case proto.StatusOK:
		body = r.final
		if strings.TrimSpace(body) == "" {
			body = "（模型没有返回内容）"
		}
		if r.truncated {
			note = "（回复太长，被截断了）"
		}
	case proto.StatusBudget:
		note = "（这一轮调用工具的次数达到上限，已停止）"
	case proto.StatusTimeout:
		note = "（这一轮运行超时，已停止）"
	case proto.StatusCancelled:
		note = "（已停止）"
	case proto.StatusRefused:
		body = r.final
		if body == "" {
			body = "（模型拒绝了这个请求）"
		}
	default:
		note = "（出错了：" + detail + "）"
	}
	if text, ok := a.afterRun(s); ok {
		body, note = "", text
	}
	a.log.Info("处理结束", "sid", s.sid, "status", status, "model_calls", r.calls,
		"prompt_tokens", r.usage.PromptTokens, "cached_tokens", r.usage.CachedTokens, "completion_tokens", r.usage.CompletionTokens)
	if r.calls > 0 {
		for _, v := range r.views() {
			v.Note(usageLine(r, a.now()))
		}
	}
	a.publish(events.Event{T: "run", SID: s.sid, Kind: "end", Text: status, Data: map[string]any{
		"calls": r.calls, "usage": r.usage, "ms": a.now().Sub(r.start).Milliseconds(), "detail": detail}})
	a.endRun(s, "")
	// 这一轮显示到的窗口都看到了这里
	end := a.histEnd(s.sid)
	for _, o := range r.outs {
		a.markSeen(o.ep, s.sid, end, false)
	}
	a.saveEndpoints()
	if note != "" {
		a.publish(events.Event{T: "say", SID: s.sid, Text: note})
	}
	for _, o := range r.outs {
		text := note
		if body != "" && (o.view == nil || !r.streamed) {
			text = strings.TrimSpace(body + "\n\n" + note)
		}
		if text != "" {
			a.deliver(o.ep, text, "")
		}
	}
	a.startRun(s)
}

// callModel 发起一次模型请求。调用方持有锁；请求本身在协程里跑。
func (a *App) callModel(s *sess) {
	r := s.run
	if r.cancelLLM != nil {
		a.log.Warn("上一次模型请求还没结束，忽略重复的 model", "sid", s.sid)
		return
	}
	high := a.cfg.LLM.MaxHistoryTokens
	low := int(float64(high) * a.cfg.LLM.TrimTo)
	h := a.store.History(s.sid)
	if session.Estimate(h) > high && session.CutPoint(h, low) > 0 {
		a.compact(s, high, low)
		return
	}
	a.generateModel(s)
}

// generateModel 用准备好的上下文请求本轮回复，调用方持有锁。
func (a *App) generateModel(s *sess) {
	r := s.run
	system, _ := a.persona.For(s.sid)
	st := a.store.Settings(s.sid)
	req := llm.Request{
		Messages: session.BuildMessages(system, st.Summary, a.store.History(s.sid), a.cfg.LLM.SendReasoning),
		Thinking: a.thinkingOf(s.sid),
	}
	if n := a.nodes[r.node]; n != nil && !r.noTools {
		req.Tools = slices.Clone(n.tools)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	r.cancelLLM = cancel
	r.calls++
	a.totals.ModelCalls++
	a.publish(events.Event{T: "model", SID: s.sid, Kind: "start", Data: map[string]any{"thinking": req.Thinking}})

	// 过程总是发布到事件总线（网页实时显示），支持流式的窗口再自己显示一份
	views, bus, sid := r.views(), a.bus, s.sid
	contentEmitted := false
	onDelta := func(d llm.Delta) {
		if d.Reasoning != "" {
			bus.Publish(events.Event{T: "delta", SID: sid, Kind: "reasoning", Text: d.Reasoning})
			for _, v := range views {
				v.Reasoning(d.Reasoning)
			}
		}
		if d.Content != "" {
			contentEmitted = true
			bus.Publish(events.Event{T: "delta", SID: sid, Kind: "content", Text: d.Content})
			for _, v := range views {
				v.Content(d.Content)
			}
		}
	}
	client := a.llm

	go func() {
		start := time.Now()
		res, err := client.Chat(ctx, req, onDelta)
		a.mu.Lock()
		defer a.mu.Unlock()
		stale := s.run != r || ctx.Err() != nil // run 已经结束（停止、超时、断线），结果作废
		cancel()
		if stale {
			return
		}
		r.cancelLLM = nil
		// 拒绝回复等结果可能没有正文增量，仍需交给流式窗口显示一次。
		if err == nil && res.Content != "" && !contentEmitted {
			onDelta(llm.Delta{Content: res.Content})
		}
		for _, v := range views {
			v.EndMessage()
		}
		if err != nil {
			a.publish(events.Event{T: "model", SID: s.sid, Kind: "end", Text: "error: " + err.Error()})
			a.log.Warn("模型请求失败", "sid", s.sid, "err", err)
			a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, Reason: proto.ReasonError, Detail: err.Error()})
			return
		}
		u := res.Usage
		a.addUsage(r, u)
		a.publish(events.Event{T: "model", SID: s.sid, Kind: "end", Text: res.Reason,
			Data: map[string]any{"usage": u, "ms": time.Since(start).Milliseconds()}})
		if res.Content != "" {
			r.streamed = true
		}
		a.log.Info("模型回复", "sid", s.sid, "reason", res.Reason, "calls", len(res.Calls),
			"prompt_tokens", u.PromptTokens, "cached_tokens", u.CachedTokens,
			"completion_tokens", u.CompletionTokens, "reasoning_tokens", u.ReasoningTokens,
			"took", time.Since(start).Round(time.Millisecond))
		if err := a.store.Append(s.sid, session.Msg{Role: "assistant", Content: res.Content, ToolCalls: res.Calls,
			Reasoning: res.Reasoning, Usage: &u}); err != nil {
			a.log.Warn("保存模型回复失败", "sid", s.sid, "err", err)
		}
		turn := proto.Msg{T: proto.TTurn, SID: s.sid}
		switch res.Reason {
		case llm.ReasonToolUse:
			turn.Reason = proto.ReasonToolUse
			for _, c := range res.Calls {
				turn.Calls = append(turn.Calls, proto.Call{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)})
				detail := toolDetail(c.Function.Name, c.Function.Arguments)
				for _, v := range views {
					v.Tool(c.Function.Name, detail)
				}
				a.publish(events.Event{T: "tool", SID: s.sid, ID: c.ID, Name: c.Function.Name, Text: detail,
					Data: clip(c.Function.Arguments, 4000)})
			}
		case llm.ReasonRefused:
			turn.Reason = proto.ReasonRefused
			r.final = res.Content
		default:
			turn.Reason = proto.ReasonFinal
			r.final, r.truncated = res.Content, res.Truncated
		}
		a.hub.Send(r.node, turn)
	}()
}

// argsJSON 把模型给的参数字符串转成协议里的 args：合法的 JSON 对象原样放进去，
// 否则作为字符串传给内核，由内核判定为参数错误并告诉模型。
func argsJSON(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(s)
	return b
}

func prettyArgs(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	s := string(b)
	if r := []rune(s); len(r) > 800 {
		s = string(r[:800]) + "…"
	}
	return s
}
