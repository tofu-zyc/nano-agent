package commit

import (
	"encoding/json"
	"fmt"
	"time"

	"nano-agent/gateway/internal/journal"
)

// 承诺记录 data/runtime/commitments.jsonl（internal/journal，单条 8KB，整份 16MB）：
// 干活的会话用 Report 汇报“主人需要知道什么”，主人点“知道了”或回答问题，都记在这里。
// 先记再说：写成功才算数；内存里只留每件事最近一条汇报，和最近 16 个看过或回答过的 Mark。

const (
	RecordLimit  = 8 << 10  // 单条
	FileLimit    = 16 << 20 // 整份
	compactAt    = 8 << 20  // 超过这么大就整份重写
	MaxMarks     = 16       // 每件事留最近几个看过的 Mark
	recordFormat = 1
)

// 记录的种类。
const (
	TypeReport = "report" // 干活的会话汇报
	TypeSeen   = "seen"   // 主人点了“知道了”
	TypeAnswer = "answer" // 主人回答了问题
)

// 汇报的状态。
const (
	StatusProgress  = "progress"   // 进展：只记一笔
	StatusWaiting   = "waiting"    // 在等：只记一笔
	StatusReady     = "ready"      // 有结果可用
	StatusBlocked   = "blocked"    // 卡住了，要人处理（可以带一个问题）
	StatusFailed    = "failed"     // 做不下去了
	StatusNotNeeded = "not_needed" // 不用做了
)

// CommitRec 是承诺记录里的一条。
type CommitRec struct {
	Seq  uint64 `json:"seq"`
	Type string `json:"type"` // report | seen | answer
	TS   int64  `json:"ts"`   // 毫秒
	Kind string `json:"kind"` // 承诺种类
	ID   string `json:"id"`   // 在自己种类里的 ID
	// report
	Agent    string   `json:"agent,omitempty"`
	Status   string   `json:"status,omitempty"`
	Text     string   `json:"text,omitempty"`
	Question string   `json:"question,omitempty"`
	Evidence []string `json:"evidence,omitempty"` // 凭据 id 和工具名
	Run      string   `json:"run,omitempty"`
	// seen、answer：针对哪一条事实，谁做的
	Mark   string `json:"mark,omitempty"`
	By     string `json:"by,omitempty"`
	Answer string `json:"answer,omitempty"`
}

type key struct{ kind, id string }

// Store 是承诺记录。不加锁：由使用方（网关的大锁）串行调用。
type Store struct {
	j      *journal.Journal
	seq    uint64
	report map[key]CommitRec   // 每件事最近一条汇报
	marks  map[key][]CommitRec // 每件事最近的看过 / 回答（最多 MaxMarks 条，旧的在前）
	// Alive 在压缩时调用一次，返回“这件承诺还在不在”的判断；不在的记录在这时丢掉。没设就都留着。
	Alive func() func(kind, id string) bool
	// Now 给记录打时间（毫秒）；没设用真实时间。
	Now func() time.Time
}

// Open 打开（不存在就创建）并重放记录。文件损坏、版本不支持时返回错误，文件保持原样。
func Open(path string) (*Store, error) {
	s := &Store{report: map[key]CommitRec{}, marks: map[key][]CommitRec{}}
	j, err := journal.Open(path, recordFormat, journal.Limits{Record: RecordLimit, File: FileLimit}, func(_ uint64, data json.RawMessage) error {
		var r CommitRec
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

func (s *Store) apply(r CommitRec) {
	if r.Seq > s.seq {
		s.seq = r.Seq
	}
	k := key{r.Kind, r.ID}
	switch r.Type {
	case TypeReport:
		s.report[k] = r
	case TypeSeen, TypeAnswer:
		m := append(s.marks[k], r)
		if len(m) > MaxMarks {
			m = m[len(m)-MaxMarks:]
		}
		s.marks[k] = m
	}
}

// Close 关掉文件。
func (s *Store) Close() error {
	if s == nil || s.j == nil {
		return nil
	}
	return s.j.Close()
}

// Append 先写进记录，写成功才改内存。写不进去（太大、满了、磁盘出错）返回错误。
// 返回写下的那一条（带序号和时间）。文件超过 8MB 时顺手整份重写。
func (s *Store) Append(r CommitRec) (CommitRec, error) {
	if r.Kind == "" || r.ID == "" {
		return CommitRec{}, fmt.Errorf("承诺记录要有种类和 ID")
	}
	switch r.Type {
	case TypeReport, TypeSeen, TypeAnswer:
	default:
		return CommitRec{}, fmt.Errorf("不认识的记录种类：%s", r.Type)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	r.Seq, r.TS = s.seq+1, now().UnixMilli()
	if err := s.j.Append(r); err != nil {
		return CommitRec{}, err
	}
	s.apply(r)
	if s.j.Size() > compactAt {
		_ = s.Compact() // 失败不影响这一条：下一次再试
	}
	return r, nil
}

// Latest 是这件事最近一条汇报。
func (s *Store) Latest(kind, id string) (CommitRec, bool) {
	r, ok := s.report[key{kind, id}]
	return r, ok
}

// Seen 是这个 Mark 已经被主人看过或回答过。
func (s *Store) Seen(kind, id, mark string) bool {
	if mark == "" {
		return false
	}
	for _, r := range s.marks[key{kind, id}] {
		if r.Mark == mark {
			return true
		}
	}
	return false
}

// Answer 是回答某个 Mark 的那条记录。
func (s *Store) Answer(kind, id, mark string) (CommitRec, bool) {
	for _, r := range s.marks[key{kind, id}] {
		if r.Type == TypeAnswer && r.Mark == mark {
			return r, true
		}
	}
	return CommitRec{}, false
}

// Size 是文件现在多大。
func (s *Store) Size() int64 { return s.j.Size() }

// Compact 整份重写，只留内存里的这些（每件事最近一条汇报和最近 16 个看过的 Mark）；
// Alive 说已经不在的承诺，它们的记录在这时丢掉。
func (s *Store) Compact() error {
	alive := func(k key) bool { return true }
	if s.Alive != nil {
		in := s.Alive()
		alive = func(k key) bool { return in(k.kind, k.id) }
	}
	var keep []CommitRec
	for k, r := range s.report {
		if alive(k) {
			keep = append(keep, r)
		}
	}
	for k, rs := range s.marks {
		if alive(k) {
			keep = append(keep, rs...)
		}
	}
	// 按原来的序号排，重写后回放得到同样的状态
	for i := 1; i < len(keep); i++ {
		for j := i; j > 0 && keep[j-1].Seq > keep[j].Seq; j-- {
			keep[j-1], keep[j] = keep[j], keep[j-1]
		}
	}
	all := make([]any, len(keep))
	for i, r := range keep {
		all[i] = r
	}
	if err := s.j.Rewrite(all); err != nil {
		return err
	}
	for k := range s.report {
		if !alive(k) {
			delete(s.report, k)
		}
	}
	for k := range s.marks {
		if !alive(k) {
			delete(s.marks, k)
		}
	}
	return nil
}
