package session

import "nano-agent/gateway/internal/llm"

// Estimate 粗略估算 token 数：按字节数 / 3（中文 UTF-8 三字节约合一个 token 左右，
// 英文偏保守），每条消息再加一点固定开销。只用来决定裁剪，不需要精确。
func Estimate(h []Msg) int {
	n := 0
	for _, m := range h {
		n += msgTokens(m)
	}
	return n
}

func msgTokens(m Msg) int {
	b := len(m.Content) + len(m.Role)
	for _, tc := range m.ToolCalls {
		b += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return b/3 + 8
}

// Trim 在不超过 budget 的前提下保留尽量多的最近历史。只在用户消息处切，
// 这样不会把一次工具调用和它的结果拆开；最后一条用户消息之后的内容（当前这一轮）永远保留。
func Trim(h []Msg, budget int) []Msg {
	if Estimate(h) <= budget {
		return h
	}
	last := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return h
	}
	// 从后往前累加，找到能放下的最早的用户消息
	total := 0
	cut := last
	for i := len(h) - 1; i >= 0; i-- {
		total += msgTokens(h[i])
		if i <= last && h[i].Role == "user" {
			if total > budget {
				break
			}
			cut = i
		}
	}
	return h[cut:]
}

// Sanitize 保证每个工具调用后面都紧跟着它的结果（OpenAI 格式的硬性要求）：
// 缺结果的补一条占位结果，没有对应调用的孤立结果丢掉。正常流程下内核会保证结果齐全，
// 这里兜底的是进程崩溃、中途断线之类留下的残缺历史。
func Sanitize(h []Msg) []Msg {
	out := make([]Msg, 0, len(h))
	var pending []string // 还没有结果的调用 ID，按调用顺序
	flush := func() {
		for _, id := range pending {
			out = append(out, Msg{Role: "tool", ToolCallID: id, Content: "（这次调用没有返回结果，可能被中断了）"})
		}
		pending = nil
	}
	for _, m := range h {
		if m.Role == "tool" {
			found := false
			for i, id := range pending {
				if id == m.ToolCallID {
					pending = append(pending[:i], pending[i+1:]...)
					found = true
					break
				}
			}
			if found {
				out = append(out, m)
			}
			continue
		}
		flush()
		out = append(out, m)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				pending = append(pending, tc.ID)
			}
		}
	}
	flush()
	return out
}

// BuildMessages 生成发给模型的消息：系统提示 + 修补、裁剪后的历史。
// sendReasoning 为 true 时，当前这一轮（最后一条用户消息之后）的推理内容会回传给模型。
func BuildMessages(system string, h []Msg, budget int, sendReasoning bool) []llm.Message {
	h = Trim(Sanitize(h), budget)
	last := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			last = i
			break
		}
	}
	msgs := make([]llm.Message, 0, len(h)+1)
	if system != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: system})
	}
	for i, m := range h {
		lm := llm.Message{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		if sendReasoning && i > last && m.Role == "assistant" {
			lm.ReasoningContent = m.Reasoning
		}
		msgs = append(msgs, lm)
	}
	return msgs
}
