package app

// agent 在网关这边的逻辑。
//
// 每个会话属于一个 agent：默认 agent 的会话 ID 没有前缀（main、tg:123），别的 agent 是
// @<agent>/<名字>。agent 有自己的名字、头像、人格（agents/<id>/persona.md）、自己的模型和
// 思考强度，还可以覆盖全局的设置（每条消息的上限、权限模式、上下文整理、工具白名单）；
// 没覆盖的项跟随全局（配置文件里的 agent、workspace、context 组）。
//
// 机器人绑在 agent 上：一个机器人只绑一个 agent，同一个机器人上的所有聊天都归那个 agent。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
)

// SetAgents 接上 agent 列表、机器人列表和 agents 目录。
func (a *App) SetAgents(store *agents.Store, bots *agents.Bots, root string) {
	a.mu.Lock()
	a.agentStore, a.botStore, a.agentRoot = store, bots, root
	a.personaAgent = persona.New(root, a.log)
	a.mu.Unlock()
}

// SetBots 换上一份新的机器人列表（网页里改过机器人之后）。
func (a *App) SetBots(bots *agents.Bots) {
	a.mu.Lock()
	a.botStore = bots
	a.mu.Unlock()
}

// AgentRoot 是 agents 目录（每个 agent 一个子目录）。
func (a *App) AgentRoot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentRoot
}

// agentOf 是会话所属的 agent，找不到时是默认 agent。调用方持有锁。
func (a *App) agentOf(sid string) agents.Agent {
	id := agents.SessionAgent(sid)
	if a.agentStore != nil {
		if ag, ok := a.agentStore.Get(id); ok {
			return ag
		}
	}
	return agents.Agent{ID: id, Name: id}
}

// agentByID 按 ID 找 agent。调用方持有锁。
func (a *App) agentByID(id string) (agents.Agent, bool) {
	if a.agentStore == nil {
		return agents.Agent{ID: agents.DefaultID, Name: "小纳"}, id == agents.DefaultID
	}
	return a.agentStore.Get(id)
}

// personaFor 是会话的系统提示词，查找顺序：
//
//	personas/<会话ID>.md      某个会话专属
//	agents/<agent>/persona.md 这个 agent 自己的人格
//	personas/default.md      老版本的人格（升级时复制到上面那个文件，原文件留着给退回旧版本用）
//
// 调用方持有锁。
func (a *App) personaFor(sid string) (string, string) {
	if t, p := a.persona.File(persona.FileFor(sid)); p != "" {
		return t, p
	}
	ag := a.agentOf(sid)
	if a.personaAgent != nil {
		if t, p := a.personaAgent.File(filepath.Join(ag.ID, "persona.md")); p != "" {
			return t, p
		}
	}
	// 正常安装升级时 MigrateAgents 已经把 personas/default.md 复制到 agents/<id>/persona.md；
	// 这里不再兜底，免得同一个“默认人格”有两处能改。只有没有 agent 列表的老模式才用它。
	if a.agentStore == nil {
		if t, p := a.persona.File("default.md"); p != "" {
			return t, p
		}
	}
	return "", ""
}

// agentPersonaFile 是某个 agent 的人格文件。
func (a *App) agentPersonaFile(id string) string {
	return filepath.Join(a.agentRoot, id, "persona.md")
}

// agentLimits 是这一轮实际生效的上限。
type agentLimits struct {
	Steps    int
	Run      time.Duration
	Approval time.Duration
	Output   int
}

// limitsOf 是会话所属 agent 的每条消息上限：agent 覆盖了的用自己的，没覆盖的用全局。
// 调用方持有锁。
func (a *App) limitsOf(sid string) agentLimits {
	cfg := a.cfg.Agent
	l := agentLimits{Steps: cfg.Steps(), Run: cfg.RunLimit(), Approval: cfg.ApprovalLimit(), Output: cfg.OutputBytes()}
	ag := a.agentOf(sid)
	if ag.MaxSteps != nil {
		l.Steps = *ag.MaxSteps
	}
	if ag.RunTimeout != nil && strings.TrimSpace(*ag.RunTimeout) != "" {
		l.Run = parseLimitDuration(*ag.RunTimeout)
	}
	if ag.ApprovalTimeout != nil && strings.TrimSpace(*ag.ApprovalTimeout) != "" {
		l.Approval = parseLimitDuration(*ag.ApprovalTimeout)
	}
	if ag.MaxOutputKB != nil && *ag.MaxOutputKB > 0 {
		l.Output = *ag.MaxOutputKB << 10
	}
	return l
}

// parseLimitDuration 和配置里的时限写法一致：30m、2h，0 表示不限。
func parseLimitDuration(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// agentModeDefault 是会话的默认权限模式：agent 覆盖了就用它的，否则用全局的。调用方持有锁。
func (a *App) agentModeDefault(sid string) string {
	return modeFor(a.agentOf(sid), a.cfg.Workspace.Mode)
}

// modeFor 是 agent 的默认权限模式：自己设了的用自己的；拟人型没设时放行（像人一样做事，不停下来等审批），
// 工具型跟随全局。
func modeFor(ag agents.Agent, global string) string {
	if ag.Mode != nil && *ag.Mode != "" {
		return *ag.Mode
	}
	if ag.Life() {
		return proto.ModeFull
	}
	return global
}

// contextConfigFor 是会话的上下文整理参数：agent 覆盖了的用自己的，没覆盖的用全局。
// 调用方持有锁。
func (a *App) contextConfigFor(sid string) (float64, float64, int) {
	at, to, keep := a.contextConfig()
	ag := a.agentOf(sid)
	if ag.CompactAt != nil && *ag.CompactAt > 0 && *ag.CompactAt < 1 {
		at = *ag.CompactAt
	}
	if ag.CompactTo != nil && *ag.CompactTo > 0 && *ag.CompactTo < 1 {
		to = *ag.CompactTo
	}
	if ag.KeepTurns != nil && *ag.KeepTurns > 0 {
		keep = *ag.KeepTurns
	}
	return at, to, keep
}

// idleCompactFor 是会话所属 agent 的空闲整理间隔。调用方持有锁。
func (a *App) idleCompactFor(sid string) string {
	if ag := a.agentOf(sid); ag.IdleCompact != nil && strings.TrimSpace(*ag.IdleCompact) != "" {
		return strings.TrimSpace(*ag.IdleCompact)
	}
	return a.cfg.Context.IdleCompact
}

// filterAgentTools 按 agent 的工具白名单过滤（空表示全部可以用）。调用方持有锁。
func (a *App) filterAgentTools(sid string, tools []llm.Tool) []llm.Tool {
	ag := a.agentOf(sid)
	if len(ag.Tools) == 0 {
		return tools
	}
	out := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		if slices.Contains(ag.Tools, t.Function.Name) {
			out = append(out, t)
		}
	}
	return out
}

// botForChannel 是渠道名对应的机器人。调用方持有锁。
func (a *App) botForChannel(ch string) (agents.Bot, bool) {
	if a.botStore == nil {
		return agents.Bot{}, false
	}
	for _, b := range a.botStore.List() {
		if agents.ChannelName(b) == ch {
			return b, true
		}
	}
	return agents.Bot{}, false
}

// agentForChannel 是这个渠道的聊天归哪个 agent：机器人绑了谁就是谁，没绑就是默认 agent。
// 调用方持有锁。
func (a *App) agentForChannel(ch string) string {
	if b, ok := a.botForChannel(ch); ok && b.Agent != "" {
		return b.Agent
	}
	return agents.DefaultID
}

// botShow 是机器人自己的显示档位（空表示跟随 agent）。调用方持有锁。
func (a *App) botShow(ch string) string {
	if b, ok := a.botForChannel(ch); ok {
		return b.Show
	}
	return ""
}

// ---- 网页和聊天里的 agent 管理 ----

// AgentSetting 是一个 agent 可以覆盖的全局设置：全局值、自己的值（null 表示跟随全局）、
// 实际生效的值。网页的 agent 面板按它渲染，聊天里的 Gateway 工具也用它。
type AgentSetting struct {
	Key       string   `json:"key"`
	Group     string   `json:"group"` // work 干活、group 群聊、life 拟人、memory 记忆
	Label     string   `json:"label"`
	Type      string   `json:"type"` // int | float | string | enum
	Unit      string   `json:"unit,omitempty"`
	Help      string   `json:"help,omitempty"`
	Options   []string `json:"options,omitempty"`
	Global    any      `json:"global"`
	Value     any      `json:"value"`
	Effective any      `json:"effective"`
}

// BotBrief 是一个机器人，给网页的 agent 面板看。
type BotBrief struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
}

// AgentInfo 是一个 agent 的概况，网页和 Gateway 工具都用它。
type AgentInfo struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Emoji          string         `json:"emoji"`
	Default        bool           `json:"default"`
	Model          string         `json:"model"`
	ModelLabel     string         `json:"model_label"`
	Thinking       string         `json:"thinking"`
	ThinkingLevels []string       `json:"thinking_levels"`
	Tools          []string       `json:"tools"`
	GroupTools     []string       `json:"group_tools"`
	ToolsAvailable []string       `json:"tools_available"` // 可以填进工具白名单的名字
	Bots           []BotBrief     `json:"bots"`
	Sessions       int            `json:"sessions"`
	Running        bool           `json:"running"`
	PersonaFile    string         `json:"persona_file"`
	PersonaChars   int            `json:"persona_chars"`
	Settings       []AgentSetting `json:"settings"`
	Style          string         `json:"style"` // tool | life
	Memory         MemoryBrief    `json:"memory"`
	Life           *LifeBrief     `json:"life,omitempty"` // 拟人型的内心活动状态
	Doing          Doing          `json:"doing"`          // 现在在干嘛（doing.go）
}

// MemoryBrief 是 agent 长期记忆的概况：多少条、多少人、上次整理。
type MemoryBrief struct {
	On        bool   `json:"on"`
	Total     int    `json:"total"`
	People    int    `json:"people"`
	Dreaming  bool   `json:"dreaming"`
	LastDream int64  `json:"last_dream,omitempty"` // 上次整理结束（毫秒）
	LastNote  string `json:"last_note,omitempty"`
}

// botBrief 是机器人的概况（渠道状态网页从概况接口里读）。调用方持有锁。
func (a *App) botBrief(b agents.Bot) BotBrief {
	return BotBrief{ID: b.ID, Kind: b.Kind, Name: b.Name, Channel: agents.ChannelName(b), Enabled: b.Enabled}
}

// agentSettings 是一个 agent 可以覆盖的项，带全局值（或默认值）、自己的值和实际生效的值，按组排：
// work 干活、group 群聊、life 拟人（只有拟人型用）、memory 记忆。调用方持有锁。
func (a *App) agentSettings(ag agents.Agent) []AgentSetting {
	c := a.cfg
	at, to, keep := a.contextConfig()
	strOrEmpty := func(s string) any {
		if strings.TrimSpace(s) == "" {
			return ""
		}
		return s
	}
	eagerDef, eagerHelp := "low", "low 每小时最多主动说 2 次；normal 6 次；high 15 次；off 只回被叫到的。要不要说话的判断本身也是一次模型调用，所以判断次数也有上限"
	if !ag.Life() {
		eagerDef, eagerHelp = "off", "工具型 agent 在群里只回被叫到的，不主动插话；改成拟人型才用这一项"
	}
	modeDef, modeHelp := c.Workspace.Mode, "新会话用的权限模式，会话里还能单独改。full 放行：所有工具直接执行、不审批"
	if ag.Life() {
		modeDef, modeHelp = proto.ModeFull, "新会话用的权限模式，会话里还能单独改。拟人型默认 full 放行：所有工具直接执行、不审批"
	}
	models := []string{""}
	if a.models != nil {
		for _, m := range a.models.Models() {
			models = append(models, m.ID)
		}
	}
	out := []AgentSetting{
		{Key: "max_steps", Group: "work", Label: "工具调用轮数上限", Type: "int", Global: c.Agent.Steps(),
			Help:      "一条消息里最多请求模型几次，0 表示不限",
			Value:     ptrAny(ag.MaxSteps),
			Effective: pickInt(ag.MaxSteps, c.Agent.Steps())},
		{Key: "run_timeout", Group: "work", Label: "最长处理多久", Type: "string",
			Help:      "例如 30m、2h；0 表示不限",
			Global:    strOrEmpty(c.Agent.RunTimeout),
			Value:     ptrAny(ag.RunTimeout),
			Effective: durText(pickDuration(ag.RunTimeout, c.Agent.RunLimit()))},
		{Key: "approval_timeout", Group: "work", Label: "等审批多久", Type: "string",
			Help:      "例如 30m；0 表示一直等",
			Global:    strOrEmpty(c.Agent.ApprovalTimeout),
			Value:     ptrAny(ag.ApprovalTimeout),
			Effective: durText(pickDuration(ag.ApprovalTimeout, c.Agent.ApprovalLimit()))},
		{Key: "max_output_kb", Group: "work", Label: "工具结果最多保留（KB）", Type: "int",
			Help:      "一次工具输出超过这么多就截断",
			Global:    c.Agent.OutputBytes() >> 10,
			Value:     ptrAny(ag.MaxOutputKB),
			Effective: pickPositive(ag.MaxOutputKB, c.Agent.OutputBytes()>>10)},
		{Key: "mode", Group: "work", Label: "默认权限模式", Type: "enum", Options: []string{"full", "auto", "ask", "readonly"},
			Help:      modeHelp,
			Global:    modeDef,
			Value:     ptrAny(ag.Mode),
			Effective: pickString(ag.Mode, modeDef)},
		{Key: "compact_at", Group: "work", Label: "自动整理阈值", Type: "float",
			Help:      "上下文到模型窗口的这个比例时自动整理",
			Global:    at,
			Value:     ptrAny(ag.CompactAt),
			Effective: pickRatio(ag.CompactAt, at)},
		{Key: "compact_to", Group: "work", Label: "整理后压到多少", Type: "float", Help: "整理完把上下文压到模型窗口的这个比例，给后面的对话留地方",
			Global:    to,
			Value:     ptrAny(ag.CompactTo),
			Effective: pickRatio(ag.CompactTo, to)},
		{Key: "keep_turns", Group: "work", Label: "最近几轮保留原文", Type: "int", Help: "最近几轮对话一个字不改地留着，更早的才压成摘要",
			Global:    keep,
			Value:     ptrAny(ag.KeepTurns),
			Effective: pickPositive(ag.KeepTurns, keep)},
		{Key: "idle_compact", Group: "work", Label: "空闲整理间隔", Type: "string",
			Help:      "会话空闲这么久后在后台整理一次，例如 30m；0 关闭",
			Global:    strOrEmpty(c.Context.IdleCompact),
			Value:     ptrAny(ag.IdleCompact),
			Effective: pickString(ag.IdleCompact, c.Context.IdleCompact)},
		{Key: "idle_compact_min", Group: "work", Label: "空闲整理要压掉多少", Type: "float", Help: "会话空闲时顺手整理，上下文至少占这么多才动手，免得白忙",
			Global:    c.Context.IdleCompactMin,
			Value:     ptrAny(ag.IdleCompactMin),
			Effective: pickPositiveFloat(ag.IdleCompactMin, c.Context.IdleCompactMin)},
		{Key: "group_eagerness", Group: "group", Label: "群里插话积极度", Type: "enum", Options: []string{"off", "low", "normal", "high"},
			Help:      eagerHelp,
			Global:    eagerDef,
			Value:     ptrAny(ag.GroupEagerness),
			Effective: a.groupRuleFor(ag).Eagerness},
		{Key: "group_quiet", Group: "group", Label: "群里安静多久才插话", Type: "string",
			Help:      "群里安静这么久以后才可能主动说一句，例如 20s、1m",
			Global:    "20s",
			Value:     ptrAny(ag.GroupQuiet),
			Effective: pickString(ag.GroupQuiet, "20s")},
		{Key: "group_min_gap", Group: "group", Label: "两条发言最短间隔", Type: "string", Help: "群里连着说话时两条之间至少隔多久，免得刷屏；例如 10s、30s",
			Global:    "10s",
			Value:     ptrAny(ag.GroupMinGap),
			Effective: pickString(ag.GroupMinGap, "10s")},
		{Key: "group_max_len", Group: "group", Label: "群聊一条最多多少字", Type: "int", Help: "超过就在合适的地方截断，像人在群里发消息一样",
			Global:    300,
			Value:     ptrAny(ag.GroupMaxLen),
			Effective: pickPositive(ag.GroupMaxLen, 300)},
		{Key: "group_parts", Group: "group", Label: "一条回复最多拆成几条", Type: "int", Help: "像人一样按空行拆开、隔一会儿再发，最多这么多条",
			Global:    3,
			Value:     ptrAny(ag.GroupParts),
			Effective: pickPositive(ag.GroupParts, 3)},
		{Key: "life_awake", Group: "life", Label: "醒着的时间", Type: "string", Help: "这段时间里它有自己的活动、会主动找人；之外在睡觉（睡着时整理记忆）。例如 08:00-23:30，可以跨午夜（22:00-06:00）；00:00-24:00 表示不睡",
			Global:    defaultAwake,
			Value:     ptrAny(ag.LifeAwake),
			Effective: pickString(ag.LifeAwake, defaultAwake)},
		{Key: "life_level", Group: "life", Label: "活跃度", Type: "enum", Options: []string{"low", "normal", "high"},
			Help:      "多久自己醒来一次：low 2–4 小时（每天最多 4 次，主动找人最多 1 次）；normal 1–2 小时（8 次，2 次）；high 半小时到一小时（16 次，4 次）。越久没人找它越容易醒",
			Global:    "normal",
			Value:     ptrAny(ag.LifeLevel),
			Effective: pickString(ag.LifeLevel, "normal")},
		{Key: "life_reach", Group: "life", Label: "主动找谁聊天", Type: "enum", Options: []string{"owner", "all", "off"},
			Help:      "owner 只找主人；all 私聊过的人都可能找；off 不主动找人。网关里的群和别的 agent 不受限制。对方没回上一条就不会再找他",
			Global:    "owner",
			Value:     ptrAny(ag.LifeReach),
			Effective: pickString(ag.LifeReach, "owner")},
		{Key: "life_budget", Group: "life", Label: "内心活动每天最多用多少 token", Type: "int", Help: "token 是 AI 的用量单位；自己醒来、冲浪、找人用的都算，到了就当天不再醒。0 表示不限",
			Global:    0,
			Value:     ptrAny(ag.LifeBudget),
			Effective: pickInt(ag.LifeBudget, 0)},
		{Key: "memory", Group: "memory", Label: "长期记忆", Type: "enum", Options: []string{"on", "off"},
			Help:      "开着时它会记住人、事、偏好和学到的东西（知识库在记忆节点里能看能改），每次聊天带上相关的记忆，空闲或睡着时整理一次。关掉就不记也不整理",
			Global:    "on",
			Value:     ptrAny(ag.Memory),
			Effective: pickString(ag.Memory, "on")},
		{Key: "dream_when", Group: "memory", Label: "什么时候整理记忆", Type: "enum", Options: []string{"idle", "nightly", "sleep", "off"},
			Help:      "idle 所有聊天都空闲一阵、攒了不少新聊天时；nightly 每天定点；sleep 睡着的时候（按醒着的时间，拟人型默认）；off 不自动整理（还能手动）",
			Global:    a.dreamWhen(agents.Agent{Style: ag.Style}),
			Value:     ptrAny(ag.DreamWhen),
			Effective: a.dreamWhen(ag)},
		{Key: "dream_at", Group: "memory", Label: "每天几点整理", Type: "string", Help: "选了 nightly 时用，例如 03:00",
			Global:    "03:00",
			Value:     ptrAny(ag.DreamAt),
			Effective: pickString(ag.DreamAt, "03:00")},
		{Key: "dream_idle", Group: "memory", Label: "空闲多久整理", Type: "string", Help: "选了 idle 时用：所有聊天都安静这么久才整理，例如 2h",
			Global:    "2h",
			Value:     ptrAny(ag.DreamIdle),
			Effective: pickString(ag.DreamIdle, "2h")},
		{Key: "dream_max", Group: "memory", Label: "每天最多自动整理几次", Type: "int", Help: "手动整理（/sleep、网页按钮）不算",
			Global:    1,
			Value:     ptrAny(ag.DreamMax),
			Effective: a.dreamMax(ag)},
		{Key: "dream_model", Group: "memory", Label: "整理记忆用的模型", Type: "model", Options: models,
			Help:      "整理记忆读的聊天多，可以换个便宜的模型；空就用这个 agent 自己的模型",
			Global:    "",
			Value:     ptrAny(ag.DreamModel),
			Effective: pickString(ag.DreamModel, "")},
	}
	return out
}

func ptrAny[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func pickInt(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func pickPositive(p *int, def int) int {
	if p != nil && *p > 0 {
		return *p
	}
	return def
}

func pickPositiveFloat(p *float64, def float64) float64 {
	if p != nil && *p > 0 {
		return *p
	}
	return def
}

func pickRatio(p *float64, def float64) float64 {
	if p != nil && *p > 0 && *p < 1 {
		return *p
	}
	return def
}

func pickString(p *string, def string) string {
	if p != nil && strings.TrimSpace(*p) != "" {
		return *p
	}
	return def
}

func pickDuration(p *string, def time.Duration) time.Duration {
	if p != nil && strings.TrimSpace(*p) != "" {
		return parseLimitDuration(*p)
	}
	return def
}

func durText(d time.Duration) string {
	if d <= 0 {
		return "不限"
	}
	// 30m0s 这种写出来太啰嗦：按 2h / 1h30m / 30m / 20s 写
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 && h == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}

// agentInfo 是一个 agent 的概况。调用方持有锁。
func (a *App) agentInfo(ag agents.Agent) AgentInfo {
	info := AgentInfo{ID: ag.ID, Name: ag.Name, Emoji: ag.Emoji, Default: ag.ID == agents.DefaultID,
		Model: ag.Model, Thinking: ag.Thinking, Tools: slices.Clone(ag.Tools), GroupTools: slices.Clone(ag.GroupTools),
		Bots: []BotBrief{}, Settings: a.agentSettings(ag), PersonaFile: "agents/" + ag.ID + "/persona.md"}
	if len(info.Tools) == 0 {
		info.Tools = []string{}
	}
	if len(info.GroupTools) == 0 {
		info.GroupTools = []string{}
	}
	if m, _, ok := a.modelFor(ag); ok {
		info.ModelLabel = modelLabel(m)
		if ag.Model == "" {
			info.ModelLabel += "（默认）"
		}
	}
	info.ThinkingLevels = a.levelsForModel(ag.Model)
	info.Style = agents.StyleTool
	if ag.Life() {
		info.Style = agents.StyleLife
		info.Life = a.lifeBrief(ag)
	}
	info.Doing = a.agentDoing(ag, "")
	if a.agentRoot != "" {
		info.Memory.On = a.memoryOn(ag)
		info.Memory.Total, info.Memory.People, _ = a.memStore(ag.ID).Counts()
		ds := a.dreamOf(ag.ID)
		info.Memory.LastDream, info.Memory.LastNote = ds.LastEnd*1000, ds.LastNote
		if s := a.sessions[dreamSID(ag.ID)]; s != nil && s.run != nil {
			info.Memory.Dreaming = true
		}
	}
	info.ToolsAvailable = a.toolNames()
	for _, sid := range a.sessionIDs() {
		if agents.SessionAgent(sid) != ag.ID || isInnerSID(sid) || isRoomMemberSID(sid) {
			continue
		}
		info.Sessions++
		if s := a.sessions[sid]; s != nil && s.run != nil {
			info.Running = true
		}
	}
	if a.botStore != nil {
		for _, b := range a.botStore.OfAgent(ag.ID) {
			info.Bots = append(info.Bots, a.botBrief(b))
		}
	}
	if a.personaAgent != nil {
		if t, p := a.personaAgent.File(filepath.Join(ag.ID, "persona.md")); p != "" {
			info.PersonaChars = len([]rune(t))
		}
	}
	return info
}

// toolNames 是现在能给模型的工具名（内核的工具 + 网关的工具），白名单里填这些。调用方持有锁。
func (a *App) toolNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, n := range a.nodes {
		for _, t := range n.info.Tools {
			add(t)
		}
	}
	for _, spec := range a.gatewayTools() {
		add(spec.Name)
	}
	add("Skill")
	add("Schedule")
	add(knowledgeLabel)
	add("FollowUp")
	for _, spec := range a.searchTools() {
		add(spec.Name)
	}
	sort.Strings(out)
	return out
}

// AgentList 是所有 agent 的概况（网页和 Gateway 工具用）。
func (a *App) AgentList() []AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []AgentInfo{}
	if a.agentStore == nil {
		return out
	}
	for _, ag := range a.agentStore.List() {
		out = append(out, a.agentInfo(ag))
	}
	return out
}

// Agent 是一个 agent 的概况。
func (a *App) Agent(id string) (AgentInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, ok := a.agentByID(id)
	if !ok {
		return AgentInfo{}, false
	}
	return a.agentInfo(ag), true
}

// AgentPatch 是网页里对 agent 的修改，nil 表示不改。
type AgentPatch struct {
	Name       *string        `json:"name"`
	Emoji      *string        `json:"emoji"`
	Model      *string        `json:"model"`
	Thinking   *string        `json:"thinking"`
	Tools      *[]string      `json:"tools"`
	GroupTools *[]string      `json:"group_tools"`
	Style      *string        `json:"style"`    // tool 工具型 / life 拟人型
	Settings   map[string]any `json:"settings"` // 值为 null 表示恢复跟随全局
}

// applyAgentPatch 把网页的修改套到一个 agent 上。
func applyAgentPatch(ag *agents.Agent, p AgentPatch) error {
	if p.Name != nil {
		ag.Name = *p.Name
	}
	if p.Emoji != nil {
		ag.Emoji = *p.Emoji
	}
	if p.Style != nil {
		ag.Style = *p.Style
	}
	if p.Model != nil {
		ag.Model = *p.Model
	}
	if p.Thinking != nil {
		ag.Thinking = *p.Thinking
	}
	if p.Tools != nil {
		ag.Tools = append([]string{}, *p.Tools...)
	}
	if p.GroupTools != nil {
		ag.GroupTools = append([]string{}, *p.GroupTools...)
	}
	for key, v := range p.Settings {
		if err := applyAgentSetting(ag, key, v); err != nil {
			return err
		}
	}
	return nil
}

// applyAgentSetting 设置一个可覆盖的项；v 是 nil 表示恢复跟随全局（或者默认值）。
func applyAgentSetting(ag *agents.Agent, key string, v any) error {
	strs := map[string]**string{"run_timeout": &ag.RunTimeout, "approval_timeout": &ag.ApprovalTimeout, "idle_compact": &ag.IdleCompact,
		"mode": &ag.Mode, "group_eagerness": &ag.GroupEagerness, "group_quiet": &ag.GroupQuiet, "group_min_gap": &ag.GroupMinGap,
		"life_awake": &ag.LifeAwake, "life_level": &ag.LifeLevel, "life_reach": &ag.LifeReach, "memory": &ag.Memory,
		"dream_when": &ag.DreamWhen, "dream_at": &ag.DreamAt, "dream_idle": &ag.DreamIdle, "dream_model": &ag.DreamModel}
	ints := map[string]**int{"max_steps": &ag.MaxSteps, "keep_turns": &ag.KeepTurns, "max_output_kb": &ag.MaxOutputKB,
		"group_max_len": &ag.GroupMaxLen, "group_parts": &ag.GroupParts, "life_budget": &ag.LifeBudget, "dream_max": &ag.DreamMax}
	floats := map[string]**float64{"compact_at": &ag.CompactAt, "compact_to": &ag.CompactTo, "idle_compact_min": &ag.IdleCompactMin}
	if p, ok := strs[key]; ok {
		if v == nil {
			*p = nil
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 要填文字", key)
		}
		s = strings.TrimSpace(s)
		enums := map[string][]string{"mode": {"full", "auto", "ask", "readonly"}, "group_eagerness": {"off", "low", "normal", "high"},
			"life_level": {"low", "normal", "high"}, "life_reach": {"owner", "all", "off"}, "memory": {"on", "off"},
			"dream_when": {"idle", "nightly", "sleep", "off"}}
		switch {
		case enums[key] != nil:
			if !slices.Contains(enums[key], s) {
				return fmt.Errorf("%s 只能是 %s", key, strings.Join(enums[key], "、"))
			}
		case key == "group_quiet" || key == "group_min_gap":
			if _, err := time.ParseDuration(s); err != nil {
				return fmt.Errorf("%s 的写法不对，比如 20s、1m", key)
			}
		case key == "run_timeout" || key == "approval_timeout" || key == "idle_compact" || key == "dream_idle":
			if s != "" && s != "0" {
				if _, err := time.ParseDuration(s); err != nil {
					return fmt.Errorf("%s 的写法不对，比如 30m、2h、0", key)
				}
			}
		case key == "life_awake":
			if _, err := parseWindow(s); err != nil {
				return err
			}
		case key == "dream_at":
			if _, err := parseClock(s); err != nil {
				return err
			}
		}
		*p = &s
		return nil
	}
	if p, ok := ints[key]; ok {
		if v == nil {
			*p = nil
			return nil
		}
		n, ok := toInt(v)
		switch {
		case !ok || n < 0:
			return fmt.Errorf("%s 要填不小于 0 的整数", key)
		case (key == "group_max_len" || key == "group_parts" || key == "dream_max") && n == 0:
			return fmt.Errorf("%s 要填大于 0 的整数", key)
		case key == "group_parts" && n > 5:
			return errors.New("最多拆成 5 条")
		}
		*p = &n
		return nil
	}
	if p, ok := floats[key]; ok {
		if v == nil {
			*p = nil
			return nil
		}
		f, ok := toFloat(v)
		if !ok || f <= 0 || f >= 1 {
			return fmt.Errorf("%s 要是 0 到 1 之间的小数", key)
		}
		*p = &f
		return nil
	}
	return fmt.Errorf("不认识设置项 %s", key)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// CreateAgent 新建一个 agent。
func (a *App) CreateAgent(p AgentPatch) (AgentInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil {
		return AgentInfo{}, errors.New("agent 列表还没准备好")
	}
	ag := agents.Agent{}
	if err := applyAgentPatch(&ag, p); err != nil {
		return AgentInfo{}, err
	}
	if strings.TrimSpace(ag.Name) == "" {
		return AgentInfo{}, errors.New("要给 agent 起个名字")
	}
	if other, ok := a.agentByName(ag.Name); ok {
		return AgentInfo{}, fmt.Errorf("已经有叫「%s」的 agent 了（%s），换个名字", other.Name, other.ID)
	}
	ag.ID = agentIDSuggestion(a.agentStore, ag.Name)
	if err := a.agentStore.Create(ag); err != nil {
		return AgentInfo{}, err
	}
	if err := os.MkdirAll(filepath.Join(a.agentRoot, ag.ID), 0o700); err != nil {
		a.log.Warn("建 agent 目录失败", "id", ag.ID, "err", err)
	}
	a.log.Info("新建 agent", "id", ag.ID, "name", ag.Name)
	a.lifeBegin(ag)
	a.publishAgents()
	got, _ := a.agentStore.Get(ag.ID)
	return a.agentInfo(got), nil
}

// agentIDSuggestion 按名字起一个没被占用的 ID（中文名退到 agent、agent-2……）。
// agentByName 按名字找（不分大小写和首尾空格）。调用方持有锁。
func (a *App) agentByName(name string) (agents.Agent, bool) {
	if a.agentStore == nil {
		return agents.Agent{}, false
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for _, ag := range a.agentStore.List() {
		if strings.ToLower(strings.TrimSpace(ag.Name)) == want {
			return ag, true
		}
	}
	return agents.Agent{}, false
}

func agentIDSuggestion(s *agents.Store, name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteString("-")
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		// 名字里没有拉丁字母（比如“小鲍”）：用短的随机 ID，面板上显示名字
		for {
			id := "a-" + randHex(3)
			if _, ok := s.Get(id); !ok {
				return id
			}
		}
	}
	if _, ok := s.Get(base); !ok {
		return base
	}
	for i := 2; i < 100; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if _, ok := s.Get(id); !ok {
			return id
		}
	}
	return fmt.Sprintf("agent-%d", time.Now().UnixNano())
}

// UpdateAgent 改一个 agent。
func (a *App) UpdateAgent(id string, p AgentPatch) (AgentInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil {
		return AgentInfo{}, errors.New("agent 列表还没准备好")
	}
	ag, ok := a.agentStore.Get(id)
	if !ok {
		return AgentInfo{}, errors.New("没有这个 agent")
	}
	if err := applyAgentPatch(&ag, p); err != nil {
		return AgentInfo{}, err
	}
	if other, ok := a.agentByName(ag.Name); ok && other.ID != id {
		return AgentInfo{}, fmt.Errorf("已经有叫「%s」的 agent 了（%s），换个名字", other.Name, other.ID)
	}
	if err := a.agentStore.Update(ag); err != nil {
		return AgentInfo{}, err
	}
	a.log.Info("改了 agent", "id", id)
	a.lifeBegin(ag)
	a.publishAgents()
	got, _ := a.agentStore.Get(id)
	return a.agentInfo(got), nil
}

// DeleteAgent 删除一个 agent。默认 agent 不能删；还有机器人绑着它时要先换绑。
func (a *App) DeleteAgent(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil {
		return errors.New("agent 列表还没准备好")
	}
	if id == agents.DefaultID {
		return errors.New("默认 agent 不能删除")
	}
	// 机器人不能没有 agent：删的时候把它们交给默认 agent，不然用户得先一个个换绑
	if a.botStore != nil {
		for _, b := range a.botStore.OfAgent(id) {
			if err := a.botStore.Bind(b.ID, agents.DefaultID); err != nil {
				return fmt.Errorf("把机器人「%s」交给默认 agent 失败：%w", b.Name, err)
			}
			a.log.Info("agent 删掉了，机器人交给默认 agent", "bot", b.ID, "agent", agents.DefaultID)
		}
	}
	// 群里的成员/禁言名单也去掉它，免得群面板里人数对不上
	a.dropAgentFromRooms(id)
	if err := a.agentStore.Delete(id); err != nil {
		return err
	}
	a.log.Info("删除 agent", "id", id)
	a.publishAgents()
	return nil
}

// dropAgentFromRooms 把某个 agent 从所有群里去掉（成员和禁言名单）。调用方持有锁。
func (a *App) dropAgentFromRooms(id string) {
	if a.roomStore == nil {
		return
	}
	for _, r := range a.roomStore.List() {
		changed := false
		if members := withoutString(r.Members, id); len(members) != len(r.Members) {
			r.Members = members
			changed = true
		}
		if muted := withoutString(r.Muted, id); len(muted) != len(r.Muted) {
			r.Muted = muted
			changed = true
		}
		if !changed {
			continue
		}
		// 私聊少了一个人就没意义了：连这个房间一起删（聊天记录还在会话里）
		if r.IsDM() && len(r.Members) < 2 {
			if err := a.roomStore.Delete(r.ID); err != nil {
				a.log.Warn("两个人的私聊少了一个人，没删掉", "room", r.ID, "err", err)
			}
			continue
		}
		if len(r.Members) == 0 {
			if err := a.roomStore.Delete(r.ID); err != nil {
				a.log.Warn("清空成员的群没删掉", "room", r.ID, "err", err)
			}
			continue
		}
		if err := a.roomStore.Update(r); err != nil {
			a.log.Warn("群成员没更新", "room", r.ID, "err", err)
		}
	}
	a.publish(events.Event{T: "rooms"})
}

// withoutString 返回去掉 x 以后的新切片。
func withoutString(xs []string, x string) []string {
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// AgentPersona 是某个 agent 的人格文本。
func (a *App) AgentPersona(id string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, ok := a.agentByID(id)
	if !ok {
		return "", errors.New("没有这个 agent")
	}
	if a.personaAgent == nil {
		return "", nil
	}
	t, _ := a.personaAgent.File(filepath.Join(ag.ID, "persona.md"))
	return t, nil
}

// SaveAgentPersona 保存某个 agent 的人格（保存后下一条消息就生效）。
func (a *App) SaveAgentPersona(id, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, ok := a.agentByID(id)
	if !ok {
		return errors.New("没有这个 agent")
	}
	dir := filepath.Join(a.agentRoot, ag.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "persona.md")
	text = strings.TrimSpace(text)
	if text == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		a.publish(events.Event{T: "personas"})
		a.publishAgents()
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	a.log.Info("保存了 agent 的人格", "id", ag.ID, "chars", len([]rune(text)))
	a.publish(events.Event{T: "personas"})
	a.publishAgents()
	return nil
}

// cmdAgent 是 /agent：告诉用户现在在和哪个 agent 说话。
func (a *App) cmdAgent(c in) {
	if c.s == nil || c.s.sid == "" {
		a.answer(c, "还没有接到会话：发一条消息再说 /agent")
		return
	}
	ag := a.agentOf(c.s.sid)
	var b strings.Builder
	if ag.ID == agents.DefaultID {
		b.WriteString("你在和默认 agent「" + ag.Name + "」说话。")
	} else {
		b.WriteString("你在和 agent「" + ag.Name + "」说话（" + ag.ID + "）。")
	}
	if m, _, ok := a.modelFor(ag); ok {
		b.WriteString("\n它用的模型：" + modelLabel(m))
	}
	if ag.Life() {
		b.WriteString("\n类型：拟人型（有作息，没事时自己上网、想事情、找人聊天；")
		if lb := a.lifeBrief(ag); lb != nil {
			b.WriteString(map[string]string{"sleeping": "现在在睡觉", "awake": "现在醒着", "thinking": "现在正在想事情"}[lb.State])
		}
		b.WriteString("）")
	} else {
		b.WriteString("\n类型：工具型（有人找才干活）")
	}
	if a.memoryOn(ag) {
		total, people, _ := a.memStore(ag.ID).Counts()
		fmt.Fprintf(&b, "\n长期记忆：%d 条，认识 %d 个人（/memory 看）", total, people)
	} else if a.agentRoot != "" {
		b.WriteString("\n长期记忆：关着")
	}
	if bot, ok := a.botForChannel(c.ep.ch); ok {
		b.WriteString("\n这个窗口的机器人：" + bot.Name + "（" + agents.ChannelName(bot) + "）")
	}
	b.WriteString("\n换 agent 就是换个机器人聊。管理员可以在网页拓扑图的 Agent 列里新建 agent、改人格、给它们绑机器人。")
	a.answer(c, b.String())
}

// publishAgents 通知网页 agent 列表变了。
func (a *App) publishAgents() {
	a.publish(events.Event{T: "agents"})
}
