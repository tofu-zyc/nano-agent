package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
)

// 扫码绑定 QQ 机器人：拿到凭据就加一个机器人、绑给选的 agent，扫码的人成了管理员
func TestBotRegisterQQ(t *testing.T) {
	srv, s, conf := setup(t)
	dir := t.TempDir()
	store, _ := agents.Open(filepath.Join(dir, "agents.json"))
	bots, _ := agents.OpenBots(filepath.Join(dir, "bots.json"))
	s.app.SetAgents(store, bots, filepath.Join(dir, "agents"))
	if err := s.app.MigrateAgents(); err != nil {
		t.Fatal(err)
	}

	var key string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lite/create_bind_task", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		key = in["key"]
		w.Write([]byte(`{"retcode":0,"data":{"task_id":"t1"}}`))
	})
	mux.HandleFunc("POST /lite/poll_bind_result", func(w http.ResponseWriter, r *http.Request) {
		k, _ := base64.StdEncoding.DecodeString(key)
		block, _ := aes.NewCipher(k)
		gcm, _ := cipher.NewGCM(block)
		iv := make([]byte, 12)
		rand.Read(iv)
		enc := base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, []byte("sec"), nil))
		json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": map[string]any{"status": 2, "bot_appid": "777", "bot_encrypt_secret": enc, "user_openid": "OWN"}})
	})
	// 机器人建好以后渠道会去连开放平台：连到这个假服务（凭证接口说凭据不对，渠道停下）
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":100016,"message":"invalid appid or secret"}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()
	oldBase, oldAPI, oldTok := channel.QQConnectBase, channel.QQAPIBase, channel.QQTokenURL
	channel.QQConnectBase, channel.QQAPIBase, channel.QQTokenURL = fake.URL, fake.URL, fake.URL+"/token"
	defer func() { channel.QQConnectBase, channel.QQAPIBase, channel.QQTokenURL = oldBase, oldAPI, oldTok }()

	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	code, out := c.do("POST", "/api/bots/register", map[string]string{"kind": "qq", "name": "我的 QQ", "agent": "default"}, true)
	if code != 200 || !strings.HasPrefix(out["qr_svg"].(string), "<svg") || !strings.Contains(out["url"].(string), "task_id=t1") {
		t.Fatalf("开始扫码：%d %v", code, out)
	}
	var bot map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for bot == nil && time.Now().Before(deadline) {
		_, list := c.do("GET", "/api/bots", nil, true)
		for _, b := range list["items"].([]any) {
			if m := b.(map[string]any); m["kind"] == "qq" {
				bot = m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bot == nil {
		t.Fatal("没加上机器人")
	}
	if bot["name"] != "我的 QQ" || bot["agent"] != "default" || bot["app_id"] != "777" || bot["has_secret"] != true || bot["channel"] != "qq.qq" || bot["owner"] != "qq.qq:OWN" {
		t.Fatalf("机器人：%v", bot)
	}
	if !slices.Contains(conf.Current().Access.Admins, "qq.qq:OWN") {
		t.Fatalf("扫码的人没成管理员：%v", conf.Current().Access.Admins)
	}
	// 不支持的种类
	if code, _ := c.do("POST", "/api/bots/register", map[string]string{"kind": "telegram"}, true); code != http.StatusBadRequest {
		t.Fatalf("telegram 不该能扫码：%d", code)
	}
}
