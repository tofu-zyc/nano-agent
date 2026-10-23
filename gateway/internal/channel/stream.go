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
	Reasoning, Content, Notes, Source string
	Transient                         string
	Concise                           bool
	Tools                             []toolEntry
	Closed                            bool
	ver                               int // 每次变化加一，没变就不发请求
}

// toolEntry 是这一轮的一次工具调用
type toolEntry struct {
	Name, Detail string
	Done, OK     bool
	Result       string
}

func (s streamSnapshot) fallbackText() string {
	return strings.TrimSpace(s.Source + "\n\n" + s.Content + "\n\n" + s.Notes)
}

// status 是这一轮现在在干什么（给过程区的标题用）
func (s streamSnapshot) status() string {
	switch {
	case s.Closed:
		return "完成"
	case s.Transient != "":
		return s.Transient
	case len(s.Tools) > 0 && !s.Tools[len(s.Tools)-1].Done:
		return "正在执行 " + s.Tools[len(s.Tools)-1].Name
	case s.Content != "":
		return "正在回复"
	case s.Reasoning != "":
		return "正在思考"
	}
	return "处理中"
}

type streamWriter interface {
	Write(context.Context, streamSnapshot) error
	Close(context.Context) error
}

type streamRun struct {
	mu                  sync.Mutex
	reasoning, content  strings.Builder
	notes               []string
	tools               []toolEntry
	source              string
	transient           string
	concise             bool
	closed              bool
	ver                 int
	wake, closing, done chan struct{}
	interval            time.Duration
	writer              streamWriter
	fallback            func(context.Context, string) error
	log                 *slog.Logger
}

func newStreamRun(w streamWriter, fallback func(context.Context, string) error, log *slog.Logger, interval time.Duration) *streamRun {
	r := &streamRun{writer: w, fallback: fallback, log: log, interval: interval,
		wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	return r
}

// change 在锁里改一下状态，然后叫醒发送的协程
func (r *streamRun) change(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	f()
	r.ver++
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *streamRun) Status(text string) { r.change(func() { r.transient = text; r.concise = true }) }
func (r *streamRun) Reasoning(text string) {
	if text != "" {
		r.change(func() { r.reasoning.WriteString(text) })
	}
}
func (r *streamRun) Content(text string) {
	if text != "" {
		r.change(func() { r.content.WriteString(text) })
	}
}
func (r *streamRun) Tool(name, detail string) {
	r.change(func() {
		r.tools = append(r.tools, toolEntry{Name: name, Detail: streamClip(strings.TrimSpace(detail), 240)})
	})
}

// ToolResult 记到第一个还没结果的调用上（并行的几个调用按顺序对上）
func (r *streamRun) ToolResult(ok bool, detail string) {
	r.change(func() {
		for i := range r.tools {
			if !r.tools[i].Done {
				r.tools[i].Done, r.tools[i].OK, r.tools[i].Result = true, ok, streamClip(strings.TrimSpace(detail), 200)
				return
			}
		}
	})
}
func (r *streamRun) Note(text string) {
	if text != "" {
		r.change(func() { r.notes = append(r.notes, text) })
	}
}
func (r *streamRun) EndMessage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.content.Len() > 0 {
		r.content.WriteString("\n\n")
	}
}
func (r *streamRun) mirror(via, text string) RunView {
	r.change(func() { r.source = "来自" + via + "：" + streamClip(text, 1000) })
	return r
}
func (r *streamRun) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.ver++
		close(r.closing)
	}
}
func (r *streamRun) snapshot() streamSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return streamSnapshot{Reasoning: r.reasoning.String(), Content: strings.TrimSpace(r.content.String()),
		Tools: append([]toolEntry(nil), r.tools...), Notes: strings.Join(r.notes, "\n"), Source: r.source, Transient: r.transient, Concise: r.concise, Closed: r.closed, ver: r.ver}
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
		if !failed && snap.ver != last.ver {
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
