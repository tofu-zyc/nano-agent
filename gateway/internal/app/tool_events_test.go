package app

import (
	"context"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"strings"
	"testing"
)

type proposedModel struct{ calls []llm.ToolCall }

func (p proposedModel) Chat(context.Context, llm.Request, func(llm.Delta)) (*llm.Result, error) {
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: p.calls, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 3}}, nil
}

func TestInvalidToolProposalNeverDispatchesOrLeavesOrphanCalls(t *testing.T) {
	for _, kind := range []string{"too-many", "duplicate", "missing"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t)
			calls := []llm.ToolCall{{ID: "one", Function: llm.FunctionCall{Name: "Read", Arguments: `{"path":"private"}`}}}
			switch kind {
			case "too-many":
				for range 32 {
					calls = append(calls, calls[0])
				}
			case "duplicate":
				calls = append(calls, calls[0])
			case "missing":
				calls[0].ID = ""
			}
			e.a.SetLLM(proposedModel{calls: calls})
			e.say("work")
			input := e.k.expect(proto.TInput)
			e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, RunID: input.RunID, TurnID: 1})
			turn := e.k.expect(proto.TTurn)
			if turn.Reason != proto.ReasonError || len(turn.Calls) != 0 || !strings.Contains(turn.Detail, "整批未接受") {
				t.Fatalf("partially dispatched invalid batch: %+v", turn)
			}
			e.a.mu.Lock()
			defer e.a.mu.Unlock()
			h := e.a.store.History(e.sid)
			if len(h) != 2 || len(h[1].ToolCalls) != 0 || len(h[1].ProposedCalls) != len(calls) {
				t.Fatal("unaccepted calls entered accepted history", h)
			}
			for _, m := range session.BuildMessages("", "", h, false, nil) {
				if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
					t.Fatal("unaccepted proposal fabricated model tool results")
				}
			}
			if e.a.sessions[e.sid].run.usage.PromptTokens != 100 {
				t.Fatal("invalid proposal lost real model cost")
			}
		})
	}
}
