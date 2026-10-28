package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/llm"
)

type nativeLeaseModel struct{ path string }

func (m nativeLeaseModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	tools := 0
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			tools++
		}
	}
	if tools >= 2 {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "native leases completed"}, nil
	}
	args, _ := json.Marshal(map[string]string{"path": m.path})
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{
		{ID: "reused-native-id", Type: "function", Function: llm.FunctionCall{Name: "Read", Arguments: string(args)}},
	}}, nil
}

// Actual C admission and executor, over the real Go handshake and durable lease
// worker. Reusing a model call ID in a later turn must not alias an earlier lease.
func TestRealKernelNativeExecutionLeases(t *testing.T) {
	e, _ := setupRealKernel(t)
	file := filepath.Join(e.dir, "home", "leased.txt")
	if err := os.WriteFile(file, []byte("native lease evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.llm = nativeLeaseModel{path: file}
	e.a.mu.Unlock()
	e.say("read the fixture twice")
	e.ch.wait(t, "native leases completed")
	deadline := time.Now().Add(5 * time.Second)
	for {
		cp, problem := e.a.dispatcher.Snapshot()
		if problem != "" {
			t.Fatal(problem)
		}
		if len(cp.Leases) == 0 && len(cp.Closed) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native lease ownership not closed: %+v; log=%s", cp, readFileTail(filepath.Join(e.dir, "kernel.log")))
		}
		time.Sleep(time.Millisecond)
	}
	results := 0
	for _, msg := range e.a.store.History(e.sid) {
		if msg.Role == "tool" {
			if msg.ToolCallID != "reused-native-id" || msg.RunID == "" || msg.TurnID < 1 || !strings.Contains(msg.Content, "native lease evidence") {
				t.Fatalf("native result did not complete correctly: %+v", msg)
			}
			results++
		}
	}
	if results != 2 {
		t.Fatalf("native calls/results=%d, want two", results)
	}
}
