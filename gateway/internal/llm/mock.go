package llm

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// Mock 是不调用任何接口的假模型，用来不花钱地跑通整条链路：
//
//	用户消息里含 "!call 工具名 {json参数}" → 请求调用该工具
//	最后一条是工具结果                       → 把工具结果复述出来作为最终回复
//	其他                                     → 回显用户消息
type Mock struct {
	n atomic.Int64
}

func (m *Mock) Chat(ctx context.Context, req Request) (*Result, error) {
	if len(req.Messages) == 0 {
		return &Result{Reason: ReasonFinal, Content: "（mock）没有消息"}, nil
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		var parts []string
		for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role == "tool"; i-- {
			parts = append([]string{req.Messages[i].Content}, parts...)
		}
		return &Result{Reason: ReasonFinal, Content: "（mock）工具结果：" + strings.Join(parts, " | ")}, nil
	}
	text := last.Content
	if i := strings.Index(text, "!call "); i >= 0 {
		name, args, _ := strings.Cut(strings.TrimSpace(text[i+len("!call "):]), " ")
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		return &Result{Reason: ReasonToolUse, Calls: []ToolCall{{
			ID:       fmt.Sprintf("mock-%d", m.n.Add(1)),
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: strings.TrimSpace(args)},
		}}}, nil
	}
	return &Result{Reason: ReasonFinal, Content: "（mock）收到：" + text}, nil
}
