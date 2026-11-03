// Package ticket 是工单（设计 §16.1）：团队里名单上的人点名 agent 派的一件活。
// 工单是承诺的一种，共用第 15 节的契约和阶段函数；这里只管工单自己的对象和记录。
// 记录在 data/runtime/tickets.jsonl（internal/journal）：每次改动写一条完整的工单，重放时每个 ID 留最后一条；
// 编号 tk-<序号> 不重用——压缩重写时先写一条序号水位记录。不依赖其他业务包。
package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/journal"
)

// 限制。
const (
	RecordLimit = 16 << 10 // 单条
	FileLimit   = 16 << 20 // 整份
	compactAt   = 8 << 20  // 超过这么大就整份重写
	KeepEnded   = 30 * 24 * time.Hour
	recordFmt   = 1

	MaxTitle  = 100
	MaxGoal   = 2000
	MaxNote   = 2000
	MaxText   = 200 // 原消息只留开头这么多字
	MaxRework = 2
)

// 工单的状态。
const (
	StatusOpen     = "open"     // 建好了，等开工
	StatusWorking  = "working"  // 干活的分支在跑（5b）
	StatusChecking = "checking" // 宿主在跑验收命令（5b）
	StatusBlocked  = "blocked"  // 宿主记下的受阻：返工用完验收还没过、预算用完、推送或开 PR 失败；理由在 Reason（5b）
	StatusReady    = "ready"    // 验收通过、已提交，等人收下（5b）
	StatusDone     = "done"     // 收下了，或 PR 合并了
	StatusDropped  = "dropped"  // 不用做了
	StatusFailed   = "failed"   // 做不成，放弃
	StatusStopped  = "stopped"  // 叫停
	StatusPaused   = "paused"   // 暂停
)

// IsEnded 是结束状态。
func IsEnded(status string) bool {
	switch status {
	case StatusDone, StatusDropped, StatusFailed, StatusStopped:
		return true
	}
	return false
}

// Source 是原消息：从哪个渠道、哪个对话来的，开头一段。
type Source struct {
	Channel string `json:"channel"`          // 渠道名
	Chat    string `json:"chat"`             // 对话（群或私聊）ID
	MsgID   string `json:"msg_id,omitempty"` // 消息 ID；回复贴在它上面
	Text    string `json:"text,omitempty"`   // 开头 200 字
}

// Auth 是授权记录：建的时候定下，之后不变。
type Auth struct {
	By        string   `json:"by"`        // 谁派的（名单里的身份）
	Scene     string   `json:"scene"`     // 在哪个场景（团队群的会话 ID）
	Source    Source   `json:"source"`    // 原消息
	Workspace string   `json:"workspace"` // 能动到哪里（仓库所在的工作目录 ID）
	Mode      string   `json:"mode"`      // 权限模式，不超过工作目录和 agent 的上限
	Tools     []string `json:"tools"`     // 场景工具表和 agent 上限的交集
	At        int64    `json:"at"`        // 秒
}

// Ticket 是一张工单。
type Ticket struct {
	ID     string `json:"id"` // tk-<序号>
	Agent  string `json:"agent"`
	Title  string `json:"title"`
	Goal   string `json:"goal"` // 要做成什么，模型写
	Auth   Auth   `json:"auth"`
	Check  string `json:"check"`  // 验收命令，建的时候从团队规则取
	Budget int    `json:"budget"` // 最多多少 token
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Notes  []Note `json:"notes,omitempty"` // 名单里的人后来补充的话（最多留最近 20 条）

	// 5b
	Worktree string `json:"worktree,omitempty"`
	GitDir   string `json:"git_dir,omitempty"`   // worktree 自己的 git 目录 <仓库>/.git/worktrees/tk-<序号>，分支可写
	Branch   string `json:"branch,omitempty"`    // git 分支 nano/tk-<序号>
	Base     string `json:"base,omitempty"`      // 从哪个提交开始
	RunSID   string `json:"run_sid,omitempty"`   // 干活的分支会话
	Commit   string `json:"commit,omitempty"`    // 验收通过后宿主做的提交
	CheckLog string `json:"check_log,omitempty"` // 最近一次验收：退出码和最后 40 行
	Rework   int    `json:"rework,omitempty"`
	// 5c
	PR string `json:"pr,omitempty"`

	Created int64 `json:"created"` // 秒
	Updated int64 `json:"updated"`
}

// Note 是补充的一句话。
type Note struct {
	By   string `json:"by"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

const maxNotes = 20

// Seq 是编号里的序号；不是 tk-<数字> 返回 0。
func Seq(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "tk-"))
	if err != nil || !strings.HasPrefix(id, "tk-") || n <= 0 {
		return 0
	}
	return n
}

// record 是日志里的一条：工单，或序号水位。
type record struct {
	Type   string  `json:"type"` // ticket | seq
	Ticket *Ticket `json:"ticket,omitempty"`
	Seq    int     `json:"seq,omitempty"`
}

// Store 是工单记录。不加锁：由使用方（网关的大锁）串行调用。
type Store struct {
	j    *journal.Journal
	byID map[string]Ticket
	seq  int // 发过的最大序号（含已经丢掉的）
	Now  func() time.Time
}

// Open 打开（不存在就创建）并重放。文件损坏、版本不支持时返回错误，文件保持原样。
func Open(path string) (*Store, error) {
	s := &Store{byID: map[string]Ticket{}}
	j, err := journal.Open(path, recordFmt, journal.Limits{Record: RecordLimit, File: FileLimit}, func(_ uint64, data json.RawMessage) error {
		var r record
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		s.apply(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.j = j
	return s, nil
}

func (s *Store) apply(r record) {
	switch r.Type {
	case "seq":
		s.seq = max(s.seq, r.Seq)
	case "ticket":
		if r.Ticket == nil {
			return
		}
		s.byID[r.Ticket.ID] = *r.Ticket
		s.seq = max(s.seq, Seq(r.Ticket.ID))
	}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Close 关掉文件。
func (s *Store) Close() error {
	if s == nil || s.j == nil {
		return nil
	}
	return s.j.Close()
}

// Size 是文件现在多大。
func (s *Store) Size() int64 { return s.j.Size() }

// Get 按编号取一张工单（返回副本）。
func (s *Store) Get(id string) (Ticket, bool) {
	t, ok := s.byID[id]
	return clone(t), ok
}

func clone(t Ticket) Ticket {
	t.Auth.Tools = append([]string(nil), t.Auth.Tools...)
	t.Notes = append([]Note(nil), t.Notes...)
	return t
}

// List 列出全部工单，编号从小到大。
func (s *Store) List() []Ticket {
	out := make([]Ticket, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, clone(t))
	}
	sort.Slice(out, func(i, j int) bool { return Seq(out[i].ID) < Seq(out[j].ID) })
	return out
}

// BySource 找同一条原消息建的工单（渠道、对话、消息 ID 都相同；没有消息 ID 的不去重）。
func (s *Store) BySource(src Source) (Ticket, bool) {
	if src.MsgID == "" {
		return Ticket{}, false
	}
	for _, t := range s.byID {
		a := t.Auth.Source
		if a.Channel == src.Channel && a.Chat == src.Chat && a.MsgID == src.MsgID {
			return clone(t), true
		}
	}
	return Ticket{}, false
}

// Create 建一张工单：同一条原消息只建一个（重试返回已有的，existed 为真）。先记再说：写不进去返回错误。
func (s *Store) Create(t Ticket) (out Ticket, existed bool, err error) {
	if old, ok := s.BySource(t.Auth.Source); ok {
		return old, true, nil
	}
	if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Goal) == "" {
		return Ticket{}, false, errors.New("工单要有标题和目标")
	}
	if t.Auth.By == "" || t.Auth.Workspace == "" {
		return Ticket{}, false, errors.New("工单要有授权记录")
	}
	t.ID = fmt.Sprintf("tk-%d", s.seq+1)
	t.Status = StatusOpen
	t.Created = s.now().Unix()
	t.Updated = t.Created
	t.Auth.Source.Text = clipRunes(t.Auth.Source.Text, MaxText)
	if err := s.put(t); err != nil {
		return Ticket{}, false, err
	}
	return clone(t), false, nil
}

func (s *Store) put(t Ticket) error {
	if err := s.j.Append(record{Type: "ticket", Ticket: &t}); err != nil {
		return err
	}
	s.apply(record{Type: "ticket", Ticket: &t})
	if s.j.Size() > compactAt {
		_ = s.Compact() // 失败不影响这一条：下一次再试
	}
	return nil
}

// Update 改一张工单（授权记录不能改）：fn 改副本，写成功才生效。
func (s *Store) Update(id string, fn func(*Ticket)) (Ticket, error) {
	t, ok := s.byID[id]
	if !ok {
		return Ticket{}, fmt.Errorf("没有工单 %s", id)
	}
	t = clone(t)
	auth := t.Auth
	fn(&t)
	t.ID, t.Auth = id, auth
	if len(t.Notes) > maxNotes {
		t.Notes = t.Notes[len(t.Notes)-maxNotes:]
	}
	t.Updated = s.now().Unix()
	if err := s.put(t); err != nil {
		return Ticket{}, err
	}
	return clone(t), nil
}

// Compact 整份重写：先写序号水位，再写每张还要留的工单；结束超过 30 天的丢掉。
func (s *Store) Compact() error {
	cutoff := s.now().Add(-KeepEnded).Unix()
	recs := []any{record{Type: "seq", Seq: s.seq}}
	var gone []string
	for _, t := range s.List() {
		if IsEnded(t.Status) && t.Updated < cutoff {
			gone = append(gone, t.ID)
			continue
		}
		t := t
		recs = append(recs, record{Type: "ticket", Ticket: &t})
	}
	if err := s.j.Rewrite(recs); err != nil {
		return err
	}
	for _, id := range gone {
		delete(s.byID, id)
	}
	return nil
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
