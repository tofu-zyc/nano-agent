package channel

import (
	"context"
	"crypto/aes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/media"
)

// fakeWX 是假的 iLink 接口和 CDN。
type fakeWX struct {
	srv   *httptest.Server
	mu    sync.Mutex
	sent  []map[string]any
	typed []float64
	batch chan []map[string]any // 下一次 getupdates 返回的消息
	img   []byte                // CDN 上加密过的图片
}

func newFakeWX(t *testing.T) *fakeWX {
	f := &fakeWX{batch: make(chan []map[string]any, 4)}
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer bt1" && r.Header.Get("AuthorizationType") == "ilink_bot_token" &&
			r.Header.Get("X-WECHAT-UIN") != "" && r.Header.Get("iLink-App-Id") == "bot"
	}
	mux.HandleFunc("POST /ilink/bot/getupdates", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if !auth(r) {
			json.NewEncoder(w).Encode(map[string]any{"ret": -14})
			return
		}
		select {
		case msgs := <-f.batch:
			json.NewEncoder(w).Encode(map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": "cur-" + in["get_updates_buf"].(string) + "x"})
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /ilink/bot/sendmessage", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.sent = append(f.sent, in)
		f.mu.Unlock()
		w.Write([]byte(`{"ret":0}`))
	})
	mux.HandleFunc("POST /ilink/bot/getconfig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ret":0,"typing_ticket":"tt"}`))
	})
	mux.HandleFunc("POST /ilink/bot/sendtyping", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.typed = append(f.typed, in["status"].(float64))
		f.mu.Unlock()
		w.Write([]byte(`{"ret":0}`))
	})
	mux.HandleFunc("POST /ilink/bot/msg/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ret":0}`)) })
	mux.HandleFunc("GET /c2c/download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("encrypted_query_param") != "p&1" {
			w.WriteHeader(404)
			return
		}
		w.Write(f.img)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWX) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func aesECBEncrypt(data, key []byte) []byte {
	block, _ := aes.NewCipher(key)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	data = append(append([]byte(nil), data...), []byte(strings.Repeat(string(rune(pad)), pad))...)
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return out
}

func TestWeixinChannel(t *testing.T) {
	f := newFakeWX(t)
	dir := t.TempDir()
	store, err := media.Open(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 40))
	f.img = aesECBEncrypt(png, key)
	state := filepath.Join(dir, "bots", "wx1.state.json")
	cfg := WeixinConfig{Token: "bt1", BaseURL: f.srv.URL, CDN: f.srv.URL + "/c2c", StateFile: state}
	w, err := NewWeixin(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Message, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx, func(_ Channel, m Message) { got <- m }); close(done) }()

	f.batch <- []map[string]any{
		{"message_id": 11, "from_user_id": "u1@im.wechat", "message_type": 1, "context_token": "ctx1",
			"item_list": []map[string]any{{"type": 1, "text_item": map[string]string{"text": "在吗"}}}},
		{"message_id": 12, "from_user_id": "u1@im.wechat", "message_type": 1, "context_token": "ctx2",
			"item_list": []map[string]any{{"type": 2, "image_item": map[string]any{"aeskey": hex.EncodeToString(key),
				"media": map[string]string{"encrypt_query_param": "p&1"}}}, {"type": 3, "voice_item": map[string]string{"text": "晚上吃什么"}}}},
		{"message_id": 13, "from_user_id": "bot@im.bot", "message_type": 2}, // 机器人自己发的：不管
	}
	m := <-got
	if m.ChatID != "u1@im.wechat" || m.Text != "在吗" || m.Group || !m.Mentioned || m.MsgID != "11" {
		t.Fatalf("文字：%+v", m)
	}
	m = <-got
	if len(m.Images) != 1 || m.Text != "[语音] 晚上吃什么" {
		t.Fatalf("图片和语音：%+v", m)
	}
	if data, _ := store.Read(m.Images[0].ID); string(data) != string(png) {
		t.Fatalf("图片没解对：%q", data)
	}
	waitUntil(t, func() bool { return w.Status().State == StateOnline })

	// 回复带最新的 context_token，Markdown 转成纯文字
	if err := w.SendMarkdown(context.Background(), "u1@im.wechat", "## 晚饭\n- **面条**\n- [菜谱](https://x.y/z)", ""); err != nil {
		t.Fatal(err)
	}
	sent := f.messages()
	msg := sent[0]["msg"].(map[string]any)
	item := msg["item_list"].([]any)[0].(map[string]any)["text_item"].(map[string]any)["text"]
	if msg["to_user_id"] != "u1@im.wechat" || msg["context_token"] != "ctx2" || msg["message_type"] != float64(2) ||
		item != "晚饭\n- 面条\n- 菜谱（https://x.y/z）" || sent[0]["base_info"] == nil {
		t.Fatalf("发送：%v", sent[0])
	}

	// 正在输入：开始时显示，处理完取消
	stop := w.Busy("u1@im.wechat", "12")
	waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.typed) >= 1 })
	stop()
	waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.typed[len(f.typed)-1] == 2 })

	cancel()
	<-done
	// 游标和 context_token 存下来了：重启以后还能主动找人
	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), `"cursor":"cur-x"`) || !strings.Contains(string(b), `"ctx2"`) {
		t.Fatalf("状态文件：%s %v", b, err)
	}
	w2, _ := NewWeixin(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w2.Send(context.Background(), "u1@im.wechat", "早", "")
	sent = f.messages()
	if sent[len(sent)-1]["msg"].(map[string]any)["context_token"] != "ctx2" {
		t.Fatalf("重启后的主动消息：%v", sent[len(sent)-1])
	}
}

func TestWeixinStaleToken(t *testing.T) {
	f := newFakeWX(t)
	w, _ := NewWeixin(WeixinConfig{Token: "old", BaseURL: f.srv.URL}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, func(Channel, Message) {})
	waitUntil(t, func() bool { return strings.Contains(w.Status().Error, "重新扫码") })
}

// 扫码：等扫码、要输手机上的数字（输错一次）、确认
func TestWeixinRegistration(t *testing.T) {
	var mu sync.Mutex
	var codes []string
	step := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ilink/bot/get_bot_qrcode", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Query().Get("bot_type") != "3" || len(in["local_token_list"].([]any)) != 1 {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"qrcode":"q1","qrcode_img_content":"https://liteapp.weixin.qq.com/q/abc"}`))
	})
	mux.HandleFunc("GET /ilink/bot/get_qrcode_status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		code := r.URL.Query().Get("verify_code")
		codes = append(codes, code)
		step++
		switch {
		case step == 1:
			w.Write([]byte(`{"status":"wait"}`))
		case code == "" || code == "111":
			w.Write([]byte(`{"status":"need_verifycode"}`))
		default:
			w.Write([]byte(`{"status":"confirmed","bot_token":"bt9","ilink_bot_id":"b9@im.bot","baseurl":"https://ilinkai.weixin.qq.com","ilink_user_id":"me@im.wechat"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	old := WeixinLoginBase
	WeixinLoginBase = srv.URL
	defer func() { WeixinLoginBase = old }()

	reg, err := BeginWeixinRegistration(context.Background(), "", []string{"bt-old"})
	if err != nil {
		t.Fatal(err)
	}
	if reg.QRURL() != "https://liteapp.weixin.qq.com/q/abc" {
		t.Fatalf("二维码：%s", reg.QRURL())
	}
	var stages []string
	c, err := reg.Wait(context.Background(), func(stage, _ string) {
		stages = append(stages, stage)
		switch stage {
		case RegVerify:
			go reg.Verify("111") // 输错
		case RegVerifyWrong:
			go reg.Verify("222")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "bt9" || c.BotID != "b9@im.bot" || c.Owner != "me@im.wechat" {
		t.Fatalf("凭据：%+v", c)
	}
	if strings.Join(stages, ",") != RegVerify+","+RegVerifyWrong {
		t.Fatalf("进展：%v（%v）", stages, codes)
	}
}

func TestPlainText(t *testing.T) {
	in := "# 标题\n> 引用\n* 一\n```go\nx := **y**\n```\n---\n`代码` ![图](a.png)"
	want := "标题\n引用\n- 一\nx := **y**\n\n代码 图"
	if got := PlainText(in); got != want {
		t.Fatalf("PlainText:\n%q\n%q", got, want)
	}
}
