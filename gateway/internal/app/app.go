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

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/redact"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/search"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/skills"
)

// Command 是一个聊天命令。/help、网页输入框里打 / 时的提示都用这张表。
type Command struct {
	Name  string `json:"name"` // 不带斜杠
	Args  string `json:"args,omitempty"`
	Desc  string `json:"desc"`
	Admin bool   `json:"admin,omitempty"` // 需要管理员
}

var Commands = []Command{
	{Name: "help", Desc: "列出命令"},
	{Name: "cron", Args: "[run|on|off|rm <编号>]", Desc: "查看和管理定时任务", Admin: true},
	{Name: "whoami", Desc: "查看你的身份、这个窗口和它接着的会话"},
	{Name: "agent", Desc: "查看你在和哪个 agent 说话"},
	{Name: "state", Desc: "看 agent 现在在干嘛：睡着、在聊天、在冲浪、在摸鱼、在发呆……"},
	{Name: "room", Args: "[群名 pause|resume]", Desc: "查看网关里的群；暂停、恢复某个群", Admin: true},
	{Name: "memory", Args: "[关键词]", Desc: "查看、搜索 agent 的长期记忆"},
	{Name: "forget", Args: "<标题或路径>", Desc: "把一条记忆标成过时（不删，还能找回）", Admin: true},
	{Name: "sleep", Desc: "让 agent 现在去整理记忆（睡觉做梦）", Admin: true},
	{Name: "wake", Desc: "叫醒拟人型 agent，让它自己待一会儿（冲浪、想事情、找人聊天）", Admin: true},
	{Name: "session", Args: "[编号 | new 名字]", Desc: "查看会话；切换到别的会话、新建一个会话", Admin: true},
	{Name: "compact", Args: "[特别保留的重点]", Desc: "整理上下文（不动记忆）", Admin: true},
	{Name: "show", Args: "[详细 | 过程 | 简洁 | default]", Desc: "设置这个窗口显示的过程"},
	{Name: "context", Desc: "查看上下文用量、缓存命中和分项"},
	{Name: "history", Args: "[轮数]", Desc: "查看这个会话最近的对话"},
	{Name: "sync", Desc: "补上其他窗口的聊天记录（同一会话内也会自动同步）"},
	{Name: "dir", Args: "[编号 | -编号 | add 机器:路径 | none]", Desc: "查看、挂载、取下工作目录（可以同时挂好几个，在不同的机器上）", Admin: true},
	{Name: "node", Desc: "查看机器和在线的内核"},
	{Name: "status", Desc: "查看网关状态：版本、运行时长、内核、渠道、待重启设置", Admin: true},
	{Name: "restart", Desc: "检查配置后重启网关", Admin: true},
	{Name: "update", Desc: "有新版本就更新网关（起不来自动退回）", Admin: true},
	{Name: "stop", Desc: "停止当前任务（任务进行中直接发「停」也行）"},
	{Name: "undo", Desc: "撤回最近一条消息，已经回复的连同回复从上下文里删掉（直接发「撤回」也行）"},
	{Name: "reset", Desc: "清空本会话的上下文"},
	{Name: "mode", Args: "[full | auto | ask | readonly]", Desc: "查看、切换权限模式：放行、自动、审批、只读", Admin: true},
	{Name: "think", Args: "[强度 | default]", Desc: "查看、切换思考强度", Admin: true},
	{Name: "model", Args: "[编号 | default]", Desc: "查看、切换这个会话用的模型", Admin: true},
	{Name: "persona", Args: "[编号 | 名字 | default]", Desc: "查看、切换这个会话的人格（自带小纳、大肥鱼）", Admin: true},
	{Name: "approve", Args: "<编号>", Desc: "批准一次工具执行", Admin: true},
	{Name: "deny", Args: "<编号>", Desc: "拒绝一次工具执行", Admin: true},
}

var helpText = func() string {
	var b strings.Builder
	b.WriteString("可用命令：")
	for _, c := range Commands[1:] {
		line := "/" + c.Name
		if c.Args != "" {
			line += " " + c.Args
		}
		line += "  " + c.Desc
		if c.Admin {
			line += "（需管理员）"
		}
		b.WriteString("\n" + line)
	}
	return b.String()
}()

type App struct {
	schedules         *schedule.Store
	schedulesErr      error
	schedulerStarted  bool
	searchClient      search.Provider
	searchFingerprint string
	searchUsage       SearchUsage
	ctx               context.Context
	cfg               *config.Config
	llm               llm.Client
	store             *session.Store
	persona           *persona.Loader
	personaAgent      *persona.Loader // agents/<id>/persona.md
	agentStore        *agents.Store   // agent 列表（data/agents.json）
	botStore          *agents.Bots    // 机器人（data/bots.json）
	agentRoot         string          // agents 目录
	roomStore         *rooms.Store    // 网关内部群聊（data/rooms.json）
	roomRuns          map[string]*roomRun
	hub               *kernel.Hub
	mcp               *mcp.Manager // MCP 服务（data/mcp.json）
	mcpStore          *mcp.Store   // MCP 服务的配置（同一个文件的读写）
	log               *slog.Logger
	bus               *events.Bus
	started           time.Time
	lastArchiveClean  time.Time

	version        string
	mode           string
	webURL         string
	confStore      *config.Store
	restarter      func(RestartRequest) error
	updater        Updater
	update         UpdateInfo
	kernelUpdates  map[string]bool // “内核@版本”：这次运行已经自动推过
	pendingRestart string

	// 渠道管理（channels.go），用单独的锁：停止渠道要等它退出，不能拿着 mu 等
	chMu  sync.Mutex
	runs  map[string]*chanRun
	chErr map[string]string
	cli   *channel.CLI
	web   *channel.Web

	now func() time.Time // 测试里可以换成固定时间
	loc *time.Location   // 时间戳用的时区

	mu            sync.Mutex
	groupNoticed  map[string]bool     // 已经提醒过“只支持私聊”的群
	hostWaits     map[string]hostWait // 等内核回复的列目录、代执行请求（kernelhost.go）
	hostSeq       int
	skills        *skills.Registry // 技能（放在网关上）
	managedKernel string           // 网关自己启动的本机内核的名字（localkernel.go）
	models        *models.Store    // 可以用的模型（models.go），nil 时都用 llm
	newClient     func(config.LLM) (llm.Client, error)
	clients       map[string]modelClient
	media         *media.Store               // 用户发来的图片（data/uploads），nil 时不接图片
	chans         map[string]channel.Channel // 按名字找渠道：渠道重启后换成新的对象，会话照常能用
	outboxes      map[string]chan outItem
	eps           map[string]*epRec // 聊天窗口接在哪个会话上（endpoints.go）
	wss           []Workspace       // 工作目录列表（workspaces.go）
	hosts         Hosts             // 网关能直接执行工具的机器：本机、SSH 服务器（nodes.go）
	nodes         map[string]*node  // 在线的内核（nodes.go）
	localNode     string            // 最近一个和网关在同一台机器上的内核
	nodeSeq       int
	totals        Totals
	sessions      map[string]*sess
	mems          map[string]*knowledge.Store // 每个 agent 的知识库（memory.go），"" 是共用的
	agentSkills   map[string]*skills.Registry // 每个 agent 自己的技能目录
	dreams        map[string]*dreamState      // 每个 agent 整理记忆的进度（dream.go）
	bootAt        time.Time                   // 网关启动的时间（算“空闲多久”的起点）
	lifes         map[string]*lifeState       // 拟人型的内心活动状态（life.go，data/life.json）
	rnd           func() float64              // 随机数，测试里换成固定的
	approvals     map[int]*approval
	nextAppr      int
	runSeq        uint64
}

type sess struct {
	scheduled           *schedule.Task
	sid                 string
	compacting          bool
	compactCancel       context.CancelFunc
	lastActivity        time.Time
	idleCompacted       bool
	deleteAfter         bool
	deleteArchivesAfter bool
	queue               []queued
	run                 *run
	resetAfter          bool     // run 结束后清空上下文（/reset 时正好有任务在跑）
	undoAfter           bool     // run 结束后撤回这一轮（/undo 时正好有任务在跑）
	recent              []msgPos // 最近处理过的平台消息在历史里的位置，平台上撤回消息时用

	// 群聊（group.go）：记进历史的最新一条、要不要插话、主动发言的额度
	groupEp        endpoint
	groupLast      time.Time
	groupFresh     bool
	groupProactive bool
	groupTimer     *time.Timer
	groupSpokeAt   time.Time
	groupSpeaks    []time.Time
	groupThoughtAt time.Time   // 上一次判断“要不要说话”的时间
	groupThoughts  []time.Time // 最近一小时判断了几次
	groupSilent    int         // 自己凑上来、又决定不说话的连续次数（退避用）
	groupPending   int         // 上一轮之后进来的新消息条数
}

// queued 是排队等处理的一条消息。trigger 是群聊里“该你说话了”的触发：
// 内容已经在历史里了，不再追加一条用户消息。
type queued struct {
	m       channel.Message
	ep      endpoint
	who     string
	trigger bool
	inner   bool // agent 自己的一轮（整理记忆、内心活动）：不对任何窗口回复、没有环境说明
}

// in 是收到一条命令时的情况：哪个会话、哪个窗口、谁说的
type in struct {
	s    *sess
	ep   endpoint
	who  string
	name string // 说话的人的名字（有的话）
}

// out 是一轮处理要显示到的一个窗口
type out struct {
	ep   endpoint
	view channel.RunView // nil 表示这个窗口不能流式显示，回复在结束时整条发
}

type run struct {
	scheduled  *schedule.Task
	id         uint64
	node       string // 跑这一轮的内核
	from       string // 触发者身份，内核据此判断权限
	outs       []out  // 第一个是说话的窗口，后面是同步显示的窗口和中途来停止、清空的窗口
	final      string
	truncated  bool
	cancelLLM  context.CancelFunc // 非空表示有模型请求在路上
	stopTyping func()
	full       bool // 放行模式：内核要审批的调用由网关自动批准

	streamed   bool // 回复正文已经通过 RunView 显示过了
	start      time.Time
	calls      int       // 这一轮请求了几次模型
	usage      llm.Usage // 这一轮的累计用量
	noTools    bool      // 访客没有工作目录：不给模型工具，只聊天
	readOnly   bool      // 保留给以后可能的只读场景；现在没有工作目录也给全部工具
	elevate    bool      // 这个身份可以提权写工作目录外（管理员）
	proactive  bool      // 群聊里自己凑上来的（上一次是 groupTick 触发的）
	inner      bool      // agent 自己的一轮（整理记忆、内心活动）
	withImages bool      // 这一轮的模型能接收图片
	usedTools  bool      // 这一轮调过工具（拟人型私聊里纯聊天的回复才拆成几条发）
	tool       string    // 最近一次调的工具（/state 说它在干嘛）

	callArgs     map[string]proto.Call         // 这一轮的工具调用（看 Read 读到的是不是图片，fetch.go）
	sent         map[string][]media.Attachment // SendFile 发出去的文件，记在它的工具结果上（网页显示）
	fetching     int                           // 正在取的图片：取完之前不请求模型
	modelWaiting bool                          // 内核要求请求模型的时候图片还没取完

	// 这一轮挂上的工作目录和怎么执行（nodes.go 的 route），交给网关执行的工具按它生成约束（exec.go）
	rt    route
	w     workspace
	write bool
	execs map[string]context.CancelFunc // 网关正在执行的工具调用、正在放的技能文件
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

// knownSecrets 是网关自己知道的密钥，脱敏时精确匹配。
func (a *App) knownSecrets() []string {
	out := a.cfg.Secrets()
	if a.models != nil {
		out = append(out, a.models.Secrets()...)
	}
	if s, ok := a.hosts.(interface{ Secrets() []string }); ok {
		out = append(out, s.Secrets()...)
	}
	return out
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

type outItem struct {
	ch      channel.Channel
	chatID  string
	text    string
	replyTo string
	md      bool              // agent 的回复正文：渠道能渲染 Markdown 就按 Markdown 发
	delay   time.Duration     // 发之前停一会儿（像人一样一条条发）
	att     *media.Attachment // 不是文字而是文件（SendFile）：渠道有 MediaSender 就发文件，没有就发一行说明
	data    []byte
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
		hostWaits:    map[string]hostWait{},
		clients:      map[string]modelClient{},
		skills:       skills.New(cfg.Skills.Dir),
		outboxes:     map[string]chan outItem{},
		sessions:     map[string]*sess{},
		roomRuns:     map[string]*roomRun{},
		approvals:    map[int]*approval{},
		nodes:        map[string]*node{},
	}
	a.migrate()
	a.loadEndpoints()
	a.loadWorkspaces()
	a.loadSchedules()
	a.skills.Scan()
	go a.maintenance()
	go a.scheduleLoop()
	go a.groupLoop()
	go a.agentLoop()
	return a
}

func (a *App) SetHub(h *kernel.Hub) { a.mu.Lock(); a.hub = h; a.mu.Unlock() }

// SetMCP 接上 MCP 服务（data/mcp.json）；nil 表示没有。
func (a *App) SetMCP(store *mcp.Store, m *mcp.Manager) {
	a.mu.Lock()
	a.mcpStore, a.mcp = store, m
	a.mu.Unlock()
}

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
			if it.delay > 0 {
				select {
				case <-time.After(it.delay):
				case <-a.ctx.Done():
					return
				}
			}
			ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
			if it.att != nil {
				if err := sendMedia(ctx, it.ch, it.chatID, *it.att, it.data); err != nil {
					a.log.Warn("发送文件失败", "channel", name, "chat", it.chatID, "file", it.att.Name, "err", err)
				}
				cancel()
				continue
			}
			if err := sendText(ctx, it.ch, it.chatID, it.text, it.replyTo, it.md); err != nil {
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

var commands = func() map[string]bool {
	m := map[string]bool{"start": true} // Telegram 的 /start
	for _, c := range Commands {
		m[c.Name] = true
	}
	return m
}()

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
	if m.Group && cmd == "" && m.Action == "" {
		// 群聊：每条消息都记进历史（不触发模型），被 @、回复机器人、叫到名字才回
		c := in{s: a.sessOf(a.attach(ep, m, who)), ep: ep, who: who, name: m.SenderName}
		st := a.store.Settings(c.s.sid)
		if !st.Group {
			st.Group = true
			if err := a.store.SaveSettings(c.s.sid, st); err != nil {
				a.log.Warn("保存会话设置失败", "sid", c.s.sid, "err", err)
			}
			a.publishSession(c.s.sid)
		}
		a.listenGroup(c.s, ep, m, who)
		if m.Mentioned || groupMentionsName(m.Text, a.agentOf(c.s.sid).Name) {
			a.groupReply(c.s, ep, who)
		}
		return
	}
	switch cmd {
	case "help":
		a.tell(a.boundSession(ep, m), ep, helpText)
		return
	case "room":
		a.cmdRoom(in{s: a.sessOf(a.boundSession(ep, m)), ep: ep, who: who}, arg)
		return
	case "agent":
		a.cmdAgent(in{s: a.sessOf(a.boundSession(ep, m)), ep: ep, who: who})
		return
	case "state":
		a.cmdState(in{s: a.sessOf(a.boundSession(ep, m)), ep: ep, who: who})
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
	c := in{s: a.sessOf(a.attach(ep, m, who)), ep: ep, who: who, name: m.SenderName}
	// 在房间的记录会话里说话：记进群记录、转给群里所有成员，被叫到的会回；这里不跑普通的一轮
	if room, ok := a.roomOfSession(c.s.sid); ok && c.s.sid == roomLogSID(room.ID) {
		name := c.name
		if name == "" {
			name = "有人"
		}
		if text := strings.TrimSpace(m.Text); text != "" {
			_ = a.store.Append(c.s.sid, session.Msg{Role: "user", Content: "[" + name + "] " + text})
			a.publish(events.Event{T: "message", SID: c.s.sid, Name: m.SenderName, Text: text})
		}
		a.roomHumanLine(room.ID)
		a.roomHumanSay(room, name, m.Text)
		return
	}
	if m.Action == channel.ActionStop {
		if c.s.run != nil || len(c.s.queue) > 0 {
			a.stop(c)
		}
		return
	}
	if m.Action == channel.ActionApprove || m.Action == channel.ActionDeny {
		// 审批消息上的按钮：和发 /approve、/deny 一样
		a.decide(c, m.Text, m.Action == channel.ActionApprove)
		return
	}
	if cmd == "" {
		cmd = a.plainCommand(c.s, m.Text)
	}
	switch cmd {
	case "start":
		a.answer(c, helpText)
		return
	case "memory":
		a.cmdMemory(c, arg)
		return
	case "sleep":
		a.cmdSleep(c)
		return
	case "wake":
		a.cmdWake(c)
		return
	case "forget":
		a.cmdForget(c, arg)
		return
	case "session":
		a.cmdSession(c, arg)
		return
	case "cron":
		a.cmdCron(c, arg)
		return
	case "compact":
		a.cmdCompact(c, arg)
		return
	case "show":
		a.cmdShow(c, arg)
		return
	case "context":
		a.answer(c, a.contextText(c.s.sid))
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
	case "status":
		if !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能查看网关状态")
			return
		}
		// OnMessage 持有 a.mu；Status 还要读渠道，放到锁外算完再回话
		go func() {
			text := a.StatusBrief()
			a.mu.Lock()
			a.answer(c, text)
			a.mu.Unlock()
		}()
		return
	case "restart":
		if !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能重启网关")
			return
		}
		req := RestartRequest{By: c.who, SID: c.s.sid, Channel: c.ep.ch, Chat: c.ep.chat, At: time.Now().UnixMilli()}
		if err := a.restartLocked(req); err != nil {
			a.answer(c, "没有重启："+err.Error())
		}
		return
	case "update":
		if !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能更新网关")
			return
		}
		req := RestartRequest{By: c.who, SID: c.s.sid, Channel: c.ep.ch, Chat: c.ep.chat, At: time.Now().UnixMilli()}
		a.answer(c, "检查新版本…")
		// 要联网下载，不能拿着锁做
		go func() {
			text, err := a.RunUpdate(a.ctx, req)
			if err != nil {
				text = "没有更新：" + a.safeToolText(err.Error())
			}
			a.mu.Lock()
			a.answer(c, text)
			a.mu.Unlock()
		}()
		return
	case "stop":
		a.stop(c)
		return
	case "undo":
		a.undo(c)
		return
	case "reset":
		if arg == "删除" && !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能删除历史")
			return
		}
		c.s.deleteAfter = arg == "删除"
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
	case "model":
		a.cmdModel(c, arg)
		return
	case "persona":
		a.cmdPersona(c, arg)
		return
	}
	if m.Text == "" && len(m.Images) == 0 {
		return
	}
	s := c.s
	s.lastActivity = a.now()
	s.idleCompacted = false
	if s.compacting {
		a.deliver(ep, "正在整理上下文…，这条消息已排队", "")
	}
	data := map[string]any{"from": who, "via": sourceOf(ep.ch), "channel": chanName(ep.ch)}
	if len(m.Images) > 0 {
		data["images"] = m.Images
	}
	a.publish(events.Event{T: "message", SID: s.sid, Name: m.SenderName, Text: m.Text, Data: data})
	s.queue = append(s.queue, queued{m: m, ep: ep, who: who})
	if s.run == nil && !s.compacting {
		a.startRun(s)
	}
}

func (a *App) sessOf(sid string) *sess {
	if sid == "" {
		// 空 ID 不是会话。窗口还没接过线时（比如在那里发 /state）以前会在这里建一个 ID 为空的
		// 会话，网页上就多出一个没标题的幽灵会话：给一个不记账的临时会话。
		return &sess{}
	}
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
	a.enqueue(ep, outItem{text: text, replyTo: replyTo})
}

// deliverReply 发 agent 的回复正文：能渲染 Markdown 的渠道按 Markdown 发。调用方持有锁。
func (a *App) deliverReply(ep endpoint, text string) { a.enqueue(ep, outItem{text: text, md: true}) }

func (a *App) enqueue(ep endpoint, it outItem) {
	ch := a.chans[ep.ch]
	if ch == nil {
		a.log.Warn("渠道已经不在了，消息没发出去", "chat", ep.key())
		return
	}
	it.ch, it.chatID = ch, ep.chat
	select {
	case a.outboxes[ep.ch] <- it:
	default:
		a.log.Warn("发送队列已满，丢弃一条消息", "chat", ep.key())
	}
}

// sendMedia 发一个文件：渠道有 MediaSender 就按它的方式发（发不出去时退回一行说明），没有就发一行说明。
func sendMedia(ctx context.Context, ch channel.Channel, chat string, att media.Attachment, data []byte) error {
	if ms, ok := ch.(channel.MediaSender); ok {
		err := ms.SendMedia(ctx, chat, att, data)
		if err == nil {
			return nil
		}
		ch.Send(ctx, chat, fmt.Sprintf("［%s：%s，没发出去：%s］", kindLabel(att.MIME), att.Name, err), "")
		return err
	}
	return ch.Send(ctx, chat, fmt.Sprintf("［%s：%s］", kindLabel(att.MIME), att.Name), "")
}

// sendText 发一条消息；md 时渠道能渲染 Markdown 就按 Markdown 发。
func sendText(ctx context.Context, ch channel.Channel, chat, text, replyTo string, md bool) error {
	if m, ok := ch.(channel.Markdowner); ok && md {
		return m.SendMarkdown(ctx, chat, text, replyTo)
	}
	return ch.Send(ctx, chat, text, replyTo)
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
	if len(s.queue) == 0 || s.run != nil || s.compacting {
		return
	}
	head := s.queue[0]
	// 没有配置模型：不进 run，直接告诉用户去加接口和模型
	if a.models != nil && len(a.models.Models()) == 0 {
		const text = "还没有配置模型。请先在网页的模型节点里添加接口和模型。"
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
	a.refreshSkills()
	w := a.workspaceOf(s.sid)
	rt := a.route(w)
	node := rt.kernel
	if a.isAdmin(head.who) && len(rt.mounts) == 0 {
		if k := a.localKernel(); k != "" {
			node = k // HOME 模式必须在网关本机执行
		}
	}
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
	var images []media.Attachment
	for _, q := range batch {
		images = append(images, q.m.Images...)
	}
	if len(images) > media.MaxPerMessage {
		images = images[:media.MaxPerMessage]
	}
	group := a.isGroupSession(s.sid)
	content := text
	if !head.trigger && config.On(a.cfg.Session.Timestamps) {
		if content == "" {
			content = a.stamp(sourceOf(head.ep.ch))
		} else {
			content = a.stamp(sourceOf(head.ep.ch)) + " " + content
		}
	}
	// 工作目录、权限、机器变了：把新的环境说明放在这条消息前面，而不是改系统提示词（前缀缓存不失效）
	st := a.store.Settings(s.sid)
	dirty := false
	if !st.WorkspacesSet && len(w.wss) > 0 {
		st.Workspaces, st.WorkspacesSet, dirty = w.ids(), true, true // 新会话第一次说话：定下默认挂载的工作目录
	}
	// 群里说话的人按访客算；管理员在群里也只能用群聊白名单里的工具
	admin := a.isAdmin(head.who) && !group
	note := ""
	if !group && !head.inner && !head.trigger {
		note = a.envNote(w, rt, admin)
	}
	// 长期记忆：记忆说明（变了才放）和这一轮想起来的几条，也放在消息前面（memory.go）
	recallText := text
	if head.trigger {
		recallText = a.recentLines(s.sid, 3)
	}
	if head.inner {
		recallText = "" // 自己的一轮：放记忆说明，不“想起来”
	}
	if mem := a.memoryNotes(s.sid, head.who, recallText, &st, &dirty); mem != "" {
		if head.trigger {
			// 群里由触发开的一轮：消息已经在历史里了，记忆单独作为一条放在后面
			if err := a.store.Append(s.sid, session.Msg{Role: "user", Content: mem}); err != nil {
				a.log.Warn("保存记忆说明失败", "sid", s.sid, "err", err)
			}
		} else {
			content = mem + "\n" + content
		}
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
	if s.scheduled == nil && !head.inner {
		a.catchUp(head.ep, s.sid)
	}
	if !head.trigger && !head.inner {
		a.lifeAnswered(s.sid) // 有人在这里说话了：之前的主动消息算回了
	}
	a.remember(s, batch, a.histEnd(s.sid))
	if !head.trigger {
		if err := a.store.Append(s.sid, session.Msg{Role: "user", Content: content, Images: images}); err != nil {
			a.log.Warn("保存消息失败", "sid", s.sid, "err", err)
		}
	}
	a.markSeen(head.ep, s.sid, a.histEnd(s.sid), false)

	a.runSeq++
	r := &run{scheduled: s.scheduled, id: a.runSeq, node: node, from: head.who, start: a.now(), outs: []out{{ep: head.ep}}, execs: map[string]context.CancelFunc{},
		callArgs: map[string]proto.Call{}, sent: map[string][]media.Attachment{},
		rt: rt, w: w, elevate: admin, withImages: a.modelSupportsImages(s.sid)}
	s.run = r
	s.groupPending = 0
	// 拟人型的私聊不流式显示：像人一样打完字再发（纯聊天的回复还会拆成几条）
	if st, ok := a.chans[head.ep.ch].(channel.Streamer); ok && r.scheduled == nil && !group && !a.agentOf(s.sid).Life() {
		r.outs[0].view = a.filterView(head.ep, st.OpenRun(head.ep.chat, ""))
	}
	// 同一个会话的其他窗口里能同步显示的（命令行）：先显示这条消息，再显示回复过程
	via := sourceOf(head.ep.ch)
	if name := head.m.SenderName; name != "" && head.ep.ch != "web" && head.ep.ch != "cli" {
		via += " · " + name
	}
	for key, rec := range a.eps {
		ep := parseEndpoint(key)
		if r.scheduled != nil || rec.Session != s.sid || ep == head.ep {
			continue
		}
		if wt, ok := a.chans[ep.ch].(channel.Watcher); ok {
			a.catchUp(ep, s.sid)
			mirrorText := text
			if mirrorText == "" && len(images) > 0 {
				mirrorText = fmt.Sprintf("（%d 张图片）", len(images))
			}
			r.outs = append(r.outs, out{ep: ep, view: a.filterView(ep, wt.OpenMirror(ep.chat, via, mirrorText))})
		}
	}
	if len(images) > 0 && !r.withImages {
		a.notify(s, "（这个模型不能接收图片：图片已保存在历史里，但没有发给它。需要的话用 /model 换一个支持图片的模型）")
	}
	r.stopTyping = func() {}
	if head.inner {
		r.inner, r.outs = true, nil // 不对任何窗口说话；结果由 innerDone 处理
	} else if r.scheduled == nil {
		r.stopTyping = a.startTyping(head.ep)
		// 没有流式卡片的私聊（拟人型）：在这条消息上显示“正在处理”，处理完撤掉
		last := batch[len(batch)-1].m.MsgID // 连着发了几条：贴在最后一条上
		if b, ok := a.chans[head.ep.ch].(channel.Busy); ok && !group && r.outs[0].view == nil && last != "" {
			stop, done := r.stopTyping, b.Busy(head.ep.chat, last)
			r.stopTyping = func() { stop(); done() }
		}
	} else {
		r.outs = nil
		for _, key := range r.scheduled.Deliver {
			r.outs = append(r.outs, out{ep: parseEndpoint(key)})
		}
	}
	mode := a.runMode(w, r.from)
	if group {
		// 群聊按访客权限跑，工具由群聊白名单决定
		mode = a.cfg.Access.Guest
		if mode == "" {
			mode = proto.ModeNone
		}
	}
	r.proactive = s.groupProactive
	s.groupProactive = false
	// 放行：内核按自动模式跑（沙箱和审批规则都不变），要审批的调用由网关当场批准（approvals.go）
	if mode == proto.ModeFull {
		r.full, mode = true, proto.ModeAuto
	}
	in := proto.Msg{T: proto.TInput, SID: s.sid, From: r.from, Mode: mode}
	// 每条消息的上限都由网关决定（拓扑图里改），内核的 kernel.conf 只在连着旧网关时兜底
	ag := a.limitsOf(s.sid)
	in.MaxSteps = wireLimit(ag.Steps)
	in.RunTimeoutMs = wireMs(ag.Run)
	in.ApprovalTimeoutMs = wireMs(ag.Approval)
	in.MaxOutput = ag.Output
	if r.scheduled != nil {
		limit, _ := schedule.Timeout(*r.scheduled)
		in.RunTimeoutMs = int(limit / time.Millisecond)
		in.ApprovalTimeoutMs = int((30 * time.Minute) / time.Millisecond)
	}
	if r.inner {
		steps, limit := innerLimits(s.sid)
		in.MaxSteps, in.RunTimeoutMs = steps, int(limit/time.Millisecond)
	}
	for _, m := range rt.mounts {
		in.Workspaces = append(in.Workspaces, rt.spec(m, admin))
	}
	r.write = admin && (mode == proto.ModeAuto || mode == proto.ModeAsk)
	in.Elevate = admin
	if len(rt.mounts) == 0 {
		if admin && mode != proto.ModeNone && node != "" && a.nodes[node] != nil && a.nodes[node].info.Local {
			// 没有工作目录：在网关本机的 HOME 下工作。读不限，写只到临时目录；写外面要 elevated。
			r.write = false
			in.Mode = mode
			in.Workspaces = []proto.WorkspaceSpec{homeSpec()}
		} else if admin && mode != proto.ModeNone {
			// 本机内核不在线：没法在网关自己的 HOME 上执行
			r.noTools = true
			in.Mode = proto.ModeNone
		} else {
			// 访客没有工作目录：不给模型工具，只聊天
			r.noTools = true
			in.Mode = proto.ModeNone
		}
	}
	a.log.Info("开始处理", "sid", s.sid, "node", node, "workspaces", len(rt.mounts), "from", r.from, "chat", head.ep.key(), "messages", n, "mode", in.Mode)
	a.totals.Runs++
	shown := in.Mode
	if r.full && in.Mode == proto.ModeAuto {
		shown = proto.ModeFull
	}
	runData := map[string]any{"from": r.from, "mode": shown, "channel": chanName(head.ep.ch), "node": node}
	if len(images) > 0 {
		runData["images"] = images
	}
	a.publish(events.Event{T: "run", SID: s.sid, Kind: "start", Text: content, Data: runData})
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
	s.scheduled = nil
	if r.scheduled != nil {
		why := note
		if why == "" {
			why = "任务运行中断"
		}
		a.completeSchedule(r.scheduled, false, why)
		r.scheduled = nil
		note = ""
	}
	for n, ap := range a.approvals {
		if ap.sid == s.sid {
			a.dropApproval(n, "这一轮已经结束，不用审批了")
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
	if c.s.compacting && c.s.run == nil {
		c.s.compactCancel()
		c.s.compactCancel = nil
		c.s.compacting = false
		c.s.queue = nil
		a.publish(events.Event{T: "context", SID: c.s.sid, Kind: "end"})
		a.answer(c, "已停止整理上下文")
		return
	}
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
	if err := a.resetHistory(s); err != nil {
		a.log.Warn("清空上下文失败", "sid", s.sid, "err", err)
	}
	a.answer(c, "上下文已清空")
}

func (a *App) decide(c in, arg string, ok bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	ap := a.approvals[n]
	if err != nil || ap == nil || !a.canApproveFrom(c, ap) {
		a.answer(c, "没有这个编号的待审批请求")
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能审批")
		return
	}
	if a.hub.Send(a.sessions[ap.sid].run.node, proto.Msg{T: proto.TApprove, SID: ap.sid, ID: ap.id, OK: proto.Bool(ok), From: c.who}) {
		who := c.who
		if c.name != "" && !strings.HasPrefix(who, "web:") && !strings.HasPrefix(who, "cli:") {
			who = c.name
		}
		a.resolveApproval(ap, a.decided(ok, who))
	}
}

// ---- 内核事件（kernel.Handler） ----

func (a *App) KernelMsg(node string, m *proto.Msg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.T == proto.TLsResult || m.T == proto.TRunResult {
		a.hostReply(node, m)
		return
	}
	if m.T == proto.TTools {
		a.toolsChanged(node, m)
		return
	}
	if m.T == proto.TUpdateResult {
		if m.OK != nil && *m.OK {
			a.log.Info("内核已换上新版本，正在重启", "node", node, "version", m.Version)
		} else {
			a.log.Warn("内核更新失败", "node", node, "version", m.Version, "err", m.Detail)
		}
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
		if s.run.fetching > 0 {
			s.run.modelWaiting = true // 等图片取完（seeImage）
			break
		}
		a.callModel(s)
	case proto.TExec:
		a.execTool(node, s, m)
	case proto.TExecCancel:
		if cancel := s.run.execs[m.ID]; cancel != nil {
			cancel()
		}
	case proto.TResult:
		content := m.Content
		if m.OK != nil && !*m.OK {
			content = "[执行失败] " + content
		}
		// Read 读到图片：取过来附上以后再记进历史（fetch.go）
		if !a.seeImage(s, m) {
			if err := a.store.Append(s.sid, session.Msg{Role: "tool", ToolCallID: m.ID, Content: content, Sent: s.run.sent[m.ID]}); err != nil {
				a.log.Warn("保存工具结果失败", "sid", s.sid, "err", err)
			}
		}
		for _, o := range s.run.outs {
			if o.view == nil {
				continue
			}
			detail := m.Content
			if (o.ep.ch == "tg" || o.ep.ch == "feishu") && redact.Needed(a.cfg.Security.Redact, false, true) {
				detail = redact.ToolResult(detail, a.knownSecrets())
			}
			o.view.ToolResult(m.OK == nil || *m.OK, resultDetail(detail))
		}
		for n, ap := range a.approvals {
			if ap.sid == s.sid && ap.id == m.ID {
				why := "⏱ 没等到审批（超时按拒绝处理）"
				if strings.Contains(m.Content, "拒绝") {
					why = "❌ 已拒绝"
				}
				a.dropApproval(n, why)
			}
		}
		a.publish(events.Event{T: "tool_result", SID: s.sid, ID: m.ID, OK: events.Bool(m.OK == nil || *m.OK),
			Text: resultDetail(m.Content), Data: clip(m.Content, 4000)})
	case proto.TApproveReq:
		if s.run.full {
			a.autoApprove(s, m)
		} else {
			a.askApproval(s, m)
		}
	case proto.TNotice:
		if s.run.full && strings.HasPrefix(m.Text, "已批准") {
			break // 自动批准的：网页上已经记过了，不往聊天里发
		}
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
		note = fmt.Sprintf("（这条消息已经请求了 %d 次模型，到了工具调用轮数上限，先停在这里。发「继续」接着做；上限在网页拓扑图的 Agent 节点里改，0 表示不限）", r.calls)
	case proto.StatusTimeout:
		note = "（这条消息处理的时间到了上限，先停在这里。发「继续」接着做；时限在网页拓扑图的 Agent 节点里改，0 表示不限）"
		if r.scheduled != nil {
			note = "（任务运行超时，已停止；时限在定时任务里改）"
		}
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
	if r.scheduled != nil {
		a.completeSchedule(r.scheduled, status == proto.StatusOK, strings.TrimSpace(body+"\n"+note))
		r.scheduled = nil
		body, note = "", ""
	}
	if r.inner {
		a.innerDone(s, r, status, strings.TrimSpace(body+"\n"+note))
		body, note = "", ""
	}
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
	group := a.isGroupSession(s.sid)
	roomOf, inRoom := a.roomOfSession(s.sid)
	if group && r.proactive {
		// 自己凑上来又不想说：记着，连着几次就越等越久
		if isSilence(body) {
			s.groupSilent++
		} else {
			s.groupSilent = 0
		}
	}
	// 拟人型的私聊：纯聊天的回复像人一样分几条发；只回 [不回复] 就不接
	lifeDM := !group && !inRoom && a.agentOf(s.sid).Life()
	for _, o := range r.outs {
		text := note
		if body != "" && (o.view == nil || !r.streamed) {
			text = strings.TrimSpace(body + "\n\n" + note)
		}
		if text == "" {
			continue
		}
		if group && o.ep.ch == "room" {
			continue // 房间里的发言由 roomAgentSay 记进群记录
		}
		if group {
			a.sendGroup(s, o.ep, text)
			continue
		}
		if lifeDM && o.view == nil && o.ep.ch != "web" {
			if isSilence(body) {
				if note != "" {
					a.deliver(o.ep, note, "")
				}
				continue
			}
			if !r.usedTools && !strings.Contains(body, "```") && status == proto.StatusOK {
				parts := dmParts(body, 3)
				if note != "" {
					parts = append(parts, note)
				}
				a.sendParts(o.ep, parts)
				continue
			}
		}
		if body != "" && (o.view == nil || !r.streamed) {
			a.deliverReply(o.ep, text)
			continue
		}
		a.deliver(o.ep, text, "")
	}
	// agent 在房间里的回复：记进群记录，转给其他成员；说出口的也算进每小时的额度；顺手记用量
	if inRoom {
		if roomLogSID(roomOf.ID) != s.sid && !isSilence(body) {
			s.groupSpokeAt = a.now()
			s.groupSpeaks = append(s.groupSpeaks, s.groupSpokeAt)
			a.roomAgentSay(roomOf, roomSender(s.sid), body)
		}
		a.roomUsage(roomOf, r.usage.PromptTokens, r.usage.CompletionTokens)
	}
	s.lastActivity = a.now()
	a.startRun(s)
	if s.run == nil && !isCronSID(s.sid) {
		a.maybeCompact(s, false)
	}
}

// callModel 发起一次模型请求。调用方持有锁；请求本身在协程里跑。
func (a *App) callModel(s *sess) {
	r := s.run
	if r.cancelLLM != nil {
		a.log.Warn("上一次模型请求还没结束，忽略重复的 model", "sid", s.sid)
		return
	}
	high, low, keep := a.compactLimits(s.sid)
	h := a.store.EffectiveHistory(s.sid)
	if a.contextInfo(s.sid).Total > high && session.PlanCut(h, low, keep) > 0 {
		a.compact(s, high, low)
		return
	}
	a.generateModel(s)
}

// generateModel 用准备好的上下文请求本轮回复，调用方持有锁。
func (a *App) generateModel(s *sess) {
	r := s.run
	system, snapshotTools, _, _ := a.contextParts(s.sid)
	st := a.store.Settings(s.sid)
	// 群聊和房间不套用快照（见 contextParts），也就不写；其余会话权限一变就重写一次
	perm := a.permSignature(s.sid)
	if !a.isGroupSession(s.sid) && (st.Prefix == nil || st.Prefix.System != system || st.Prefix.Ver != prefixVer || st.Prefix.Perm != perm) {
		st.Prefix = &session.Prefix{System: system, Tools: snapshotTools, Ver: prefixVer, Perm: perm}
		if err := a.store.SaveSettings(s.sid, st); err != nil {
			a.log.Warn("保存提示快照失败", "err", err)
		}
	}
	var images session.ImageLoader
	if r.withImages {
		images = a.imageLoader()
	}
	req := llm.Request{
		Messages: session.BuildMessages(system, st.Summary, a.store.EffectiveHistory(s.sid), a.cfg.LLM.SendReasoning, images),
		Thinking: a.thinkingOf(s.sid),
	}
	if r.proactive {
		// 自己凑上来的那一轮：只在这次请求里加一句提醒，不改历史（前缀缓存基本不受影响）
		req.Messages = append(req.Messages, llm.Message{Role: "user",
			Content: "[系统] 群里安静了一会儿。想接一句就回一两句；不想说就只回 [不回复]。"})
	}
	if a.cfg.Security.Redact != "off" {
		_, e, ok := a.modelOf(s.sid)
		public := ok && e.Kind != "mock" && redact.IsPublicBaseURL(e.BaseURL)
		if redact.Needed(a.cfg.Security.Redact, public, false) {
			known := a.knownSecrets()
			for i := range req.Messages {
				if req.Messages[i].Role == "tool" {
					req.Messages[i].Content = redact.ToolResult(req.Messages[i].Content, known)
				}
			}
		}
	}
	req.Tools = snapshotTools
	_, _, meterSignature, _ := a.contextParts(s.sid)
	meterPosition := st.Base + len(a.store.History(s.sid))
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
	client, cerr := a.clientFor(s.sid)

	go func() {
		start := time.Now()
		var res *llm.Result
		err := cerr
		if err == nil {
			res, err = client.Chat(ctx, req, onDelta)
		}
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
		if u.PromptTokens > 0 {
			st := a.store.Settings(s.sid)
			st.Meter = session.Meter{Position: meterPosition, Usage: u, Signature: meterSignature}
			if err := a.store.SaveSettings(s.sid, st); err != nil {
				a.log.Warn("保存上下文计量失败", "err", err)
			}
		}
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
			r.usedTools = true
			a.sayMidway(s, r, res.Content)
			for _, c := range res.Calls {
				r.tool = c.Function.Name
				r.callArgs[c.ID] = proto.Call{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)}
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

// sayMidway 发调工具之前说的话（“等等，我看一眼”）。流式显示的窗口已经看到了；别的窗口（拟人型的私聊、
// QQ、微信）只在这一轮结束时收到最后一段回复，前面说的要在说的时候就发出去，不然就丢了。
// 定时任务、内心活动不发（结果另有去处）。调用方持有锁。
func (a *App) sayMidway(s *sess, r *run, text string) {
	text = strings.TrimSpace(text)
	if text == "" || isSilence(text) || r.scheduled != nil || r.inner {
		return
	}
	group := a.isGroupSession(s.sid)
	life := a.agentOf(s.sid).Life()
	for _, o := range r.outs {
		if o.view != nil || o.ep.ch == "web" || o.ep.ch == "room" {
			continue // 网页从事件里看到；网关里的群只记最后的发言
		}
		switch {
		case group:
			a.sendGroup(s, o.ep, text)
		case life && !strings.Contains(text, "```"):
			a.sendParts(o.ep, dmParts(text, 3))
		default:
			a.deliverReply(o.ep, text)
		}
	}
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

// wireLimit 把“0 表示不限”的设置换成发给内核的值：内核那边 0 表示用它自己的配置，小于 0 才是不限。
func wireLimit(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

func wireMs(d time.Duration) int {
	if d <= 0 {
		return -1
	}
	return int(d / time.Millisecond)
}
