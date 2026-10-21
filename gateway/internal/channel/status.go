package channel

import (
	"sync"
	"time"
)

// 渠道的连接状态
const (
	StateStarting = "starting"
	StateOnline   = "online"
	StateError    = "error"
	StateStopped  = "stopped"
)

// Status 是渠道的连接状态，网页界面用它画拓扑图。
type Status struct {
	State    string `json:"state"`
	Identity string `json:"identity,omitempty"` // 机器人在平台上的名字
	Error    string `json:"error,omitempty"`
	Since    int64  `json:"since"` // 进入这个状态的时刻（毫秒）
}

// StatusReporter 是可选能力：报告连接状态。
type StatusReporter interface {
	Status() Status
	OnStatus(func(Status)) // 状态变化时回调（在渠道自己的协程里执行，不能阻塞）
}

// statusBox 由各渠道内嵌，实现 StatusReporter。
type statusBox struct {
	mu       sync.Mutex
	st       Status
	onChange func(Status)
}

func (b *statusBox) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

func (b *statusBox) OnStatus(f func(Status)) {
	b.mu.Lock()
	b.onChange = f
	b.mu.Unlock()
}

func (b *statusBox) update(f func(*Status)) {
	b.mu.Lock()
	old := b.st
	f(&b.st)
	if b.st.State != old.State {
		b.st.Since = time.Now().UnixMilli()
	}
	st, cb := b.st, b.onChange
	b.mu.Unlock()
	if cb != nil && st != old {
		cb(st)
	}
}

func (b *statusBox) setState(state, errText string) {
	b.update(func(s *Status) { s.State, s.Error = state, errText })
}

func (b *statusBox) setIdentity(id string) {
	b.update(func(s *Status) { s.Identity = id })
}
