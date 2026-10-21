// Package config 读取网关配置（TOML）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Kernel    Kernel    `toml:"kernel" json:"kernel"`
	LLM       LLM       `toml:"llm" json:"llm"`
	Persona   Persona   `toml:"persona" json:"persona"`
	Session   Session   `toml:"session" json:"session"`
	Access    Access    `toml:"access" json:"access"`
	Workspace Workspace `toml:"workspace" json:"workspace"`
	CLI       CLI       `toml:"cli" json:"cli"`
	Telegram  Telegram  `toml:"telegram" json:"telegram"`
	Feishu    Feishu    `toml:"feishu" json:"feishu"`
	Web       Web       `toml:"web" json:"web"`

	Dir string `toml:"-" json:"-"` // 配置文件所在目录（里面有密钥，不能被设为工作目录）
}

type Kernel struct {
	Listen string `toml:"listen" json:"listen"` // unix:/path 或 tcp:host:port
	Token  string `toml:"token" json:"token"`   // 与内核配置里的 token 一致
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

	// 思考强度。thinking 是默认值（空表示用模型自己的默认），每个会话可以用 /think 改；
	// thinking_style 是传给模型的方式：vllm（chat_template_kwargs）| openai（reasoning_effort）| none；
	// thinking_levels 是 /think 可选的值。
	Thinking       string   `toml:"thinking" json:"thinking"`
	ThinkingStyle  string   `toml:"thinking_style" json:"thinking_style"`
	ThinkingLevels []string `toml:"thinking_levels" json:"thinking_levels"`
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
	AllowChats []string `toml:"allow_chats" json:"allow_chats"` // 会话 ID 白名单，空表示不限制
	Admins     []string `toml:"admins" json:"admins"`           // 管理员身份；命令行用户始终是管理员
	Guest      string   `toml:"guest" json:"guest"`             // 非管理员能用的工具：none（默认，只聊天）| readonly
}

// Workspace 是工具能碰到的范围：工作目录之外的文件一律读写不了（由内核的沙箱保证）。
// 这里是默认值，每个会话可以用 /dir、/mode 单独改。
type Workspace struct {
	Dirs    []string `toml:"dirs" json:"dirs"`         // 工作目录，第一个是默认目录
	Mode    string   `toml:"mode" json:"mode"`         // auto | ask（默认）| readonly
	ExtraRO []string `toml:"extra_ro" json:"extra_ro"` // 沙箱里额外只读的路径，比如 ~/.gitconfig
	ExtraRW []string `toml:"extra_rw" json:"extra_rw"` // 沙箱里额外可写的路径，比如某些工具的缓存目录
}

type CLI struct {
	Enabled       bool  `toml:"enabled" json:"enabled"`
	ShowReasoning *bool `toml:"show_reasoning" json:"show_reasoning"` // 显示模型的思考过程，默认开
	ShowUsage     *bool `toml:"show_usage" json:"show_usage"`         // 每轮结束后显示用量和缓存命中率，默认开
}

// On 读取一个默认为 true 的开关。
func On(b *bool) bool { return b == nil || *b }

type Telegram struct {
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
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, md, nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		if len(k) > 1 && k[0] == "llm" && k[1] == "extra" {
			continue
		}
		warnings = append(warnings, "未知配置项："+k.String())
	}

	c.applyEnv()
	if len(overlay) > 0 {
		if err := json.Unmarshal(overlay, &c); err != nil {
			return nil, md, warnings, fmt.Errorf("网页里保存的配置有误：%w", err)
		}
	}
	c.applyDefaults()
	if !md.IsDefined("workspace", "extra_ro") && !strings.Contains(string(overlay), `"extra_ro"`) {
		c.Workspace.ExtraRO = []string{"~/.gitconfig", "~/.config/git"} // git 要读用户名和邮箱
	}
	base := filepath.Dir(path)
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	c.Dir = base
	c.Persona.Dir = resolve(base, c.Persona.Dir)
	c.Session.DataDir = resolve(base, c.Session.DataDir)
	if strings.HasPrefix(c.Kernel.Listen, "unix:") {
		c.Kernel.Listen = "unix:" + resolve(base, strings.TrimPrefix(c.Kernel.Listen, "unix:"))
	}
	return &c, md, warnings, c.validate()
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
	def(&c.Session.DataDir, "data")
	def(&c.Telegram.APIBase, "https://api.telegram.org")
	def(&c.Feishu.Domain, "feishu")
	def(&c.Web.Listen, "127.0.0.1:7788")
	def(&c.Workspace.Mode, "ask")
	def(&c.Access.Guest, "none")
	def(&c.LLM.ThinkingStyle, "none")
	if c.LLM.TrimTo <= 0 || c.LLM.TrimTo >= 1 {
		c.LLM.TrimTo = 0.5
	}
	if len(c.LLM.ThinkingLevels) == 0 {
		switch c.LLM.ThinkingStyle {
		case "vllm":
			c.LLM.ThinkingLevels = []string{"off", "low", "high", "max"}
		case "openai":
			c.LLM.ThinkingLevels = []string{"off", "low", "medium", "high"}
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Kernel.Token == "" {
		errs = append(errs, errors.New("kernel.token 不能为空"))
	}
	switch c.LLM.Provider {
	case "openai":
		if c.LLM.BaseURL == "" || c.LLM.Model == "" {
			errs = append(errs, errors.New("llm.base_url 和 llm.model 不能为空"))
		}
	case "mock":
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
	switch c.LLM.ThinkingStyle {
	case "vllm", "openai", "none":
	default:
		errs = append(errs, fmt.Errorf("llm.thinking_style 只能是 vllm、openai 或 none，现在是 %q", c.LLM.ThinkingStyle))
	}
	if c.LLM.Thinking != "" && c.LLM.ThinkingStyle == "none" {
		errs = append(errs, errors.New("设置了 llm.thinking，但 llm.thinking_style 是 none（不知道怎么传给模型）"))
	}
	if c.Session.Timezone != "" {
		if _, err := time.LoadLocation(c.Session.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("session.timezone：%w", err))
		}
	}
	switch c.Workspace.Mode {
	case "auto", "ask", "readonly":
	default:
		errs = append(errs, fmt.Errorf("workspace.mode 只能是 auto、ask 或 readonly，现在是 %q", c.Workspace.Mode))
	}
	if c.Access.Guest != "none" && c.Access.Guest != "readonly" {
		errs = append(errs, fmt.Errorf("access.guest 只能是 none 或 readonly，现在是 %q", c.Access.Guest))
	}
	for _, d := range c.Workspace.Dirs {
		if !strings.HasPrefix(d, "/") && !strings.HasPrefix(d, "~") {
			errs = append(errs, fmt.Errorf("workspace.dirs 里的 %q 要写成绝对路径或 ~ 开头", d))
		}
	}
	if !c.CLI.Enabled && !c.Telegram.Enabled && !c.Feishu.Enabled {
		errs = append(errs, errors.New("cli、telegram、feishu 至少要启用一个"))
	}
	return errors.Join(errs...)
}

func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
