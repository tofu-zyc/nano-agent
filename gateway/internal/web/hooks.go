package web

import (
	"encoding/json"
	"io"
	"nano-agent/gateway/internal/app"
	"net/http"
)

func (s *Server) getHooks(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		sid = "main"
	}
	action := r.URL.Query().Get("action")
	if action == "" {
		action = "list"
	}
	if action != "list" && action != "runs" && action != "get" {
		writeErr(w, 400, "查询入口只允许 list/get/runs")
		return
	}
	out, err := s.app.HookAction(r.Context(), sid, "web:local", app.HookRequest{Action: action, Agent: r.URL.Query().Get("agent"), ID: r.URL.Query().Get("id"), After: r.URL.Query().Get("after")})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, out)
}
func (s *Server) hookAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		app.HookRequest
		SID string `json:"sid"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "只接受一个对象")
		return
	}
	if body.SID == "" {
		body.SID = "main"
	}
	out, err := s.app.HookAction(r.Context(), body.SID, "web:local", body.HookRequest)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, out)
}
