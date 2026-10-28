// Package config 读取网关配置（TOML）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"nano-agent/gateway/internal/paths"
)

type Config struct {
	Runtime   Runtime   `toml:"runtime" json:"runtime"`
	Agent     Agent     `toml:"agent" json:"agent"`
	Search    Search    `toml:"search" json:"search"`
	Kernel    Kernel    `toml:"kernel" json:"kernel"`
	Channels  Channels  `toml:"channels" json:"channels"`
	Context   Context   `toml:"context" json:"context"`
	LLM       LLM       `toml:"llm" json:"llm"`
	Persona   Persona   `toml:"persona" json:"persona"`
	Skills    Skills    `toml:"skills" json:"skills"`
	Session   Session   `toml:"session" json:"session"`
	Access    Access    `toml:"access" json:"access"`
	Workspace Workspace `toml:"workspace" json:"workspace"`
	CLI       CLI       `toml:"cli" json:"cli"`
	Telegram  Telegram  `toml:"telegram" json:"telegram"`
	Feishu    Feishu    `toml:"feishu" json:"feishu"`
	Web       Web       `toml:"web" json:"web"`
	Network   Network   `toml:"network" json:"network"`
	Security  Security  `toml:"security" json:"security"`
	Sandbox   Sandbox   `toml:"sandbox" json:"sandbox"`
	Update    Update    `toml:"update" json:"update"`

	Dir string `toml:"-" json:"-"` // 配置文件所在目录（里面有密钥，不能被设为工作目录）
}

// Sandbox 是执行隔离的请求策略（设计文档 §5）。和审批策略分开表达：
//   - auto：用当前可用的实现；没有完整隔离时保留旧行为（Shell 审批）
//   - on：要求完整隔离，机器给不了就明确失败，不静默降级
//   - off：明确跳过 Landlock / seccomp / no_new_privs，按当前 OS 用户身份执行
//
// 执行节点自己的 kernel.conf 里 sandbox 更严时以节点为准（节点要求不能被网关绕开）。
type Sandbox struct {
	Mode string `toml:"mode" json:"mode"` // auto（默认）/ on / off
}

// Mode 是请求的沙箱模式，空表示 auto。
func (s Sandbox) ModeOrDefault() string {
	switch s.Mode {
	case "on", "off":
		return s.Mode
	}
	return "auto"
}

// Agent 是每条消息怎么跑：最多请求模型几次、最长多久、等审批多久、工具结果留多少。
// 网关每一轮都把它们带给内核，kernel.conf 里的同名项只在连着旧网关时兜底。
type Agent struct {
	MaxSteps        *int   `toml:"max_steps" json:"max_steps"`               // 最多请求模型几次，0 不限，默认 200
	RunTimeout      string `toml:"run_timeout" json:"run_timeout"`           // 最长处理多久（30m、2h），0 不限（默认）
	ApprovalTimeout string `toml:"approval_timeout" json:"approval_timeout"` // 等审批多久，0 一直等，默认 30m
	MaxOutputKB     int    `toml:"max_output_kb" json:"max_output_kb"`       // 一个工具结果最多保留多少 KB，默认 64
}

// DefaultMaxSteps 是每条消息默认最多请求模型的次数。
const DefaultMaxSteps = 200

// Steps 是每条消息最多请求模型几次，0 表示不限。
func (a Agent) Steps() int {
	if a.MaxSteps == nil {
		return DefaultMaxSteps
	}
	return *a.MaxSteps
}

// RunLimit 是每条消息最长处理多久，0 表示不限。
func (a Agent) RunLimit() time.Duration { return parseLimit(a.RunTimeout) }

// ApprovalLimit 是等审批的最长时间，0 表示一直等；没写是 30 分钟。
func (a Agent) ApprovalLimit() time.Duration {
	if strings.TrimSpace(a.ApprovalTimeout) == "" {
		return 30 * time.Minute
	}
	return parseLimit(a.ApprovalTimeout)
}

// OutputBytes 是一个工具结果最多保留多少字节。
func (a Agent) OutputBytes() int {
	if a.MaxOutputKB <= 0 {
		return 64 << 10
	}
	return a.MaxOutputKB << 10
}

func parseLimit(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// maxLimit 是时限最长能设多久：发给内核时是毫秒数（32 位整数）
const maxLimit = 7 * 24 * time.Hour

type Search struct {
	Enabled  *bool  `toml:"enabled" json:"enabled"`
	Provider string `toml:"provider" json:"provider"`
	APIKey   string `toml:"api_key" json:"api_key"`
	Count    int    `toml:"count" json:"count"`
}
type Channels struct {
	Show string `toml:"show" json:"show"`
}

type Context struct {
	CompactAt      float64 `toml:"compact_at" json:"compact_at"`
	CompactTo      float64 `toml:"compact_to" json:"compact_to"`
	KeepTurns      int     `toml:"keep_turns" json:"keep_turns"`
	IdleCompact    string  `toml:"idle_compact" json:"idle_compact"`
	IdleCompactMin float64 `toml:"idle_compact_min" json:"idle_compact_min"`
	ArchiveDays    int     `toml:"archive_days" json:"archive_days"`
}

func DefaultContext() Context { return Context{.75, .4, 4, "30m", .3, 90} }

type Kernel struct {
	Listen       string `toml:"listen" json:"listen"`               // unix:/path 或 tcp:host:port
	Token        string `toml:"token" json:"token"`                 // 与内核配置里的 token 一致
	DeviceListen string `toml:"device_listen" json:"device_listen"` // 额外的电脑接入地址，空表示关闭，可热更新
	Local        *bool  `toml:"local" json:"local"`                 // 网关自己在本机启动一个内核，默认开
	LocalName    string `toml:"local_name" json:"local_name"`       // 本机内核的名字，默认是 gateway
}

type LLM struct {
	Provider         string         `toml:"provider" json:"provider"` // openai | mock
	BaseURL          string         `toml:"base_url" json:"base_url"`
	APIKey           string         `toml:"api_key" json:"api_key"`
	Model            string         `toml:"model" json:"model"`
	ContextWindow    int            `toml:"context_window" json:"context_window"`
	MaxHistoryTokens int            `toml:"max_history_tokens" json:"max_history_tokens"`
	Timeout          int            `toml:"timeout" json:"timeout"` // 秒
	Proxy            string         `toml:"proxy" json:"proxy"`
	Temperature      *float64       `toml:"temperature" json:"temperature"`
	MaxTokens        int            `toml:"max_tokens" json:"max_tokens"`
	SendReasoning    bool           `toml:"send_reasoning" json:"send_reasoning"`
	Extra            map[string]any `toml:"extra" json:"extra"`

	// 历史超过 max_history_tokens 时，一次裁到它的这个比例，之后只追加，直到下次超出。
	// 不每次都裁一点，是为了让请求的前缀尽量不变，模型那边的前缀缓存才能命中。
	TrimTo float64 `toml:"trim_to" json:"trim_to"`

	// 思考强度，作为 reasoning_effort 原样传给模型（none 表示关闭）。thinking 是默认值
	// （空表示不传，用模型自己的默认），每个会话可以用 /think 改；thinking_levels 是可选的值。
	Thinking       string   `toml:"thinking" json:"thinking"`
	ThinkingLevels []string `toml:"thinking_levels" json:"thinking_levels"`

	// 这个模型能接收什么：text（文字）、image（图片）。默认只有 text；不是多模态的模型别加 image。
	Modalities []string `toml:"modalities" json:"modalities"`
}

// Network 是网关的网络设置。
type Network struct {
	// 网关的默认代理，模型和各个聊天渠道的代理留空时用它。
	// 空：跟随环境变量（HTTPS_PROXY 等）；none：直连；或者写代理地址。
	Proxy string `toml:"proxy" json:"proxy"`
}

// Security 是工具结果脱敏：off 不脱敏；public（默认）只在模型接口不在本机/内网、
// 以及发到 Telegram/飞书时脱敏；always 发给模型时总是脱敏。
type Security struct {
	Redact string `toml:"redact" json:"redact"`
}

// Update 是版本更新：网关自己只提示、要用户发起；内核自动跟网关保持同一个版本。
type Update struct {
	Check   *bool  `toml:"check" json:"check"`     // 每天查一次有没有新版本（只提示），默认开
	Kernels *bool  `toml:"kernels" json:"kernels"` // 内核版本和网关不一样时自动推送网关的版本，默认开
	URL     string `toml:"url" json:"url"`         // 下载地址（镜像）；空表示 NANO_DOWNLOAD_URL 或 GitHub Release
}

// Secrets 是网关自己保存的密钥原文（给脱敏用，不要写日志）。
func (c *Config) Secrets() []string {
	return []string{c.Kernel.Token, c.Web.Password, c.Telegram.Token, c.Feishu.AppSecret, c.LLM.APIKey, c.Search.APIKey}
}

// ProxyFor 是某个组件实际用的代理：组件自己没设置就用网关的默认代理。
func (c *Config) ProxyFor(p string) string {
	if p == "" {
		return c.Network.Proxy
	}
	return p
}

// LLMConfig、TelegramConfig、FeishuConfig 返回代理已经换成实际值的配置，给各组件用。
func (c *Config) LLMConfig() LLM {
	l := c.LLM
	l.Proxy = c.ProxyFor(l.Proxy)
	return l
}

func (c *Config) TelegramConfig() Telegram {
	t := c.Telegram
	t.Proxy = c.ProxyFor(t.Proxy)
	return t
}

func (c *Config) FeishuConfig() Feishu {
	f := c.Feishu
	f.Proxy = c.ProxyFor(f.Proxy)
	return f
}

type Persona struct {
	Dir string `toml:"dir" json:"dir"`
}

type Session struct {
	DataDir    string `toml:"data_dir" json:"data_dir"`
	Timestamps *bool  `toml:"timestamps" json:"timestamps"` // 在用户消息前加上发送时间，默认开
	Timezone   string `toml:"timezone" json:"timezone"`     // 时间戳用的时区，比如 Asia/Shanghai；空表示系统时区
}

type Access struct {
	AllowChats []string `toml:"allow_chats" json:"allow_chats"` // 聊天窗口 ID 白名单，空表示不限制
	Admins     []string `toml:"admins" json:"admins"`           // 管理员身份；命令行用户始终是管理员
	Guest      string   `toml:"guest" json:"guest"`             // 非管理员能用的工具：none（默认，只聊天）| readonly
}

// Workspace 是工具能碰到的范围：工作目录之外的文件一律读写不了（由内核的沙箱保证）。
// 这里是默认值，每个会话可以用 /dir、/mode 单独改。
type Workspace struct {
	// 旧版本的默认机器和默认工作目录：第一次启动时迁移到工作目录列表（data/workspaces.json），之后不再使用
	Node string   `toml:"node" json:"node"`
	Dirs []string `toml:"dirs" json:"dirs"`
	Mode string   `toml:"mode" json:"mode"` // full | auto | ask（默认）| readonly
}

// Skills：技能放在网关上，每个技能一个目录（SKILL.md 和附带文件）
type Skills struct {
	Dir string `toml:"dir" json:"dir"` // 默认是配置文件旁边的 skills/
}

type CLI struct {
	Enabled       *bool `toml:"enabled" json:"enabled"`               // 终端聊天（make chat 连进来），默认开
	ShowReasoning *bool `toml:"show_reasoning" json:"show_reasoning"` // 显示模型的思考过程，默认开
	ShowUsage     *bool `toml:"show_usage" json:"show_usage"`         // 每轮结束后显示用量和缓存命中率，默认开
}

// On 读取一个默认为 true 的开关。
func On(b *bool) bool { return b == nil || *b }

type Telegram struct {
	Show          string `toml:"show" json:"show"`
	Enabled       bool   `toml:"enabled" json:"enabled"`
	Token         string `toml:"token" json:"token"`
	Proxy         string `toml:"proxy" json:"proxy"`
	APIBase       string `toml:"api_base" json:"api_base"`
	GroupReplyAll bool   `toml:"group_reply_all" json:"group_reply_all"`
}

type Web struct {
	Listen   string `toml:"listen" json:"listen"`     // 网页管理界面的地址，默认 127.0.0.1:7788；"off" 表示不开
	Password string `toml:"password" json:"password"` // 登录密码；空表示自动生成一个，存在数据目录的 web.secret 里
}

type Feishu struct {
	Show          string `toml:"show" json:"show"`
	Enabled       bool   `toml:"enabled" json:"enabled"`
	AppID         string `toml:"app_id" json:"app_id"`
	AppSecret     string `toml:"app_secret" json:"app_secret"`
	Domain        string `toml:"domain" json:"domain"` // feishu | lark
	Proxy         string `toml:"proxy" json:"proxy"`
	GroupReplyAll bool   `toml:"group_reply_all" json:"group_reply_all"`
}

// Load 读取配置文件，填默认值，并把相对路径换算成相对配置文件所在目录。
// 返回的 warnings 是不认识的配置项（多半是拼错了）。
func Load(path string) (*Config, []string, error) {
	c, _, warnings, err := parse(path, nil)
	return c, warnings, err
}

// parse 读取配置文件，再把 overlay（网页里改过的配置，JSON）叠加上去。
// 优先级：配置文件 < 环境变量 < 网页。
func parse(path string, overlay []byte) (*Config, toml.MetaData, []string, error) {
	return parseCheck(path, overlay, true)
}

// parseRaw 不校验（token 可能已经被搬到 config.json，底稿里是空值；等叠加后再校验）。
func parseRaw(path string, overlay []byte) (*Config, toml.MetaData, []string, error) {
	return parseCheck(path, overlay, false)
}

func parseCheck(path string, overlay []byte, check bool) (*Config, toml.MetaData, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, toml.MetaData{}, nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	c := Config{Context: DefaultContext()}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return nil, md, nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	return finish(&c, md, overlay, path, check)
}

// finish 在已经解析出 TOML 之后叠加环境变量、网页修改和默认值，并换算路径。
func finish(c *Config, md toml.MetaData, overlay []byte, path string, check bool) (*Config, toml.MetaData, []string, error) {
	var warnings []string
	for _, k := range md.Undecoded() {
		if len(k) > 1 && k[0] == "llm" && k[1] == "extra" {
			continue
		}
		warnings = append(warnings, "未知配置项："+k.String())
	}

	c.applyEnv()
	if len(overlay) > 0 {
		if err := json.Unmarshal(overlay, c); err != nil {
			return nil, md, warnings, fmt.Errorf("网页里保存的配置有误：%w", err)
		}
	}
	c.applyDefaults()
	base := filepath.Dir(path)
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	c.Dir = base
	c.Persona.Dir = resolve(base, c.Persona.Dir)
	c.Skills.Dir = resolve(base, c.Skills.Dir)
	c.Session.DataDir = resolve(base, c.Session.DataDir)
	if strings.HasPrefix(c.Kernel.Listen, "unix:") {
		// 目录很深时（比如导入到较长的 NANO_HOME）换成短路径，否则 bind 失败、网关起不来
		c.Kernel.Listen = "unix:" + paths.ShortSocket(resolve(base, strings.TrimPrefix(c.Kernel.Listen, "unix:")))
	}
	if check {
		if err := c.validate(); err != nil {
			return c, md, warnings, err
		}
	}
	return c, md, warnings, nil
}

// 敏感信息也可以放在环境变量里，优先于配置文件。
func (c *Config) applyEnv() {
	for env, dst := range map[string]*string{
		"NANO_KERNEL_TOKEN":      &c.Kernel.Token,
		"NANO_LLM_API_KEY":       &c.LLM.APIKey,
		"NANO_TELEGRAM_TOKEN":    &c.Telegram.Token,
		"NANO_FEISHU_APP_SECRET": &c.Feishu.AppSecret,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
}

func (c *Config) applyDefaults() {
	c.Runtime = c.Runtime.Defaults()
	if c.Agent.MaxSteps == nil {
		n := DefaultMaxSteps
		c.Agent.MaxSteps = &n
	}
	if strings.TrimSpace(c.Agent.RunTimeout) == "" {
		c.Agent.RunTimeout = "0"
	}
	if strings.TrimSpace(c.Agent.ApprovalTimeout) == "" {
		c.Agent.ApprovalTimeout = "30m"
	}
	if c.Agent.MaxOutputKB <= 0 {
		c.Agent.MaxOutputKB = 64
	}
	if c.Search.Provider == "" {
		c.Search.Provider = "deepseek"
	}
	if c.Search.Count == 0 {
		c.Search.Count = 6
	}
	if c.Channels.Show == "" {
		c.Channels.Show = "过程"
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	defInt := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&c.Kernel.Listen, "unix:/tmp/nano-agent.sock")
	def(&c.LLM.Provider, "openai")
	defInt(&c.LLM.ContextWindow, 128000)
	defInt(&c.LLM.MaxHistoryTokens, 64000)
	if limit := c.LLM.ContextWindow * 3 / 4; c.LLM.MaxHistoryTokens > limit {
		c.LLM.MaxHistoryTokens = limit // 给工具定义、系统提示和输出留余量
	}
	defInt(&c.LLM.Timeout, 600)
	def(&c.Persona.Dir, "personas")
	def(&c.Skills.Dir, "skills")
	def(&c.Session.DataDir, "data")
	def(&c.Telegram.APIBase, "https://api.telegram.org")
	def(&c.Feishu.Domain, "feishu")
	def(&c.Web.Listen, "127.0.0.1:7788")
	def(&c.Workspace.Mode, "ask")
	def(&c.Access.Guest, "none")
	def(&c.Security.Redact, "public")
	if c.LLM.TrimTo <= 0 || c.LLM.TrimTo >= 1 {
		c.LLM.TrimTo = 0.5
	}
	if len(c.LLM.ThinkingLevels) == 0 {
		c.LLM.ThinkingLevels = []string{"none", "low", "medium", "high"}
	}
	if len(c.LLM.Modalities) == 0 {
		c.LLM.Modalities = []string{"text"}
	}
	if !slices.Contains(c.LLM.Modalities, "text") {
		c.LLM.Modalities = append([]string{"text"}, c.LLM.Modalities...)
	}
}

func (c *Config) validate() error {
	var errs []error
	if err := c.Runtime.Validate(); err != nil {
		errs = append(errs, err)
	}
	if n := c.Agent.Steps(); n < 0 || n > 100000 {
		errs = append(errs, errors.New("agent.max_steps：每条消息最多请求模型几次，0 表示不限，最多 100000"))
	}
	for _, f := range []struct{ name, v string }{{"agent.run_timeout", c.Agent.RunTimeout}, {"agent.approval_timeout", c.Agent.ApprovalTimeout}} {
		if d, err := time.ParseDuration(strings.TrimSpace(f.v)); err != nil || d < 0 || d > maxLimit {
			errs = append(errs, fmt.Errorf("%s 要写成 30m、2h 这样（0 表示不限），最长 7 天，现在是 %q", f.name, f.v))
		} else if d > 0 && d < time.Second {
			errs = append(errs, fmt.Errorf("%s 至少 1 秒（0 表示不限）", f.name))
		}
	}
	if c.Agent.MaxOutputKB < 1 || c.Agent.MaxOutputKB > 64<<10 {
		errs = append(errs, errors.New("agent.max_output_kb：工具结果最多保留多少 KB，1–65536"))
	}
	if c.Search.Provider != "deepseek" || c.Search.Count < 1 || c.Search.Count > 10 {
		errs = append(errs, errors.New("search：provider 只能是 deepseek，count 为 1–10"))
	}
	for _, show := range []string{c.Channels.Show, c.Telegram.Show, c.Feishu.Show} {
		if show != "" && show != "详细" && show != "过程" && show != "简洁" {
			errs = append(errs, errors.New("显示档位只能是详细、过程或简洁"))
		}
	}
	if c.Context.CompactAt <= 0 || c.Context.CompactAt >= 1 || c.Context.CompactTo <= 0 || c.Context.CompactTo >= c.Context.CompactAt || c.Context.KeepTurns < 1 || c.Context.IdleCompactMin < 0 || c.Context.IdleCompactMin >= 1 || c.Context.ArchiveDays < 0 {
		errs = append(errs, errors.New("context：比例需在 0 到 1 之间，compact_to 小于 compact_at，keep_turns 至少 1，archive_days 非负"))
	}
	if d, err := time.ParseDuration(c.Context.IdleCompact); err != nil || d < 0 {
		errs = append(errs, errors.New("context.idle_compact 要写成 30m 或 0"))
	}
	if c.Kernel.Token == "" {
		errs = append(errs, errors.New("kernel.token 不能为空"))
	}
	// [llm] 里的 provider/base_url/model 是旧版单模型配置，只在升级时迁移用；
	// 新装用户不需要写，模型全部在网页的模型节点里配置。
	switch c.LLM.Provider {
	case "", "openai", "mock":
	default:
		errs = append(errs, fmt.Errorf("llm.provider 只能是 openai 或 mock，现在是 %q", c.LLM.Provider))
	}
	if c.Telegram.Enabled && c.Telegram.Token == "" {
		errs = append(errs, errors.New("启用了 telegram，但 telegram.token 为空"))
	}
	if c.Feishu.Enabled && (c.Feishu.AppID == "" || c.Feishu.AppSecret == "") {
		errs = append(errs, errors.New("启用了 feishu，但 app_id / app_secret 为空"))
	}
	if c.Feishu.Domain != "feishu" && c.Feishu.Domain != "lark" {
		errs = append(errs, fmt.Errorf("feishu.domain 只能是 feishu 或 lark，现在是 %q", c.Feishu.Domain))
	}
	if c.Session.Timezone != "" {
		if _, err := time.LoadLocation(c.Session.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("session.timezone：%w", err))
		}
	}
	switch c.Sandbox.Mode {
	case "", "auto", "on", "off":
	default:
		errs = append(errs, fmt.Errorf("sandbox.mode 只能是 auto、on 或 off，现在是 %q", c.Sandbox.Mode))
	}
	switch c.Workspace.Mode {
	case "full", "auto", "ask", "readonly":
	default:
		errs = append(errs, fmt.Errorf("workspace.mode 只能是 full、auto、ask 或 readonly，现在是 %q", c.Workspace.Mode))
	}
	if c.Access.Guest != "none" && c.Access.Guest != "readonly" {
		errs = append(errs, fmt.Errorf("access.guest 只能是 none 或 readonly，现在是 %q", c.Access.Guest))
	}
	if c.Security.Redact != "off" && c.Security.Redact != "public" && c.Security.Redact != "always" {
		errs = append(errs, fmt.Errorf("security.redact 只能是 off、public 或 always，现在是 %q", c.Security.Redact))
	}
	for _, m := range c.LLM.Modalities {
		switch m {
		case "text", "image", "audio", "video":
		default:
			errs = append(errs, fmt.Errorf("llm.modalities 里的 %q 不认识（只有 text、image、audio、video）", m))
		}
	}
	for _, d := range c.Workspace.Dirs {
		if !strings.HasPrefix(d, "/") && !strings.HasPrefix(d, "~") {
			errs = append(errs, fmt.Errorf("workspace.dirs 里的 %q 要写成绝对路径或 ~ 开头", d))
		}
	}
	if !On(c.CLI.Enabled) && !c.Telegram.Enabled && !c.Feishu.Enabled && c.Web.Listen == "off" {
		errs = append(errs, errors.New("终端、网页、Telegram、飞书都关掉了，没有地方能聊天"))
	}
	return errors.Join(errs...)
}

func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
