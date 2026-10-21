// Package llm 是模型接入层：OpenAI 兼容的 Chat Completions 接口，以及测试用的假模型。
package llm

import (
	"context"
	"encoding/json"
)

// Message 是发给模型的一条消息（OpenAI chat 格式）。
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
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
