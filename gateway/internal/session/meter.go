package session

import (
	"encoding/json"
	"nano-agent/gateway/internal/llm"
)

// Meter 是上一次正常模型请求的真实用量；Position 是请求时的历史位置（绝对下标，不含之后的回复），Signature 是当时的模型、人格、工具和摘要
type Meter struct {
	Position  int       `json:"position"`
	Usage     llm.Usage `json:"usage"`
	Signature string    `json:"signature"`
}
type ContextInfo struct {
	Lazy        bool      `json:"lazy,omitempty"`
	AutoAt      int       `json:"auto_at"`
	IdleFor     string    `json:"idle_for"`
	Total       int       `json:"total"`
	Window      int       `json:"window"`
	Persona     int       `json:"persona"`
	Tools       int       `json:"tools"`
	Summary     int       `json:"summary"`
	Dialogue    int       `json:"dialogue"`
	ToolResults int       `json:"tool_results"`
	Messages    int       `json:"messages"`
	Last        llm.Usage `json:"last"`
	CompactedAt int64     `json:"compacted_at"`
	Compacted   int       `json:"compacted"`
	Model       string    `json:"model"`
}

func TextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/3 + 8
}
func Measure(st Settings, h []Msg, system string, tools []llm.Tool, signature string, window int) ContextInfo {
	b, _ := json.Marshal(tools)
	c := ContextInfo{Window: window, Persona: TextTokens(system), Tools: TextTokens(string(b)), Summary: TextTokens(st.Summary), Dialogue: Estimate(h), Messages: len(h), Last: st.Meter.Usage, CompactedAt: st.CompactedAt, Compacted: st.Base}
	if len(tools) == 0 {
		c.Tools = 0
	}
	for _, m := range h {
		if m.Role == "tool" {
			c.ToolResults += msgTokens(m)
		}
	}
	raw := c.Persona + c.Tools + c.Summary + c.Dialogue
	c.Total = raw
	pos := st.Meter.Position - st.Base
	if st.Meter.Usage.PromptTokens > 0 && st.Meter.Signature == signature && pos >= 0 && pos <= len(h) {
		c.Total = st.Meter.Usage.PromptTokens + Estimate(h[pos:])
	}
	if raw > 0 {
		scale := func(n int) int { return int(int64(n) * int64(c.Total) / int64(raw)) }
		c.Persona, c.Tools, c.Summary, c.ToolResults = scale(c.Persona), scale(c.Tools), scale(c.Summary), scale(c.ToolResults)
		c.Dialogue = c.Total - c.Persona - c.Tools - c.Summary
		c.ToolResults = min(c.ToolResults, c.Dialogue)
	}
	return c
}
