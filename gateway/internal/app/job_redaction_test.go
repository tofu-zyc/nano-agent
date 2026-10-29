package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

func TestJobEncodedLogMaskingAffectsModelViewAndPreservesHistory(t *testing.T) {
	for _, mode := range []string{"always", "off"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			secret := "private-job-credential-123456"
			data, _ := json.Marshal(map[string]any{"job": map[string]string{"job_id": "j_boot-1"}, "output": map[string]any{
				"chunks": []map[string]string{{"stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte(secret[:10]))},
					{"stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte(secret[10:]))}}}})
			e.a.mu.Lock()
			e.a.cfg.Security.Redact = mode
			e.a.cfg.LLM.APIKey = secret
			for _, msg := range []session.Msg{
				{Role: "user", Content: "Read the managed log"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "logs", Type: "function", Function: llm.FunctionCall{Name: "Job", Arguments: `{"action":"output","job_id":"j_boot-1"}`}}}},
				{Role: "tool", ToolCallID: "logs", Content: string(data)},
			} {
				if err := e.a.store.Append(e.sid, msg); err != nil {
					e.a.mu.Unlock()
					t.Fatal(err)
				}
			}
			e.a.mu.Unlock()
			e.llm.say("done")
			e.say("Inspect the log")
			e.k.expect(proto.TInput)
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
			e.k.expect(proto.TTurn)
			body := ""
			for _, msg := range e.llm.lastReq().Messages {
				if msg.Role == "tool" && msg.ToolCallID == "logs" {
					body = msg.Content
				}
			}
			if body == "" {
				t.Fatal("model request omitted Job history")
			}
			var result JobResponse
			if json.Unmarshal([]byte(body), &result) != nil || result.Output == nil {
				t.Fatal(body)
			}
			var decoded strings.Builder
			for _, chunk := range result.Output.Chunks {
				data, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil {
					t.Fatal(err)
				}
				decoded.Write(data)
			}
			if strings.Contains(decoded.String(), secret) != (mode == "off") {
				t.Fatal("model log masking ignored mode", mode, decoded.String())
			}
			preserved := false
			for _, msg := range e.a.store.History(e.sid) {
				if msg.Role == "tool" && msg.ToolCallID == "logs" {
					preserved = msg.Content == string(data)
				}
			}
			if !preserved {
				t.Fatal("redaction rewrote raw audit history")
			}
		})
	}
}
