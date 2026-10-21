package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 聊天平台的流式显示：只在内存里追加，后台合并更新。网络请求不会占住调度层的锁。
type streamWriter interface {
	Write(context.Context, string) error
	Close(context.Context) error
}

type streamRun struct {
	mu       sync.Mutex
	text     strings.Builder
	kind     string
	closed   bool
	wake     chan struct{}
	closing  chan struct{}
	done     chan struct{}
	interval time.Duration
	writer   streamWriter
	fallback func(context.Context, string) error
	log      *slog.Logger
}

func newStreamRun(w streamWriter, fallback func(context.Context, string) error, log *slog.Logger, interval time.Duration) *streamRun {
	r := &streamRun{writer: w, fallback: fallback, log: log, interval: interval,
		wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	return r
}

func (r *streamRun) append(kind, head, text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if kind == "" || kind != r.kind {
		if r.text.Len() > 0 {
			r.text.WriteString("\n\n")
		}
		r.text.WriteString(head)
	}
	r.text.WriteString(text)
	r.kind = kind
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *streamRun) Reasoning(text string) { r.append("reasoning", "💭 思考\n", text) }
func (r *streamRun) Content(text string)   { r.append("content", "", text) }
func (r *streamRun) Tool(name, detail string) {
	r.append("", "⚙ ", strings.TrimSpace(name+"\n"+detail))
}
func (r *streamRun) ToolResult(ok bool, detail string) {
	mark := "✓ "
	if !ok {
		mark = "✗ "
	}
	r.append("", mark, detail)
}
func (r *streamRun) Note(text string) { r.append("", "", text) }
func (r *streamRun) EndMessage() {
	r.mu.Lock()
	r.kind = ""
	r.mu.Unlock()
}
func (r *streamRun) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.closing)
	}
}

func (r *streamRun) snapshot() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.text.String(), r.closed
}

func (r *streamRun) loop() {
	defer close(r.done)
	var last string
	var next time.Time
	failed := false
	for {
		select {
		case <-r.wake:
		case <-r.closing:
		}
		if delay := time.Until(next); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-r.closing:
			}
			timer.Stop()
		}
		text, closed := r.snapshot()
		if !failed && text != "" && text != last {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := r.writer.Write(ctx, text)
			cancel()
			if err != nil {
				r.log.Warn("流式更新失败，本轮结束后发送完整文本", "err", err)
				failed = true
			}
			last = text
			next = time.Now().Add(r.interval)
		}
		if closed {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := r.writer.Close(ctx); err != nil {
				r.log.Warn("关闭流式消息失败", "err", err)
			}
			cancel()
			if failed && text != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := r.fallback(ctx, text); err != nil {
					r.log.Warn("发送完整文本失败", "err", err)
				}
				cancel()
			}
			return
		}
	}
}

// 按固定位置切分，已经发出的前几段保持不变，只需更新末段。
func streamChunks(text string, limit int) []string {
	runes := []rune(text)
	var out []string
	for len(runes) > 0 {
		n := min(len(runes), limit)
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}
