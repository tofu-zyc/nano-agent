package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

func setup(t *testing.T) (*httptest.Server, *Server, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "personas"), 0o755)
	os.WriteFile(filepath.Join(dir, "personas", "default.md"), []byte("你是测试机器人"), 0o644)
	path := filepath.Join(dir, "gateway.toml")
	os.WriteFile(path, []byte("[kernel]\ntoken = \"k\"\nlisten = \"unix:"+filepath.Join(dir, "k.sock")+"\"\n[llm]\nprovider = \"mock\"\n[web]\npassword = \"pw\"\n[cli]\nenabled = true\n"), 0o600)
	conf, _, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.Current()
	store, _ := session.New(cfg.Session.DataDir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := persona.New(cfg.Persona.Dir, log)
	a := app.New(ctx, cfg, &llm.Mock{}, store, p, log)
	a.SetHub(kernel.NewHub("k", a, log))
	conf.OnChange(func(old, cur *config.Config) { a.SetConfig(cur) })
	a.ReconcileChannels(cfg)
	s, err := New(a, conf, p, func(config.LLM) (llm.Client, error) { return &llm.Mock{}, nil }, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s, conf
}

type client struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client
}

func newClient(t *testing.T, srv *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, srv: srv, hc: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, hdr bool) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, r)
	if hdr {
		req.Header.Set("X-Nano", "1")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAuthAndGuards(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/overview", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("未登录应该 401：%d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "pw"}, false); code != http.StatusForbidden {
		t.Fatalf("没有 X-Nano 头应该被拒：%d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "wrong"}, true); code != http.StatusUnauthorized {
		t.Fatalf("密码错误：%d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "pw"}, true); code != 200 {
		t.Fatalf("登录：%d", code)
	}
	code, ov := c.do("GET", "/api/overview", nil, false)
	if code != 200 || ov["model"] == nil {
		t.Fatalf("overview：%d %v", code, ov)
	}
	chans, _ := ov["channels"].([]any)
	if len(chans) != 4 {
		t.Fatalf("channels：%v", ov["channels"])
	}
	// 首页是嵌进去的单页应用
	resp, _ := c.hc.Get(srv.URL + "/")
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !strings.Contains(string(b), "<") {
		t.Fatalf("index：%d", resp.StatusCode)
	}
}

func TestHostCheck(t *testing.T) {
	_, s, _ := setup(t)
	s.loopback = true
	h := s.Handler()
	for host, want := range map[string]int{"127.0.0.1:7788": 401, "localhost:7788": 401, "evil.example:7788": 403} {
		req := httptest.NewRequest("GET", "/api/overview", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("host %s：%d，期望 %d", host, w.Code, want)
		}
	}
}

func TestConfigAndChatEvents(t *testing.T) {
	srv, _, conf := setup(t)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)

	// 改配置：立即生效，来源变成网页；密钥不回传
	code, out := c.do("PATCH", "/api/config", map[string]any{"values": map[string]any{"workspace.mode": "auto", "llm.api_key": "sk-x"}}, true)
	if code != 200 || conf.Current().Workspace.Mode != "auto" || conf.Current().LLM.APIKey != "sk-x" {
		t.Fatalf("patch config：%d %v", code, out)
	}
	if strings.Contains(mustJSON(out), "sk-x") {
		t.Fatalf("密钥被返回了")
	}
	if code, _ := c.do("PATCH", "/api/config", map[string]any{"values": map[string]any{"workspace.mode": "yolo"}}, true); code != 400 {
		t.Fatalf("非法值应该 400：%d", code)
	}

	// 订阅事件，然后在网页里说话：没有内核时会收到提示
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	if code, out := c.do("POST", "/api/chat", map[string]string{"sid": "main", "text": "你好"}, true); code != 200 || out["sid"] != "main" {
		t.Fatalf("chat：%d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/chat", map[string]string{"sid": "../x", "text": "你好"}, true); code != 404 {
		t.Fatalf("不存在的会话：%d", code)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, `"t":"say"`) && strings.Contains(l, "没有在线的内核") && strings.Contains(l, `"sid":"main"`) {
				return
			}
		case <-deadline:
			t.Fatalf("没收到事件")
		}
	}
}

func TestPersonas(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	if code, _ := c.do("PUT", "/api/personas/tg%3A-100.md", map[string]string{"text": "群里的人格"}, true); code != 200 {
		t.Fatalf("put：%d", code)
	}
	code, out := c.do("GET", "/api/personas", nil, false)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 2 || items[0].(map[string]any)["name"] != "default.md" || items[1].(map[string]any)["sid"] != "tg:-100" {
		t.Fatalf("list：%v", out)
	}
	if code, _ := c.do("GET", "/api/personas/..%2Fgateway.toml", nil, false); code != 400 && code != 404 {
		t.Fatalf("路径穿越：%d", code)
	}
	if code, _ := c.do("DELETE", "/api/personas/default.md", nil, true); code != 400 {
		t.Fatalf("默认人格不能删：%d", code)
	}
}

func TestSessions(t *testing.T) {
	srv, s, _ := setup(t)
	s.app.SetCLI(channel.NewCLI(filepath.Join(t.TempDir(), "cli.sock"), false, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	// 主会话一直在，哪怕还没有消息（以前新会话没发过消息时打开设置会显示“会话不存在了”）
	if code, out := c.do("GET", "/api/sessions/main", nil, false); code != 200 || out["info"].(map[string]any)["title"] != "主会话" {
		t.Fatalf("main：%d %v", code, out)
	}
	if code, _ := c.do("GET", "/api/sessions/nope", nil, false); code != 404 {
		t.Fatalf("不存在的会话：%d", code)
	}
	code, out := c.do("POST", "/api/sessions", map[string]string{"title": "新的"}, true)
	sid, _ := out["sid"].(string)
	if code != 200 || out["title"] != "新的" || !strings.HasPrefix(sid, "s-") {
		t.Fatalf("new：%d %v", code, out)
	}
	// 新建的会话还没有消息也能打开、能改设置
	if code, out := c.do("PATCH", "/api/sessions/"+sid, map[string]any{"title": "改个名", "mode": "readonly"}, true); code != 200 || out["title"] != "改个名" || out["mode"] != "readonly" {
		t.Fatalf("patch：%d %v", code, out)
	}
	if code, _ := c.do("PATCH", "/api/sessions/nope", map[string]any{"mode": "auto"}, true); code != 400 {
		t.Fatalf("patch 不存在的会话：%d", code)
	}
	code, out = c.do("GET", "/api/overview", nil, false)
	if n := len(out["sessions"].([]any)); code != 200 || n != 2 {
		t.Fatalf("overview：%d %d", code, n)
	}
	// 命令行窗口一开始就接在主会话上
	if !strings.Contains(mustJSON(out["sessions"]), `"key":"cli:local"`) {
		t.Fatalf("命令行窗口：%v", out["sessions"])
	}
	if code, _ := c.do("PUT", "/api/endpoints/cli:local", map[string]string{"session": sid}, true); code != 200 {
		t.Fatalf("改接：%d", code)
	}
	if code, _ := c.do("DELETE", "/api/sessions/main", nil, true); code != 400 {
		t.Fatalf("主会话不能删：%d", code)
	}
	if code, _ := c.do("DELETE", "/api/sessions/"+sid, nil, true); code != 200 {
		t.Fatalf("delete：%d", code)
	}
	if code, _ := c.do("GET", "/api/sessions/"+sid, nil, false); code != 404 {
		t.Fatalf("删掉之后：%d", code)
	}
}

type fakeCh struct{}

func (fakeCh) Name() string                                     { return "fs" }
func (fakeCh) Run(ctx context.Context, _ channel.Handler) error { <-ctx.Done(); return nil }
func (fakeCh) Send(context.Context, string, string, string) error {
	return nil
}

func TestMakeAdmin(t *testing.T) {
	srv, s, conf := setup(t)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	s.app.AddChannel(fakeCh{})
	s.app.OnMessage(fakeCh{}, channel.Message{ChatID: "c9", SenderID: "u9", SenderName: "我", ChatName: "我", Text: "/session", Mentioned: true})
	if code, _ := c.do("POST", "/api/endpoints/fs:nope/admin", nil, true); code != 400 {
		t.Fatalf("不存在的窗口：%d", code)
	}
	if code, out := c.do("POST", "/api/endpoints/fs:c9/admin", nil, true); code != 200 || out["who"] != "fs:u9" {
		t.Fatalf("admin：%d %v", code, out)
	}
	if admins := conf.Current().Access.Admins; len(admins) != 1 || admins[0] != "fs:u9" {
		t.Fatalf("admins: %v", admins)
	}
	_, out := c.do("GET", "/api/sessions/main", nil, false)
	if !strings.Contains(mustJSON(out["info"]), `{"admin":true,"channel":"fs","key":"fs:c9","name":"我","session":"main","who":"fs:u9"}`) {
		t.Fatalf("窗口应该接到主会话：%v", out["info"])
	}
	// 再点一次不会重复添加
	c.do("POST", "/api/endpoints/fs:c9/admin", nil, true)
	if n := len(conf.Current().Access.Admins); n != 1 {
		t.Fatalf("重复添加：%d", n)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
