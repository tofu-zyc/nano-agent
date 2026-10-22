// Package llm 是模型接入层：OpenAI 兼容的 Chat Completions 接口，以及测试用的假模型。
package llm

import (
	"context"
	"encoding/json"
)

// Message 是发给模型的一条消息（OpenAI chat 格式）。有 Parts 时按多模态格式发 content 数组，
// 否则 Content 还是一个字符串。
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	Parts            []Part     `json:"-"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// Part 是 content 数组里的一项：文字或图片。
type Part struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 是一张图片（data: URL，或者 http 地址）。
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // low | high | auto
}

// TextPart、ImagePart 构造 content 的一部分。
func TextPart(text string) Part { return Part{Type: "text", Text: text} }

func ImagePart(url string) Part { return Part{Type: "image_url", ImageURL: &ImageURL{URL: url}} }

// MarshalJSON 让只有文字的消息还是发 "content": "..."（各家兼容接口都认），
// 有图片时发 OpenAI 的 content 数组。
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role             string     `json:"role"`
		Content          any        `json:"content"`
		ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string     `json:"tool_call_id,omitempty"`
		ReasoningContent string     `json:"reasoning_content,omitempty"`
	}
	content := any(m.Content)
	if len(m.Parts) > 0 {
		content = m.Parts
	}
	return json.Marshal(wire{Role: m.Role, Content: content, ToolCalls: m.ToolCalls,
		ToolCallID: m.ToolCallID, ReasoningContent: m.ReasoningContent})
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Messages  []Message
	Tools     []Tool
	Thinking  string // reasoning_effort，空表示用模型默认，none 表示关闭
	MaxTokens int    // 单次请求的输出上限；0 表示用配置值
}

// Delta 是流式输出的一个片段。
type Delta struct {
	Reasoning string
	Content   string
}

// 模型一轮的结局。
const (
	ReasonFinal   = "final"
	ReasonToolUse = "tool_use"
	ReasonRefused = "refused"
)

type Result struct {
	Reason    string
	Content   string
	Reasoning string
	Calls     []ToolCall
	Truncated bool // 回复因 max_tokens 被截断
	Usage     Usage
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`    // 输入里命中前缀缓存的部分
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"` // 输出里思考的部分
}

type Client interface {
	// Chat 请求一轮回复。onDelta 不为 nil 时走流式接口，生成过程中的片段依次交给它
	// （在调用 Chat 的协程里执行），返回的 Result 仍然是完整的。
	Chat(ctx context.Context, req Request, onDelta func(Delta)) (*Result, error)
}
