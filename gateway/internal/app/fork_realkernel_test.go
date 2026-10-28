package app

import (
	"context"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/session"
	"testing"
)

type parallelForkModel struct{}

func (parallelForkModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	if req.Messages[len(req.Messages)-1].Role == "tool" {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "parallel fork finished"}, nil
	}
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{
		{ID: "fork", Type: "function", Function: llm.FunctionCall{Name: "Session", Arguments: `{"action":"fork","request_id":"real-boundary","title":"real C branch"}`}},
		{ID: "sibling", Type: "function", Function: llm.FunctionCall{Name: "Skill", Arguments: `{"name":"missing-fixture"}`}},
	}}, nil
}

func TestRealKernelForkWaitsForCompleteParallelBatch(t *testing.T) {
	e, _ := setupRealKernel(t)
	e.a.mu.Lock()
	e.a.llm = parallelForkModel{}
	e.a.mu.Unlock()
	e.say("Fork with a parallel sibling")
	e.ch.wait(t, "parallel fork finished")
	e.a.forkWG.Wait()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	children := e.a.store.Children(mainSID)
	if len(children) != 1 || children[0].Status != session.BranchReady {
		t.Fatalf("real fork not ready: %+v", children)
	}
	h := e.a.store.History(children[0].SID)
	calls, results := map[string]int{}, map[string]int{}
	for _, m := range h {
		for _, c := range m.ToolCalls {
			calls[c.ID]++
		}
		if m.Role == "tool" {
			results[m.ToolCallID]++
		}
	}
	if len(calls) != 2 || len(results) != 2 {
		t.Fatalf("snapshot omitted parallel boundary: calls=%v results=%v", calls, results)
	}
	for id, n := range calls {
		if n != 1 || results[id] != 1 {
			t.Fatalf("unpaired/repeated tool at snapshot: %s %d/%d", id, n, results[id])
		}
	}
}
