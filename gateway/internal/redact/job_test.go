package redact

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestJobOutputEncodedChunksApplyToolRedaction(t *testing.T) {
	secret := "private-test-credential-123456"
	raw := "before " + secret + " after"
	cut := 16
	input := map[string]any{"job": map[string]string{"job_id": "j_boot-1"}, "output": map[string]any{
		"chunks": []map[string]any{
			{"cursor": 0, "next_cursor": cut, "stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte(raw[:cut]))},
			{"cursor": cut, "next_cursor": len(raw), "stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte(raw[cut:]))},
		}, "next_cursor": len(raw), "output_cursor": len(raw), "gap": false,
	}}
	body, _ := json.Marshal(input)
	masked := ToolResult(string(body), []string{secret})
	var output struct {
		Output struct {
			Chunks []struct {
				Cursor     int `json:"cursor"`
				NextCursor int `json:"next_cursor"`
				Data       string
			}
			Redacted bool
		}
	}
	if err := json.Unmarshal([]byte(masked), &output); err != nil {
		t.Fatal(err)
	}
	var decoded strings.Builder
	for _, chunk := range output.Output.Chunks {
		bytes, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes) != chunk.NextCursor-chunk.Cursor {
			t.Fatal("redaction changed source cursor span")
		}
		decoded.Write(bytes)
	}
	if strings.Contains(decoded.String(), secret) || decoded.String() != strings.ReplaceAll(raw, secret, Mask(secret)) {
		t.Fatal("encoded credential escaped tool redaction", decoded.String())
	}
	if !output.Output.Redacted {
		t.Fatal("lossy model view did not identify redaction")
	}
}

func TestJobOutputNonSensitiveViewKeepsOriginalBytesAndInvalidLogsDoNotPanic(t *testing.T) {
	body := `{ "job":{"job_id":"j_boot-1"}, "output":{"chunks":[{"data":"aGVsbG8="}],"next_cursor":5} }`
	if got := ToolResult(body, nil); got != body {
		t.Fatal("non-sensitive view was rewritten", got)
	}
	for _, body := range []string{"", "plain result", `{"output":{"chunks":[]}}`,
		`{"job":{"job_id":"j_boot-1"},"output":{"chunks":[{"data":"invalid!"}]}}`} {
		_ = ToolResult(body, []string{"sensitive"})
	}
}

func TestJobPrivateKeyRedactionKeepsOriginalCursorWidths(t *testing.T) {
	for _, key := range []string{
		"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("private-key-material", 12) + "\n-----END PRIVATE KEY-----",
	} {
		body, _ := json.Marshal(map[string]any{"job": map[string]string{"job_id": "j_boot-1"}, "output": map[string]any{
			"chunks": []map[string]any{{"cursor": 0, "next_cursor": len(key), "data": base64.StdEncoding.EncodeToString([]byte(key))}}}})
		masked := ToolResult(string(body), nil)
		var output struct {
			Output struct {
				Chunks   []struct{ Data string }
				Redacted bool
			}
		}
		if json.Unmarshal([]byte(masked), &output) != nil || !output.Output.Redacted {
			t.Fatal(masked)
		}
		raw, err := base64.StdEncoding.DecodeString(output.Output.Chunks[0].Data)
		if err != nil || len(raw) != len(key) || strings.Contains(string(raw), "private-key-material") || string(raw) == key {
			t.Fatal("private key escaped or cursor width changed", err, string(raw))
		}
	}
}
