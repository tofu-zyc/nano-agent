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
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

const helpText = `可用命令：
/whoami  查看你的身份和当前会话 ID
/stop    停止当前任务
/reset   清空本会话的上下文
/dir     查看和修改工作目录（修改需管理员）
/mode    查看和切换权限模式：auto 自动、ask 审批、readonly 只读（切换需管理员）
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

	outboxes map[string]chan outItem

	mu        sync.Mutex
	tools     []llm.Tool
	sandbox   bool // 内核能把工具关在工作目录的沙箱里
	sessions  map[string]*sess
	approvals map[int]*approval
	nextAppr  int
	runSeq    uint64
}

type sess struct {
	sid        string
	ch         channel.Channel
	chatID     string
	group      bool
	queue      []channel.Message
	run        *run
	resetAfter bool // run 结束后清空上下文（/reset 时正好有任务在跑）
}

type run struct {
	id         uint64
	from       string // 触发者身份，内核据此判断权限
	replyTo    string
	final      string
	truncated  bool
	cancelLLM  context.CancelFunc // 非空表示有模型请求在路上
	stopTyping func()
}

type approval struct {
	sid, id string
}

type outItem struct {
	ch      channel.Channel
	chatID  string
	text    string
	replyTo string
}

func New(ctx context.Context, cfg *config.Config, client llm.Client, store *session.Store, p *persona.Loader, log *slog.Logger) *App {
	return &App{
		ctx: ctx, cfg: cfg, llm: client, store: store, persona: p, log: log,
		outboxes:  map[string]chan outItem{},
		sessions:  map[string]*sess{},
		approvals: map[int]*approval{},
	}
}

func (a *App) SetHub(h *kernel.Hub) { a.hub = h }

// AddChannel 登记一个聊天通道，并为它启动一个按顺序发送消息的协程。
func (a *App) AddChannel(ch channel.Channel) {
	q := make(chan outItem, 256)
	a.outboxes[ch.Name()] = q
	go func() {
		for it := range q {
			ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
			if err := it.ch.Send(ctx, it.chatID, it.text, it.replyTo); err != nil {
				a.log.Warn("发送消息失败", "channel", ch.Name(), "chat", it.chatID, "err", err)
			}
			cancel()
		}
	}()
}

// ---- 聊天消息 ----

var commands = map[string]bool{"help": true, "whoami": true, "stop": true, "reset": true, "approve": true, "deny": true,
	"dir": true, "mode": true}

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
	sid := ch.Name() + ":" + m.ChatID
	who := ch.Name() + ":" + m.SenderID
	m.Text = strings.TrimSpace(m.Text)
	cmd, arg := parseCommand(m.Text)

	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[sid]
	if s == nil {
		s = &sess{sid: sid, ch: ch, chatID: m.ChatID}
		a.sessions[sid] = s
	}
	s.group = m.Group
	reply := a.replyTo(s, m)

	switch cmd {
	case "help":
		a.say(s, helpText, reply)
		return
	case "whoami":
		a.say(s, fmt.Sprintf("你的身份：%s\n当前会话：%s", who, sid), reply)
		return
	}
	if !a.allowed(sid) {
		a.log.Info("忽略不在白名单里的会话", "sid", sid, "from", who)
		return
	}
	switch cmd {
	case "stop":
		a.stop(s, reply)
		return
	case "reset":
		a.reset(s, reply)
		return
	case "approve", "deny":
		a.decide(s, who, arg, cmd == "approve", reply)
		return
	case "dir":
		a.cmdDir(s, who, arg, reply)
		return
	case "mode":
		a.cmdMode(s, who, strings.ToLower(arg), reply)
		return
	}
	if m.Text == "" || (m.Group && !m.Mentioned) {
		return
	}
	s.queue = append(s.queue, m)
	if s.run == nil {
		a.startRun(s)
	}
}

func (a *App) allowed(sid string) bool {
	if len(a.cfg.Access.AllowChats) == 0 || strings.HasPrefix(sid, "cli:") {
		return true
	}
	return slices.Contains(a.cfg.Access.AllowChats, sid)
}

func (a *App) replyTo(s *sess, m channel.Message) string {
	if s.group {
		return m.MsgID
	}
	return ""
}

func (a *App) say(s *sess, text, replyTo string) {
	select {
	case a.outboxes[s.ch.Name()] <- outItem{ch: s.ch, chatID: s.chatID, text: text, replyTo: replyTo}:
	default:
		a.log.Warn("发送队列已满，丢弃一条消息", "sid", s.sid)
	}
}

func (a *App) startTyping(s *sess) func() {
	t, ok := s.ch.(channel.Typer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		for {
			t.Typing(a.ctx, s.chatID)
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

// startRun 取出队首的消息（连同同一个人紧接着发的几条）开一个 run。调用方持有锁。
func (a *App) startRun(s *sess) {
	if len(s.queue) == 0 || s.run != nil {
		return
	}
	head := s.queue[0]
	if !a.hub.Connected() {
		a.say(s, "（内核没有连接，暂时没法处理消息）", a.replyTo(s, head))
		s.queue = nil
		return
	}
	n := 1
	for n < len(s.queue) && s.queue[n].SenderID == head.SenderID {
		n++
	}
	batch := s.queue[:n]
	s.queue = slices.Clone(s.queue[n:])

	texts := make([]string, len(batch))
	for i, m := range batch {
		texts[i] = m.Text
	}
	content := strings.Join(texts, "\n")
	if s.group {
		content = fmt.Sprintf("[%s] %s", head.SenderName, content)
	}
	// 工作目录或权限变了：把新的环境说明放在这条消息前面，而不是改系统提示词（前缀缓存不失效）
	w := a.workspaceOf(s.sid)
	st := a.store.Settings(s.sid)
	if note := envNote(w, st.RO, st.EnvNote); note != st.EnvNote && note != "" {
		content = note + "\n" + content
		st.EnvNote = note
		if err := a.store.SaveSettings(s.sid, st); err != nil {
			a.log.Warn("保存会话设置失败", "sid", s.sid, "err", err)
		}
	}
	if err := a.store.Append(s.sid, session.Msg{Role: "user", Content: content}); err != nil {
		a.log.Warn("保存消息失败", "sid", s.sid, "err", err)
	}

	a.runSeq++
	r := &run{id: a.runSeq, from: s.ch.Name() + ":" + head.SenderID, replyTo: a.replyTo(s, batch[len(batch)-1])}
	r.stopTyping = a.startTyping(s)
	s.run = r
	mode := a.runMode(w, r.from)
	a.log.Info("开始处理", "sid", s.sid, "from", r.from, "messages", n, "mode", mode)
	in := proto.Msg{T: proto.TInput, SID: s.sid, From: r.from, Mode: mode, Dirs: w.dirs, RO: w.ro, RW: w.rw}
	if !a.hub.Send(in) {
		a.endRun(s, "（内核连接断开，本轮已中止）")
	}
}

// endRun 清理一个 run，可选地发一条提示。调用方持有锁。
func (a *App) endRun(s *sess, note string) {
	r := s.run
	if r == nil {
		return
	}
	if r.cancelLLM != nil {
		r.cancelLLM()
	}
	r.stopTyping()
	s.run = nil
	for n, ap := range a.approvals {
		if ap.sid == s.sid {
			delete(a.approvals, n)
		}
	}
	if note != "" {
		a.say(s, note, r.replyTo)
	}
}

func (a *App) stop(s *sess, reply string) {
	s.queue = nil
	if s.run == nil {
		a.say(s, "当前没有进行中的任务", reply)
		return
	}
	if s.run.cancelLLM != nil {
		s.run.cancelLLM()
		s.run.cancelLLM = nil
	}
	a.hub.Send(proto.Msg{T: proto.TCancel, SID: s.sid}) // 内核会回 done(cancelled)
}

func (a *App) reset(s *sess, reply string) {
	s.queue = nil
	if s.run != nil {
		s.resetAfter = true
		a.stop(s, reply)
		return
	}
	if err := a.store.Reset(s.sid); err != nil {
		a.log.Warn("清空上下文失败", "sid", s.sid, "err", err)
	}
	a.say(s, "上下文已清空", reply)
}

func (a *App) decide(s *sess, who, arg string, ok bool, reply string) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	ap := a.approvals[n]
	if err != nil || ap == nil || ap.sid != s.sid {
		a.say(s, "没有这个编号的待审批请求", reply)
		return
	}
	if !a.isAdmin(who) {
		a.say(s, "只有管理员能审批", reply)
		return
	}
	a.hub.Send(proto.Msg{T: proto.TApprove, SID: ap.sid, ID: ap.id, OK: proto.Bool(ok), From: who})
}

// ---- 内核事件（kernel.Handler） ----

func (a *App) KernelUp(hello *proto.Msg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = a.tools[:0]
	names := make([]string, 0, len(hello.Tools))
	for _, t := range hello.Tools {
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		a.tools = append(a.tools, llm.Tool{Type: "function", Function: llm.ToolFunction{
			Name: t.Name, Description: t.Description, Parameters: params,
		}})
		names = append(names, t.Name)
	}
	a.sandbox = hello.Sandbox != nil && hello.Sandbox.Landlock > 0
	a.log.Info("内核报告的工具", "node", hello.Node, "tools", strings.Join(names, ","), "sandbox", a.sandbox)
	if !a.sandbox {
		a.log.Warn("内核所在机器没有沙箱（Landlock），自动模式下的 Shell 也需要审批")
	}
}

func (a *App) KernelDown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		s.queue = nil
		s.resetAfter = false
		a.endRun(s, "（内核连接断开，本轮已中止）")
	}
}

func (a *App) KernelMsg(m *proto.Msg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[m.SID]
	if s == nil || s.run == nil {
		if m.T == proto.TModel { // 内核以为还有 run，告诉它结束
			a.hub.Send(proto.Msg{T: proto.TTurn, SID: m.SID, Reason: proto.ReasonError, Detail: "网关没有这个会话的进行中任务"})
		}
		a.log.Debug("忽略没有对应 run 的内核消息", "t", m.T, "sid", m.SID)
		return
	}
	switch m.T {
	case proto.TModel:
		a.callModel(s)
	case proto.TResult:
		content := m.Content
		if m.OK != nil && !*m.OK {
			content = "[执行失败] " + content
		}
		if err := a.store.Append(s.sid, session.Msg{Role: "tool", ToolCallID: m.ID, Content: content}); err != nil {
			a.log.Warn("保存工具结果失败", "sid", s.sid, "err", err)
		}
		for n, ap := range a.approvals {
			if ap.sid == s.sid && ap.id == m.ID {
				delete(a.approvals, n)
			}
		}
	case proto.TApproveReq:
		a.nextAppr++
		n := a.nextAppr
		a.approvals[n] = &approval{sid: s.sid, id: m.ID}
		a.say(s, fmt.Sprintf("⚠️ 需要审批 #%d\n%s\n发起人：%s\n管理员回复 /approve %d 批准，/deny %d 拒绝",
			n, describeCall(m.Name, m.Args), m.From, n, n), "")
	case proto.TNotice:
		a.say(s, m.Text, "")
	case proto.TDone:
		a.finish(s, m.Status, m.Detail)
	default:
		a.log.Debug("忽略未知的内核消息", "t", m.T)
	}
}

func (a *App) finish(s *sess, status, detail string) {
	r := s.run
	var note string
	switch status {
	case proto.StatusOK:
		note = r.final
		if strings.TrimSpace(note) == "" {
			note = "（模型没有返回内容）"
		}
		if r.truncated {
			note += "\n\n（回复太长，被截断了）"
		}
	case proto.StatusBudget:
		note = "（这一轮调用工具的次数达到上限，已停止）"
	case proto.StatusTimeout:
		note = "（这一轮运行超时，已停止）"
	case proto.StatusCancelled:
		note = "（已停止）"
	case proto.StatusRefused:
		note = r.final
		if note == "" {
			note = "（模型拒绝了这个请求）"
		}
	default:
		note = "（出错了：" + detail + "）"
	}
	if s.resetAfter {
		s.resetAfter = false
		if err := a.store.Reset(s.sid); err != nil {
			a.log.Warn("清空上下文失败", "sid", s.sid, "err", err)
		}
		note = "上下文已清空"
	}
	a.log.Info("处理结束", "sid", s.sid, "status", status)
	a.endRun(s, note)
	a.startRun(s)
}

// callModel 发起一次模型请求。调用方持有锁；请求本身在协程里跑。
func (a *App) callModel(s *sess) {
	r := s.run
	if r.cancelLLM != nil {
		a.log.Warn("上一次模型请求还没结束，忽略重复的 model", "sid", s.sid)
		return
	}
	system, _ := a.persona.For(s.sid)
	req := llm.Request{
		Messages: session.BuildMessages(system, a.store.History(s.sid), a.cfg.LLM.MaxHistoryTokens, a.cfg.LLM.SendReasoning),
		Tools:    slices.Clone(a.tools),
	}
	ctx, cancel := context.WithCancel(a.ctx)
	r.cancelLLM = cancel

	go func() {
		start := time.Now()
		res, err := a.llm.Chat(ctx, req)
		a.mu.Lock()
		defer a.mu.Unlock()
		stale := s.run != r || ctx.Err() != nil // run 已经结束（停止、超时、断线），结果作废
		cancel()
		if stale {
			return
		}
		r.cancelLLM = nil
		if err != nil {
			a.log.Warn("模型请求失败", "sid", s.sid, "err", err)
			a.hub.Send(proto.Msg{T: proto.TTurn, SID: s.sid, Reason: proto.ReasonError, Detail: err.Error()})
			return
		}
		a.log.Info("模型回复", "sid", s.sid, "reason", res.Reason, "calls", len(res.Calls),
			"prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens,
			"took", time.Since(start).Round(time.Millisecond))
		if err := a.store.Append(s.sid, session.Msg{Role: "assistant", Content: res.Content, ToolCalls: res.Calls, Reasoning: res.Reasoning}); err != nil {
			a.log.Warn("保存模型回复失败", "sid", s.sid, "err", err)
		}
		turn := proto.Msg{T: proto.TTurn, SID: s.sid}
		switch res.Reason {
		case llm.ReasonToolUse:
			turn.Reason = proto.ReasonToolUse
			for _, c := range res.Calls {
				turn.Calls = append(turn.Calls, proto.Call{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)})
			}
		case llm.ReasonRefused:
			turn.Reason = proto.ReasonRefused
			r.final = res.Content
		default:
			turn.Reason = proto.ReasonFinal
			r.final, r.truncated = res.Content, res.Truncated
		}
		a.hub.Send(turn)
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
