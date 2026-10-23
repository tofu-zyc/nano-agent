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
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if len(texts) != 2 || texts[2] != "这是最终答复" || modes[2] != "HTML" {
		t.Fatalf("正文混入过程或消息过多: %v", texts)
	}
	if !strings.Contains(texts[1], "<blockquote expandable>") || strings.Contains(texts[1], "<标签>") || !strings.Contains(texts[1], "100 次工具调用") || !strings.Contains(texts[1], "来自网页") {
		t.Fatalf("过程没有折叠、转义或来源: %s", texts[1])
	}
}

func TestTelegramStopButton(t *testing.T) {
	var mu sync.Mutex
	marked := map[int]bool{} // 消息现在有没有“停止”按钮
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			ID     int             `json:"message_id"`
			Markup json.RawMessage `json:"reply_markup"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		defer mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/bottest/") {
		case "sendMessage":
			next++
			p.ID = next
		case "editMessageText", "editMessageReplyMarkup":
		case "answerCallbackQuery": // 点按钮后的提示，和按钮状态无关
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
			return
		default:
			t.Errorf("未知方法 %s", r.URL.Path)
		}
		marked[p.ID] = strings.Contains(string(p.Markup), tgStopData)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": p.ID}})
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := tg.OpenRun("123", "").(*streamRun)
	r.Tool("Shell", "ls")
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		on := marked[1]
		mu.Unlock()
		if on {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("处理中的过程消息下面没有停止按钮")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.Content("好了")
	r.Close()
	awaitStream(t, r.done)
	mu.Lock()
	defer mu.Unlock()
	for id, on := range marked {
		if on {
			t.Fatalf("结束后消息 %d 还留着停止按钮", id)
		}
	}

	// 点按钮：交给网关的是一个停止操作
	got := make(chan Message, 1)
	tg.onButton(context.Background(), &tgCallbackQuery{ID: "q", From: tgUser{ID: 7, FirstName: "张"},
		Message: &tgMessage{Chat: tgChat{ID: 123, Type: "private"}}, Data: tgStopData}, func(_ Channel, m Message) { got <- m })
	if m := <-got; m.Action != ActionStop || m.ChatID != "123" || m.SenderID != "7" {
		t.Fatalf("按钮消息不对：%+v", m)
	}
}

func TestTelegramRendersMarkdownAndFallsBackToPlainText(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p)
		n := len(got)
		mu.Unlock()
		if n == 1 { // 第一次假装 HTML 解析失败
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &telegramStream{t: tg, chat: "123"}
	if err := s.Write(context.Background(), streamSnapshot{Content: "**粗体** 和 `代码`", Closed: true}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["text"] != "<b>粗体</b> 和 <code>代码</code>" || got[0]["parse_mode"] != "HTML" {
		t.Fatalf("第一次应当发 HTML：%v", got)
	}
	if got[1]["text"] != "粗体 和 代码" || got[1]["parse_mode"] != nil {
		t.Fatalf("HTML 发不出去时改发纯文本：%v", got[1])
	}
}

func TestTelegramApprovalButtons(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		p["method"] = strings.TrimPrefix(r.URL.Path, "/bottest/")
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	card := ApprovalCard{N: 3, Title: "执行命令", Detail: "rm -rf <build>", From: "tg:1"}
	ref, err := tg.SendApproval(context.Background(), "123", card)
	if err != nil || ref != "42" {
		t.Fatal(ref, err)
	}
	kb, _ := json.Marshal(got[0]["reply_markup"])
	if !strings.Contains(got[0]["text"].(string), "需要审批 #3 · 执行命令") || !strings.Contains(got[0]["text"].(string), "rm -rf &lt;build&gt;") ||
		!strings.Contains(string(kb), `"ap:3:1"`) || !strings.Contains(string(kb), `"ap:3:0"`) {
		t.Fatalf("审批消息：%v %s", got[0], kb)
	}
	if err := tg.ResolveApproval(context.Background(), "123", ref, card, "已批准 · 张三"); err != nil {
		t.Fatal(err)
	}
	if got[1]["method"] != "editMessageText" || !strings.Contains(got[1]["text"].(string), "已批准 · 张三") {
		t.Fatalf("处理后的审批消息：%v", got[1])
	}
	msgs := make(chan Message, 1)
	tg.onButton(context.Background(), &tgCallbackQuery{ID: "q", From: tgUser{ID: 7, FirstName: "张"},
		Message: &tgMessage{Chat: tgChat{ID: 123, Type: "private"}}, Data: "ap:3:0"}, func(_ Channel, m Message) { msgs <- m })
	if m := <-msgs; m.Action != ActionDeny || m.Text != "3" || m.SenderID != "7" {
		t.Fatalf("拒绝按钮：%+v", m)
	}
}

func TestTelegramConciseDeletesStatus(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path)
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &telegramStream{t: tg, chat: "123"}
	if err := s.Write(context.Background(), streamSnapshot{Concise: true, Transient: "正在执行 Shell"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), streamSnapshot{Concise: true, Content: "答案", Closed: true}); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 3 || !strings.HasSuffix(methods[1], "deleteMessage") || s.processID != 0 {
		t.Fatal(methods, s.processID)
	}
}
