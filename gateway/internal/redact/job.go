package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// The model's Job output view is encoded. Mask the bounded combined page so a
// credential straddling two binary frames cannot bypass ordinary tool masking.
// The audit history and user-facing raw log API remain unchanged.
func jobChunks(text string, known []string) string {
	if len(text) > 256<<10 || !strings.Contains(text, `"chunks"`) || !strings.Contains(text, `"output"`) {
		return text
	}
	var message map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &message) != nil {
		return text
	}
	var job struct {
		ID string `json:"job_id"`
	}
	if json.Unmarshal(message["job"], &job) != nil || !strings.HasPrefix(job.ID, "j_") {
		return text
	}
	var output map[string]json.RawMessage
	if json.Unmarshal(message["output"], &output) != nil {
		return text
	}
	var chunks []map[string]json.RawMessage
	if json.Unmarshal(output["chunks"], &chunks) != nil || len(chunks) == 0 || len(chunks) > 64 {
		return text
	}
	var raw bytes.Buffer
	sizes := make([]int, len(chunks))
	for i, chunk := range chunks {
		var data string
		if json.Unmarshal(chunk["data"], &data) != nil || len(data) > 90000 {
			return text
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(data)
		if err != nil || raw.Len()+len(decoded) > 65536 {
			return text
		}
		sizes[i] = len(decoded)
		raw.Write(decoded)
	}
	masked := toolText(raw.String(), known)
	if masked == raw.String() {
		return text
	}
	// Keep source cursor spans valid. Length-changing private-key summaries are
	// padded; a summary larger than the original is conservatively hidden.
	if len(masked) > raw.Len() {
		masked = strings.Repeat("*", raw.Len())
	} else if len(masked) < raw.Len() {
		masked += strings.Repeat(" ", raw.Len()-len(masked))
	}
	at := 0
	for i, chunk := range chunks {
		chunk["data"], _ = json.Marshal(base64.StdEncoding.EncodeToString([]byte(masked[at : at+sizes[i]])))
		at += sizes[i]
	}
	output["chunks"], _ = json.Marshal(chunks)
	output["redacted"] = json.RawMessage("true")
	message["output"], _ = json.Marshal(output)
	result, err := json.Marshal(message)
	if err != nil {
		return text
	}
	return string(result)
}
