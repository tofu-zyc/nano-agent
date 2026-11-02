package web

// 网关内部群聊（房间）的网页接口：
//   GET/POST     /api/rooms
//   PATCH/DELETE /api/rooms/{id}
//   POST         /api/rooms/{id}/say、/cue
//   POST         /api/rooms/{id}/fork（剧场：从某一句分出平行世界）
//   GET          /api/rooms/{id}/export（字幕、剧本、原始记录）
//   GET          /api/rooms/{id}/timeline、/timeline/prefix/{hash}（剧场的场景记录）

import (
	"net/http"
	"net/url"
	"strconv"

	"nano-agent/gateway/internal/app"
)

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"items": s.app.Rooms()})
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var p app.RoomPatch
	if !readJSON(w, r, &p) {
		return
	}
	info, err := s.app.CreateRoom(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) patchRoom(w http.ResponseWriter, r *http.Request) {
	var p app.RoomPatch
	if !readJSON(w, r, &p) {
		return
	}
	info, err := s.app.UpdateRoom(r.PathValue("id"), p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteRoom(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// listObserve 是场景最近的 observe 记录：GET /api/observe?scene=<键>&n=50[&all=1]。
// all=1 时包含“本来也不会说话”的判断。
func (s *Server) listObserve(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	all := r.URL.Query().Get("all") == "1"
	writeJSON(w, map[string]any{"items": s.app.Observe(r.URL.Query().Get("scene"), n, all)})
}

// roomSay 往群里说一句：POST /api/rooms/{id}/say，{text, as, channel}。
func (s *Server) roomSay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string `json:"text"`
		As      string `json:"as"`
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.app.RoomSay(r.PathValue("id"), req.Text, req.As, req.Channel, "我"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// roomCue 导演让某个成员说一句：POST /api/rooms/{id}/cue，{agent, note, channel}。
func (s *Server) roomCue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent   string `json:"agent"`
		Note    string `json:"note"`
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.app.RoomCue(r.PathValue("id"), req.Agent, req.Note, req.Channel); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// roomTimeline 是剧场的场景记录：GET /api/rooms/{id}/timeline?after=<序号>&n=200。
func (s *Server) roomTimeline(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	items, last, err := s.app.StageTimeline(r.PathValue("id"), after, n)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": items, "last": last})
}

// roomTimelinePrefix 是某个前缀的全文：GET /api/rooms/{id}/timeline/prefix/{hash}。
func (s *Server) roomTimelinePrefix(w http.ResponseWriter, r *http.Request) {
	body, err := s.app.StagePrefix(r.PathValue("id"), r.PathValue("hash"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"hash": r.PathValue("hash"), "body": body})
}

// roomFork 从第 at 句分出平行世界：POST /api/rooms/{id}/fork，{at, name, rewind}。
func (s *Server) roomFork(w http.ResponseWriter, r *http.Request) {
	var req app.ForkRequest
	if !readJSON(w, r, &req) {
		return
	}
	info, err := s.app.ForkRoom(r.PathValue("id"), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

// roomExport 导出一场戏：GET /api/rooms/{id}/export?format=srt|md|json&inner=1&channels=1&from=&to=。
func (s *Server) roomExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var from, to int
	for name, dst := range map[string]*int{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeErr(w, http.StatusBadRequest, name+" 要是句子序号（数字）")
				return
			}
			*dst = n
		}
	}
	data, ctype, name, err := s.app.ExportStage(r.PathValue("id"), app.ExportOpts{
		Format: q.Get("format"), Inner: q.Get("inner") == "1", Channels: q.Get("channels") == "1", From: from, To: to})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	w.Write(data)
}

// listCommitments 是承诺列表：GET /api/commitments?agent=&phase=。只读；网页登录就是管理员。
func (s *Server) listCommitments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, map[string]any{"items": s.app.Commitments(q.Get("agent"), q.Get("phase"))})
}
