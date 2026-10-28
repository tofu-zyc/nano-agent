// Package events 是网关内部的事件总线：调度层把发生的事情（消息进出、模型调用、工具执行、
// 审批、内核和渠道的状态变化）发布出来，网页界面通过 SSE 订阅，实时显示。
package events

import (
	"nano-agent/gateway/internal/proto"
	"sync"
	"time"
)

// Event 是一条事件。T 是类型，其余字段按类型取用，没用到的省略。
type Event struct {
	RunID   string         `json:"run_id,omitempty"`
	TurnID  int            `json:"turn_id,omitempty"`
	Sandbox *proto.Sandbox `json:"sandbox,omitempty"`
	T       string         `json:"t"`
	At      int64          `json:"at"` // 毫秒时间戳
	SID     string         `json:"sid,omitempty"`
	Kind    string         `json:"kind,omitempty"`
	ID      string         `json:"id,omitempty"`
	Name    string         `json:"name,omitempty"`
	Text    string         `json:"text,omitempty"`
	OK      *bool          `json:"ok,omitempty"`
	N       int            `json:"n,omitempty"`
	Data    any            `json:"data,omitempty"`
}

// Bus 把事件分发给所有订阅者。发布从不阻塞：订阅者处理不过来时丢掉它的事件。
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe 订阅事件，用完调用返回的 cancel。
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 1024)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Bool 返回指向 v 的指针，用于 Event.OK。
func Bool(v bool) *bool { return &v }
