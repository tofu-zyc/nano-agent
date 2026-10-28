package app

import (
	"fmt"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"strings"
)

func (a *App) emitToolState(s *sess, m *proto.Msg, state string) {
	name := m.Name
	if name == "" {
		name = s.run.callArgs[m.ID].Name
	}
	for _, out := range s.run.outs {
		channel.EmitToolEvent(out.view, channel.ToolEvent{RunID: s.run.runID, TurnID: s.run.turnID, CallID: m.ID, Name: name, State: state, Detail: m.Text})
	}
}

func validateProposedCalls(calls []llm.ToolCall, limit int) string {
	if len(calls) > limit {
		return fmt.Sprintf("本次提出 %d 个工具调用，超过上限 %d；整批未接受、未执行，请分轮处理", len(calls), limit)
	}
	seen := map[string]bool{}
	for i, call := range calls {
		if strings.TrimSpace(call.ID) == "" || len(call.ID) > 256 {
			return fmt.Sprintf("第 %d 个工具调用的 ID 为空或超过 256 字节；整批未接受、未执行", i+1)
		}
		if seen[call.ID] {
			return "工具提案包含重复调用 ID；整批未接受、未执行"
		}
		seen[call.ID] = true
	}
	return ""
}
