// Package health 是各个部件自己报告的健康状态。
//
// 每个部件认领一个条目，只有认领者能写；网关把它们汇总成一份清单，
// 聊天的 /status、nano-gateway status 和网页都读这一份。
package health

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// State 有三种：ok 正常；degraded 能用，但有事要人看；error 这一部分现在不能工作。
type State string

const (
	OK       State = "ok"
	Degraded State = "degraded"
	Error    State = "error"
)

// maxDetail 是说明的字节上限，超出的截掉（按 UTF-8 边界）。
const maxDetail = 300

type Entry struct {
	Name   string    `json:"name"`
	State  State     `json:"state"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"` // 进入当前状态的时间
}

type slot struct {
	entry    Entry
	released bool
}

// Board 是全部条目。方法可以并发调用；nil 的 Board 什么也不记。
type Board struct {
	mu    sync.Mutex
	slots map[string]*slot
	now   func() time.Time
}

func NewBoard() *Board { return &Board{slots: map[string]*slot{}, now: time.Now} }

// Handle 是认领者写条目的凭据。nil 的 Handle 什么也不做。
type Handle struct {
	b *Board
	s *slot
}

// Claim 认领一个条目，初始状态 ok。同名条目还在时返回错误，要先 Release。
func (b *Board) Claim(name string) (*Handle, error) {
	if b == nil {
		return nil, nil
	}
	if name == "" {
		return nil, errors.New("健康条目要有名字")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.slots[name] != nil {
		return nil, errors.New("健康条目已被认领：" + name)
	}
	s := &slot{entry: Entry{Name: name, State: OK, Since: b.now()}}
	b.slots[name] = s
	return &Handle{b, s}, nil
}

// Set 更新状态和说明。状态没变时 Since 不变。已经 Release 的 Handle 上的 Set 无效，
// 不报错也不改条目：停掉的部件迟到的回报，不能写到新认领的同名条目上。
func (h *Handle) Set(state State, detail string) {
	if h == nil {
		return
	}
	switch state {
	case OK, Degraded, Error:
	default:
		return
	}
	if len(detail) > maxDetail {
		cut := maxDetail
		for cut > 0 && detail[cut]&0xC0 == 0x80 {
			cut--
		}
		detail = detail[:cut]
	}
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if h.s.released {
		return
	}
	if h.s.entry.State != state {
		h.s.entry.State, h.s.entry.Since = state, h.b.now()
	}
	h.s.entry.Detail = detail
}

// Release 撤销认领，条目从清单里消失，同名可以重新认领。可以重复调用。
func (h *Handle) Release() {
	if h == nil {
		return
	}
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	h.s.released = true
	if h.b.slots[h.s.entry.Name] == h.s {
		delete(h.b.slots, h.s.entry.Name)
	}
}

// Snapshot 是当前全部条目，按名字排序。
func (b *Board) Snapshot() []Entry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, len(b.slots))
	for _, s := range b.slots {
		out = append(out, s.entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Problems 是不是 ok 的条目（聊天 /status 和命令行 status 只列这些）。
func Problems(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.State != OK {
			out = append(out, e)
		}
	}
	return out
}

// HasError 报告清单里有没有 error 条目。
func HasError(entries []Entry) bool {
	for _, e := range entries {
		if e.State == Error {
			return true
		}
	}
	return false
}
