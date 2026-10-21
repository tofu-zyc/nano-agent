package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 过程与正文分别保存，平台自己决定折叠和布局。网络请求不占调度锁。
type streamSnapshot struct {
	Reasoning, Content, Tools, Notes, Source string
	ToolCount                                int
	Closed                                   bool
}

func (s streamSnapshot) fallbackText() string {
	return strings.TrimSpace(s.Source + "\n\n" + s.Content + "\n\n" + s.Notes)
}

type streamWriter interface {
	Write(context.Context, streamSnapshot) error
	Close(context.Context) error
}

type streamRun struct {
	mu                               sync.Mutex
	reasoning, content, tools, notes strings.Builder
	source                           string
	toolCount                        int
	closed                           bool
	wake, closing, done              chan struct{}
	interval                         time.Duration
	writer                           streamWriter
	fallback                         func(context.Context, string) error
	log                              *slog.Logger
}

func newStreamRun(w streamWriter, fallback func(context.Context, string) error, log *slog.Logger, interval time.Duration) *streamRun {
	r := &streamRun{writer: w, fallback: fallback, log: log, interval: interval,
		wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	return r
}

func (r *streamRun) append(kind, text string, tool bool) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	var dst *strings.Builder
	switch kind {
	case "reasoning":
		dst = &r.reasoning
	case "content":
		dst = &r.content
	case "tools":
		dst = &r.tools
	default:
		dst = &r.notes
	}
	if (kind == "tools" || kind == "notes") && dst.Len() > 0 {
		dst.WriteString("\n")
	}
	dst.WriteString(text)
	if tool {
		r.toolCount++
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *streamRun) Reasoning(text string) { r.append("reasoning", text, false) }
func (r *streamRun) Content(text string)   { r.append("content", text, false) }
func (r *streamRun) Tool(name, detail string) {
	r.append("tools", "⚙ "+strings.TrimSpace(name+" · "+streamClip(detail, 240)), true)
}
func (r *streamRun) ToolResult(ok bool, detail string) {
	mark := "✓ "
	if !ok {
		mark = "✗ "
	}
	r.append("tools", mark+streamClip(detail, 200), false)
}
func (r *streamRun) Note(text string) { r.append("notes", text, false) }
func (r *streamRun) EndMessage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.content.Len() > 0 {
		r.content.WriteString("\n\n")
	}
}
func (r *streamRun) mirror(via, text string) RunView {
	r.mu.Lock()
	r.source = "来自" + via + "\n用户：" + streamClip(text, 1000)
	r.mu.Unlock()
	r.Note("正在处理…")
	return r
}
func (r *streamRun) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.closing)
	}
}
func (r *streamRun) snapshot() streamSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return streamSnapshot{Reasoning: r.reasoning.String(), Content: strings.TrimSpace(r.content.String()),
		Tools: r.tools.String(), Notes: r.notes.String(), Source: r.source, ToolCount: r.toolCount, Closed: r.closed}
}
func (r *streamRun) loop() {
	defer close(r.done)
	var last streamSnapshot
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
		snap := r.snapshot()
		if !failed && snap != last {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := r.writer.Write(ctx, snap)
			cancel()
			if err != nil {
				r.log.Warn("流式更新失败，本轮结束后发送正文", "err", err)
				failed = true
			}
			last = snap
			next = time.Now().Add(r.interval)
		}
		if snap.Closed {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := r.writer.Close(ctx); err != nil {
				r.log.Warn("关闭流式消息失败", "err", err)
			}
			cancel()
			if text := snap.fallbackText(); failed && text != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := r.fallback(ctx, text); err != nil {
					r.log.Warn("发送正文失败", "err", err)
				}
				cancel()
			}
			return
		}
	}
}

func streamClip(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…（完整过程见网页）"
	}
	return s
}

// 按固定位置切分，已经发出的前几段不再改变。
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
