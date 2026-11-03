package repos

import (
	"encoding/json"
	"sort"
	"time"

	"nano-agent/gateway/internal/journal"
)

// 每个仓库一份状态日志 data/runtime/repos/<id>.jsonl（internal/journal）：
// 收到的事件先记下、处理完再记“处理了”，所以崩溃后能把没处理完的接着处理；游标和 ETag 也记在这里。
// 去重靠事件的键（GitHub 对象），两种进来的方式共用。

const (
	stateFormat   = 1
	recordLimit   = 64 << 10
	fileLimit     = 32 << 20
	compactAt     = 4 << 20
	maxSeen       = 5000 // 留最近这么多个键
	maxDeliveries = 500
)

// Cursor 是轮询的游标：各个端点的 since 和 ETag。
type Cursor struct {
	IssuesSince       time.Time         `json:"issues_since"`
	CommentsSince     time.Time         `json:"comments_since"`
	PullCommentsSince time.Time         `json:"pull_comments_since"`
	ETags             map[string]string `json:"etags,omitempty"`
}

type stateRec struct {
	T        string  `json:"t"` // event | done | cursor | delivery
	Event    *Event  `json:"event,omitempty"`
	Key      string  `json:"key,omitempty"`
	Cursor   *Cursor `json:"cursor,omitempty"`
	Delivery string  `json:"delivery,omitempty"`
}

// State 是一个仓库的状态。不加锁：由使用方串行调用。
type State struct {
	j          *journal.Journal
	seen       map[string]bool
	order      []string
	pending    map[string]Event
	cursor     Cursor
	deliveries map[string]bool
	delOrder   []string
}

// OpenState 打开（不存在就创建）并重放。
func OpenState(path string) (*State, error) {
	s := &State{seen: map[string]bool{}, pending: map[string]Event{}, deliveries: map[string]bool{}}
	j, err := journal.Open(path, stateFormat, journal.Limits{Record: recordLimit, File: fileLimit}, func(_ uint64, data json.RawMessage) error {
		var r stateRec
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

func (s *State) apply(r stateRec) {
	switch r.T {
	case "event":
		if r.Event != nil && !s.seen[r.Event.Key] {
			s.markSeen(r.Event.Key)
			s.pending[r.Event.Key] = *r.Event
		}
	case "done":
		s.markSeen(r.Key)
		delete(s.pending, r.Key)
	case "cursor":
		if r.Cursor != nil {
			s.cursor = *r.Cursor
		}
	case "delivery":
		if !s.deliveries[r.Delivery] {
			s.deliveries[r.Delivery] = true
			s.delOrder = append(s.delOrder, r.Delivery)
			if len(s.delOrder) > maxDeliveries {
				delete(s.deliveries, s.delOrder[0])
				s.delOrder = s.delOrder[1:]
			}
		}
	}
}

func (s *State) markSeen(key string) {
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.order = append(s.order, key)
	if len(s.order) > maxSeen {
		old := s.order[0]
		s.order = s.order[1:]
		if _, p := s.pending[old]; !p {
			delete(s.seen, old)
		}
	}
}

func (s *State) append(r stateRec) error {
	if err := s.j.Append(r); err != nil {
		return err
	}
	s.apply(r)
	if s.j.Size() > compactAt {
		_ = s.compact()
	}
	return nil
}

// Close 关掉文件。
func (s *State) Close() error {
	if s == nil || s.j == nil {
		return nil
	}
	return s.j.Close()
}

// Record 记下一个事件：键没见过才记（返回 true），见过的丢掉。先记再处理。
func (s *State) Record(ev Event) (bool, error) {
	if s.seen[ev.Key] {
		return false, nil
	}
	if err := s.append(stateRec{T: "event", Event: &ev}); err != nil {
		return false, err
	}
	return true, nil
}

// Done 记下一个事件处理完了。
func (s *State) Done(key string) error { return s.append(stateRec{T: "done", Key: key}) }

// Pending 是记下了但还没处理完的事件（按键排序，重放用）。
func (s *State) Pending() []Event {
	out := make([]Event, 0, len(s.pending))
	for _, e := range s.pending {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At || out[i].At == out[j].At && out[i].Key < out[j].Key })
	return out
}

// Seen 是这个键已经受理过。
func (s *State) Seen(key string) bool { return s.seen[key] }

// Cursor 是现在的游标。
func (s *State) Cursor() Cursor {
	c := s.cursor
	c.ETags = map[string]string{}
	for k, v := range s.cursor.ETags {
		c.ETags[k] = v
	}
	return c
}

// SetCursor 记下新的游标。
func (s *State) SetCursor(c Cursor) error { return s.append(stateRec{T: "cursor", Cursor: &c}) }

// SeenDelivery 是这次 webhook 投递已经收过；没见过就记下并返回 false。
func (s *State) SeenDelivery(id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if s.deliveries[id] {
		return true, nil
	}
	return false, s.append(stateRec{T: "delivery", Delivery: id})
}

// compact 整份重写：游标、最近的键（已处理的）、没处理完的事件、最近的投递。
func (s *State) compact() error {
	recs := []any{}
	c := s.cursor
	recs = append(recs, stateRec{T: "cursor", Cursor: &c})
	for _, k := range s.order {
		if _, p := s.pending[k]; !p {
			recs = append(recs, stateRec{T: "done", Key: k})
		}
	}
	for _, e := range s.Pending() {
		e := e
		recs = append(recs, stateRec{T: "event", Event: &e})
	}
	for _, d := range s.delOrder {
		recs = append(recs, stateRec{T: "delivery", Delivery: d})
	}
	return s.j.Rewrite(recs)
}
