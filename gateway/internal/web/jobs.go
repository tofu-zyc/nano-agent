package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"nano-agent/gateway/internal/app"
)

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "任务分页数量需为整数")
			return
		}
		limit = n
	}
	out, err := s.app.JobAction(r.Context(), "web:local", "", app.JobRequest{Action: "list",
		Node: r.URL.Query().Get("node"), After: r.URL.Query().Get("after"), Limit: limit})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, out)
}
func (s *Server) jobControl(w http.ResponseWriter, r *http.Request) {
	var request app.JobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, http.StatusBadRequest, "任务参数不正确："+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "任务参数只能包含一个对象")
		return
	}
	if request.Action == "list" {
		writeErr(w, http.StatusBadRequest, "请从任务列表入口查询")
		return
	}
	request.JobID = r.PathValue("job")
	out, err := s.app.JobAction(r.Context(), "web:local", "", request)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, out)
}
