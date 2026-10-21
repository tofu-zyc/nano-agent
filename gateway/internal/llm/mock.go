package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
)

// Mock 是不调用任何接口的假模型，用来不花钱地跑通整条链路：
//
//	用户消息里含 "!call 工具名 {json参数}" → 请求调用该工具
//	最后一条是工具结果                       → 把工具结果复述出来作为最终回复
//	其他                                     → 回显用户消息（去掉网关加的环境说明和时间戳）
//
// 流式请求时把回复切成几段依次输出；开了思考（Thinking 不是空或 none）时先输出一段假的思考。
type Mock struct {
	n atomic.Int64
}

var mockStamp = regexp.MustCompile(`(?m)^\[\d{4}-\d{2}-\d{2} [^\]]*\] `)

func (m *Mock) Chat(ctx context.Context, req Request, onDelta func(Delta)) (*Result, error) {
	res := m.reply(req)
	if onDelta != nil {
		if req.Thinking != "" && req.Thinking != "none" {
			res.Reasoning = "（mock）想一想……"
			onDelta(Delta{Reasoning: res.Reasoning})
		}
		r := []rune(res.Content)
		for i := 0; i < len(r); i += 4 {
			onDelta(Delta{Content: string(r[i:min(i+4, len(r))])})
		}
	}
	res.Usage = Usage{PromptTokens: 100, CompletionTokens: 10, CachedTokens: 90}
	return res, nil
}

func (m *Mock) reply(req Request) *Result {
	if len(req.Messages) == 0 {
		return &Result{Reason: ReasonFinal, Content: "（mock）没有消息"}
	}
	if req.Messages[0].Content == summaryPrompt {
		return &Result{Reason: ReasonFinal, Content: "（mock）较早对话已整理，继续处理最近的任务。"}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		var parts []string
		for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role == "tool"; i-- {
			parts = append([]string{req.Messages[i].Content}, parts...)
		}
		return &Result{Reason: ReasonFinal, Content: "（mock）工具结果：" + strings.Join(parts, " | ")}
	}
	text := last.Content
	// 摘要和环境说明可能占多行，时间戳后面才是这次用户的正文。
	if loc := mockStamp.FindStringIndex(text); loc != nil {
		text = text[loc[1]:]
	} else if strings.HasPrefix(text, "[系统] ") {
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			text = rest
		}
	}
	if i := strings.Index(text, "!call "); i >= 0 {
		name, args, _ := strings.Cut(strings.TrimSpace(text[i+len("!call "):]), " ")
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		return &Result{Reason: ReasonToolUse, Calls: []ToolCall{{
			ID:       fmt.Sprintf("mock-%d", m.n.Add(1)),
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: strings.TrimSpace(args)},
		}}}
	}
	return &Result{Reason: ReasonFinal, Content: "（mock）收到：" + text}
}
