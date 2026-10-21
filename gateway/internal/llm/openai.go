package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/httpx"
)

// OpenAI 对接 OpenAI 兼容的 /chat/completions 接口（vLLM、DeepSeek、通义、Kimi 等）。
type OpenAI struct {
	url         string
	apiKey      string
	model       string
	temperature *float64
	maxTokens   int
	extra       map[string]any
	hc          *http.Client
	retryDelays []time.Duration
}

func NewOpenAI(c config.LLM) (*OpenAI, error) {
	hc, err := httpx.NewClient(c.Proxy, time.Duration(c.Timeout)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("llm.proxy：%w", err)
	}
	return &OpenAI{
		url:         strings.TrimRight(c.BaseURL, "/") + "/chat/completions",
		apiKey:      c.APIKey,
		model:       c.Model,
		temperature: c.Temperature,
		maxTokens:   c.MaxTokens,
		extra:       c.Extra,
		hc:          hc,
		retryDelays: []time.Duration{time.Second, 3 * time.Second},
	}, nil
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          *string    `json:"content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
			Reasoning        string     `json:"reasoning"`
			ReasoningContent string     `json:"reasoning_content"`
			Refusal          *string    `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// 可以重试的错误：网络错误、429、5xx。
type retryable struct{ error }

func (o *OpenAI) Chat(ctx context.Context, req Request) (*Result, error) {
	body := map[string]any{}
	for k, v := range o.extra {
		body[k] = v
	}
	body["model"] = o.model
	body["messages"] = req.Messages
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if o.temperature != nil {
		body["temperature"] = *o.temperature
	}
	if o.maxTokens > 0 {
		body["max_tokens"] = o.maxTokens
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		resp, err := o.post(ctx, payload)
		var re retryable
		if err == nil || !errors.As(err, &re) || attempt >= len(o.retryDelays) {
			if err != nil {
				return nil, err
			}
			return toResult(resp)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(o.retryDelays[attempt]):
		}
	}
}

func (o *OpenAI) post(ctx context.Context, payload []byte) (*chatResponse, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		hr.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.hc.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, retryable{fmt.Errorf("请求模型失败：%w", err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, retryable{fmt.Errorf("读取模型响应失败：%w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("模型接口返回 %d：%s", resp.StatusCode, snippet(data, 300))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, retryable{err}
		}
		return nil, err
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("模型响应不是合法 JSON：%w（%s）", err, snippet(data, 200))
	}
	return &cr, nil
}

func toResult(cr *chatResponse) (*Result, error) {
	if len(cr.Choices) == 0 {
		return nil, errors.New("模型响应里没有 choices")
	}
	ch := cr.Choices[0]
	m := ch.Message
	r := &Result{Usage: cr.Usage, Reasoning: m.Reasoning}
	if r.Reasoning == "" {
		r.Reasoning = m.ReasoningContent
	}
	if m.Content != nil {
		r.Content = *m.Content
	}

	switch {
	case len(m.ToolCalls) > 0:
		// 输出被截断时，最后一个调用的参数很可能不完整，不能执行
		if ch.FinishReason == "length" {
			return nil, errors.New("模型输出达到长度上限，工具调用参数可能不完整")
		}
		r.Reason = ReasonToolUse
		for i, tc := range m.ToolCalls {
			if tc.ID == "" {
				tc.ID = fmt.Sprintf("call_%d", i)
			}
			tc.Type = "function"
			if strings.TrimSpace(tc.Function.Arguments) == "" {
				tc.Function.Arguments = "{}"
			}
			r.Calls = append(r.Calls, tc)
		}
	case ch.FinishReason == "content_filter" || (m.Refusal != nil && *m.Refusal != "" && r.Content == ""):
		r.Reason = ReasonRefused
		if m.Refusal != nil {
			r.Content = *m.Refusal
		}
	default:
		r.Reason = ReasonFinal
		r.Truncated = ch.FinishReason == "length"
	}
	return r, nil
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
