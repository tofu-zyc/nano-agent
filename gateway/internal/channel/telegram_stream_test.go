package channel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"nano-agent/gateway/internal/config"
)

func TestTelegramStreamsEditsAndPreservesLongUnicodeReply(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	var methods []string
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
			ID   int    `json:"message_id"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		if len(utf16.Encode([]rune(p.Text))) > 4096 {
			t.Error("消息超过长度限制")
		}
		mu.Lock()
		defer mu.Unlock()
		method := strings.TrimPrefix(r.URL.Path, "/bottest/")
		methods = append(methods, method)
		if method == "sendMessage" {
			texts = append(texts, p.Text)
			p.ID = len(texts)
		} else if method == "editMessageText" {
			texts[p.ID-1] = p.Text
		} else {
			t.Errorf("未知方法 %s", method)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": p.ID}})
		select {
		case first <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := tg.OpenRun("123", "").(*streamRun)
	r.Content("开始")
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("没有增量消息")
	}
	long := strings.Repeat("中文🙂", 3000)
	r.Content(long)
	r.Close()
	awaitStream(t, r.done)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(texts, "") != "开始"+long {
		t.Fatal("长回复被截断或重复")
	}
	if len(methods) < 2 || methods[0] != "sendMessage" || methods[1] != "editMessageText" {
		t.Fatalf("methods: %v", methods)
	}
}

func TestTelegramStreamHonorsRateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]int{"retry_after": 1}})
		} else {
			io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		}
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &telegramStream{t: tg, chat: "123"}
	if err := s.Write(context.Background(), streamSnapshot{Content: "你好"}); err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestTelegramSeparatesCollapsedProcessFromReply(t *testing.T) {
	var mu sync.Mutex
	texts := map[int]string{}
	modes := map[int]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
			ID   int    `json:"message_id"`
			Mode string `json:"parse_mode"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		defer mu.Unlock()
		if p.ID == 0 {
			p.ID = len(texts) + 1
		}
		texts[p.ID] = p.Text
		modes[p.ID] = p.Mode
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": p.ID}})
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := tg.OpenMirror("123", "网页", "看看目录").(*streamRun)
	r.Reasoning("思考中的 <标签> & 说明")
	for range 100 {
		r.Tool("Shell", strings.Repeat("原始工具参数", 200))
		r.ToolResult(true, "执行成功")
	}
	r.Content("这是最终答复")
	r.Note("完成统计")
	r.Close()
	awaitStream(t, r.done)
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 2 || texts[2] != "这是最终答复" || modes[2] != "" {
		t.Fatalf("正文混入过程或消息过多: %v", texts)
	}
	if !strings.Contains(texts[1], "<blockquote expandable>") || strings.Contains(texts[1], "<标签>") || !strings.Contains(texts[1], "100 次工具调用") || !strings.Contains(texts[1], "来自网页") {
		t.Fatalf("过程没有折叠、转义或来源: %s", texts[1])
	}
}
