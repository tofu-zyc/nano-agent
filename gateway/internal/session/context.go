package session

import (
	"fmt"
	"strings"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
)

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

// CutPoint 返回从哪条消息开始保留，才能在不超过 budget 的前提下保留尽量多的最近历史。
// 只在用户消息处切，这样不会把一次工具调用和它的结果拆开；最后一条用户消息之后的内容
// （当前这一轮）永远保留。不需要切时返回 0。
func CutPoint(h []Msg, budget int) int {
	if Estimate(h) <= budget {
		return 0
	}
	last := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return 0
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
	return cut
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

// ImageLoader 把一张图片变成发给模型的 data: URL；返回 false 表示读不出来。
type ImageLoader func(media.Attachment) (string, bool)

// BuildMessages 生成发给模型的消息：系统提示 + 修补过的历史。summary 是被 Compact 丢掉的
// 那部分历史的摘要，放在第一条用户消息前面。
// sendReasoning 为 true 时，当前这一轮（最后一条用户消息之后）的推理内容会回传给模型。
// images 为 nil 时（模型不支持图片，或者生成摘要）图片只留一个文字说明。
func BuildMessages(system, summary string, h []Msg, sendReasoning bool, images ImageLoader) []llm.Message {
	h = Sanitize(h)
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
	first := true
	// 工具读到的图片：OpenAI 兼容接口的工具结果不能带图，跟在这一批工具结果后面作为一条用户消息
	var toolImgs []media.Attachment
	flushImgs := func() {
		if len(toolImgs) > 0 && images != nil {
			msgs = append(msgs, llm.Message{Role: "user", Parts: imageParts("[系统] 上面的工具读到的图片：", toolImgs, images)})
		}
		toolImgs = nil
	}
	for i, m := range h {
		if m.Role != "tool" {
			flushImgs()
		} else {
			toolImgs = append(toolImgs, m.Images...)
		}
		content := m.Content
		if first && summary != "" && m.Role == "user" {
			content = summary + "\n" + content
			first = false
		}
		lm := llm.Message{Role: m.Role, Content: content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		if sendReasoning && i > last && m.Role == "assistant" {
			lm.ReasoningContent = m.Reasoning
		}
		if m.Role == "user" && len(m.Images) > 0 {
			if images == nil {
				// 模型不支持图片（或者正在生成摘要）：保留文字内容的格式，只在后面说明有几张图片
				note := fmt.Sprintf("[图片×%d]", len(m.Images))
				if strings.TrimSpace(content) == "" {
					lm.Content = note
				} else {
					lm.Content = strings.TrimRight(content, "\n") + "\n" + note
				}
			} else {
				lm.Parts = imageParts(content, m.Images, images)
			}
		}
		msgs = append(msgs, lm)
	}
	flushImgs()
	return msgs
}

// imageParts 把一条带图片的用户消息做成 content 数组（模型支持图片时才会走到这里）。
func imageParts(text string, imgs []media.Attachment, images ImageLoader) []llm.Part {
	var parts []llm.Part
	if strings.TrimSpace(text) != "" {
		parts = append(parts, llm.TextPart(text))
	}
	for _, a := range imgs {
		if url, ok := images(a); ok {
			parts = append(parts, llm.ImagePart(url))
		} else {
			parts = append(parts, llm.TextPart("[一张图片读不出来]"))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, llm.TextPart("[图片]"))
	}
	return parts
}
