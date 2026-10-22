package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"nano-agent/gateway/internal/config"
)

type cardServer struct {
	mu      sync.Mutex
	texts   []string
	parts   []map[string]string
	panels  int
	seq     map[string]int
	closed  map[string]bool
	sent    []string
	deleted map[string]bool // 卡片ID/组件ID
	patched []string
	started chan struct{}
	deny    bool
}

func mockFeishu(t *testing.T, deny bool) (*Feishu, *cardServer) {
	t.Helper()
	m := &cardServer{seq: map[string]int{}, closed: map[string]bool{}, deleted: map[string]bool{}, started: make(chan struct{}, 1), deny: deny}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/open-apis/auth/") {
			io.WriteString(w, `{"code":0,"expire":7200,"tenant_access_token":"test-token","app_access_token":"test-app-token"}`)
			return
		}
		var p struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MsgType  string `json:"msg_type"`
			Content  string `json:"content"`
			Settings string `json:"settings"`
			Sequence int    `json:"sequence"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		m.mu.Lock()
		defer m.mu.Unlock()
		data := map[string]any{}
		switch {
		case r.Method == "POST" && r.URL.Path == "/open-apis/cardkit/v1/cards":
			if len(p.Data) > 30000 {
				t.Error("单卡 JSON 超出体积预算")
			}
			if m.deny {
				io.WriteString(w, `{"code":20001,"msg":"需要开通卡片权限"}`)
				return
			}
			var card struct {
				Schema string `json:"schema"`
				Config struct {
					Streaming bool `json:"streaming_mode"`
				} `json:"config"`
				Body struct {
					Elements []map[string]any `json:"elements"`
				} `json:"body"`
			}
			json.Unmarshal([]byte(p.Data), &card)
			if p.Type != "card_json" || card.Schema != "2.0" || !card.Config.Streaming || len(card.Body.Elements) == 0 {
				t.Errorf("卡片结构不对：%s", p.Data)
				return
			}
			parts := map[string]string{}
			var walk func(map[string]any)
			walk = func(e map[string]any) {
				if id, ok := e["element_id"].(string); ok && e["tag"] == "markdown" {
					parts[id] = e["content"].(string)
				}
				if e["tag"] == "collapsible_panel" {
					m.panels++
					if e["expanded"] != false {
						t.Error("过程面板应当默认折叠")
					}
				}
				if children, ok := e["elements"].([]any); ok {
					for _, child := range children {
						walk(child.(map[string]any))
					}
				}
			}
			for _, e := range card.Body.Elements {
				walk(e)
			}
			m.parts = append(m.parts, parts)
			m.texts = append(m.texts, parts["reply"])
			data["card_id"] = fmt.Sprint(len(m.texts))
		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/"):
			m.patched = append(m.patched, p.Content)
		case r.Method == "POST" && r.URL.Path == "/open-apis/im/v1/messages":
			m.sent = append(m.sent, p.MsgType+":"+p.Content)
			data["message_id"] = "msg-test"
			if p.MsgType == "interactive" {
				select {
				case m.started <- struct{}{}:
				default:
				}
			}
		case strings.HasPrefix(r.URL.Path, "/open-apis/cardkit/v1/cards/"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/open-apis/cardkit/v1/cards/"), "/")[0]
			if p.Sequence <= m.seq[id] {
				t.Errorf("序号没有递增：%d <= %d", p.Sequence, m.seq[id])
			}
			m.seq[id] = p.Sequence
			if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/content") {
				var i int
				fmt.Sscan(id, &i)
				path := strings.Split(r.URL.Path, "/")
				element := path[len(path)-2]
				m.parts[i-1][element] = p.Content
				if element == "reply" {
					m.texts[i-1] = p.Content
				}
			} else if r.Method == "DELETE" && strings.Contains(r.URL.Path, "/elements/") {
				path := strings.Split(r.URL.Path, "/")
				m.deleted[id+"/"+path[len(path)-1]] = true
			} else if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/settings") {
				var settings struct {
					Config struct {
						Streaming bool `json:"streaming_mode"`
					} `json:"config"`
				}
				json.Unmarshal([]byte(p.Settings), &settings)
				m.closed[id] = !settings.Config.Streaming
			} else {
				t.Errorf("未知卡片请求：%s %s", r.Method, r.URL.Path)
			}
		default:
			t.Errorf("未知请求：%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := NewFeishu(config.Feishu{Proxy: "none"}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	f.client = lark.NewClient(t.Name(), "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()), lark.WithLogger(sdkLogger{log}))
	return f, m
}

func TestFeishuStreamsAndClosesCard(t *testing.T) {
	f, m := mockFeishu(t, false)
	r := f.OpenRun("chat-test", "").(*streamRun)
	r.Reasoning("先看一下")
	awaitStream(t, m.started)
	r.EndMessage()
	r.Tool("Read", "a.txt")
	r.ToolResult(true, "读取成功")
	r.Content("完整回复")
	r.Note("1 次模型调用")
	r.Close()
	awaitStream(t, r.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	// 一张卡片：折叠的思考和工具、回复、状态；结束后去掉停止按钮
	p := m.parts[0]
	if len(m.texts) != 1 || m.texts[0] != "完整回复" || !strings.Contains(p["tools"], "读取成功") || !strings.Contains(p["think"], "先看一下") ||
		m.panels != 1 || !strings.Contains(p["foot"], "完成") || !strings.Contains(p["foot"], "1 次模型调用") || !m.closed["1"] || m.seq["1"] < 2 {
		t.Fatalf("parts=%v closed=%v seq=%v", m.parts, m.closed, m.seq)
	}
	if !m.deleted["1/stop"] || m.deleted["1/proc"] {
		t.Fatalf("deleted=%v", m.deleted)
	}
	if len(m.sent) != 1 || !strings.HasPrefix(m.sent[0], "interactive:") || !strings.Contains(m.sent[0], `"card_id":"1"`) {
		t.Fatalf("sent=%v", m.sent)
	}
}

func TestFeishuSimpleReplyDropsEmptyPanel(t *testing.T) {
	f, m := mockFeishu(t, false)
	r := f.OpenRun("chat-test", "").(*streamRun)
	r.Content("你好")
	r.Close()
	awaitStream(t, r.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.texts[0] != "你好" || !m.deleted["1/proc"] || !m.deleted["1/stop"] {
		t.Fatalf("texts=%v deleted=%v", m.texts, m.deleted)
	}
}

func TestFeishuSplitsLongReplyAndClosesEveryCard(t *testing.T) {
	f, m := mockFeishu(t, false)
	s := &feishuStream{f: f, chat: "chat-test"}
	text := strings.Repeat("文档🙂", 7000)
	if err := s.Write(context.Background(), streamSnapshot{Content: text, ver: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.texts, "") != text || len(m.closed) != len(m.texts) || len(m.texts) < 4 {
		t.Fatalf("长回复丢失或有卡片未关闭：%d %v", len(m.texts), m.closed)
	}
}

func TestFeishuWithoutCardPermissionSendsFullText(t *testing.T) {
	f, m := mockFeishu(t, true)
	r := f.OpenRun("chat-test", "").(*streamRun)
	r.Content("你好")
	r.Content("，这是完整回复")
	r.Close()
	awaitStream(t, r.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) != 1 || !strings.HasPrefix(m.sent[0], "text:") || !strings.Contains(m.sent[0], "你好，这是完整回复") {
		t.Fatalf("sent=%v", m.sent)
	}
}

func TestFeishuLargeProcessStaysBoundedAndReplyIsComplete(t *testing.T) {
	f, m := mockFeishu(t, false)
	r := f.OpenMirror("chat-test", "网页", strings.Repeat("用户消息🙂", 500)).(*streamRun)
	r.Reasoning(strings.Repeat("思考🙂", 6000))
	for range 100 {
		r.Tool("Shell", strings.Repeat("参数🙂", 200))
		r.ToolResult(true, strings.Repeat("结果🙂", 100))
	}
	reply := strings.Repeat("🙂", 7000)
	r.Content(reply)
	r.Close()
	awaitStream(t, r.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.texts, "") != reply || len(m.parts) != 2 || m.panels != 1 || !strings.Contains(m.parts[0]["foot"], "100 次工具调用") ||
		!strings.Contains(m.parts[0]["src"], "来自网页") {
		t.Fatalf("过程挤入正文或长回复丢失: cards=%d panels=%d", len(m.parts), m.panels)
	}
}

func TestFeishuFirstCardStaysSmallWhenWrittenLate(t *testing.T) {
	f, m := mockFeishu(t, false)
	s := &feishuStream{f: f, chat: "chat-test"}
	snap := streamSnapshot{Source: "来自网页：" + strings.Repeat("用户消息🙂", 200), Reasoning: strings.Repeat("思考🙂", 6000),
		Content: strings.Repeat("🙂", 7000)}
	for range 100 {
		snap.Tools = append(snap.Tools, toolEntry{Name: "Shell", Detail: strings.Repeat("参数🙂", 200), Done: true, OK: true,
			Result: strings.Repeat("结果🙂", 100)})
	}
	if err := s.Write(context.Background(), snap); err != nil { // 体积超了 mock 会报错
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.parts) != 2 {
		t.Fatalf("长回复应该分两张卡：%d", len(m.parts))
	}
}

func TestFeishuApprovalCard(t *testing.T) {
	f, m := mockFeishu(t, false)
	card := ApprovalCard{N: 5, Title: "写入文件", Detail: "a.txt（3 字节）", From: "feishu:ou_1"}
	ref, err := f.SendApproval(context.Background(), "chat-test", card)
	if err != nil || ref != "msg-test" {
		t.Fatal(ref, err)
	}
	if err := f.ResolveApproval(context.Background(), "chat-test", ref, card, "已拒绝 · 张三"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !strings.Contains(m.sent[0], `"a":"approve"`) || !strings.Contains(m.sent[0], `"n":"5"`) || !strings.Contains(m.sent[0], "需要审批 #5 · 写入文件") {
		t.Fatalf("审批卡片：%s", m.sent[0])
	}
	if len(m.patched) != 1 || strings.Contains(m.patched[0], "approve") || !strings.Contains(m.patched[0], "已拒绝 · 张三") || !strings.Contains(m.patched[0], `"red"`) {
		t.Fatalf("处理后的卡片：%v", m.patched)
	}
}
