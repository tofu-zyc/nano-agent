package app

// 机器人（data/bots.json）和 agent 的迁移。
//
// 内置的两个机器人 tg、feishu 的凭据还是 gateway.toml 里的 [telegram]、[feishu]，所以设置只有
// 一个家：Telegram / 飞书节点。另外加的机器人凭据存在 bots.json 里（加密），渠道名是 tg.<id>、
// feishu.<id>。一个机器人只绑一个 agent。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/secret"
)

// 默认 agent 的名字和头像（第一次升级时建的，用户可以改）。
const (
	defaultAgentName  = "小纳"
	defaultAgentEmoji = "🐱"
)

// MigrateAgents 升级到多 agent：建默认 agent、把老版本的默认人格挪到它下面、补上内置机器人。
func (a *App) MigrateAgents() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil {
		return nil
	}
	ag, err := a.agentStore.EnsureDefault(defaultAgentName, defaultAgentEmoji)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.agentRoot, ag.ID), 0o700); err != nil {
		return err
	}
	// 老版本的人格：personas/default.md 挪到 agents/<默认 agent>/persona.md。
	// 迁移过一次就不会再来一遍（目标文件已经在了）。
	src := filepath.Join(a.cfg.Persona.Dir, "default.md")
	dst := filepath.Join(a.agentRoot, ag.ID, "persona.md")
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		if data, err := os.ReadFile(src); err == nil {
			if err := os.WriteFile(dst, data, 0o600); err != nil {
				a.log.Warn("把老版本的人格复制到默认 agent 下失败", "err", err)
			} else {
				// 原文件留着：升级失败自动退回旧版本时，旧版本还认 personas/default.md
				a.log.Info("老版本的默认人格已经复制到默认 agent 下", "file", dst, "from", src)
			}
		}
	}
	if a.botStore != nil {
		if err := a.botStore.EnsureLegacy(a.agentStore.Default()); err != nil {
			return err
		}
	}
	return nil
}

// botChannel 是机器人的渠道配置和构造方法，want 表示现在要不要开着。调用方持有锁。
func (a *App) botChannel(cfg *config.Config, b agents.Bot) (conf any, build func() (channel.Channel, error), want bool) {
	if b.Legacy {
		if b.Kind == agents.KindFeishu {
			c := cfg.FeishuConfig()
			return c, func() (channel.Channel, error) {
				return channel.NewFeishu(c, a.MediaStore(), a.log)
			}, c.Enabled
		}
		c := cfg.TelegramConfig()
		return c, func() (channel.Channel, error) {
			return channel.NewTelegram(c, a.MediaStore(), a.log)
		}, c.Enabled
	}
	name := agents.ChannelName(b)
	if b.Kind == agents.KindFeishu {
		appSecret, err := secret.Decrypt(b.AppSecret)
		if err != nil {
			a.log.Error("机器人的 App Secret 解不开", "bot", b.ID, "err", err)
			return nil, nil, false
		}
		c := config.Feishu{Enabled: b.Enabled, AppID: b.AppID, AppSecret: appSecret, Domain: b.Domain,
			Proxy: cfg.ProxyFor(b.Proxy), GroupReplyAll: b.GroupAll}
		if strings.TrimSpace(c.AppID) == "" || c.AppSecret == "" {
			return nil, nil, false
		}
		return c, func() (channel.Channel, error) {
			ch, err := channel.NewFeishu(c, a.MediaStore(), a.log)
			if err != nil {
				return nil, err
			}
			ch.SetName(name)
			return ch, nil
		}, b.Enabled
	}
	token, err := secret.Decrypt(b.Token)
	if err != nil {
		a.log.Error("机器人的 Token 解不开", "bot", b.ID, "err", err)
		return nil, nil, false
	}
	c := config.Telegram{Enabled: b.Enabled, Token: token, Proxy: cfg.ProxyFor(b.Proxy), APIBase: b.APIBase, GroupReplyAll: b.GroupAll}
	if strings.TrimSpace(c.Token) == "" {
		return nil, nil, false
	}
	return c, func() (channel.Channel, error) {
		ch, err := channel.NewTelegram(c, a.MediaStore(), a.log)
		if err != nil {
			return nil, err
		}
		ch.SetName(name)
		return ch, nil
	}, b.Enabled
}

// botKinds 是机器人渠道名可能的前缀，用来认出“已经不存在的机器人渠道”。
func isBotChannel(name string) bool {
	return agents.KindOfChannel(name) != ""
}

// ---- 网页上的机器人管理 ----

// BotView 是一个机器人的全部状态（凭据只说要没有要，不返回明文）。
type BotView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Show      string `json:"show"`
	Channel   string `json:"channel"`
	Enabled   bool   `json:"enabled"`
	Legacy    bool   `json:"legacy"`
	AppID     string `json:"app_id"`
	HasToken  bool   `json:"has_token"`
	HasSecret bool   `json:"has_secret"`
	Proxy     string `json:"proxy"`
	APIBase   string `json:"api_base"`
	Domain    string `json:"domain"`
	GroupAll  bool   `json:"group_all"`
}

func botView(b agents.Bot) BotView {
	return BotView{ID: b.ID, Kind: b.Kind, Name: b.Name, Agent: b.Agent, Show: b.Show, Channel: agents.ChannelName(b),
		Enabled: b.Enabled, Legacy: b.Legacy, AppID: b.AppID, HasToken: b.Token != "" || b.Legacy, HasSecret: b.AppSecret != "" || b.Legacy,
		Proxy: b.Proxy, APIBase: b.APIBase, Domain: b.Domain, GroupAll: b.GroupAll}
}

// Bots 是所有机器人。
func (a *App) Bots() []BotView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []BotView{}
	if a.botStore == nil {
		return out
	}
	for _, b := range a.botStore.List() {
		out = append(out, botView(b))
	}
	return out
}

// BotPatch 是网页里对一个机器人的修改，nil 表示不改。
type BotPatch struct {
	Name      *string `json:"name"`
	Agent     *string `json:"agent"`
	Show      *string `json:"show"`
	Enabled   *bool   `json:"enabled"`
	Token     *string `json:"token"`
	AppID     *string `json:"app_id"`
	AppSecret *string `json:"app_secret"`
	Proxy     *string `json:"proxy"`
	APIBase   *string `json:"api_base"`
	Domain    *string `json:"domain"`
	GroupAll  *bool   `json:"group_all"`
}

// applyBotPatch 把网页的修改套到机器人上。凭据留空表示不改。
func applyBotPatch(b *agents.Bot, p BotPatch) error {
	if p.Name != nil {
		b.Name = *p.Name
	}
	if p.Agent != nil {
		b.Agent = *p.Agent
	}
	if p.Show != nil {
		b.Show = *p.Show
	}
	if p.Enabled != nil {
		b.Enabled = *p.Enabled
	}
	if p.Proxy != nil {
		b.Proxy = strings.TrimSpace(*p.Proxy)
	}
	if p.APIBase != nil {
		b.APIBase = strings.TrimSpace(*p.APIBase)
	}
	if p.Domain != nil {
		b.Domain = *p.Domain
	}
	if p.GroupAll != nil {
		b.GroupAll = *p.GroupAll
	}
	if p.AppID != nil {
		b.AppID = strings.TrimSpace(*p.AppID)
	}
	if p.Token != nil && strings.TrimSpace(*p.Token) != "" {
		enc, err := secret.Encrypt(strings.TrimSpace(*p.Token))
		if err != nil {
			return err
		}
		b.Token = enc
	}
	if p.AppSecret != nil && strings.TrimSpace(*p.AppSecret) != "" {
		enc, err := secret.Encrypt(strings.TrimSpace(*p.AppSecret))
		if err != nil {
			return err
		}
		b.AppSecret = enc
	}
	return nil
}

// CreateBot 加一个机器人。kind 是 telegram 或 feishu，ID 按名字起。
func (a *App) CreateBot(kind string, p BotPatch) (BotView, error) {
	a.mu.Lock()
	if a.botStore == nil {
		a.mu.Unlock()
		return BotView{}, errors.New("机器人列表还没准备好")
	}
	b := agents.Bot{Kind: kind, Enabled: true, Created: time.Now().Unix()}
	if err := applyBotPatch(&b, p); err != nil {
		a.mu.Unlock()
		return BotView{}, err
	}
	if b.Agent == "" {
		b.Agent = agents.DefaultID
	}
	b.ID = botIDSuggestion(a.botStore, b.Name)
	err := a.botStore.Create(b)
	a.mu.Unlock()
	if err != nil {
		return BotView{}, err
	}
	a.log.Info("加了机器人", "id", b.ID, "kind", kind)
	a.reconcileBots()
	got, _ := a.botStore.Get(b.ID)
	return botView(got), nil
}

// botIDSuggestion 按名字起一个没被占用的机器人 ID。
func botIDSuggestion(s *agents.Bots, name string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			out.WriteString("-")
		}
	}
	base := strings.Trim(out.String(), "-")
	if base == "" {
		base = "bot"
	}
	if _, ok := s.Get(base); !ok {
		return base
	}
	for i := 2; i < 100; i++ {
		id := base + "-" + itoa(i)
		if _, ok := s.Get(id); !ok {
			return id
		}
	}
	return base + "-x"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// UpdateBot 改一个机器人。
func (a *App) UpdateBot(id string, p BotPatch) (BotView, error) {
	a.mu.Lock()
	if a.botStore == nil {
		a.mu.Unlock()
		return BotView{}, errors.New("机器人列表还没准备好")
	}
	b, ok := a.botStore.Get(id)
	if !ok {
		a.mu.Unlock()
		return BotView{}, errors.New("没有这个机器人")
	}
	if err := applyBotPatch(&b, p); err != nil {
		a.mu.Unlock()
		return BotView{}, err
	}
	if b.Agent == "" {
		b.Agent = agents.DefaultID
	}
	err := a.botStore.Update(b)
	a.mu.Unlock()
	if err != nil {
		return BotView{}, err
	}
	a.reconcileBots()
	got, _ := a.botStore.Get(id)
	return botView(got), nil
}

// DeleteBot 删掉另外加的机器人。
func (a *App) DeleteBot(id string) error {
	a.mu.Lock()
	if a.botStore == nil {
		a.mu.Unlock()
		return errors.New("机器人列表还没准备好")
	}
	err := a.botStore.Delete(id)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.reconcileBots()
	a.publish(events.Event{T: "agents"})
	return nil
}

// reconcileBots 让正在跑的机器人渠道和列表一致。调用方不能持有 a.mu。
func (a *App) reconcileBots() {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	a.ReconcileChannels(cfg)
	a.publish(events.Event{T: "agents"})
	a.publish(events.Event{T: "bots"})
}
