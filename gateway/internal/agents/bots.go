package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// 机器人实例（data/bots.json）：一个机器人只绑一个 agent。
//
// 内置的两个机器人 ID 是 tg 和 feishu，凭据还是 gateway.toml 里的 [telegram]、[feishu]（Legacy），
// 所以设置只有一个家：Telegram / 飞书节点。另外加的机器人凭据存在 bots.json 里（加密）。
// 渠道名：内置的是 tg、feishu，另外加的是 tg.<id>、feishu.<id>、qq.<id>、wx.<id>。
// QQ 和微信（v0.7 起）只能另外加，扫码绑定。

// BotKind 是机器人的种类。
const (
	KindTelegram = "telegram"
	KindFeishu   = "feishu"
	KindQQ       = "qq"     // QQ 开放平台的官方机器人：AppID + AppSecret
	KindWeixin   = "weixin" // 微信 ClawBot：Token 是 bot_token，APIBase 是扫码时给的接口地址
)

// Bot 是一个机器人实例。
type Bot struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Agent   string `json:"agent"`          // 绑定哪个 agent
	Show    string `json:"show,omitempty"` // 显示档位，空表示跟随 agent
	Enabled bool   `json:"enabled"`
	Legacy  bool   `json:"legacy,omitempty"` // 凭据在 gateway.toml 里

	Token     string `json:"token,omitempty"` // enc:v1:...
	AppID     string `json:"app_id,omitempty"`
	AppSecret string `json:"app_secret,omitempty"` // enc:v1:...
	Proxy     string `json:"proxy,omitempty"`
	APIBase   string `json:"api_base,omitempty"`
	Domain    string `json:"domain,omitempty"`
	GroupAll  bool   `json:"group_all,omitempty"`
	BotID     string `json:"bot_id,omitempty"` // 微信的 ilink_bot_id
	Owner     string `json:"owner,omitempty"`  // 扫码的人在这个机器人那里的 ID

	Created int64 `json:"created,omitempty"`
}

// Bots 是 data/bots.json。
type Bots struct {
	path string
	list []Bot
}

// OpenBots 读入机器人列表；文件不存在时是空列表。
func OpenBots(path string) (*Bots, error) {
	b := &Bots{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &b.list); err != nil {
		return nil, fmt.Errorf("读取 bots.json：%w", err)
	}
	for i := range b.list {
		if b.list[i].Name == "" {
			b.list[i].Name = b.list[i].ID
		}
	}
	return b, nil
}

// List 返回所有机器人（副本）。
func (b *Bots) List() []Bot { return slices.Clone(b.list) }

// Get 按 ID 找一个机器人。
func (b *Bots) Get(id string) (Bot, bool) {
	for _, x := range b.list {
		if x.ID == id {
			return x, true
		}
	}
	return Bot{}, false
}

// OfAgent 是这个 agent 绑的机器人。
func (b *Bots) OfAgent(agent string) []Bot {
	var out []Bot
	for _, x := range b.list {
		if x.Agent == agent {
			out = append(out, x)
		}
	}
	return out
}

// EnsureLegacy 保证内置的两个机器人存在（老版本只有一份配置，没有机器人列表）。
func (b *Bots) EnsureLegacy(agent string) error {
	changed := false
	for _, id := range []string{"tg", "feishu"} {
		if _, ok := b.Get(id); ok {
			continue
		}
		kind := KindTelegram
		name := "Telegram"
		if id == "feishu" {
			kind, name = KindFeishu, "飞书"
		}
		if b.list == nil {
			b.list = []Bot{}
		}
		b.list = append(b.list, Bot{ID: id, Kind: kind, Name: name, Agent: agent, Enabled: true, Legacy: true, Created: time.Now().Unix()})
		changed = true
	}
	if changed {
		return b.save()
	}
	return nil
}

// Create 新建一个机器人（另外加的，凭据自己存）。
func (b *Bots) Create(x Bot) error {
	if err := normalizeBot(&x); err != nil {
		return err
	}
	if x.Legacy {
		return errors.New("内置的机器人不用新建")
	}
	if _, ok := b.Get(x.ID); ok {
		return fmt.Errorf("已经有 ID 为 %s 的机器人了", x.ID)
	}
	if x.Created == 0 {
		x.Created = time.Now().Unix()
	}
	b.list = append(b.list, x)
	return b.save()
}

// Update 改一个机器人（ID 和种类不能改）。凭据留空表示不改。
func (b *Bots) Update(x Bot) error {
	for i := range b.list {
		cur := b.list[i]
		if cur.ID != x.ID {
			continue
		}
		if x.Legacy || cur.Legacy {
			// 内置的机器人：只改绑定和显示档位，凭据在配置文件里
			if err := normalizeBotBinding(&x); err != nil {
				return err
			}
			cur.Agent, cur.Show, cur.Enabled = x.Agent, x.Show, x.Enabled
			b.list[i] = cur
			return b.save()
		}
		x.Kind = cur.Kind
		x.Created = cur.Created
		if x.Token == "" {
			x.Token = cur.Token
		}
		if x.AppSecret == "" {
			x.AppSecret = cur.AppSecret
		}
		if err := normalizeBot(&x); err != nil {
			return err
		}
		b.list[i] = x
		return b.save()
	}
	return errors.New("没有这个机器人")
}

// Bind 把一个机器人接到某个 agent。
func (b *Bots) Bind(id, agent string) error {
	for i := range b.list {
		if b.list[i].ID == id {
			b.list[i].Agent = agent
			return b.save()
		}
	}
	return errors.New("没有这个机器人")
}

// Delete 删掉另外加的机器人；内置的不能删（可以在自己的节点里停用）。
func (b *Bots) Delete(id string) error {
	for i := range b.list {
		if b.list[i].ID != id {
			continue
		}
		if b.list[i].Legacy {
			return errors.New("内置的机器人不能删除，可以在节点里停用")
		}
		b.list = append(b.list[:i], b.list[i+1:]...)
		return b.save()
	}
	return errors.New("没有这个机器人")
}

// ChannelName 是机器人在网关里的渠道名：内置的 tg、feishu，另外加的 tg.<id>、feishu.<id>。
func ChannelName(x Bot) string {
	if x.Legacy {
		if x.Kind == KindFeishu {
			return "feishu"
		}
		return "tg"
	}
	switch x.Kind {
	case KindFeishu:
		return "feishu." + x.ID
	case KindQQ:
		return "qq." + x.ID
	case KindWeixin:
		return "wx." + x.ID
	}
	return "tg." + x.ID
}

// KindOfChannel 是渠道名对应的机器人种类（tg、tg.bob → telegram）。
func KindOfChannel(name string) string {
	if name == "feishu" || strings.HasPrefix(name, "feishu.") {
		return KindFeishu
	}
	if name == "tg" || strings.HasPrefix(name, "tg.") {
		return KindTelegram
	}
	if strings.HasPrefix(name, "qq.") {
		return KindQQ
	}
	if strings.HasPrefix(name, "wx.") {
		return KindWeixin
	}
	return ""
}

func normalizeBot(x *Bot) error {
	x.ID = strings.ToLower(strings.TrimSpace(x.ID))
	if !idPattern.MatchString(x.ID) {
		return errors.New("ID 只能用 a-z、0-9 和 -，最多 32 个字符")
	}
	if x.ID == "tg" || x.ID == "feishu" {
		return errors.New("tg 和 feishu 是内置机器人的 ID")
	}
	x.Name = strings.TrimSpace(x.Name)
	if x.Name == "" {
		x.Name = x.ID
	}
	if !slices.Contains([]string{KindTelegram, KindFeishu, KindQQ, KindWeixin}, x.Kind) {
		return errors.New("种类只能是 telegram、feishu、qq 或 weixin")
	}
	if x.Kind == KindTelegram && strings.TrimSpace(x.Token) == "" {
		return errors.New("Telegram 机器人要填 Bot Token")
	}
	if x.Kind == KindFeishu && (strings.TrimSpace(x.AppID) == "" || strings.TrimSpace(x.AppSecret) == "") {
		return errors.New("飞书机器人要填 App ID 和 App Secret")
	}
	if x.Kind == KindQQ && (strings.TrimSpace(x.AppID) == "" || strings.TrimSpace(x.AppSecret) == "") {
		return errors.New("QQ 机器人要填 AppID 和 AppSecret（或者扫码创建）")
	}
	if x.Kind == KindWeixin && strings.TrimSpace(x.Token) == "" {
		return errors.New("微信机器人要扫码连接")
	}
	return normalizeBotBinding(x)
}

func normalizeBotBinding(x *Bot) error {
	if x.Agent == "" {
		return errors.New("要选一个 agent")
	}
	if !slices.Contains([]string{"", "详细", "过程", "简洁"}, x.Show) {
		return errors.New("显示档位只能是详细、过程、简洁，或者空（跟随 agent）")
	}
	return nil
}

func (b *Bots) save() error {
	if b.list == nil {
		b.list = []Bot{}
	}
	data, err := json.MarshalIndent(b.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}
