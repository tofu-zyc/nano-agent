package channel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type blockedStream struct {
	started chan struct{}
	release chan struct{}
	texts   []string
	closed  bool
	fail    bool
}

func (s *blockedStream) Write(_ context.Context, text string) error {
	if len(s.texts) == 0 {
		close(s.started)
		<-s.release
	}
	s.texts = append(s.texts, text)
	if s.fail {
		return errors.New("测试网络错误")
	}
	return nil
}
func (s *blockedStream) Close(context.Context) error { s.closed = true; return nil }

func awaitStream(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("流式任务没有结束")
	}
}

func TestStreamRunCoalescesAndClosesWithoutBlocking(t *testing.T) {
	w := &blockedStream{started: make(chan struct{}), release: make(chan struct{})}
	r := newStreamRun(w, func(context.Context, string) error { t.Error("不该降级"); return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	r.Reasoning("先看目录")
	awaitStream(t, w.started)
	// 第一次网络请求卡住时，模型增量、工具事件和 Close 都能立即返回。
	produced := make(chan struct{})
	go func() {
		r.EndMessage()
		r.Tool("Read", "a.txt")
		r.ToolResult(true, "读到了")
		for range 100 {
			r.Content("好")
		}
		r.EndMessage()
		r.Note("1 次模型调用")
		r.Close()
		r.Close()
		r.Content("不能出现")
		close(produced)
	}()
	awaitStream(t, produced)
	close(w.release)
	awaitStream(t, r.done)
	if !w.closed || len(w.texts) != 2 {
		t.Fatalf("close=%v writes=%d", w.closed, len(w.texts))
	}
	text := w.texts[1]
	for _, want := range []string{"💭 思考\n先看目录", "⚙ Read\na.txt", "✓ 读到了", strings.Repeat("好", 100), "1 次模型调用"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：%q", want, text)
		}
	}
	if strings.Contains(text, "不能出现") {
		t.Fatal(text)
	}
}

func TestStreamFailureFallsBackWithFinalText(t *testing.T) {
	w := &blockedStream{started: make(chan struct{}), release: make(chan struct{}), fail: true}
	var fallback string
	r := newStreamRun(w, func(_ context.Context, text string) error { fallback = text; return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	r.Content("开头")
	awaitStream(t, w.started)
	r.Content("和最后一段")
	r.Close()
	close(w.release)
	awaitStream(t, r.done)
	if fallback != "开头和最后一段" || !w.closed {
		t.Fatalf("fallback=%q closed=%v", fallback, w.closed)
	}
}
