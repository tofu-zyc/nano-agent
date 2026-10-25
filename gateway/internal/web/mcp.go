package web

import (
	"net/http"

	"nano-agent/gateway/internal/mcp"
)

// MCP 服务（阶段 11）：列表跟着 /api/overview 一起给（网页会自动刷新），
// 保存和测试在这个文件里。

// mcpSaveBody 是网页发上来的一份列表。增、删、改都是一个动作：整份覆盖。
type mcpSaveBody struct {
	Items []mcpSaveItem `json:"items"`
}

type mcpSaveItem struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"` // stdio / http，只是给界面看的
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	URL      string            `json:"url"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env"`
	Enabled  bool              `json:"enabled"`
	Tools    []string          `json:"tools"`
	Agents   []string          `json:"agents"`
	Approval string            `json:"approval"`
}

func (s *Server) mcpList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"items": s.app.MCPInfo()})
}

func (s *Server) mcpSave(w http.ResponseWriter, r *http.Request) {
	var body mcpSaveBody
	if !readJSON(w, r, &body) {
		return
	}
	list := make([]mcp.Server, 0, len(body.Items))
	for _, it := range body.Items {
		srv := mcp.Server{
			Name: it.Name, Command: it.Command, Args: it.Args, Cwd: it.Cwd, Env: it.Env,
			Enabled: it.Enabled, Tools: it.Tools, Agents: it.Agents, Approval: it.Approval,
		}
		if it.Kind == "http" || it.URL != "" {
			srv.URL = it.URL
		}
		list = append(list, srv)
	}
	if err := s.app.SaveMCP(list); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": s.app.MCPInfo()})
}

// mcpTest 现连一个服务，把它的工具列出来。
func (s *Server) mcpTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	// 连不上是测试的正常结果（命令写错、服务没起），不是服务器出错：回 200 + error
	tools, err := s.app.TestMCP(body.Name)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"tools": tools})
}
