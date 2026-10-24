package web

// agent 和机器人的网页接口。
//
// 每个 agent、每个机器人都是拓扑图上的一个节点，点开就能改它的全部设置：
//   GET/POST    /api/agents
//   PATCH/DELETE /api/agents/{id}
//   GET/PUT     /api/agents/{id}/persona
//   GET/POST    /api/bots
//   PATCH/DELETE /api/bots/{id}

import (
	"encoding/json"
	"net/http"
	"strings"

	"nano-agent/gateway/internal/app"
)

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"items": s.app.AgentList()})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var p app.AgentPatch
	if !readJSON(w, r, &p) {
		return
	}
	info, err := s.app.CreateAgent(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) patchAgent(w http.ResponseWriter, r *http.Request) {
	var p app.AgentPatch
	if !readJSON(w, r, &p) {
		return
	}
	info, err := s.app.UpdateAgent(r.PathValue("id"), p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteAgent(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) getAgentPersona(w http.ResponseWriter, r *http.Request) {
	text, err := s.app.AgentPersona(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"text": text})
}

func (s *Server) putAgentPersona(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return
	}
	if err := s.app.SaveAgentPersona(r.PathValue("id"), body.Text); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) listBots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"items": s.app.Bots()})
}

func (s *Server) createBot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		app.BotPatch
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind != "telegram" && kind != "feishu" {
		writeErr(w, http.StatusBadRequest, "种类只能是 telegram 或 feishu")
		return
	}
	view, err := s.app.CreateBot(kind, body.BotPatch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, view)
}

func (s *Server) patchBot(w http.ResponseWriter, r *http.Request) {
	var p app.BotPatch
	if !readJSON(w, r, &p) {
		return
	}
	view, err := s.app.UpdateBot(r.PathValue("id"), p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, view)
}

func (s *Server) deleteBot(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteBot(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 记忆（MemoryPanel）----

func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	v, err := s.app.Memory(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, v)
}

func (s *Server) searchMemory(w http.ResponseWriter, r *http.Request) {
	items, err := s.app.MemorySearch(r.PathValue("id"), r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) getMemoryItem(w http.ResponseWriter, r *http.Request) {
	text, err := s.app.MemoryRaw(r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"raw": text})
}

func (s *Server) putMemoryItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return
	}
	item, err := s.app.MemorySave(r.PathValue("id"), r.URL.Query().Get("path"), body.Raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, item)
}

func (s *Server) invalidateMemoryItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason  string `json:"reason"`
		Restore bool   `json:"restore"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	if err := s.app.MemoryInvalidate(r.PathValue("id"), r.URL.Query().Get("path"), body.Reason, body.Restore); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) deleteMemoryItem(w http.ResponseWriter, r *http.Request) {
	if err := s.app.MemoryDelete(r.PathValue("id"), r.URL.Query().Get("path")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) putMemorySelf(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return
	}
	if err := s.app.SaveMemorySelf(r.PathValue("id"), body.Text); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) startDream(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Dream(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) getDreams(w http.ResponseWriter, r *http.Request) {
	days, text, err := s.app.DreamLog(r.PathValue("id"), r.URL.Query().Get("day"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"days": days, "text": text})
}

func (s *Server) wakeAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Wake(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
