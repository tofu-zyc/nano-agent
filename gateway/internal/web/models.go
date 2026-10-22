package web

import (
	"context"
	"net/http"
	"time"

	"nano-agent/gateway/internal/models"
)

// 可以用的模型：接口（OpenAI 兼容）和模型都只来自 models.json，每个接口上可以有多个模型，
// 一个默认模型，每个会话也能单独选（/model、会话设置）。接口不返回 API key，只告诉有没有设置。

type endpointView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // openai | mock
	BaseURL string `json:"base_url"`
	Proxy   string `json:"proxy,omitempty"`
	HasKey  bool   `json:"has_key"` // 有没有设置 API key（值不返回）
}

type modelView struct {
	ID             string   `json:"id"`
	Endpoint       string   `json:"endpoint"`
	Name           string   `json:"name"`
	Label          string   `json:"label"`
	ContextWindow  int      `json:"context_window"`
	MaxTokens      int      `json:"max_tokens"`
	MaxHistory     int      `json:"max_history_tokens"`
	Thinking       string   `json:"thinking"`
	ThinkingLevels []string `json:"thinking_levels"`
	Modalities     []string `json:"modalities"`
	Owner          string   `json:"owned_by,omitempty"`
	Default        bool     `json:"default"`
}

type modelsView struct {
	Endpoints []endpointView `json:"endpoints"`
	Models    []modelView    `json:"models"`
	Default   string         `json:"default"`
}

func (s *Server) modelsView() modelsView {
	v := modelsView{Endpoints: []endpointView{}, Models: []modelView{}}
	st := s.app.ModelStore()
	if st == nil {
		return v
	}
	v.Default = st.Default()
	for _, e := range st.Endpoints() {
		v.Endpoints = append(v.Endpoints, endpointView{ID: e.ID, Name: e.Name, Kind: e.Kind, BaseURL: e.BaseURL,
			Proxy: e.Proxy, HasKey: e.APIKey != ""})
	}
	for _, m := range st.Models() {
		v.Models = append(v.Models, modelView{ID: m.ID, Endpoint: m.Endpoint, Name: m.Name, Label: modelLabel(m),
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, MaxHistory: m.MaxHistory,
			Thinking: m.Thinking, ThinkingLevels: m.ThinkingLevels, Modalities: m.Modalities, Owner: m.Owner,
			Default: m.ID == v.Default})
	}
	return v
}

// modelLabel 是模型在网页上显示的名字
func modelLabel(m models.Model) string {
	if m.Label != "" {
		return m.Label
	}
	return m.Name
}

// modelStore 取模型列表，没有启用时回一个错误。调用方自己处理返回值。
func (s *Server) modelStore(w http.ResponseWriter) *models.Store {
	st := s.app.ModelStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "模型列表没有启用")
	}
	return st
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.modelsView())
}

type endpointReq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Proxy   string `json:"proxy"`
}

// saveEndpoint 新建（POST）或修改（PUT，ID 在路径里）一个端点。API key 留空表示不改原来的。
func (s *Server) saveEndpoint(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	var req endpointReq
	if !readJSON(w, r, &req) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		req.ID = id
	}
	e, err := st.SaveEndpoint(models.Endpoint{ID: req.ID, Name: req.Name, Kind: req.Kind, BaseURL: req.BaseURL,
		APIKey: req.APIKey, Proxy: req.Proxy})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.ModelsChanged()
	writeJSON(w, map[string]string{"id": e.ID})
}

// deleteEndpoint 删掉一个端点，连同它上面的模型。接着这些模型的会话下一条消息用默认模型。
func (s *Server) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	if err := st.DeleteEndpoint(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.ModelsChanged()
	writeJSON(w, map[string]bool{"ok": true})
}

type modelReq struct {
	Endpoint       string   `json:"endpoint"`
	Name           string   `json:"name"`
	Label          string   `json:"label"`
	ContextWindow  int      `json:"context_window"`
	MaxTokens      int      `json:"max_tokens"`
	MaxHistory     int      `json:"max_history_tokens"`
	Thinking       string   `json:"thinking"`
	ThinkingLevels []string `json:"thinking_levels"`
	Modalities     []string `json:"modalities"` // 能接收什么：text、image
}

// saveModel 新建或修改一个模型（ID 是 端点/模型名，由网关算）。
func (s *Server) saveModel(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	var req modelReq
	if !readJSON(w, r, &req) {
		return
	}
	m, err := st.SaveModel(models.Model{Endpoint: req.Endpoint, Name: req.Name, Label: req.Label,
		ContextWindow: req.ContextWindow, MaxTokens: req.MaxTokens, MaxHistory: req.MaxHistory,
		Thinking: req.Thinking, ThinkingLevels: req.ThinkingLevels, Modalities: req.Modalities})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.ModelsChanged()
	writeJSON(w, map[string]string{"id": m.ID})
}

// deleteModel 删掉一个模型（ID 里有斜杠，用查询参数传）。
func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	if err := st.DeleteModel(r.URL.Query().Get("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.ModelsChanged()
	writeJSON(w, map[string]bool{"ok": true})
}

// defaultModel 设置默认模型：新会话、没单独选模型的会话都用它。
func (s *Server) defaultModel(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := st.SetDefault(req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.app.ModelsChanged()
	writeJSON(w, map[string]bool{"ok": true})
}

// fetchModels 从端点的 /models 拉取模型列表（网页上添加模型时选）。
func (s *Server) fetchModels(w http.ResponseWriter, r *http.Request) {
	st := s.modelStore(w)
	if st == nil {
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	list, err := st.Fetch(ctx, req.Endpoint)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"models": list})
}
