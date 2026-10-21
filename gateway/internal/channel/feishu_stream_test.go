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
	started chan struct{}
	deny    bool
}

func mockFeishu(t *testing.T, deny bool) (*Feishu, *cardServer) {
	t.Helper()
	m := &cardServer{seq: map[string]int{}, closed: map[string]bool{}, started: make(chan struct{}, 1), deny: deny}
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
				if e["tag"] == "markdown" {
					parts[e["element_id"].(string)] = e["content"].(string)
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
	f, err := NewFeishu(config.Feishu{Proxy: "none"}, log)
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
	r.Close()
	awaitStream(t, r.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.texts) != 2 || m.texts[1] != "完整回复" || !strings.Contains(m.parts[0]["tools"], "读取成功") || m.parts[0]["reasoning"] != "先看一下" || m.panels != 2 || !m.closed["1"] || !m.closed["2"] || m.seq["1"] < 2 {
		t.Fatalf("texts=%v closed=%v seq=%v", m.texts, m.closed, m.seq)
	}
	if len(m.sent) != 2 || !strings.HasPrefix(m.sent[0], "interactive:") || !strings.Contains(m.sent[0], `"card_id":"1"`) {
		t.Fatalf("sent=%v", m.sent)
	}
}

func TestFeishuSplitsLongReplyAndClosesEveryCard(t *testing.T) {
	f, m := mockFeishu(t, false)
	s := &feishuStream{f: f, chat: "chat-test"}
	text := strings.Repeat("文档🙂", 7000)
	if err := s.Write(context.Background(), streamSnapshot{Content: text}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.texts, "") != text || len(m.closed) != len(m.texts) {
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
