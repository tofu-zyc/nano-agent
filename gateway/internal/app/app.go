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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/experience"
	"nano-agent/gateway/internal/hooks"
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
	{Name: "hooks", Desc: "查看可靠事件规则", Admin: true},
	{Name: "hook", Args: "<编号> [get|runs|enable|disable|delete] | create <JSON>", Desc: "管理事件规则与投递", Admin: true},
	{Name: "jobs", Desc: "查看已登记的长时间和后台任务"},
	{Name: "bg", Args: "[job_id]", Desc: "把当前前台命令原地转后台"},
	{Name: "job", Args: "<job_id> [status|output [游标]|wait [秒]|cancel|detach]", Desc: "查看日志、短等待或停止一个任务"},
	{Name: "cron", Args: "[run|on|off|rm <编号>]", Desc: "查看和管理定时任务", Admin: true},
	{Name: "whoami", Desc: "查看你的身份、这个窗口和它接着的会话"},
	{Name: "agent", Desc: "查看你在和哪个 agent 说话"},
	{Name: "state", Desc: "看 agent 现在在干嘛：睡着、在聊天、在冲浪、在摸鱼、在发呆……"},
	{Name: "autonomy", Args: "[pause | resume | wake | profile | set <编号> <字段>=<值> | preset <名字> | weight <字段>=<值> | dimension <名字> <字段>=<值> | lock <字段>=<值> | unlock <字段>]",
		Desc: "查看自主活动：今天的探索/约定/Hook/手动、候选权重和心智维度；可以暂停、恢复、叫醒、改数值和预设、锁定数值"},
	{Name: "room", Args: "[群名 pause|resume]", Desc: "查看网关里的群；暂停、恢复某个群", Admin: true},
	{Name: "memory", Args: "[关键词]", Desc: "查看、搜索 agent 的长期记忆"},
	{Name: "forget", Args: "<标题或路径>", Desc: "把一条记忆标成过时（不删，还能找回）", Admin: true},
	{Name: "sleep", Desc: "让 agent 现在去整理记忆（睡觉做梦）", Admin: true},
	{Name: "wake", Desc: "叫醒拟人型 agent，让它自己待一会儿（冲浪、想事情、找人聊天）", Admin: true},
	{Name: "session", Args: "[编号 | new 名字]", Desc: "查看会话；切换到别的会话、新建一个会话", Admin: true},
	{Name: "branch", Args: "[report <分支编号>]", Desc: "查看这个会话的安静分支；把某个分支的结论带回这个会话"},
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
	{Name: "stop", Args: "[all | job_id]", Desc: "停止当前任务（任务进行中直接发「停」也行）"},
	{Name: "undo", Desc: "撤回最近一条消息，已经回复的连同回复从上下文里删掉（直接发「撤回」也行）"},
	{Name: "reset", Desc: "清空本会话的上下文"},
	{Name: "sandbox", Args: "[global|agent|workspace ID] [auto|on|off|default]", Desc: "查看、设置执行隔离；审批和文件权限独立", Admin: true},
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
	hookEngine      *hooks.Engine
	watchDigest     map[string]string // 每个内核最近一次下发的观察集合
	watchBroken     map[string]bool   // 断开过的内核：重新下发时那段时间按缺口上报
	hookCauses      []hookCause       // 正在执行的 Hook 动作：观察事件据此记因果，防自写循环
	watchSeq        map[string]uint64 // watch 来源的事件序号：按内核启动身份分开记，不然会互相挤出新序号
	messageSeq      uint64            // message 来源的事件序号
	runtimeEventSeq uint64
	jobControls     map[string]jobControlWait
	jobCompletions  map[string]jobCompletionWait
	jobEvents       *eventlog.Store
	jobEventsErr    error
	jobPersist      map[string]*jobPersistence
	runtimeLimits   config.Runtime
	runWaitSerial   uint64
	dispatcher      *dispatch.Engine
	dispatchErr     error
	dispatchBoot    string
	dispatchWait    map[string]func(dispatch.Change)

	forkWG            sync.WaitGroup
	forkWork          map[string]chan struct{}
	forkSlots         chan struct{}
	experiences       map[string]*experience.Store
	mindsErr          error
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
	hubRef            atomic.Pointer[kernel.Hub] // 同一个 hub，不用拿总锁就能读（MCP 管道收发用）
	mcp               *mcp.Manager               // MCP 服务（data/mcp.json）
	mcpStore          *mcp.Store                 // MCP 服务的配置（同一个文件的读写）
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

	mu             sync.Mutex
	groupNoticed   map[string]bool     // 已经提醒过“只支持私聊”的群
	hostWaits      map[string]hostWait // 等内核回复的列目录、代执行请求（kernelhost.go）
	hostSeq        int
	skills         *skills.Registry // 技能（放在网关上）
	managedKernel  string           // 网关自己启动的本机内核的名字（localkernel.go）
	models         *models.Store    // 可以用的模型（models.go），nil 时都用 llm
	newClient      func(config.LLM) (llm.Client, error)
	clients        map[string]modelClient
	media          *media.Store               // 用户发来的图片（data/uploads），nil 时不接图片
	chans          map[string]channel.Channel // 按名字找渠道：渠道重启后换成新的对象，会话照常能用
	jobHints       map[string]*jobHint
	jobCards       map[string]*jobPresentation
	jobButtons     map[string]jobButtonAction
	jobStopGroups  map[string]bool
	outboxes       map[string]chan outItem
	eps            map[string]*epRec // 聊天窗口接在哪个会话上（endpoints.go）
	wss            []Workspace       // 工作目录列表（workspaces.go）
	hosts          Hosts             // 网关能直接执行工具的机器：本机、SSH 服务器（nodes.go）
	nodes          map[string]*node  // 在线的内核（nodes.go）
	localNode      string            // 最近一个和网关在同一台机器上的内核
	nodeSeq        int
	totals         Totals
	sessions       map[string]*sess
	mems           map[string]*knowledge.Store // 每个 agent 的知识库（memory.go），"" 是共用的
	agentSkills    map[string]*skills.Registry // 每个 agent 自己的技能目录
	dreams         map[string]*dreamState      // 每个 agent 整理记忆的进度（dream.go）
	bootAt         time.Time                   // 网关启动的时间（算“空闲多久”的起点）
	lifes          map[string]*lifeState       // 拟人型的内心活动状态（life.go，data/life.json）
	minds          map[string]*autonomy.State  // 自主活动的心智状态（autonomy.go，data/autonomy.json）
	mindsLoaded    bool
	mindMachines   map[string]bool            // 已通知心智状态的设备在线状态
	mindsRecovered map[string]bool            // 这次进程启动已经做过重启恢复的 agent（不能持久化：每次重启都要检查）
	desktopGates   map[string]chan struct{}   // 同一台桌面上的桌面工具调用串行（computer use）
	desktopUsed    map[string]map[string]bool // 哪些会话用过哪个桌面 MCP 服务（结束时清理 Driver session）
	desktopLeases  map[string]*desktopLease   // 一轮任务从观察到操作、验证期间独占同一桌面
	deskMu         sync.Mutex                 // 上面两张表单独的锁：结束 Driver session 时可能正拿着总锁
	mcpRef         atomic.Pointer[mcp.Manager]
	mcpStoreRef    atomic.Pointer[mcp.Store]
	rnd            func() float64                  // 随机数，测试里换成固定的
	journals       map[string]*interjectionJournal // 插话日志句柄（interject_journal.go）
	pendingForks   map[string][]forkPending        // 等安全边界的 Fork 操作（session_fork.go）
	approvals      map[int]*approval
	nextAppr       int
	runSeq         uint64
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
	runQueueSerial      uint64
	runQueueNode        string
	runQueuedAt         time.Time
	queue               []queued
	run                 *run
	resetAfter          bool           // run 结束后清空上下文（/reset 时正好有任务在跑）
	undoAfter           bool           // run 结束后撤回这一轮（/undo 时正好有任务在跑）
	recent              []msgPos       // 最近处理过的平台消息在历史里的位置，平台上撤回消息时用
	interjectSeq        int64          // 插话接收序号（单调递增，落进日志）
	restored            bool           // 已经从日志恢复过插话
	pending             []interjection // 从日志恢复的补充：等这一轮开始时装进收件箱
	pendingBytes        int
	interrupted         []interjection // 上次中断、还没有结局的补充（已进历史，等用户继续）

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

	// mind 是这一轮如果属于自主活动：结束时把真实结果写回哪个议程 / 意图（autonomy.go）
	mind *mindRun
}

// queued 是排队等处理的一条消息。trigger 是群聊里“该你说话了”的触发：
// 内容已经在历史里了，不再追加一条用户消息。
type queued struct {
	hook    *hookInvocation
	m       channel.Message
	ep      endpoint
	who     string
	trigger bool
	ticket  uint64 // immutable queue admission order; cleared together with this entry
	inner   bool   // agent 自己的一轮（整理记忆、内心活动）：不对任何窗口回复、没有环境说明
	report  *session.Report // 宿主生成的报告卡片（分支报告）：随这条消息一起进历史
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
	hook                 *hookInvocation
	experienceActions    []string
	experienceActionTurn int
	experienceActionKeys map[string]bool
	experiencePrivate    bool
	asked                string // 原始输入；经历索引不读取附加的系统/记忆说明
	scheduled            *schedule.Task
	id                   uint64
	node                 string // 跑这一轮的内核
	from                 string // 触发者身份，内核据此判断权限
	outs                 []out  // 第一个是说话的窗口，后面是同步显示的窗口和中途来停止、清空的窗口
	final                string
	truncated            bool
	cancelLLM            context.CancelFunc // 非空表示有模型请求在路上
	stopTyping           func()
	full                 bool // 放行模式：内核要审批的调用由网关自动批准

	streamed        bool // 回复正文已经通过 RunView 显示过了
	start           time.Time
	calls           int                              // 这一轮请求了几次模型
	usage           llm.Usage                        // 这一轮的累计用量
	noTools         bool                             // 访客没有工作目录：不给模型工具，只聊天
	mode            string                           // 这一轮交给内核的权限模式（放行模式这里是 auto，另有 full 标记）
	mcpOK           map[string]bool                  // 这一轮已经批准过的 MCP 服务（approval=per_turn）
	readOnly        bool                             // 保留给以后可能的只读场景；现在没有工作目录也给全部工具
	elevate         bool                             // 这个身份可以提权写工作目录外（管理员）
	proactive       bool                             // 群聊里自己凑上来的（上一次是 groupTick 触发的）
	inner           bool                             // agent 自己的一轮（整理记忆、内心活动）
	withImages      bool                             // 这一轮的模型能接收图片
	usedTools       bool                             // 这一轮调过工具（拟人型私聊里纯聊天的回复才拆成几条发）
	tool            string                           // 最近一次调的工具（/state 说它在干嘛）
	toolEvidence    map[string]autonomy.ToolEvidence // 内核实际回报的结果，任务验收引用它
	unverifiedTools map[string]bool                  // MCP 正常返回但明确说效果未核验/未生效
	agendaDone      map[string]agendaCompletion      // 本轮申请完成的日程，正常结束并核验结果后提交

	callArgs     map[string]proto.Call         // 这一轮的工具调用（看 Read 读到的是不是图片，fetch.go）
	sent         map[string][]media.Attachment // SendFile 发出去的文件，记在它的工具结果上（网页显示）
	images       map[string][]media.Attachment // 工具返回的图片（MCP）：要交给模型看的那份
	fetching     int                           // 正在取的图片：取完之前不请求模型
	modelWaiting bool                          // 内核要求请求模型的时候图片还没取完

	// 这一轮挂上的工作目录和怎么执行（nodes.go 的 route），交给网关执行的工具按它生成约束（exec.go）
	rt    route
	w     workspace
	write bool
	execs map[string]context.CancelFunc // 网关正在执行的工具调用、正在放的技能文件

	// 完整调用身份（协议 v11）：run_id 由网关生成，turn_id 是内核告诉我们的这一次模型轮次
	runID       string
	turnID      int
	modelReq    int
	sandboxMode string // 这一轮请求的沙箱模式（auto / on / off），远程执行时要转给那台机器
	// 用户插话（interject.go / interject_journal.go）：本 run 进行中收到的补充，在安全边界插入
	inbox        []interjection
	inboxBytes   int
	interjectSeq int
	notified     bool // 这一批补充已经回执过“已收到”
	injected     int  // 已经插入历史的条数
	readRevision int  // 最近一次模型请求实际读到的条数（injected 的快照）
	// 持久日志用：还没进过模型请求快照的、这一轮处理过的插话 ID
	pendingInclude []string
	journalIDs     []string
	outcome        string          // 这一轮的终态（结束前由 finish 填）
	dropped        int             // 因为插话而没有派发的工具调用数
	seenIDs        map[string]bool // 最近受理过的平台消息 ID（去重）
	dropInbox      bool            // 用户明确停止/清空：剩下的补充不再交给下一轮
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
	if a.mcpStore != nil {
		out = append(out, a.mcpStore.Secrets()...) // MCP 服务 env 里的密钥
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
	job     func(context.Context) // bounded task-card I/O on the existing channel sender
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
		chans:          map[string]channel.Channel{},
		groupNoticed:   map[string]bool{},
		hostWaits:      map[string]hostWait{},
		clients:        map[string]modelClient{},
		skills:         skills.New(cfg.Skills.Dir),
		outboxes:       map[string]chan outItem{},
		sessions:       map[string]*sess{},
		roomRuns:       map[string]*roomRun{},
		approvals:      map[int]*approval{},
		pendingForks:   map[string][]forkPending{},
		nodes:          map[string]*node{},
		minds:          map[string]*autonomy.State{},
		mindsRecovered: map[string]bool{},
		desktopGates:   map[string]chan struct{}{},
		desktopUsed:    map[string]map[string]bool{},
	}
	for _, recovery := range []struct {
		name string
		run  func() error
	}{
		{"renames", store.ResumeRenames}, {"promotions", store.ResumePromotions},
		{"migration", func() error { _, err := store.Migrate(); return err }},
	} {
		if err := recovery.run(); err != nil {
			store.RecordStorageIssue(recovery.name, err)
			a.log.Error("会话存储恢复未完成，保留现场", "stage", recovery.name, "err", err)
		}
	}
	a.runtimeLimits = cfg.Runtime.Defaults()
	a.dispatchBoot = randHex(16)
	a.dispatchWait = map[string]func(dispatch.Change){}
	a.dispatcher, a.dispatchErr = dispatch.Open(ctx, filepath.Join(store.Dir(), "runtime", "leases.json"), a.runtimeLimits.Dispatch(), a.leaseChanged)
	if a.dispatchErr != nil {
		store.RecordStorageIssue("execution_leases", a.dispatchErr)
		a.log.Error("执行租约恢复失败，保留现场", "err", a.dispatchErr)
	}
	a.jobHints = map[string]*jobHint{}
	a.jobCards = map[string]*jobPresentation{}
	a.jobButtons = map[string]jobButtonAction{}
	a.jobStopGroups = map[string]bool{}
	a.jobPersist = map[string]*jobPersistence{}
	a.jobControls = map[string]jobControlWait{}
	a.jobCompletions = map[string]jobCompletionWait{}
	a.jobEvents, a.jobEventsErr = eventlog.Open(ctx, filepath.Join(store.Dir(), "runtime", "events.jsonl"), eventlog.Limits{})
	if a.jobEventsErr != nil {
		store.RecordStorageIssue("runtime_events", a.jobEventsErr)
		a.log.Error("可靠事件恢复失败，保留现场", "err", a.jobEventsErr)
	}
	if a.persona != nil {
		a.persona.BindSessions(store)
		if err := store.MigrateAuxiliary("persona", a.persona.Dir(), ".md"); err != nil {
			store.RecordStorageIssue("persona", err)
			a.log.Error("会话人格迁移未完成，保留现场", "err", err)
		}
	}
	if err := store.MigrateAuxiliary("interjections", a.journalDir(), ".jsonl"); err != nil {
		store.RecordStorageIssue("interjections", err)
		a.log.Error("插话日志迁移未完成，保留现场", "err", err)
	}
	a.migrate()
	a.loadEndpoints()
	a.recoverForksLocked()
	a.loadWorkspaces()
	a.loadSchedules()
	a.skills.Scan()
	go a.maintenance()
	go a.scheduleLoop()
	go a.groupLoop()
	go a.agentLoop()
	return a
}

func (a *App) SetHub(h *kernel.Hub) {
	a.mu.Lock()
	a.hub = h
	if a.hookEngine == nil && a.jobEvents != nil {
		a.hookEngine = hooks.New(a.ctx, a.jobEvents, hookExecutor{a}, func() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.now() })
	}
	a.mu.Unlock()
	a.hubRef.Store(h)
}

// SetMCP 接上 MCP 服务（data/mcp.json）；nil 表示没有。
func (a *App) SetMCP(store *mcp.Store, m *mcp.Manager) {
	a.mu.Lock()
	a.mcpStore, a.mcp = store, m
	a.mu.Unlock()
	a.mcpRef.Store(m)
	a.mcpStoreRef.Store(store)
	if m != nil {
		m.OnChange(func() { a.publish(events.Event{T: "mcp"}) }) // 网页刷新在线状态
		m.Warm(20 * time.Second)                                 // 启动时后台连一遍，工具定义那边只读缓存
	}
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
		for {
			var it outItem
			select {
			case <-a.ctx.Done():
				return
			case it = <-q:
			}
			if it.delay > 0 {
				select {
				case <-time.After(it.delay):
				case <-a.ctx.Done():
					return
				}
			}
			ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
			if it.job != nil {
				jobCtx, jobCancel := context.WithTimeout(ctx, 15*time.Second)
				it.job(jobCtx)
				jobCancel()
				cancel()
				continue
			}
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
	case "autonomy":
		a.cmdAutonomy(in{s: a.sessOf(a.boundSession(ep, m)), ep: ep, who: who}, arg)
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
	if m.Action == channel.ActionJob {
		a.jobButton(ch, ep, m, who)
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
	case "branch":
		a.cmdBranch(c, arg)
		return
	case "session":
		if strings.HasPrefix(strings.TrimSpace(arg), "fork") {
			a.cmdSessionFork(c, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), "fork")))
			return
		}
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
	case "hooks", "hook":
		a.cmdHook(c, cmd, arg, m.MsgID)
		return
	case "jobs", "bg", "job":
		a.cmdJobs(c, cmd, arg)
		return
	case "stop":
		if arg == "all" {
			a.cmdStopAllJobs(c)
			return
		}
		if strings.HasPrefix(arg, "j_") {
			a.cmdJobs(c, "job", arg+" cancel")
			return
		}
		a.stop(c)
		return
	case "undo":
		a.undo(c)
		return
	case "sessionfork":
		a.cmdSessionFork(c, arg)
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
	case "sandbox":
		a.cmdSandbox(c, arg)
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
	heard := m.Text
	if heard == "" {
		heard = fmt.Sprintf("（发来 %d 张图片）", len(m.Images))
	}
	// 心跳状态：拟人型的每次聊天都记一条事件（同一个会话合并），不每条都唤醒模型
	a.mindHeard(c.s.sid, who, m.SenderName, heard)
	s := c.s
	if br, ok := a.store.Branch(s.sid); ok && (br.Status == session.BranchPreparing || br.Status == session.BranchPendingBoundary || br.SnapshotID == "") {
		a.answer(c, "分支还没有就绪，请先查看分支状态")
		return
	}
	if isLifeSID(s.sid) && a.agentOf(s.sid).Life() && s.run == nil && !s.compacting {
		ag := a.agentOf(s.sid)
		if err := a.startMindLocked(ag, a.mind(ag.ID), autonomy.Trigger{Kind: autonomy.EventManualWake, Reason: "主人留言：" + m.Text, Due: a.now().Unix()}, true); err != nil {
			a.answer(c, "留言未能开始："+err.Error())
		}
		return
	}
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
	// 校验通过的入站消息先写成可靠事件，Hook 只在这之后才可能看到它
	a.recordMessage(s.sid, ep, m, who, heard)
	// 当前这一轮还在跑：发起人在这个会话里继续发来的普通消息是插话，进本轮的有序收件箱，
	// 等这一批工具调用或这次生成结束再插入——不取消正在跑的命令，也不打断正在生成的回复。
	if s.run != nil && (s.run.canInterject(who) || (isLifeSID(s.sid) && a.isAdmin(who))) {
		if a.acceptInterjection(s, s.run, ep, m, who) {
			return
		}
	}
	if !a.queueFits(s, m) {
		a.deliver(ep, "等待处理的消息已达上限，这条尚未接收。请等当前任务结束后再发。", "")
		return
	}
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
		// 第一次访问这个会话：把上次没处理完的插话从日志里恢复出来
		a.restoreInterjections(s)
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

func (a *App) enqueue(ep endpoint, it outItem) bool {
	ch := a.chans[ep.ch]
	if ch == nil {
		a.log.Warn("渠道已经不在了，消息没发出去", "chat", ep.key())
		return false
	}
	it.ch, it.chatID = ch, ep.chat
	select {
	case a.outboxes[ep.ch] <- it:
		return true
	default:
		a.log.Warn("发送队列已满，丢弃一条消息", "chat", ep.key())
		return false
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
	if s.queue[0].ticket == 0 {
		a.runWaitSerial++
		s.queue[0].ticket = a.runWaitSerial
		s.runQueuedAt = time.Now()
	}
	s.runQueueSerial = s.queue[0].ticket
	head := s.queue[0]
	if err := a.store.Check(s.sid); err != nil {
		a.publish(events.Event{T: "say", SID: s.sid, Text: err.Error()})
		a.deliver(head.ep, err.Error(), "")
		s.queue = nil
		return
	}
	// 排队期间（例如压缩时）收到的内心留言也必须开启独立片段，
	// 不能从 startRun 的普通聊天入口接回旧上下文。
	if isLifeSID(s.sid) && !head.inner && a.agentOf(s.sid).Life() {
		var notes []string
		for _, q := range s.queue {
			notes = append(notes, q.m.Text)
		}
		s.queue = nil
		ag := a.agentOf(s.sid)
		if err := a.startMindLocked(ag, a.mind(ag.ID), autonomy.Trigger{Kind: autonomy.EventManualWake, Reason: "留言：" + strings.Join(notes, "\n"), Due: a.now().Unix()}, true); err != nil {
			a.log.Warn("内心留言未能开始", "err", err)
			a.deliver(head.ep, "留言未能开始："+err.Error(), "")
		}
		return
	}
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
	s.runQueueNode = node
	if !a.runCapacity(s, node) {
		return
	}
	if n := a.nodes[node]; n != nil && n.info.Features["machine_leases"] > 0 && !n.info.LeasesReady {
		return
	}
	sandboxMode := a.sandboxModeFor(s.sid)
	a.freezeSandboxRoute(&rt, sandboxMode)
	if err := sandboxRouteError(sandboxMode, rt); err != nil {
		a.publish(events.Event{T: "say", SID: s.sid, Text: err.Error()})
		a.deliver(head.ep, err.Error(), "")
		s.queue = nil
		return
	}
	if n := a.nodes[node]; n != nil {
		unsupported := sandboxMode != "auto" && n.info.Features["sandbox_policy"] < 1
		for _, mt := range rt.mounts {
			unsupported = unsupported || (mt.sandboxMode != sandboxMode && n.info.Features["sandbox_policy"] < 2)
		}
		if unsupported {
			text := "这个内核尚不支持所选执行隔离策略，请更新内核后重试。"
			a.publish(events.Event{T: "say", SID: s.sid, Text: text})
			a.deliver(head.ep, text, "")
			s.queue = nil
			return
		}
	}
	if node != "" && a.hasBeforeHooks(s.sid) && a.nodes[node] != nil && a.nodes[node].info.Features["hook_check"] < 1 {
		text := "这个会话已启用前置 Hook，但内核不支持检查屏障；请更新内核。未执行。"
		a.publish(events.Event{T: "say", SID: s.sid, Text: text})
		a.deliver(head.ep, text, "")
		s.queue = nil
		return
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
	s.runQueueSerial = 0
	s.runQueueNode = ""

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
	if !st.WorkspacesSet && len(w.wss) > 0 && !(isLifeSID(s.sid) && s.mind != nil && s.mind.Trigger.AgendaID != "") {
		st.Workspaces, st.WorkspacesSet, dirty = w.ids(), true, true // 新会话第一次说话：定下默认挂载的工作目录
	}
	// 群里说话的人按访客算；管理员在群里也只能用群聊白名单里的工具
	admin := a.isAdmin(head.who) && !group
	note := ""
	if !group && !isDreamSID(s.sid) && !head.trigger {
		note = a.envNote(w, rt, admin)
		if admin {
			note += a.sandboxNote(sandboxMode, rt)
		}
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
	// 会话换过 agent：下一条用户消息前给新 agent 一次交接说明（像环境说明，不改系统提示词，前缀缓存不失效）
	if !head.inner && !head.trigger && st.Handoff != "" {
		content = st.Handoff + "\n" + content
		st.Handoff, dirty = "", true
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
		msg := session.Msg{Role: "user", Content: content, Images: images}
		if head.report != nil {
			msg.Kind = "branch_report"
			msg.Report = head.report
		}
		if isLifeSID(s.sid) && head.inner {
			msg.Kind = "life_episode"
			if s.mind != nil {
				msg.Reason = s.mind.Trigger.Reason
				msg.AgendaID = s.mind.Trigger.AgendaID
			}
		}
		if err := a.store.Append(s.sid, msg); err != nil {
			a.log.Warn("保存消息失败", "sid", s.sid, "err", err)
		}
	}
	a.markSeen(head.ep, s.sid, a.histEnd(s.sid), false)

	a.runSeq++
	r := &run{hook: head.hook, scheduled: s.scheduled, id: a.runSeq, node: node, from: head.who, start: a.now(), outs: []out{{ep: head.ep}}, execs: map[string]context.CancelFunc{},
		callArgs: map[string]proto.Call{}, sent: map[string][]media.Attachment{}, images: map[string][]media.Attachment{},
		runID: fmt.Sprintf("r%d-%s", a.runSeq, randHex(8)), asked: text,
		rt: rt, w: w, elevate: admin, withImages: a.modelSupportsImages(s.sid)}
	s.run = r
	s.groupPending = 0
	// 上次进程退出时没处理完的补充：装进这一轮的收件箱，到了安全边界再插入
	if len(s.pending) > 0 {
		r.inbox = append(r.inbox, s.pending...)
		r.inboxBytes += s.pendingBytes
		n := len(s.pending)
		s.pending, s.pendingBytes = nil, 0
		a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "resumed",
			Data: map[string]any{"count": n}})
		a.deliver(head.ep, fmt.Sprintf("上次中断时还有 %d 条补充没有处理完，已经放回这一轮一起处理。", n), "")
	}
	// 上次中断、已经写进上下文但没拿到结局的补充：说明一下，用户发「继续」就能接着做
	if len(s.interrupted) > 0 {
		a.deliver(head.ep, fmt.Sprintf("另外有 %d 条补充上次已经写进上下文，但没有处理完；需要接着做就发「继续」。", len(s.interrupted)), "")
	}
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
	r.mode = mode
	r.sandboxMode = sandboxMode
	in := proto.Msg{T: proto.TInput, SID: s.sid, RunID: r.runID, From: r.from, Mode: mode,
		HookChecks:  a.nodes[node].info.Features["hook_check"] >= 1 && a.hookEngine != nil && a.hasBeforeHooks(s.sid),
		SandboxMode: r.sandboxMode, Silent: a.branchSilent(s.sid), OwnerAgent: a.execAgentID(s.sid), MaxParallelTools: a.runtimeLimits.MaxParallelToolsPerRun}
	if r.inner || a.isBranchSID(s.sid) {
		in.Priority = 10
	}
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
	if r.inner && !isLifeSID(s.sid) {
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
			home := homeSpec()
			home.SandboxMode = sandboxMode
			in.Workspaces = []proto.WorkspaceSpec{home}
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
	a.publish(events.Event{T: "run", SID: s.sid, RunID: r.runID, Kind: "start", Text: content, Data: runData})
	a.emitRuntimeEvent("run.started", s.sid, r, map[string]string{"status": "running"})
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
	// 这一轮没来得及插入的补充不能悄悄丢掉：放回会话队列，交给后续 run
	if !r.dropInbox {
		a.resumeAfterRequeue(s, a.requeueInterjections(s, r))
	}
	// 自主活动的一轮没有正常收尾（内核断线等）：记成没做成，稍后重新确认，不能当成正常休息
	if r.inner && s.mind != nil {
		why := note
		if why == "" {
			why = "这一轮被中止，结果未知"
		}
		a.mindAborted(s, r, why)
	}
	if r.cancelLLM != nil {
		r.cancelLLM()
	}
	if r.outcome == "" {
		r.outcome = proto.StatusError
	}
	// 源会话到了安全边界：这一轮的工具批次和生成都已经结束，等着的 Fork 可以复制上下文了
	a.finishPendingForks(s.sid)
	for _, cancel := range r.execs {
		cancel() // 交给网关执行的工具调用
	}
	a.releaseDesktopLeases(s.sid, r.id)
	r.stopTyping()
	for _, v := range r.views() {
		v.Close()
	}
	a.emitRuntimeEvent("run.finished", s.sid, r, map[string]string{"status": r.outcome})
	a.finishHook(s, r, note)
	s.run = nil
	if isHookSID(s.sid) {
		a.store.UnloadHistory(s.sid)
		delete(a.sessions, s.sid)
	}
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
		// 没有 run 不代表没有待处理责任：恢复出来的补充、重新排队过的补充也要一起作废
		if err := a.discardPending(s, nil, "stop", "用户停止任务"); err != nil {
			a.answer(c, fmt.Sprintf("当前没有进行中的任务。补充的丢弃没能保存下来（%v），重启后可能还会出现", err))
			return
		}
		a.answer(c, "当前没有进行中的任务")
		return
	}
	s.run.addOut(c.ep)
	s.run.dropInbox = true // 用户明确停止：还在等边界的那几条补充不再自动开新一轮
	// 停止是持久终态：收件箱里这几条也必须跟着作废，不能只清内存。
	// 先杀进程（用户要求停止就得停下来），再做失效转换；失败时把条目放回去并如实报告。
	inbox := append([]interjection(nil), s.run.inbox...)
	a.cancelRun(s) // 内核会回 done(cancelled)
	if err := a.discardPending(s, inbox, "stop", "用户停止任务"); err != nil {
		// 收件箱本来就没动（discardPending 失败时不清任何内存状态），不要回填，
		// 否则重试几次就翻倍。只如实说明持久化没成功。
		a.log.Warn("停止时丢弃补充失败", "sid", s.sid, "err", err)
		a.deliver(c.ep, fmt.Sprintf("已停止任务；但补充的失效没能保存下来（%v），重启后可能还会出现。", err), "")
		return
	}
	s.run.inbox, s.run.inboxBytes = nil, 0
}

func (a *App) reset(c in) {
	s := c.s
	s.queue = nil
	if s.run != nil {
		s.resetAfter = true
		s.run.addOut(c.ep)
		s.run.dropInbox = true
		inbox := append([]interjection(nil), s.run.inbox...)
		a.cancelRun(s)
		// 活动分支和空闲分支走同一个提交点：世代一定要换，只换一次
		if err := a.commitReset(s, inbox); err != nil {
			a.answer(c, fmt.Sprintf("已停止并清空上下文；但作废补充或保存世代时出错（%v），重启后可能还有旧补充", err))
			return
		}
		return
	}
	if err := a.commitReset(s, nil); err != nil {
		a.answer(c, fmt.Sprintf("清空上下文时出错（%v）", err))
		return
	}
	a.answer(c, "上下文已清空")
}

// commitReset 是重置的唯一提交点：换世代、作废收件箱/待处理/中断未完成的补充，
// 再清历史（活动分支由 afterRun 在 run 结束后清）。返回第一个真实错误。
func (a *App) commitReset(s *sess, inbox []interjection) error {
	genErr := a.newGeneration(s)
	if err := a.discardPending(s, inbox, "reset", "用户清空上下文"); err != nil && genErr == nil {
		genErr = err
	}
	// 世代已经换了：内存里的旧状态一律清掉，不能等到重启
	s.pending, s.pendingBytes, s.interrupted = nil, 0, nil
	if s.run != nil {
		s.run.inbox, s.run.inboxBytes = nil, 0
	}
	if s.resetAfter {
		return genErr // 活动分支：历史由 afterRun → resetHistory 清
	}
	if err := a.resetHistory(s); err != nil && genErr == nil {
		genErr = err
	}
	return genErr
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
	who := c.who
	if c.name != "" && !strings.HasPrefix(who, "web:") && !strings.HasPrefix(who, "cli:") {
		who = c.name
	}
	// MCP 这类工具在网关这边执行：批不批交回调用方，不用告诉内核
	if ap.resolve != nil {
		res := ap.resolve
		delete(a.approvals, n)
		a.resolveApproval(ap, a.decided(ok, who))
		a.publish(events.Event{T: "approval_done", SID: ap.sid, N: n})
		a.answer(c, a.decided(ok, who))
		res(ok)
		return
	}
	if a.hub.Send(a.sessions[ap.sid].run.node, proto.Msg{T: proto.TApprove, SID: ap.sid, ID: ap.id,
		RunID: ap.runID, TurnID: ap.turnID, OK: proto.Bool(ok), From: c.who}) {
		who := c.who
		if c.name != "" && !strings.HasPrefix(who, "web:") && !strings.HasPrefix(who, "cli:") {
			who = c.name
		}
		a.resolveApproval(ap, a.decided(ok, who))
	}
}

// execStarted 告诉内核：交给网关的这个调用真的开始执行了（协议 v11）。
// 旧内核不认识这条消息，用协议版本挡住就行。
func (a *App) execStarted(node string, m *proto.Msg) {
	n := a.nodes[node]
	if n == nil || n.info.Proto < 11 {
		return
	}
	notice := proto.Msg{T: proto.TExecStarted, SID: m.SID, ID: m.ID, RunID: m.RunID, TurnID: m.TurnID}
	if m.JobID != "" && n.info.Features["exec_started_timeout"] >= 1 {
		notice.JobID = m.JobID
		notice.TimeoutMs = m.TimeoutMs
	}
	a.hub.Send(node, notice)
}

// ---- 内核事件（kernel.Handler） ----

func (a *App) KernelMsg(node string, m *proto.Msg) {
	if m.T == proto.TPipeData || m.T == proto.TPipeClosed {
		// MCP 管道的数据不过网关总锁：交给管道时可能要等 MCP 客户端读走（背压），
		// 拿着总锁等就会把整个网关卡住
		a.mu.Lock()
		mg := a.mcp
		a.mu.Unlock()
		if mg != nil {
			if p := mg.Pipes(); p != nil {
				if m.T == proto.TPipeData {
					p.Data(node, m.Pipe, m.Data)
				} else {
					p.Closed(node, m.Pipe, m.Exit, m.Detail)
				}
			}
		}
		return
	}
	if m.T == proto.TFileEvent {
		// 观察结果是普通外部事实，写可靠日志时不占网关总锁
		a.fileChanged(node, m)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jobControlReply(node, m) {
		return
	}
	if a.jobEventMessage(node, m) {
		return
	}
	if a.leaseMessage(node, m) {
		return
	}
	if m.T == proto.TRunStarted {
		a.notifyHostStarted(node, m.ID, m.JobID, int64(m.TimeoutMs), true)
		return
	}
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
	// 完整调用身份：上一个 run 的 done / 上一轮的迟到结果都不能改动当前 run
	if m.RunID != "" && s.run.runID != "" && m.RunID != s.run.runID {
		a.log.Warn("忽略别的 run 的迟到消息", "node", node, "t", m.T, "sid", m.SID,
			"got_run", m.RunID, "run", s.run.runID)
		return
	}
	if m.TurnID > 0 && s.run.turnID > 0 && m.TurnID != s.run.turnID && m.T != proto.TModel {
		a.log.Debug("忽略上一轮的迟到消息", "node", node, "t", m.T, "sid", m.SID,
			"got_turn", m.TurnID, "turn", s.run.turnID)
		return
	}
	switch m.T {
	case proto.THookCheck:
		a.preToolCheck(s, m)
	case proto.TModel:
		// 完整调用身份：内核告诉我们这是第几次模型请求，后面 approve / exec_result 原样带回去
		s.run.turnID = m.TurnID
		if m.RunID != "" && s.run.runID == "" {
			s.run.runID = m.RunID
		}
		if s.run.fetching > 0 {
			s.run.modelWaiting = true // 等图片取完（seeImage）
			break
		}
		a.callModel(s)
	case proto.TToolQueued:
		a.emitToolState(s, m, "queued")
		// 这个调用还在等执行槽或等资源：只更新界面，不占执行结果的位置
		s.run.tool = m.Name
		a.publish(events.Event{T: "tool_queued", SID: s.sid, RunID: s.run.runID, TurnID: s.run.turnID, ID: m.ID, Name: m.Name, Text: m.Text,
			Data: map[string]any{"run_id": m.RunID, "turn_id": m.TurnID}})
	case proto.TToolStarted, proto.TExecStarted:
		a.emitToolState(s, m, "running")
		a.publish(events.Event{T: "tool_started", SID: s.sid, RunID: s.run.runID, TurnID: s.run.turnID, ID: m.ID,
			Data: map[string]any{"run_id": m.RunID, "turn_id": m.TurnID}})
	case proto.TExec:
		a.execTool(node, s, m)
	case proto.TExecCancel:
		if cancel := s.run.execs[m.ID]; cancel != nil {
			cancel()
		}
		a.cancelMCP(s.sid, m.ID)
	case proto.TResult:
		a.recordToolEvidence(s.run, m)
		name := "tool.after"
		if m.OK != nil && !*m.OK {
			name = "tool.failed"
		}
		a.emitRuntimeEvent(name, s.sid, s.run, map[string]any{"tool": s.run.callArgs[m.ID].Name, "call_id": m.ID, "turn_id": s.run.turnID, "ok": m.OK == nil || *m.OK})
		content := m.Content
		if m.OK != nil && !*m.OK {
			content = "[执行失败] " + content
		}
		// Read 读到图片：取过来附上以后再记进历史（fetch.go）
		if !a.seeImage(s, m) {
			// MCP 工具返回的图片：模型支持看图时一起发过去
			if err := a.store.Append(s.sid, session.Msg{Role: "tool", RunID: s.run.runID, TurnID: s.run.turnID, ToolCallID: m.ID, Content: content, Agent: a.execAgentID(s.sid),
				Sent: s.run.sent[m.ID], Images: s.run.images[m.ID], Sandbox: m.Sandbox}); err != nil {
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
			channel.EmitToolEvent(o.view, channel.ToolEvent{RunID: s.run.runID, TurnID: s.run.turnID, CallID: m.ID, Name: s.run.callArgs[m.ID].Name, State: "done", OK: m.OK == nil || *m.OK, Detail: resultDetail(detail)})
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
		a.publish(events.Event{T: "tool_result", SID: s.sid, RunID: s.run.runID, TurnID: s.run.turnID, ID: m.ID, OK: events.Bool(m.OK == nil || *m.OK),
			Text: resultDetail(m.Content), Data: clip(m.Content, 4000), Sandbox: m.Sandbox})
	case proto.TApproveReq:
		a.emitRuntimeEvent("approval.waiting", s.sid, s.run, map[string]string{"tool": m.Name, "call_id": m.ID})
		if s.run.full && !m.ForceApproval {
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
	// 插话的三个状态：已受理（收件箱）、已注入（写进历史）、已被模型请求读到。
	// 这里是最后一个状态的对账：写进历史但这一轮没再请求模型，要明确告诉用户，不能装作已处理。
	if unread := r.injected - r.readRevision; unread > 0 && r.scheduled == nil {
		more := fmt.Sprintf("（你后来的 %d 条补充已经放进对话，但这一轮没有再请求模型处理它们；下一轮会看到。）", unread)
		if note != "" {
			note += "\n" + more
		} else {
			note = more
		}
	}
	// 这一轮的终态：结束时给处理过的补充记上明确结局（不是"读过"就算完）
	r.outcome = status
	a.settleInterjections(s.sid, r.journalIDs, interjectionOutcome(status), detail)
	r.journalIDs = nil
	// 上次中断的补充：这一轮模型读过上下文、正常结束，就算处理过了
	if status == proto.StatusOK && len(s.interrupted) > 0 {
		ids := make([]string, 0, len(s.interrupted))
		for _, it := range s.interrupted {
			ids = append(ids, it.id)
		}
		a.settleInterjections(s.sid, ids, outcomeOK, "用户继续后已处理")
		s.interrupted = nil
	}
	a.log.Info("处理结束", "sid", s.sid, "status", status, "model_calls", r.calls,
		"prompt_tokens", r.usage.PromptTokens, "cached_tokens", r.usage.CachedTokens, "completion_tokens", r.usage.CompletionTokens)
	if r.calls > 0 {
		for _, v := range r.views() {
			v.Note(usageLine(r, a.now()))
		}
	}
	a.publish(events.Event{T: "run", SID: s.sid, RunID: r.runID, Kind: "end", Text: status, Data: map[string]any{
		"calls": r.calls, "usage": r.usage, "ms": a.now().Sub(r.start).Milliseconds(), "detail": detail}})
	if r.scheduled != nil {
		a.completeSchedule(r.scheduled, status == proto.StatusOK, strings.TrimSpace(body+"\n"+note))
		r.scheduled = nil
		body, note = "", ""
	}
	a.recordRunExperience(s, r, status, strings.TrimSpace(body+"\n"+note))
	if r.inner {
		a.commitAgendaResults(s, r, status)
		a.innerDone(s, r, status, strings.TrimSpace(body+"\n"+note))
		body, note = "", ""
	} else {
		a.commitAgendaResults(s, r, status)
	}
	a.endRun(s, "")
	// 这一轮显示到的窗口都看到了这里
	end := 0
	if len(r.outs) > 0 {
		end = a.histEnd(s.sid)
	}
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
	a.pumpRuns()
	if s.run == nil && isHookSID(s.sid) {
		a.store.UnloadHistory(s.sid)
		delete(a.sessions, s.sid)
	} else if s.run == nil && a.isBranchSID(s.sid) {
		a.store.UnloadHistory(s.sid)
	} else if s.run == nil && !isCronSID(s.sid) {
		a.maybeCompact(s, false)
	}
}

// callModel 发起一次模型请求。调用方持有锁；请求本身在协程里跑。
func (a *App) callModel(s *sess) {
	r := s.run
	if r.hook != nil && r.hook.work.Spec.Handler.TokenBudget > 0 && r.usage.PromptTokens+r.usage.CompletionTokens >= r.hook.work.Spec.Handler.TokenBudget {
		a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, RunID: r.runID, TurnID: r.turnID, Reason: proto.ReasonError, Detail: "Hook token 子预算已用尽"})
		return
	}
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
	// 边界一：上一批工具调用已经全部闭合、这次模型请求还没构造——先把等着的插话写进历史，
	// 本次请求的快照就能看到补充。快照之后才到的留给下一个边界，不改写在途请求。
	a.drainInterjections(s, r)
	if r.calls > 0 && len(r.inbox) == 0 {
		a.finishPendingForks(s.sid)
	}
	// 这次请求的快照包含到哪一条补充：结束时用它判断“已注入但模型没读到”的补充
	r.readRevision = r.injected
	r.modelReq++
	// 记进日志：这些补充已经被某个模型请求的快照包含（不等于请求已经成功）
	if len(r.pendingInclude) > 0 {
		reqID := fmt.Sprintf("%s-t%d", r.runID, r.calls+1)
		for _, id := range r.pendingInclude {
			if err := a.journalAppend(s.sid, journalRec{Kind: jrecIncluded, ID: id, RequestID: reqID,
				Revision: r.injected, RunID: r.runID}); err != nil {
				a.log.Warn("记录插话已进入请求失败", "sid", s.sid, "id", id, "err", err)
			}
		}
		r.journalIDs = append(r.journalIDs, r.pendingInclude...)
		r.pendingInclude = nil
	}
	reqID := fmt.Sprintf("%s-m%d", r.runID, r.modelReq)
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
	// 桌面任务会连续截很多图：模型请求里只保留最近两张，旧的换成一句说明（历史原样留着）
	hist := trimToolImages(a.store.EffectiveHistory(s.sid), 2)
	for _, tool := range snapshotTools {
		if tool.Function.Name == "Agenda" {
			hist = evidenceHistory(r, hist)
			break
		}
	}
	req := llm.Request{
		Messages: session.BuildMessages(system, st.Summary, hist, a.cfg.LLM.SendReasoning, images),
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
	if r.hook != nil && r.hook.work.Spec.Handler.TokenBudget > 0 {
		req.MaxTokens = max(1, r.hook.work.Spec.Handler.TokenBudget-r.usage.PromptTokens-r.usage.CompletionTokens)
	}
	req.Tools = snapshotTools
	_, _, meterSignature, _ := a.contextParts(s.sid)
	meterPosition := st.Base + len(a.store.History(s.sid))
	ctx, cancel := context.WithCancel(a.ctx)
	r.cancelLLM = cancel
	r.calls++
	a.totals.ModelCalls++
	a.publish(events.Event{T: "model", SID: s.sid, RunID: r.runID, TurnID: r.turnID, Kind: "start",
		Data: map[string]any{"thinking": req.Thinking, "run_id": r.runID, "turn_id": r.turnID,
			"request_id": reqID, "input_revision": r.injected}})

	// 过程总是发布到事件总线（网页实时显示），支持流式的窗口再自己显示一份
	views, bus, sid := r.views(), a.bus, s.sid
	contentEmitted := false
	onDelta := func(d llm.Delta) {
		if d.Reasoning != "" {
			bus.Publish(events.Event{T: "delta", SID: sid, RunID: r.runID, TurnID: r.turnID, Kind: "reasoning", Text: d.Reasoning})
			for _, v := range views {
				v.Reasoning(d.Reasoning)
			}
		}
		if d.Content != "" {
			contentEmitted = true
			bus.Publish(events.Event{T: "delta", SID: sid, RunID: r.runID, TurnID: r.turnID, Kind: "content", Text: d.Content})
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
			a.publish(events.Event{T: "model", SID: s.sid, RunID: r.runID, TurnID: r.turnID, Kind: "end", Text: "error: " + err.Error()})
			a.log.Warn("模型请求失败", "sid", s.sid, "err", err)
			a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, RunID: r.runID, TurnID: r.turnID, Reason: proto.ReasonError, Detail: err.Error()})
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
		a.publish(events.Event{T: "model", SID: s.sid, RunID: r.runID, TurnID: r.turnID, Kind: "end", Text: res.Reason,
			Data: map[string]any{"usage": u, "ms": time.Since(start).Milliseconds()}})
		// 边界二：生成完整结束（用量已经结算），但还没把结果交给内核。这期间收到的插话要
		// 在这里处理：不派发这批尚未接受的工具，先写补充再重新规划。
		if a.yieldToInterjection(s, r, res, u) {
			return
		}
		if res.Reason == llm.ReasonToolUse {
			limit := a.runtimeLimits.MaxCallsPerTurn
			if n := a.nodes[r.node]; n != nil && n.info.Features["max_calls_per_turn"] > 0 {
				limit = min(limit, n.info.Features["max_calls_per_turn"])
			}
			if why := validateProposedCalls(res.Calls, limit); why != "" {
				proposals := res.Calls
				if len(proposals) > 256 {
					proposals = proposals[:256]
				}
				if err := a.store.Append(s.sid, session.Msg{Role: "assistant", RunID: r.runID, TurnID: r.turnID, Content: res.Content, Reasoning: res.Reasoning, Usage: &u, Agent: a.execAgentID(s.sid), Kind: "unaccepted_tool_proposal", Reason: why, ProposedCalls: proposals}); err != nil {
					a.log.Warn("保存未接受提案失败", "err", err)
				}
				a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, RunID: r.runID, TurnID: r.turnID, Reason: proto.ReasonError, Detail: why})
				return
			}
		}
		if res.Content != "" {
			r.streamed = true
		}
		a.log.Info("模型回复", "sid", s.sid, "reason", res.Reason, "calls", len(res.Calls),
			"prompt_tokens", u.PromptTokens, "cached_tokens", u.CachedTokens,
			"completion_tokens", u.CompletionTokens, "reasoning_tokens", u.ReasoningTokens,
			"took", time.Since(start).Round(time.Millisecond))
		if err := a.store.Append(s.sid, session.Msg{Role: "assistant", RunID: r.runID, TurnID: r.turnID, Content: res.Content, ToolCalls: res.Calls,
			Reasoning: res.Reasoning, Usage: &u, Agent: a.execAgentID(s.sid)}); err != nil {
			a.log.Warn("保存模型回复失败", "sid", s.sid, "err", err)
		}
		turn := proto.Msg{T: proto.TTurn, SID: s.sid, RunID: r.runID, TurnID: r.turnID}
		switch res.Reason {
		case llm.ReasonToolUse:
			turn.Reason = proto.ReasonToolUse
			r.usedTools = true
			a.sayMidway(s, r, res.Content)
			for _, c := range res.Calls {
				r.tool = c.Function.Name
				delete(r.toolEvidence, c.ID)
				delete(r.unverifiedTools, c.ID)
				for agendaID, done := range r.agendaDone {
					if slices.Contains(done.Evidence, c.ID) {
						delete(r.agendaDone, agendaID)
					}
				}
				r.callArgs[c.ID] = proto.Call{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)}
				turn.Calls = append(turn.Calls, proto.Call{ID: c.ID, Name: c.Function.Name, Args: argsJSON(c.Function.Arguments)})
				detail := toolDetail(c.Function.Name, c.Function.Arguments)
				for _, v := range views {
					channel.EmitToolEvent(v, channel.ToolEvent{RunID: r.runID, TurnID: r.turnID, CallID: c.ID, Name: c.Function.Name, State: "accepted", Detail: detail})
				}
				a.publish(events.Event{T: "tool", SID: s.sid, RunID: r.runID, TurnID: r.turnID, ID: c.ID, Name: c.Function.Name, Text: detail,
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

// Wait joins durable workers after the App context has been cancelled. Call it
// before re-exec or releasing the data directory; an in-progress sync may finish.
func (a *App) Wait() {
	if a.hookEngine != nil {
		a.hookEngine.Close()
	}
	if a.dispatcher != nil {
		<-a.dispatcher.Done()
	}
	if a.jobEvents != nil {
		<-a.jobEvents.Done()
	}
	a.forkWG.Wait()
}
