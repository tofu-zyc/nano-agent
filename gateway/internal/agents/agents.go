// Package agents 保存每个 agent 的配置（data/agents.json）。
//
// agent 是一个一直在的个体，不是一个用完就扔的工具：它有自己的名字和头像、自己的人格
// （agents/<id>/persona.md）、自己的模型和思考强度，还可以覆盖全局的设置（每条消息的上限、
// 默认权限模式、上下文整理）。没覆盖的项跟随全局（配置文件里的 agent 组）。
//
// 会话 ID：默认 agent 的会话 ID 和以前一样（main、tg:123……），这样不用迁移历史；
// 其他 agent 的会话 ID 前面加 @<agent>/，比如 @bob/main。
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// DefaultID 是默认 agent 的 ID，不能删除。
const DefaultID = "default"

// Agent 是一个 agent 的配置。指针字段为 nil 表示跟随全局设置。
type Agent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Emoji string `json:"emoji,omitempty"`

	// Model 是这个 agent 用的模型（models 包里的 ID），空表示默认模型；Thinking 空表示模型默认
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// 覆盖全局 agent 组的设置
	MaxSteps        *int     `json:"max_steps,omitempty"`
	RunTimeout      *string  `json:"run_timeout,omitempty"`
	ApprovalTimeout *string  `json:"approval_timeout,omitempty"`
	MaxOutputKB     *int     `json:"max_output_kb,omitempty"`
	Mode            *string  `json:"mode,omitempty"`
	CompactAt       *float64 `json:"compact_at,omitempty"`
	CompactTo       *float64 `json:"compact_to,omitempty"`
	KeepTurns       *int     `json:"keep_turns,omitempty"`
	IdleCompact     *string  `json:"idle_compact,omitempty"`
	IdleCompactMin  *float64 `json:"idle_compact_min,omitempty"`

	// Tools 是干活时能用的工具白名单，空表示全部；GroupTools 是群聊里能用的工具（默认只有搜索）
	Tools      []string `json:"tools,omitempty"`
	GroupTools []string `json:"group_tools,omitempty"`

	// 群聊行为（阶段 7）：插话积极度、群里安静多久才插话、回复多长、拆几条、两次发言最短间隔
	GroupEagerness *string `json:"group_eagerness,omitempty"` // off | low | normal | high
	GroupQuiet     *string `json:"group_quiet,omitempty"`
	GroupMaxLen    *int    `json:"group_max_len,omitempty"`
	GroupParts     *int    `json:"group_parts,omitempty"`
	GroupMinGap    *string `json:"group_min_gap,omitempty"`

	// v0.6（docs/DESIGN-v0.6-living-agent.md）：类型、拟人型的内心活动、长期记忆和做梦
	Style      string  `json:"style,omitempty"`       // tool（空也是）| life
	LifeAwake  *string `json:"life_awake,omitempty"`  // 醒着的时间，如 08:00-23:30
	LifeLevel  *string `json:"life_level,omitempty"`  // low | normal | high
	LifeReach  *string `json:"life_reach,omitempty"`  // owner | all | off
	LifeBudget *int    `json:"life_budget,omitempty"` // 内心活动每天最多用多少 token，0 不限
	Memory     *string `json:"memory,omitempty"`      // on | off
	DreamWhen  *string `json:"dream_when,omitempty"`  // idle | nightly | sleep | off
	DreamAt    *string `json:"dream_at,omitempty"`    // nightly 时几点，如 03:00
	DreamIdle  *string `json:"dream_idle,omitempty"`  // idle 时空闲多久，如 2h
	DreamMax   *int    `json:"dream_max,omitempty"`   // 每天最多自动整理几次
	DreamModel *string `json:"dream_model,omitempty"` // 整理记忆用的模型，空就用 agent 的

	Created int64 `json:"created,omitempty"`
}

// State 是 agents.json 的磁盘格式。
type State struct {
	Default string  `json:"default"`
	Agents  []Agent `json:"agents"`
}

// Store 是 data/agents.json，改动立即写盘（原子替换）。
type Store struct {
	path string
	st   State
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Open 读入 agent 列表；文件不存在时是空列表（调用方随后会建默认 agent）。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, fmt.Errorf("读取 agents.json：%w", err)
	}
	if s.st.Default == "" {
		s.st.Default = DefaultID
	}
	for i := range s.st.Agents {
		if s.st.Agents[i].Name == "" {
			s.st.Agents[i].Name = s.st.Agents[i].ID
		}
	}
	return s, nil
}

// Path 是 agents.json 的位置。
func (s *Store) Path() string { return s.path }

// List 返回所有 agent（副本，改它不会影响存储）。
func (s *Store) List() []Agent {
	out := make([]Agent, len(s.st.Agents))
	copy(out, s.st.Agents)
	for i := range out {
		out[i].Tools = slices.Clone(out[i].Tools)
		out[i].GroupTools = slices.Clone(out[i].GroupTools)
	}
	return out
}

// Default 是默认 agent 的 ID。
func (s *Store) Default() string {
	if s.st.Default == "" {
		return DefaultID
	}
	return s.st.Default
}

// Get 按 ID 找一个 agent。
func (s *Store) Get(id string) (Agent, bool) {
	for _, a := range s.List() {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// EnsureDefault 保证默认 agent 存在，没有就建一个（迁移时用）。
func (s *Store) EnsureDefault(name, emoji string) (Agent, error) {
	if a, ok := s.Get(DefaultID); ok {
		return a, nil
	}
	a := Agent{ID: DefaultID, Name: name, Emoji: emoji, Created: time.Now().Unix()}
	return a, s.Create(a)
}

// Create 新建一个 agent。
func (s *Store) Create(a Agent) error {
	if err := normalize(&a); err != nil {
		return err
	}
	if _, ok := s.Get(a.ID); ok {
		return fmt.Errorf("已经有 ID 为 %s 的 agent 了", a.ID)
	}
	if a.Created == 0 {
		a.Created = time.Now().Unix()
	}
	s.st.Agents = append(s.st.Agents, a)
	return s.save()
}

// Update 改一个 agent（ID 不能改）。
func (s *Store) Update(a Agent) error {
	if err := normalize(&a); err != nil {
		return err
	}
	for i := range s.st.Agents {
		if s.st.Agents[i].ID != a.ID {
			continue
		}
		a.Created = s.st.Agents[i].Created
		s.st.Agents[i] = a
		return s.save()
	}
	return errors.New("没有这个 agent")
}

// Delete 删除一个 agent。默认 agent 不能删。
func (s *Store) Delete(id string) error {
	if id == s.Default() {
		return errors.New("默认 agent 不能删除")
	}
	for i := range s.st.Agents {
		if s.st.Agents[i].ID == id {
			s.st.Agents = append(s.st.Agents[:i], s.st.Agents[i+1:]...)
			return s.save()
		}
	}
	return errors.New("没有这个 agent")
}

func normalize(a *Agent) error {
	a.ID = strings.ToLower(strings.TrimSpace(a.ID))
	if !idPattern.MatchString(a.ID) {
		return errors.New("ID 只能用 a-z、0-9 和 -，最多 32 个字符")
	}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return errors.New("名字不能为空")
	}
	if r := []rune(a.Name); len(r) > 40 {
		a.Name = string(r[:40])
	}
	if len([]rune(a.Emoji)) > 4 {
		return errors.New("头像用一个 emoji")
	}
	for _, v := range []*string{a.RunTimeout, a.ApprovalTimeout, a.IdleCompact, a.Mode} {
		if v != nil {
			*v = strings.TrimSpace(*v)
		}
	}
	if a.Mode != nil && *a.Mode != "" && !slices.Contains([]string{"auto", "ask", "readonly"}, *a.Mode) {
		return errors.New("权限模式只能是 auto、ask、readonly，或者空（跟随全局）")
	}
	a.Style = strings.TrimSpace(a.Style)
	if a.Style == StyleTool {
		a.Style = ""
	}
	if a.Style != "" && a.Style != StyleLife {
		return errors.New("类型只能是 tool（工具型）或 life（拟人型）")
	}
	return nil
}

// 两种类型：工具型（默认）和拟人型。
const (
	StyleTool = "tool"
	StyleLife = "life"
)

// Life 是拟人型。
func (a Agent) Life() bool { return a.Style == StyleLife }

func (s *Store) save() error {
	if s.st.Default == "" {
		s.st.Default = DefaultID
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SessionAgent 是一个会话属于哪个 agent：@bob/main 属于 bob，其他都属于默认 agent。
func SessionAgent(sid string) string {
	if strings.HasPrefix(sid, "@") {
		if id, _, ok := strings.Cut(sid[1:], "/"); ok && id != "" {
			return id
		}
	}
	return DefaultID
}

// SessionFor 是某个 agent 下叫 name 的会话 ID：默认 agent 直接就是 name。
func SessionFor(agent, name string) string {
	if agent == "" || agent == DefaultID {
		return name
	}
	return "@" + agent + "/" + name
}

// Dir 是一个 agent 的数据目录：agents/<id>（人格在 persona.md）。
func Dir(root, id string) string { return filepath.Join(root, id) }
