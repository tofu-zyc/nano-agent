// Package web 是网关的网页管理界面：一个嵌在程序里的单页应用，加上它用的 JSON 接口和事件流。
//
// 安全：默认只监听本机；登录只认密码（没配置就自动生成，存在数据目录的 web.secret 里）；
// 会话 Cookie 是 HttpOnly + SameSite=Strict；改动类的请求必须带 X-Nano 头；
// 监听本机时还检查 Host 头，防 DNS 重绑定。
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
)

//go:embed all:dist
var dist embed.FS

const (
	cookieName = "nano_session"
	sessionTTL = 30 * 24 * time.Hour
)

type Server struct {
	app     *app.App
	conf    *config.Store
	persona *persona.Loader
	log     *slog.Logger
	newLLM  func(config.LLM) (llm.Client, error)

	mu        sync.Mutex
	password  string
	sessions  map[string]time.Time // 登录令牌 → 过期时间
	loopback  bool                 // 只监听本机：要检查 Host 头
	regCancel context.CancelFunc   // 进行中的飞书扫码注册
}

// New 创建网页服务。密码来自 web.password，没配置就用数据目录里的 web.secret（没有就生成一个）。
func New(a *app.App, conf *config.Store, p *persona.Loader, newLLM func(config.LLM) (llm.Client, error), log *slog.Logger) (*Server, error) {
	s := &Server{app: a, conf: conf, persona: p, log: log, newLLM: newLLM, sessions: map[string]time.Time{}}
	if err := s.loadPassword(conf.Current()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) loadPassword(cfg *config.Config) error {
	if cfg.Web.Password != "" {
		s.mu.Lock()
		s.password = cfg.Web.Password
		s.mu.Unlock()
		return nil
	}
	path := filepath.Join(cfg.Session.DataDir, "web.secret")
	b, err := os.ReadFile(path)
	pw := strings.TrimSpace(string(b))
	if err != nil || pw == "" {
		buf := make([]byte, 12)
		rand.Read(buf)
		pw = hex.EncodeToString(buf)
		if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.password = pw
	s.mu.Unlock()
	return nil
}

// ConfigChanged 在配置变化时调用：密码改了就换掉，并让所有人重新登录。
func (s *Server) ConfigChanged(old, cur *config.Config) {
	if old.Web.Password == cur.Web.Password {
		return
	}
	if err := s.loadPassword(cur); err != nil {
		s.log.Warn("更新网页登录密码失败", "err", err)
		return
	}
	s.mu.Lock()
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
}

// LoginURL 是带着密码的登录链接（密码放在 # 后面，不会发给服务器，也不会进日志）。
func (s *Server) LoginURL(addr string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	host := addr
	if h, p, err := net.SplitHostPort(addr); err == nil && (h == "" || h == "0.0.0.0" || h == "::") {
		host = net.JoinHostPort("127.0.0.1", p)
	}
	return "http://" + host + "/#k=" + s.password
}

// Serve 监听 addr，直到 ctx 结束。
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		s.loopback = true
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})

	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.auth(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]bool{"ok": true}) }))
	mux.HandleFunc("GET /api/overview", s.auth(s.overview))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("POST /api/sessions", s.auth(s.newSession))
	mux.HandleFunc("GET /api/sessions/{sid}", s.auth(s.getSession))
	mux.HandleFunc("DELETE /api/sessions/{sid}", s.auth(s.deleteSession))
	mux.HandleFunc("PATCH /api/sessions/{sid}", s.auth(s.patchSession))
	mux.HandleFunc("POST /api/sessions/{sid}/reset", s.auth(s.resetSession))
	mux.HandleFunc("POST /api/sessions/{sid}/stop", s.auth(s.stopSession))
	mux.HandleFunc("POST /api/chat", s.auth(s.chat))
	mux.HandleFunc("PUT /api/endpoints/{key}", s.auth(s.setEndpoint))
	mux.HandleFunc("POST /api/approvals/{n}", s.auth(s.approve))
	mux.HandleFunc("GET /api/fs", s.auth(s.listDir))
	mux.HandleFunc("POST /api/feishu/register", s.auth(s.feishuRegister))
	mux.HandleFunc("DELETE /api/feishu/register", s.auth(s.feishuRegisterCancel))
	mux.HandleFunc("GET /api/config", s.auth(s.getConfig))
	mux.HandleFunc("PATCH /api/config", s.auth(s.patchConfig))
	mux.HandleFunc("POST /api/config/reload", s.auth(s.reloadConfig))
	mux.HandleFunc("POST /api/test/model", s.auth(s.testModel))
	mux.HandleFunc("GET /api/personas", s.auth(s.listPersonas))
	mux.HandleFunc("GET /api/personas/{name}", s.auth(s.getPersona))
	mux.HandleFunc("PUT /api/personas/{name}", s.auth(s.putPersona))
	mux.HandleFunc("DELETE /api/personas/{name}", s.auth(s.deletePersona))
	return s.guard(mux)
}

// guard：监听本机时只接受本机的 Host（防 DNS 重绑定）；改动类请求必须带 X-Nano 头（防跨站请求）。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.loopback {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Nano") != "1" {
			http.Error(w, "missing X-Nano header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err == nil {
			s.mu.Lock()
			exp, ok := s.sessions[c.Value]
			s.mu.Unlock()
			if ok && time.Now().Before(exp) {
				h(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "请先登录")
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	pw := s.password
	s.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Password)), []byte(pw)) != 1 {
		time.Sleep(time.Second) // 拖慢暴力尝试
		writeErr(w, http.StatusUnauthorized, "密码不对")
		return
	}
	buf := make([]byte, 32)
	rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	now := time.Now()
	for t, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = now.Add(sessionTTL)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()), Secure: r.TLS != nil})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}
