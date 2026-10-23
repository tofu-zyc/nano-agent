package web

import (
	"nano-agent/gateway/internal/app"
	"net/http"
)

func (s *Server) getSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.Schedules())
}
func (s *Server) scheduleAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		app.ScheduleRequest
		SID string `json:"sid"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.SID == "" {
		body.SID = "main"
	}
	result, err := s.app.ScheduleAction(body.SID, "web:admin", body.ScheduleRequest)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": result})
}
