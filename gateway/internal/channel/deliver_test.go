package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/media"
)

func TestErrGoneAndRetryAfter(t *testing.T) {
	gone := map[string]*tgError{
		"被拉黑":   {Method: "sendMessage", Code: 403, Desc: "Forbidden: bot was blocked by the user"},
		"被踢出":   {Method: "sendMessage", Code: 403, Desc: "Forbidden: bot was kicked from the group chat"},
		"会话不存在": {Method: "sendMessage", Code: 400, Desc: "Bad Request: chat not found"},
	}
	for name, e := range gone {
		if !errors.Is(e, ErrGone) || !errors.Is(fmt.Errorf("发送失败：%w", e), ErrGone) {
			t.Errorf("%s 应是 ErrGone", name)
		}
		if _, ok := RetryAfter(e); ok {
			t.Errorf("%s 没有等待时间", name)
		}
	}
	temporary := map[string]error{
		"文字太长":  &tgError{Code: 400, Desc: "Bad Request: message is too long"},
		"解析失败":  &tgError{Code: 400, Desc: "Bad Request: can't parse entities"},
		"服务端错误": &tgError{Code: 502, Desc: "Bad Gateway"},
		"限流":    &tgError{Code: 429, Desc: "Too Many Requests: retry after 7", RetryAfter: 7},
		"网络":    errors.New("connection reset"),
	}
	for name, err := range temporary {
		if errors.Is(err, ErrGone) {
			t.Errorf("%s 不能算 ErrGone", name)
		}
	}
	d, ok := RetryAfter(fmt.Errorf("发送：%w", temporary["限流"]))
	if !ok || d != 7*time.Second {
		t.Fatalf("429 的等待时间: %v %v", d, ok)
	}
	if d, ok := RetryAfter(&tgError{Code: 429}); !ok || d != time.Second {
		t.Fatalf("429 没给时间至少等 1 秒: %v %v", d, ok)
	}
	for _, name := range []string{"文字太长", "服务端错误", "网络"} {
		if _, ok := RetryAfter(temporary[name]); ok {
			t.Errorf("%s 没有等待时间", name)
		}
	}
	// 平台上其他渠道的错误默认都是临时的
	if errors.Is(&qqError{Status: 403}, ErrGone) {
		t.Error("QQ 的错误拿不准，按临时失败处理")
	}
}

// longText 让代码围栏、加粗和链接跨过每个切点：长度超过 limit 的两倍以上。
func longText(limit int) string {
	var b strings.Builder
	b.WriteString("# 标题\n**加粗**和[链接](https://x.y/z)，<&> 🙂\n```go\n")
	for i := 0; utf8.RuneCountInString(b.String()) < limit*5/2; i++ {
		fmt.Fprintf(&b, "第 %d 行 x := %d // 中文🙂\n", i, i)
	}
	b.WriteString("```\n结尾：收尾的话。")
	return b.String()
}

// whole 是现在的发法：App.sendText。
func whole(ctx context.Context, ch Channel, chat, text, reply string, md bool) error {
	if m, ok := ch.(Markdowner); ok && md {
		return m.SendMarkdown(ctx, chat, text, reply)
	}
	return ch.Send(ctx, chat, text, reply)
}

// byParts 是出站的发法：先 Parts 切段，回复引用只用于第一段，再逐段发。
func byParts(ctx context.Context, ch Channel, chat, text, reply string, md bool) error {
	parts, plain := ch.(Parter).Parts(text, md)
	for i, part := range parts {
		r := ""
		if i == 0 {
			r = reply
		}
		var err error
		if m, ok := ch.(Markdowner); ok && !plain {
			err = m.SendMarkdown(ctx, chat, part, r)
		} else {
			err = ch.Send(ctx, chat, part, r)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkParts 用同一个假服务器比较“整条发”和“按 Parts 逐段发”：平台收到的请求序列必须完全相同。
func checkParts(t *testing.T, ch Channel, limits []int, record func() []string, reset func()) {
	t.Helper()
	ctx := context.Background()
	for _, md := range []bool{false, true} {
		for _, limit := range limits {
			for _, reply := range []string{"", "m-9"} {
				text := longText(limit)
				name := fmt.Sprintf("markdown=%v limit=%d reply=%q", md, limit, reply)
				reset()
				if err := whole(ctx, ch, "c1", text, reply, md); err != nil {
					t.Fatalf("%s 整条发: %v", name, err)
				}
				want := record()
				reset()
				if err := byParts(ctx, ch, "c1", text, reply, md); err != nil {
					t.Fatalf("%s 逐段发: %v", name, err)
				}
				got := record()
				if len(want) < 2 {
					t.Fatalf("%s 文本应被切成多段: %d", name, len(want))
				}
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("%s 请求序列不同：整条 %d 条，逐段 %d 条\n%s", name, len(want), len(got), firstDiff(want, got))
				}
			}
		}
	}
	// 没超过上限的短消息不切
	for _, md := range []bool{false, true} {
		if parts, _ := ch.(Parter).Parts("你好", md); len(parts) != 1 || parts[0] == "" {
			t.Fatalf("短消息不切段: %q", parts)
		}
	}
}

func firstDiff(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			x, y := a[i], b[i]
			n := 0
			for n < len(x) && n < len(y) && x[n] == y[n] {
				n++
			}
			lo := max(0, n-60)
			return fmt.Sprintf("第 %d 条从字节 %d 起不同：\n整条 …%q\n逐段 …%q", i, n, x[lo:min(len(x), n+60)], y[lo:min(len(y), n+60)])
		}
	}
	return "前面都相同，条数不同"
}

func TestTelegramPartsMatchSend(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, strings.TrimPrefix(r.URL.Path, "/bottest/")+" "+string(body))
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": 1}})
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := Channel(tg).(Markdowner); ok {
		t.Fatal("Telegram 没有 SendMarkdown，Parts 的 plain 才恒为 true")
	}
	if _, plain := tg.Parts("x", true); !plain {
		t.Fatal("Telegram 一律用 Send")
	}
	checkParts(t, tg, []int{tgTextLimit}, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) },
		func() { mu.Lock(); seen = nil; mu.Unlock() })
}

func TestFeishuPartsMatchSend(t *testing.T) {
	f, m := mockFeishu(t, false)
	if parts, plain := f.Parts(longText(feishuPostLimit), true); plain || len(parts) < 3 {
		t.Fatalf("Markdown 按富文本发: %d %v", len(parts), plain)
	}
	if parts, plain := f.Parts(strings.Repeat("字", feishuTextLimit+1), false); !plain || len(parts) != 2 {
		t.Fatalf("纯文字 8000 字一段: %d %v", len(parts), plain)
	}
	checkParts(t, f, []int{feishuPostLimit, feishuTextLimit}, func() []string { m.mu.Lock(); defer m.mu.Unlock(); return append([]string(nil), m.sent...) },
		func() { m.mu.Lock(); m.sent = nil; m.mu.Unlock() })
}

func TestQQPartsMatchSend(t *testing.T) {
	f := newFakeQQ(t)
	q, err := NewQQ(QQConfig{AppID: "1024", AppSecret: "secret", APIBase: f.srv.URL, TokenURL: f.srv.URL + "/token"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, plain := q.Parts("x", true); plain {
		t.Fatal("QQ 的 Markdown 用 SendMarkdown")
	}
	if _, plain := q.Parts("x", false); !plain {
		t.Fatal("QQ 的纯文字用 Send")
	}
	rec := func() []string {
		var out []string
		for _, p := range f.sent() {
			b, _ := json.Marshal(p.Body)
			out = append(out, p.Path+" "+string(b))
		}
		return out
	}
	reset := func() { f.mu.Lock(); f.posts = nil; f.mu.Unlock() }
	checkParts(t, q, []int{qqTextLimit}, rec, reset)
	// 没有 Markdown 权限：第一次被拒后 QQ 记住了，之后都发纯文本；这时两种发法也一样
	f.mu.Lock()
	f.noMD = true
	f.mu.Unlock()
	if err := q.SendMarkdown(context.Background(), "c1", "试一下", ""); err != nil {
		t.Fatal(err)
	}
	checkParts(t, q, []int{qqTextLimit}, rec, reset)
}

func TestWeixinPartsMatchSend(t *testing.T) {
	f := newFakeWX(t)
	dir := t.TempDir()
	store, err := media.Open(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWeixin(WeixinConfig{Token: "bt1", BaseURL: f.srv.URL, CDN: f.srv.URL + "/c2c", StateFile: filepath.Join(dir, "wx.state.json")}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// 微信先转纯文字再切：围栏跨过切点也不会错，所以拿 Markdown 原文切段是不一样的
	md := longText(wxTextLimit)
	parts, plain := w.Parts(md, true)
	if !plain || strings.Contains(strings.Join(parts, ""), "```") {
		t.Fatalf("Markdown 先转纯文字: plain=%v", plain)
	}
	rec := func() []string {
		var out []string
		for _, m := range f.messages() {
			msg := m["msg"].(map[string]any)
			delete(msg, "client_id") // 每条随机
			b, _ := json.Marshal(msg)
			out = append(out, string(b))
		}
		return out
	}
	reset := func() { f.mu.Lock(); f.sent = nil; f.mu.Unlock() }
	checkParts(t, w, []int{wxTextLimit}, rec, reset)
}
