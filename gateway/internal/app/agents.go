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
	if ag := a.agentOf(sid); ag.Mode != nil && *ag.Mode != "" {
		return *ag.Mode
	}
	return a.cfg.Workspace.Mode
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
}

// botBrief 是机器人的概况（渠道状态网页从概况接口里读）。调用方持有锁。
func (a *App) botBrief(b agents.Bot) BotBrief {
	return BotBrief{ID: b.ID, Kind: b.Kind, Name: b.Name, Channel: agents.ChannelName(b), Enabled: b.Enabled}
}

// agentSettings 是一个 agent 可以覆盖的项，带全局值、自己的值和实际生效的值。调用方持有锁。
func (a *App) agentSettings(ag agents.Agent) []AgentSetting {
	c := a.cfg
	at, to, keep := a.contextConfig()
	strOrEmpty := func(s string) any {
		if strings.TrimSpace(s) == "" {
			return ""
		}
		return s
	}
	out := []AgentSetting{
		{Key: "max_steps", Label: "工具调用轮数上限", Type: "int", Global: c.Agent.Steps(),
			Help:      "一条消息里最多请求模型几次，0 表示不限",
			Value:     ptrAny(ag.MaxSteps),
			Effective: pickInt(ag.MaxSteps, c.Agent.Steps())},
		{Key: "run_timeout", Label: "最长处理多久", Type: "string",
			Help:      "例如 30m、2h；0 表示不限",
			Global:    strOrEmpty(c.Agent.RunTimeout),
			Value:     ptrAny(ag.RunTimeout),
			Effective: durText(pickDuration(ag.RunTimeout, c.Agent.RunLimit()))},
		{Key: "approval_timeout", Label: "等审批多久", Type: "string",
			Help:      "例如 30m；0 表示一直等",
			Global:    strOrEmpty(c.Agent.ApprovalTimeout),
			Value:     ptrAny(ag.ApprovalTimeout),
			Effective: durText(pickDuration(ag.ApprovalTimeout, c.Agent.ApprovalLimit()))},
		{Key: "max_output_kb", Label: "工具结果最多保留（KB）", Type: "int",
			Help:      "一次工具输出超过这么多就截断",
			Global:    c.Agent.OutputBytes() >> 10,
			Value:     ptrAny(ag.MaxOutputKB),
			Effective: pickPositive(ag.MaxOutputKB, c.Agent.OutputBytes()>>10)},
		{Key: "mode", Label: "默认权限模式", Type: "enum", Options: []string{"auto", "ask", "readonly"},
			Help:      "新会话用的权限模式，会话里还能单独改",
			Global:    c.Workspace.Mode,
			Value:     ptrAny(ag.Mode),
			Effective: pickString(ag.Mode, c.Workspace.Mode)},
		{Key: "compact_at", Label: "自动整理阈值", Type: "float",
			Help:      "上下文到模型窗口的这个比例时自动整理",
			Global:    at,
			Value:     ptrAny(ag.CompactAt),
			Effective: pickRatio(ag.CompactAt, at)},
		{Key: "compact_to", Label: "整理后目标比例", Type: "float",
			Global:    to,
			Value:     ptrAny(ag.CompactTo),
			Effective: pickRatio(ag.CompactTo, to)},
		{Key: "keep_turns", Label: "原样保留最近几轮", Type: "int",
			Global:    keep,
			Value:     ptrAny(ag.KeepTurns),
			Effective: pickPositive(ag.KeepTurns, keep)},
		{Key: "idle_compact", Label: "空闲整理间隔", Type: "string",
			Help:      "会话空闲这么久后在后台整理一次，例如 30m；0 关闭",
			Global:    strOrEmpty(c.Context.IdleCompact),
			Value:     ptrAny(ag.IdleCompact),
			Effective: pickString(ag.IdleCompact, c.Context.IdleCompact)},
		{Key: "idle_compact_min", Label: "空闲整理最低占比", Type: "float",
			Global:    c.Context.IdleCompactMin,
			Value:     ptrAny(ag.IdleCompactMin),
			Effective: pickPositiveFloat(ag.IdleCompactMin, c.Context.IdleCompactMin)},
		{Key: "group_eagerness", Label: "群里插话积极度", Type: "enum", Options: []string{"off", "low", "normal", "high"},
			Help:      "off 只回被叫到的；low 每小时最多主动说 2 次；normal 6 次；high 15 次；要不要说话的判断本身也是一次模型调用，所以每小时最多判断三倍这么多次、两次判断至少隔一分钟",
			Global:    "low",
			Value:     ptrAny(ag.GroupEagerness),
			Effective: pickString(ag.GroupEagerness, "low")},
		{Key: "group_quiet", Label: "群里安静多久才插话", Type: "string",
			Help:      "群里安静这么久以后才可能主动说一句，例如 20s、1m",
			Global:    "20s",
			Value:     ptrAny(ag.GroupQuiet),
			Effective: pickString(ag.GroupQuiet, "20s")},
		{Key: "group_min_gap", Label: "两次发言最短间隔", Type: "string",
			Global:    "10s",
			Value:     ptrAny(ag.GroupMinGap),
			Effective: pickString(ag.GroupMinGap, "10s")},
		{Key: "group_max_len", Label: "群聊一条回复最多（字）", Type: "int",
			Global:    300,
			Value:     ptrAny(ag.GroupMaxLen),
			Effective: pickPositive(ag.GroupMaxLen, 300)},
		{Key: "group_parts", Label: "最多拆成几条发", Type: "int",
			Global:    3,
			Value:     ptrAny(ag.GroupParts),
			Effective: pickPositive(ag.GroupParts, 3)},
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
	info.ToolsAvailable = a.toolNames()
	for _, sid := range a.sessionIDs() {
		if agents.SessionAgent(sid) != ag.ID {
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

// applyAgentSetting 设置一个可覆盖的项；v 是 nil 表示恢复跟随全局。
func applyAgentSetting(ag *agents.Agent, key string, v any) error {
	clear := func() {
		switch key {
		case "max_steps":
			ag.MaxSteps = nil
		case "keep_turns":
			ag.KeepTurns = nil
		case "max_output_kb":
			ag.MaxOutputKB = nil
		case "compact_at":
			ag.CompactAt = nil
		case "compact_to":
			ag.CompactTo = nil
		case "idle_compact_min":
			ag.IdleCompactMin = nil
		case "run_timeout":
			ag.RunTimeout = nil
		case "approval_timeout":
			ag.ApprovalTimeout = nil
		case "idle_compact":
			ag.IdleCompact = nil
		case "mode":
			ag.Mode = nil
		}
	}
	if v == nil {
		clear()
		return nil
	}
	switch key {
	case "group_max_len", "group_parts":
		if v == nil {
			if key == "group_max_len" {
				ag.GroupMaxLen = nil
			} else {
				ag.GroupParts = nil
			}
			return nil
		}
		n, ok := toInt(v)
		if !ok || n <= 0 {
			return fmt.Errorf("%s 要填大于 0 的整数", key)
		}
		if key == "group_max_len" {
			ag.GroupMaxLen = &n
		} else {
			if n > 5 {
				return errors.New("最多拆成 5 条")
			}
			ag.GroupParts = &n
		}
	case "max_steps", "keep_turns", "max_output_kb":
		n, ok := toInt(v)
		if !ok || n < 0 {
			return fmt.Errorf("%s 要填不小于 0 的整数", key)
		}
		switch key {
		case "max_steps":
			ag.MaxSteps = &n
		case "keep_turns":
			ag.KeepTurns = &n
		default:
			ag.MaxOutputKB = &n
		}
	case "compact_at", "compact_to", "idle_compact_min":
		f, ok := toFloat(v)
		if !ok || f <= 0 || f >= 1 {
			return fmt.Errorf("%s 要是 0 到 1 之间的小数", key)
		}
		switch key {
		case "compact_at":
			ag.CompactAt = &f
		case "compact_to":
			ag.CompactTo = &f
		default:
			ag.IdleCompactMin = &f
		}
	case "group_eagerness":
		if v == nil {
			ag.GroupEagerness = nil
			return nil
		}
		s, ok := v.(string)
		if !ok || !slices.Contains([]string{"off", "low", "normal", "high"}, s) {
			return errors.New("插话积极度只能是 off、low、normal、high")
		}
		ag.GroupEagerness = &s
	case "group_quiet", "group_min_gap":
		if v == nil {
			if key == "group_quiet" {
				ag.GroupQuiet = nil
			} else {
				ag.GroupMinGap = nil
			}
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 要填时长，比如 20s", key)
		}
		s = strings.TrimSpace(s)
		if _, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("%s 的写法不对，比如 20s、1m", key)
		}
		if key == "group_quiet" {
			ag.GroupQuiet = &s
		} else {
			ag.GroupMinGap = &s
		}
	case "run_timeout", "approval_timeout", "idle_compact":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 要填时长，比如 30m", key)
		}
		s = strings.TrimSpace(s)
		if s != "" && s != "0" {
			if _, err := time.ParseDuration(s); err != nil {
				return fmt.Errorf("%s 的写法不对，比如 30m、2h、0", key)
			}
		}
		switch key {
		case "run_timeout":
			ag.RunTimeout = &s
		case "approval_timeout":
			ag.ApprovalTimeout = &s
		default:
			ag.IdleCompact = &s
		}
	case "mode":
		s, ok := v.(string)
		if !ok || !slices.Contains([]string{"auto", "ask", "readonly"}, s) {
			return errors.New("权限模式只能是 auto、ask、readonly")
		}
		ag.Mode = &s
	default:
		return fmt.Errorf("不认识设置项 %s", key)
	}
	return nil
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
	ag.ID = agentIDSuggestion(a.agentStore, ag.Name)
	if err := a.agentStore.Create(ag); err != nil {
		return AgentInfo{}, err
	}
	if err := os.MkdirAll(filepath.Join(a.agentRoot, ag.ID), 0o700); err != nil {
		a.log.Warn("建 agent 目录失败", "id", ag.ID, "err", err)
	}
	a.log.Info("新建 agent", "id", ag.ID, "name", ag.Name)
	a.publishAgents()
	got, _ := a.agentStore.Get(ag.ID)
	return a.agentInfo(got), nil
}

// agentIDSuggestion 按名字起一个没被占用的 ID（中文名退到 agent、agent-2……）。
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
	if err := a.agentStore.Update(ag); err != nil {
		return AgentInfo{}, err
	}
	a.log.Info("改了 agent", "id", id)
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
	if a.botStore != nil && len(a.botStore.OfAgent(id)) > 0 {
		return errors.New("还有机器人绑着这个 agent，先在机器人节点里换绑或者删掉")
	}
	if err := a.agentStore.Delete(id); err != nil {
		return err
	}
	a.log.Info("删除 agent", "id", id)
	a.publishAgents()
	return nil
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
