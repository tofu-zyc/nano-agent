package web

// 网关内部群聊（房间）的网页接口：
//   GET/POST     /api/rooms
//   PATCH/DELETE /api/rooms/{id}

import (
	"net/http"

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
