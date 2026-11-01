// Package scene 是“场景”的规则：内部群、外部群共用的一套类型、默认值和校验。
// 它是叶子包，不依赖其他包；规则存在哪（群记录、会话设置）由使用方决定。
package scene

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// 说话方式：点名（只有被叫到才说）和自由（可以按积极度插话）。
const (
	ModeMention = "mention"
	ModeFree    = "free"
	// 回合制的两种：时钟按回合走，不按真实时间；只用于内部群
	ModeTurns    = "turns"    // 轮流：人或旁白说一句之后，按顺序每人说一句为一轮
	ModeDirector = "director" // 导演：只有导演点到的那一个会说话
)

// 频道和轮数的上限。
const (
	MaxChannels = 8
	MaxRounds   = 1000
)

// 场景的种类。私聊（普通会话）这一步没有规则，不在这里。
type Kind string

const (
	Room     Kind = "room"     // 内部群
	DMRoom   Kind = "dm"       // 两个 agent 的私聊（内部群的一种）
	External Kind = "external" // 外部群：聊天软件里的群
)

// 默认值和范围。
const (
	DefaultGuardRun      = 8
	DefaultGuardPingPong = 6
	MinGuard, MaxGuard   = 2, 50
	MaxText              = 2000 // 场景说明和每条角色提示词最多多少字
)

// Rules 是一个场景的规则。零值表示全部沿用默认。
type Rules struct {
	Mode        string          `json:"mode,omitempty"`    // mention | free；2c 再加 turns | director
	Observe     bool            `json:"observe,omitempty"` // 主动插话只记录，不发出
	Brief       string          `json:"brief,omitempty"`   // 场景说明，每个成员都看得到
	Roles       map[string]Role `json:"roles,omitempty"`   // agent ID → 它在这里的角色
	Tools       []string        `json:"tools"`             // null 或缺省：沿用默认；[]：不给工具
	Style       Style           `json:"style,omitempty"`
	Guard       Guard           `json:"guard,omitempty"`
	Budget      int             `json:"budget,omitempty"`       // 每天最多多少 token，0 不限
	BudgetTotal int             `json:"budget_total,omitempty"` // 这一场最多多少 token，0 不限

	// 以下只用于内部群
	Order    []string  `json:"order,omitempty"`    // 轮流的发言顺序；空表示按成员顺序
	Rounds   int       `json:"rounds,omitempty"`   // 轮流时这一场最多几轮
	Channels []Channel `json:"channels,omitempty"` // 私下频道，最多 8 个
}

// Channel 是内部群里的私下频道：里面的话只有频道成员听得到。
type Channel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"` // 必须是群成员；成员退群时一并去掉
}

// ChannelByID 按 ID 找频道。
func (r Rules) ChannelByID(id string) (Channel, bool) {
	for _, c := range r.Channels {
		if c.ID == id {
			return c, true
		}
	}
	return Channel{}, false
}

// Turn 回合制：轮流或导演。
func (r Rules) Turn() bool { return r.Mode == ModeTurns || r.Mode == ModeDirector }

// WithoutMember 去掉退群成员：从顺序和各频道里拿掉；频道没人了就不留空频道。
func (r Rules) WithoutMember(id string) Rules {
	r = r.Clone()
	r.Order = slices.DeleteFunc(r.Order, func(m string) bool { return m == id })
	if len(r.Order) == 0 {
		r.Order = nil
	}
	var chans []Channel
	for _, c := range r.Channels {
		c.Members = slices.DeleteFunc(c.Members, func(m string) bool { return m == id })
		if len(c.Members) > 0 {
			chans = append(chans, c)
		}
	}
	r.Channels = chans
	delete(r.Roles, id)
	return r
}

// Role 是 agent 在场景里的角色。
type Role struct {
	Prompt    string `json:"prompt,omitempty"`    // 角色提示词
	Eagerness string `json:"eagerness,omitempty"` // off | low | normal | high；空表示沿用 agent 的
}

// Style 是说话样式；空或 0 表示沿用 agent 的群聊设置。
type Style struct {
	Quiet  string `json:"quiet,omitempty"`   // 安静多久以后才可能插话
	MinGap string `json:"min_gap,omitempty"` // 两次发言至少隔多久
	MaxLen int    `json:"max_len,omitempty"` // 一条最多多少字（外部群）
	Parts  int    `json:"parts,omitempty"`   // 最多拆成几条（外部群）
}

// Guard 是防刷屏阈值，只用于内部群的点名和自由模式；0 表示默认。
type Guard struct {
	Run      int `json:"run,omitempty"`       // 连续几条 agent 发言就暂停：默认 8，范围 2–50
	PingPong int `json:"ping_pong,omitempty"` // 两个 agent 来回几轮就暂停：默认 6，范围 2–50
}

// EffectiveGuard 是生效的防刷屏阈值。
func (g Guard) EffectiveGuard() (run, pingPong int) {
	run, pingPong = g.Run, g.PingPong
	if run == 0 {
		run = DefaultGuardRun
	}
	if pingPong == 0 {
		pingPong = DefaultGuardPingPong
	}
	return
}

// IsZero 是没设任何规则。
func (r Rules) IsZero() bool {
	return r.Mode == "" && !r.Observe && r.Brief == "" && len(r.Roles) == 0 && r.Tools == nil &&
		r.Style == (Style{}) && r.Guard == (Guard{}) && r.Budget == 0 && r.BudgetTotal == 0 &&
		len(r.Order) == 0 && r.Rounds == 0 && len(r.Channels) == 0
}

// Clone 复制一份（映射和切片不共用）。
func (r Rules) Clone() Rules {
	if r.Roles != nil {
		roles := make(map[string]Role, len(r.Roles))
		for k, v := range r.Roles {
			roles[k] = v
		}
		r.Roles = roles
	}
	if r.Tools != nil {
		r.Tools = slices.Clone(r.Tools)
	}
	r.Order = slices.Clone(r.Order)
	if r.Channels != nil {
		chans := make([]Channel, len(r.Channels))
		for i, c := range r.Channels {
			c.Members = slices.Clone(c.Members)
			chans[i] = c
		}
		r.Channels = chans
	}
	return r
}

var channelID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}$`)

// ValidateMembers 检查和群成员有关的部分：顺序、频道成员、角色都得是群成员，不能重复。
func (r Rules) ValidateMembers(members []string) error {
	seen := map[string]bool{}
	for _, m := range r.Order {
		if !slices.Contains(members, m) {
			return fmt.Errorf("发言顺序里的 %s 不在群里", m)
		}
		if seen[m] {
			return fmt.Errorf("发言顺序里 %s 重复了", m)
		}
		seen[m] = true
	}
	for _, c := range r.Channels {
		in := map[string]bool{}
		for _, m := range c.Members {
			if !slices.Contains(members, m) {
				return fmt.Errorf("频道「%s」里的 %s 不在群里", c.Name, m)
			}
			if in[m] {
				return fmt.Errorf("频道「%s」里 %s 重复了", c.Name, m)
			}
			in[m] = true
		}
	}
	return nil
}

var eagernessOK = []string{"", "off", "low", "normal", "high"}

// Validate 检查规则。sceneTools 是登记表里 scene 为真的工具名：tools 只能从里面选。
func (r Rules) Validate(kind Kind, sceneTools []string) error {
	switch r.Mode {
	case "", ModeMention, ModeFree:
	case ModeTurns, ModeDirector:
		if kind != Room {
			return errors.New("轮流和导演只能用在内部群")
		}
	default:
		return errors.New("说话方式只能是 mention（点名）、free（自由）、turns（轮流）或 director（导演）")
	}
	if kind == DMRoom && r.Mode != "" && r.Mode != ModeMention {
		return errors.New("两个 agent 的私聊只能点名")
	}
	if r.Mode == ModeTurns && r.Rounds <= 0 && r.BudgetTotal <= 0 {
		return errors.New("轮流必须设轮数或这一场的预算，保证有界")
	}
	if r.Rounds < 0 || r.Rounds > MaxRounds {
		return fmt.Errorf("轮数要在 0 到 %d 之间", MaxRounds)
	}
	if kind == External && (r.Rounds != 0 || len(r.Order) != 0 || len(r.Channels) != 0) {
		return errors.New("外部群没有轮流顺序和私下频道")
	}
	if len(r.Channels) > MaxChannels {
		return fmt.Errorf("私下频道最多 %d 个", MaxChannels)
	}
	seen := map[string]bool{}
	for _, c := range r.Channels {
		if !channelID.MatchString(c.ID) {
			return fmt.Errorf("频道 ID %q 只能用小写字母、数字和短横线，最长 16 位", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("频道 ID 重复：%s", c.ID)
		}
		seen[c.ID] = true
		if n := utf8.RuneCountInString(strings.TrimSpace(c.Name)); n == 0 || n > 30 {
			return errors.New("频道名字要有，最多 30 字")
		}
		if len(c.Members) == 0 {
			return fmt.Errorf("频道「%s」至少要有一个成员", c.Name)
		}
	}
	if n := utf8.RuneCountInString(r.Brief); n > MaxText {
		return fmt.Errorf("场景说明最多 %d 字（现在 %d）", MaxText, n)
	}
	for id, role := range r.Roles {
		if n := utf8.RuneCountInString(role.Prompt); n > MaxText {
			return fmt.Errorf("%s 的角色提示词最多 %d 字（现在 %d）", id, MaxText, n)
		}
		if !slices.Contains(eagernessOK, role.Eagerness) {
			return fmt.Errorf("%s 的积极度只能是 off、low、normal、high", id)
		}
	}
	for _, t := range r.Tools {
		if !slices.Contains(sceneTools, t) {
			return fmt.Errorf("场景里不能用工具 %s（可选：%s）", t, strings.Join(sceneTools, "、"))
		}
	}
	if r.Style.Parts > 5 {
		return errors.New("最多拆成 5 条")
	}
	if r.Style.MaxLen < 0 || r.Style.Parts < 0 || r.Budget < 0 || r.BudgetTotal < 0 || r.Guard.Run < 0 || r.Guard.PingPong < 0 {
		return errors.New("数值不能是负数")
	}
	for name, v := range map[string]string{"安静时长": r.Style.Quiet, "发言间隔": r.Style.MinGap} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d < 0 {
			return fmt.Errorf("%s写法不对：%q（例如 20s、2m）", name, v)
		}
	}
	for name, v := range map[string]int{"连续发言暂停": r.Guard.Run, "来回轮数暂停": r.Guard.PingPong} {
		if v != 0 && (v < MinGuard || v > MaxGuard) {
			return fmt.Errorf("%s要在 %d 到 %d 之间", name, MinGuard, MaxGuard)
		}
	}
	if kind == External {
		if r.Guard != (Guard{}) {
			return errors.New("外部群没有防刷屏阈值")
		}
		if r.BudgetTotal != 0 {
			return errors.New("外部群只有每天的预算")
		}
	}
	return nil
}
