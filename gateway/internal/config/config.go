// Package config 读取网关配置（TOML）。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Kernel    Kernel    `toml:"kernel"`
	LLM       LLM       `toml:"llm"`
	Persona   Persona   `toml:"persona"`
	Session   Session   `toml:"session"`
	Access    Access    `toml:"access"`
	Workspace Workspace `toml:"workspace"`
	CLI       CLI       `toml:"cli"`
	Telegram  Telegram  `toml:"telegram"`
	Feishu    Feishu    `toml:"feishu"`

	Dir string `toml:"-"` // 配置文件所在目录（里面有密钥，不能被设为工作目录）
}

type Kernel struct {
	Listen string `toml:"listen"` // unix:/path 或 tcp:host:port
	Token  string `toml:"token"`  // 与内核配置里的 token 一致
}

type LLM struct {
	Provider         string         `toml:"provider"` // openai | mock
	BaseURL          string         `toml:"base_url"`
	APIKey           string         `toml:"api_key"`
	Model            string         `toml:"model"`
	ContextWindow    int            `toml:"context_window"`
	MaxHistoryTokens int            `toml:"max_history_tokens"`
	Timeout          int            `toml:"timeout"` // 秒
	Proxy            string         `toml:"proxy"`
	Temperature      *float64       `toml:"temperature"`
	MaxTokens        int            `toml:"max_tokens"`
	SendReasoning    bool           `toml:"send_reasoning"`
	Extra            map[string]any `toml:"extra"`
}

type Persona struct {
	Dir string `toml:"dir"`
}

type Session struct {
	DataDir string `toml:"data_dir"`
}

type Access struct {
	AllowChats []string `toml:"allow_chats"` // 会话 ID 白名单，空表示不限制
	Admins     []string `toml:"admins"`      // 管理员身份；命令行用户始终是管理员
	Guest      string   `toml:"guest"`       // 非管理员能用的工具：none（默认，只聊天）| readonly
}

// Workspace 是工具能碰到的范围：工作目录之外的文件一律读写不了（由内核的沙箱保证）。
// 这里是默认值，每个会话可以用 /dir、/mode 单独改。
type Workspace struct {
	Dirs    []string `toml:"dirs"`     // 工作目录，第一个是默认目录
	Mode    string   `toml:"mode"`     // auto | ask（默认）| readonly
	ExtraRO []string `toml:"extra_ro"` // 沙箱里额外只读的路径，比如 ~/.gitconfig
	ExtraRW []string `toml:"extra_rw"` // 沙箱里额外可写的路径，比如某些工具的缓存目录
}

type CLI struct {
	Enabled bool `toml:"enabled"`
}

type Telegram struct {
	Enabled       bool   `toml:"enabled"`
	Token         string `toml:"token"`
	Proxy         string `toml:"proxy"`
	APIBase       string `toml:"api_base"`
	GroupReplyAll bool   `toml:"group_reply_all"`
}

type Feishu struct {
	Enabled       bool   `toml:"enabled"`
	AppID         string `toml:"app_id"`
	AppSecret     string `toml:"app_secret"`
	Domain        string `toml:"domain"` // feishu | lark
	Proxy         string `toml:"proxy"`
	GroupReplyAll bool   `toml:"group_reply_all"`
}

// Load 读取配置文件，填默认值，并把相对路径换算成相对配置文件所在目录。
// 返回的 warnings 是不认识的配置项（多半是拼错了）。
func Load(path string) (*Config, []string, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		if len(k) > 1 && k[0] == "llm" && k[1] == "extra" {
			continue
		}
		warnings = append(warnings, "未知配置项："+k.String())
	}

	c.applyEnv()
	c.applyDefaults()
	if !md.IsDefined("workspace", "extra_ro") {
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
	return &c, warnings, c.validate()
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
	def(&c.Workspace.Mode, "ask")
	def(&c.Access.Guest, "none")
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
