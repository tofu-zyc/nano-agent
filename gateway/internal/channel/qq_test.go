package channel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeQQ 是假的 QQ 开放平台：换凭证、网关地址、WebSocket 网关、发消息接口。
type fakeQQ struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	posts  []qqPost
	noMD   bool                // Markdown 消息回 400
	events chan map[string]any // 要推给机器人的事件
	ident  chan map[string]any
}

type qqPost struct {
	Path string
	Body map[string]any
}

func newFakeQQ(t *testing.T) *fakeQQ {
	f := &fakeQQ{t: t, events: make(chan map[string]any, 10), ident: make(chan map[string]any, 2)}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["clientSecret"] != "secret" {
			w.Write([]byte(`{"code":100016,"message":"invalid appid or secret"}`))
			return
		}
		w.Write([]byte(`{"access_token":"tok1","expires_in":"7200"}`))
	})
	mux.HandleFunc("GET /gateway", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "QQBot tok1" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 30000}})
		var id map[string]any
		if c.ReadJSON(&id) != nil {
			return
		}
		f.ident <- id
		c.WriteJSON(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "s1", "user": map[string]string{"id": "BOT1", "username": "小纳"}}})
		seq := 1
		for {
			select {
			case ev := <-f.events:
				seq++
				ev["op"], ev["s"] = 0, seq
				if c.WriteJSON(ev) != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /v2/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posts = append(f.posts, qqPost{r.URL.Path, body})
		noMD := f.noMD
		f.mu.Unlock()
		if noMD && body["msg_type"] == float64(2) {
			w.WriteHeader(400)
			w.Write([]byte(`{"code":304003,"message":"no markdown permission"}`))
			return
		}
		w.Write([]byte(`{"id":"r1"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQQ) sent() []qqPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]qqPost(nil), f.posts...)
}

func TestQQChannel(t *testing.T) {
	f := newFakeQQ(t)
	q, err := NewQQ(QQConfig{AppID: "1024", AppSecret: "secret", APIBase: f.srv.URL, TokenURL: f.srv.URL + "/token"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	q.SetName("qq.x")
	got := make(chan Message, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, func(_ Channel, m Message) { got <- m })

	id := <-f.ident
	if d := id["d"].(map[string]any); id["op"] != float64(2) || d["token"] != "QQBot tok1" || d["intents"] != float64(1<<25) {
		t.Fatalf("IDENTIFY：%v", id)
	}
	waitUntil(t, func() bool { return q.Status().State == StateOnline && q.Status().Identity == "小纳" })

	// 私聊
	f.events <- map[string]any{"t": "C2C_MESSAGE_CREATE", "d": map[string]any{"id": "m1", "content": " 你好 ", "author": map[string]string{"user_openid": "U1"}}}
	m := <-got
	if m.ChatID != "U1" || m.SenderID != "U1" || m.Text != "你好" || m.Group || !m.Mentioned || m.MsgID != "m1" {
		t.Fatalf("私聊：%+v", m)
	}
	// 重连后重发的同一条：不再交上去
	f.events <- map[string]any{"t": "C2C_MESSAGE_CREATE", "d": map[string]any{"id": "m1", "content": "你好", "author": map[string]string{"user_openid": "U1"}}}
	// 群里 @ 它，和没 @ 它的
	f.events <- map[string]any{"t": "GROUP_AT_MESSAGE_CREATE", "d": map[string]any{"id": "m2", "content": "<@!BOT1> 在吗", "group_openid": "G1",
		"author": map[string]any{"member_openid": "M1", "username": "张三"}}}
	m = <-got
	if m.ChatID != "g.G1" || m.SenderID != "M1" || m.SenderName != "张三" || m.Text != "在吗" || !m.Group || !m.Mentioned {
		t.Fatalf("群里 @：%+v", m)
	}
	f.events <- map[string]any{"t": "GROUP_MESSAGE_CREATE", "d": map[string]any{"id": "m3", "content": "随便聊聊 <@!OTHER>", "group_openid": "G1",
		"author": map[string]any{"member_openid": "M2"}}}
	m = <-got
	if m.Mentioned || m.Text != "随便聊聊 @某人" {
		t.Fatalf("群里没 @：%+v", m)
	}

	// 回复：带上那条消息的 msg_id，序号从 1 开始；回满 4 次以后就是主动消息
	bg := context.Background()
	if err := q.Typing(bg, "U1"); err != nil {
		t.Fatal(err)
	}
	q.Typing(bg, "U1") // 每条消息只显示一次“正在输入”
	for i := 0; i < 5; i++ {
		if err := q.SendMarkdown(bg, "U1", "**好的**", ""); err != nil {
			t.Fatal(err)
		}
	}
	q.Send(bg, "g.G1", "群里的回复", "")
	posts := f.sent()
	if len(posts) != 7 {
		t.Fatalf("发了 %d 条：%v", len(posts), posts)
	}
	if p := posts[0]; p.Path != "/v2/users/U1/messages" || p.Body["msg_type"] != float64(6) || p.Body["msg_id"] != "m1" {
		t.Fatalf("正在输入：%+v", p)
	}
	for i := 1; i <= 4; i++ {
		if p := posts[i]; p.Body["msg_type"] != float64(2) || p.Body["msg_id"] != "m1" || p.Body["msg_seq"] != float64(i) ||
			p.Body["markdown"].(map[string]any)["content"] != "**好的**" {
			t.Fatalf("第 %d 次回复：%+v", i, p)
		}
	}
	if p := posts[5]; p.Body["msg_id"] != nil {
		t.Fatalf("回满了还带 msg_id：%+v", p)
	}
	if p := posts[6]; p.Path != "/v2/groups/G1/messages" || p.Body["msg_type"] != float64(0) || p.Body["msg_id"] != "m3" || p.Body["content"] != "群里的回复" {
		t.Fatalf("群里回复：%+v", p)
	}

	// 没有 Markdown 权限：退回纯文本，之后都发纯文本
	f.mu.Lock()
	f.noMD, f.posts = true, nil
	f.mu.Unlock()
	q.SendMarkdown(bg, "U1", "**一**", "")
	q.SendMarkdown(bg, "U1", "**二**", "")
	posts = f.sent()
	if len(posts) != 3 || posts[1].Body["msg_type"] != float64(0) || posts[2].Body["msg_type"] != float64(0) {
		t.Fatalf("退回纯文本：%+v", posts)
	}
}

func TestQQBadSecret(t *testing.T) {
	f := newFakeQQ(t)
	q, _ := NewQQ(QQConfig{AppID: "1024", AppSecret: "wrong", APIBase: f.srv.URL, TokenURL: f.srv.URL + "/token"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := q.Run(context.Background(), func(Channel, Message) {})
	if err == nil || !strings.Contains(err.Error(), "AppSecret 不对") || q.Status().State != StateError {
		t.Fatalf("凭据不对：%v %+v", err, q.Status())
	}
}

// 扫码创建：拿任务、等扫码、二维码过期换一个、解开加密的 AppSecret
func TestQQRegistration(t *testing.T) {
	var mu sync.Mutex
	keys := map[string]string{}
	polls := map[string]int{}
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lite/create_bind_task", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		n++
		task := "task" + string(rune('0'+n))
		keys[task] = in["key"]
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": map[string]string{"task_id": task}})
	})
	mux.HandleFunc("POST /lite/poll_bind_result", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		task := in["task_id"]
		polls[task]++
		p, key := polls[task], keys[task]
		mu.Unlock()
		switch {
		case task == "task1": // 第一个二维码过期了
			json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": map[string]int{"status": 3}})
		case p < 2:
			json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": map[string]int{"status": 1}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": map[string]any{"status": 2, "bot_appid": 102030405,
				"bot_encrypt_secret": qqEncrypt(t, "s3cret", key), "user_openid": "OWNER"}})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	old := QQConnectBase
	QQConnectBase = srv.URL
	defer func() { QQConnectBase = old }()

	reg, err := BeginQQRegistration(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reg.(*qqRegistration).interval = time.Millisecond
	if !strings.Contains(reg.QRURL(), "connect.html?task_id=task1") {
		t.Fatalf("二维码：%s", reg.QRURL())
	}
	var stages []string
	c, err := reg.Wait(context.Background(), func(stage, url string) { stages = append(stages, stage+" "+url) })
	if err != nil {
		t.Fatal(err)
	}
	if c.AppID != "102030405" || c.AppSecret != "s3cret" || c.Owner != "OWNER" {
		t.Fatalf("凭据：%+v", c)
	}
	if len(stages) != 1 || !strings.HasPrefix(stages[0], RegRefreshed+" ") || !strings.Contains(stages[0], "task2") {
		t.Fatalf("进展：%v", stages)
	}
}

func qqEncrypt(t *testing.T, plain, key string) string {
	k, _ := base64.StdEncoding.DecodeString(key)
	block, err := aes.NewCipher(k)
	if err != nil {
		t.Fatal(err)
	}
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, 12)
	rand.Read(iv)
	return base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, []byte(plain), nil))
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("等太久了")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
