package channel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/media"
)

var gifData = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")

// Telegram：GIF 用 sendAnimation，文件放在 multipart 里
func TestTelegramSendMedia(t *testing.T) {
	var mu sync.Mutex
	var method, field, chat string
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		method = strings.TrimPrefix(r.URL.Path, "/bottest/")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		chat = r.FormValue("chat_id")
		for name, fs := range r.MultipartForm.File {
			field = name
			f, _ := fs[0].Open()
			got, _ = io.ReadAll(f)
		}
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	att := media.Attachment{Name: "doge.gif", MIME: "image/gif"}
	if err := tg.SendMedia(context.Background(), "42", att, gifData); err != nil {
		t.Fatal(err)
	}
	if method != "sendAnimation" || field != "animation" || chat != "42" || !bytes.Equal(got, gifData) {
		t.Fatalf("method=%s field=%s chat=%s", method, field, chat)
	}
	tg.SendMedia(context.Background(), "42", media.Attachment{Name: "a.pdf", MIME: "application/pdf"}, []byte("%PDF"))
	if method != "sendDocument" || field != "document" {
		t.Fatalf("文档：%s %s", method, field)
	}
}

// QQ：先上传（file_data 是 base64）拿 file_info，再发富媒体消息；不支持的类型报错
func TestQQSendMedia(t *testing.T) {
	var mu sync.Mutex
	var posts []qqPost
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"tok1","expires_in":7200}`))
	})
	mux.HandleFunc("POST /v2/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posts = append(posts, qqPost{r.URL.Path, body})
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/files") {
			w.Write([]byte(`{"file_info":"FI","ttl":3600}`))
			return
		}
		w.Write([]byte(`{"id":"x"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q, _ := NewQQ(QQConfig{AppID: "1", AppSecret: "s", APIBase: srv.URL, TokenURL: srv.URL + "/token"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := q.SendMedia(context.Background(), "g.G1", media.Attachment{Name: "doge.gif", MIME: "image/gif"}, gifData); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[0].Path != "/v2/groups/G1/files" || posts[0].Body["file_type"] != float64(1) ||
		posts[0].Body["file_data"] != base64.StdEncoding.EncodeToString(gifData) ||
		posts[1].Path != "/v2/groups/G1/messages" || posts[1].Body["msg_type"] != float64(7) || posts[1].Body["media"].(map[string]any)["file_info"] != "FI" {
		t.Fatalf("posts: %+v", posts)
	}
	if err := q.SendMedia(context.Background(), "U1", media.Attachment{Name: "a.pdf", MIME: "application/pdf"}, []byte("%PDF")); err == nil {
		t.Fatal("QQ 发不了 PDF，应该报错（网关改发说明）")
	}
}

// 微信：AES 加密后传到 CDN，下载参数从响应头拿，再发图片消息
func TestWeixinSendMedia(t *testing.T) {
	var mu sync.Mutex
	var key []byte
	var uploaded []byte
	var item map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ilink/bot/getuploadurl", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		key, _ = hex.DecodeString(in["aeskey"].(string))
		mu.Unlock()
		if in["media_type"] != float64(1) || in["rawsize"] != float64(len(gifData)) || in["to_user_id"] != "u1@im.wechat" {
			t.Errorf("getuploadurl: %v", in)
		}
		w.Write([]byte(`{"upload_param":"UP"}`))
	})
	mux.HandleFunc("POST /c2c/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("encrypted_query_param") != "UP" || r.URL.Query().Get("filekey") == "" {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		uploaded = b
		mu.Unlock()
		w.Header().Set("x-encrypted-param", "DP")
	})
	mux.HandleFunc("POST /ilink/bot/sendmessage", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		item = in["msg"].(map[string]any)["item_list"].([]any)[0].(map[string]any)
		mu.Unlock()
		w.Write([]byte(`{"ret":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wx, _ := NewWeixin(WeixinConfig{Token: "bt1", BaseURL: srv.URL, CDN: srv.URL + "/c2c"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := wx.SendMedia(context.Background(), "u1@im.wechat", media.Attachment{Name: "doge.gif", MIME: "image/gif"}, gifData); err != nil {
		t.Fatal(err)
	}
	plain, err := aesECBDecrypt(uploaded, key)
	if err != nil || !bytes.Equal(plain, gifData) {
		t.Fatalf("CDN 上的不是加密过的原图：%v", err)
	}
	img := item["image_item"].(map[string]any)
	ref := img["media"].(map[string]any)
	k, _ := base64.StdEncoding.DecodeString(ref["aes_key"].(string))
	if item["type"] != float64(2) || ref["encrypt_query_param"] != "DP" || string(k) != hex.EncodeToString(key) || img["mid_size"] != float64(len(uploaded)) {
		t.Fatalf("图片消息：%v", item)
	}
}
