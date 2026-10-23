package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return false
	}
	return true
}

// ---- 总览和事件 ----

// restart 重启网关。先检查配置；配置加载不了时返回 400，网关保持运行。
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SID string `json:"sid"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	err := s.app.RequestRestart(app.RestartRequest{By: "web:local", SID: body.SID, Channel: "web", Chat: "local"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.UpdateStatus())
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	info, err := s.app.CheckUpdate(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查新版本失败："+err.Error())
		return
	}
	writeJSON(w, info)
}

// updateRun 下载、校验、换上新版本，然后网关重启（页面像重启一样等它回来）。
func (s *Server) updateRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SID string `json:"sid"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	text, err := s.app.RunUpdate(ctx, app.RestartRequest{By: "web:local", SID: body.SID, Channel: "web", Chat: "local"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "text": text, "update": s.app.UpdateStatus()})
}

type modelInfo struct {
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	BaseURL       string   `json:"base_url"`
	Thinking      string   `json:"thinking"`
	Levels        []string `json:"levels"`
	ContextWindow int      `json:"context_window"`
	MaxHistory    int      `json:"max_history"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	cfg := s.conf.Current()
	snap := s.app.Snapshot()
	writeJSON(w, map[string]any{
		"gateway": map[string]any{"started": snap.Started, "kernel_listen": cfg.Kernel.Listen, "web_listen": cfg.Web.Listen,
			"config_dir": cfg.Dir, "data_dir": cfg.Session.DataDir},
		"runtime":    s.app.Status(),
		"kernels":    snap.Kernels,
		"model":      defaultModelInfo(s, cfg),
		"channels":   s.app.Channels(),
		"sessions":   snap.Sessions,
		"approvals":  snap.Approvals,
		"totals":     snap.Totals,
		"machines":   snap.Machines,
		"workspaces": snap.Workspaces,
		"models":     s.modelsView(),
		"workspace":  map[string]any{"mode": cfg.Workspace.Mode},
		"skills":     map[string]any{"dir": cfg.Skills.Dir, "list": s.app.Skills()},
		"commands":   app.Commands,
		"access":     map[string]any{"admins": cfg.Access.Admins, "guest": cfg.Access.Guest},
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.app.Bus().Subscribe()
	defer cancel()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			// 一次把积压的事件都写出去再 flush（流式回复的增量很密）
			for n := len(ch); n > 0; n-- {
				b, _ = json.Marshal(<-ch)
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ---- 会话 ----

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	info, ok := s.app.Session(sid)
	if !ok {
		writeErr(w, http.StatusNotFound, "没有这个会话")
		return
	}
	hist, summary := s.app.History(sid)
	if len(hist) > 300 {
		hist = hist[len(hist)-300:]
	}
	if hist == nil {
		hist = []session.Msg{}
	}
	writeJSON(w, map[string]any{"info": info, "history": hist, "summary": summary})
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	info, err := s.app.NewSession(req.Title)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteSession(r.PathValue("sid")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	var p app.SessionPatch
	if !readJSON(w, r, &p) {
		return
	}
	sid := r.PathValue("sid")
	if err := s.app.UpdateSession(sid, p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info, _ := s.app.Session(sid)
	writeJSON(w, info)
}

func (s *Server) resetSession(w http.ResponseWriter, r *http.Request) {
	if err := s.app.ResetSession(r.PathValue("sid")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	if err := s.app.StopSession(r.PathValue("sid")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// undoSession 撤回会话里最近的一条输入，返回撤回的那段话（网页放回输入框）。
func (s *Server) undoSession(w http.ResponseWriter, r *http.Request) {
	text, err := s.app.UndoSession(r.PathValue("sid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"text": text})
}

// chat 以网页管理员的身份往会话里发一条消息（可以带已经上传好的图片）。
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SID    string `json:"sid"`
		Text   string `json:"text"`
		Images []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"images"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.app.HasSession(req.SID) {
		writeErr(w, http.StatusNotFound, "没有这个会话")
		return
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Images) == 0 {
		writeErr(w, http.StatusBadRequest, "消息是空的")
		return
	}
	if len(req.Images) > 0 && s.media == nil {
		writeErr(w, http.StatusServiceUnavailable, "图片存储没有启用")
		return
	}
	if len(req.Images) > media.MaxPerMessage {
		writeErr(w, http.StatusBadRequest, "一条消息最多 "+strconv.Itoa(media.MaxPerMessage)+" 张图片")
		return
	}
	var images []media.Attachment
	for _, it := range req.Images {
		att, err := s.media.Info(it.ID) // 类型和大小以服务器上的文件为准
		if err != nil {
			writeErr(w, http.StatusBadRequest, "图片已经不在服务器上了，请重新上传")
			return
		}
		att.Name, att.Source = it.Name, "web"
		images = append(images, att)
	}
	if err := s.app.Web().Post(req.SID, strings.TrimSpace(req.Text), images); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, map[string]string{"sid": req.SID})
}

// setEndpoint 把一个聊天窗口改接到别的会话。
func (s *Server) setEndpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.app.SetEndpoint(r.PathValue("key"), req.Session); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// makeAdmin 把在聊天窗口里说话的人设为管理员（写进 access.admins），并把窗口接到主会话。
func (s *Server) makeAdmin(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	who, err := s.app.EndpointWho(key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if admins := s.conf.Current().Access.Admins; !slices.Contains(admins, who) {
		list := []any{}
		for _, a := range admins {
			list = append(list, a)
		}
		if err := s.conf.Set(map[string]any{"access.admins": append(list, who)}); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.app.Bus().Publish(events.Event{T: "config"})
	}
	if err := s.app.WelcomeAdmin(key); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"who": who})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	var req struct {
		OK bool `json:"ok"`
	}
	if err != nil || !readJSON(w, r, &req) {
		if err != nil {
			writeErr(w, http.StatusBadRequest, "编号不对")
		}
		return
	}
	if err := s.app.Approve(n, req.OK); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- 目录（内核所在机器上的，选工作目录用） ----

func (s *Server) listDir(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	l, err := s.app.ListDir(ctx, r.URL.Query().Get("host"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, l)
}

// ---- 配置 ----

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"fields": s.conf.Describe()})
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Values map[string]any `json:"values"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.conf.Set(req.Values); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.Bus().Publish(events.Event{T: "config"})
	writeJSON(w, map[string]any{"fields": s.conf.Describe()})
}

func (s *Server) reloadConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.conf.Reload(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.Bus().Publish(events.Event{T: "config"})
	writeJSON(w, map[string]any{"fields": s.conf.Describe()})
}

// testModel 用一个模型发一个很小的请求，看接口通不通、多快。body 里的 id 为空表示默认模型。
func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) // 可以没有正文
	cfg := s.conf.Current()
	client, m, err := s.app.ClientFor(req.ID)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	thinking := m.Thinking
	if thinking == "" {
		thinking = cfg.LLM.Thinking
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	var first time.Duration
	res, err := client.Chat(ctx, llm.Request{
		Messages: []llm.Message{{Role: "user", Content: "用一句话介绍你自己。"}},
		Thinking: thinking,
	}, func(llm.Delta) {
		if first == 0 {
			first = time.Since(start)
		}
	})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "ms": time.Since(start).Milliseconds()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "reply": res.Content, "usage": res.Usage,
		"ms": time.Since(start).Milliseconds(), "first_ms": first.Milliseconds(),
		"model": m.ID, "label": modelLabel(m)})
}

// ---- 人格 ----

var personaName = regexp.MustCompile(`^[A-Za-z0-9._:%@-]+\.md$`)

func (s *Server) personaPath(name string) (string, error) {
	if !personaName.MatchString(name) || strings.Contains(name, "..") {
		return "", errors.New("文件名不合法")
	}
	return filepath.Join(s.persona.Dir(), name), nil
}

func (s *Server) listPersonas(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name     string   `json:"name"`
		SID      string   `json:"sid,omitempty"` // 会话专属人格对应的会话
		Size     int64    `json:"size"`
		Mod      int64    `json:"mod"`
		Sessions []string `json:"sessions"` // 正在用它的会话
	}
	entries, _ := os.ReadDir(s.persona.Dir())
	items := []item{}
	idx := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		it := item{Name: e.Name(), Size: info.Size(), Mod: info.ModTime().UnixMilli(), Sessions: []string{}}
		if e.Name() != "default.md" {
			if sid, err := url.PathUnescape(strings.TrimSuffix(e.Name(), ".md")); err == nil {
				it.SID = sid
			}
		}
		idx[it.Name] = len(items)
		items = append(items, it)
	}
	for _, si := range s.app.Snapshot().Sessions {
		if i, ok := idx[si.Persona]; ok {
			items[i].Sessions = append(items[i].Sessions, si.SID)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name == "default.md" && items[j].Name != "default.md" })
	writeJSON(w, map[string]any{"dir": s.persona.Dir(), "items": items})
}

// listPersonaPresets：程序自带的人格预设，网页上“套用预设”用。
func (s *Server) listPersonaPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"items": persona.Presets()})
}

func (s *Server) getPersona(w http.ResponseWriter, r *http.Request) {
	path, err := s.personaPath(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "没有这个人格文件")
		return
	}
	writeJSON(w, map[string]string{"name": r.PathValue("name"), "text": string(b)})
}

func (s *Server) putPersona(w http.ResponseWriter, r *http.Request) {
	path, err := s.personaPath(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(req.Text), 0o644); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.app.Bus().Publish(events.Event{T: "persona", Name: r.PathValue("name")})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deletePersona(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.personaPath(name)
	if err != nil || name == "default.md" {
		writeErr(w, http.StatusBadRequest, "默认人格不能删除")
		return
	}
	if err := os.Remove(path); err != nil {
		writeErr(w, http.StatusNotFound, "没有这个人格文件")
		return
	}
	s.app.Bus().Publish(events.Event{T: "persona", Name: name})
	writeJSON(w, map[string]bool{"ok": true})
}

// defaultModelInfo 是顶栏/拓扑上显示的默认模型；没有模型时字段为空。
func defaultModelInfo(s *Server, cfg *config.Config) modelInfo {
	st := s.app.ModelStore()
	if st == nil {
		return modelInfo{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model}
	}
	id := st.Default()
	if id == "" {
		return modelInfo{}
	}
	m, e, ok := st.Find(id)
	if !ok {
		return modelInfo{}
	}
	return modelInfo{Provider: e.Kind, Model: m.Name, BaseURL: e.BaseURL, Thinking: m.Thinking,
		Levels: m.ThinkingLevels, ContextWindow: m.ContextWindow, MaxHistory: m.MaxHistory}
}

func (s *Server) archives(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.Archives(r.PathValue("sid")))
}
func (s *Server) historyDelete(w http.ResponseWriter, r *http.Request) {
	var p app.HistoryDelete
	if !readJSON(w, r, &p) {
		return
	}
	if err := s.app.DeleteHistory(r.PathValue("sid"), p); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) fullHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.FullHistory(r.PathValue("sid")))
}
