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
	Messages []Message
	Tools    []Tool
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
}

type Client interface {
	Chat(ctx context.Context, req Request) (*Result, error)
}
