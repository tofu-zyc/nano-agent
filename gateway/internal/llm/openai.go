package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
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

// apiUsage 是接口返回的用量。缓存命中数各家放的位置不一样：OpenAI、vLLM 在
// prompt_tokens_details.cached_tokens，DeepSeek 官方在 prompt_cache_hit_tokens。
type apiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptCacheHit      int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *apiUsage) usage() Usage {
	if u == nil {
		return Usage{}
	}
	r := Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CachedTokens: u.PromptCacheHit}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		r.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		r.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return r
}

type apiMessage struct {
	Content          *string    `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
	Reasoning        string     `json:"reasoning"`
	ReasoningContent string     `json:"reasoning_content"`
	Refusal          *string    `json:"refusal"`
}

type chatResponse struct {
	Choices []struct {
		Message      apiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *apiUsage `json:"usage"`
}

// 可以重试的错误：网络错误、429、5xx。
type retryable struct{ error }

func (o *OpenAI) Chat(ctx context.Context, req Request, onDelta func(Delta)) (*Result, error) {
	payload, err := json.Marshal(o.body(req, onDelta != nil))
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		var res *Result
		emitted := false
		if onDelta == nil {
			var cr *chatResponse
			if cr, err = o.post(ctx, payload); err == nil {
				res, err = toResult(cr.Choices[0].Message, cr.Choices[0].FinishReason, cr.Usage)
			}
		} else {
			res, err = o.stream(ctx, payload, func(d Delta) { emitted = true; onDelta(d) })
		}
		// 已经输出了一部分就不能重试，否则用户会看到重复的内容
		var re retryable
		if err == nil || !errors.As(err, &re) || emitted || attempt >= len(o.retryDelays) {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(o.retryDelays[attempt]):
		}
	}
}

func (o *OpenAI) body(req Request, stream bool) map[string]any {
	body := maps.Clone(o.extra)
	if body == nil {
		body = map[string]any{}
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
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// 思考强度用 OpenAI 的 reasoning_effort，值原样传（none 表示关闭）。vLLM 等兼容接口也认它。
	if req.Thinking != "" {
		body["reasoning_effort"] = req.Thinking
	}
	return body
}

func (o *OpenAI) request(ctx context.Context, payload []byte, stream bool) (*http.Response, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
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
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		err := fmt.Errorf("模型接口返回 %d：%s", resp.StatusCode, snippet(data, 300))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, retryable{err}
		}
		return nil, err
	}
	return resp, nil
}

func (o *OpenAI) post(ctx context.Context, payload []byte) (*chatResponse, error) {
	resp, err := o.request(ctx, payload, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, retryable{fmt.Errorf("读取模型响应失败：%w", err)}
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("模型响应不是合法 JSON：%w（%s）", err, snippet(data, 200))
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("模型响应里没有 choices")
	}
	return &cr, nil
}

// ---- 流式 ----

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			Reasoning        string  `json:"reasoning"`
			ReasoningContent string  `json:"reasoning_content"`
			Refusal          *string `json:"refusal"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *apiUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAI) stream(ctx context.Context, payload []byte, emit func(Delta)) (*Result, error) {
	resp, err := o.request(ctx, payload, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var content, reasoning, refusal strings.Builder
	var gotContent, gotRefusal bool
	calls := map[int]*ToolCall{}
	finish := ""
	sawDone := false
	var usage *apiUsage

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if string(data) == "[DONE]" {
			sawDone = true
			break
		}
		var ch streamChunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return nil, fmt.Errorf("模型的流式响应不是合法 JSON：%w（%s）", err, snippet(data, 200))
		}
		if ch.Error != nil {
			return nil, fmt.Errorf("模型接口报错：%s", ch.Error.Message)
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		for _, c := range ch.Choices {
			d := c.Delta
			r := d.Reasoning
			if r == "" {
				r = d.ReasoningContent
			}
			var out Delta
			if r != "" {
				reasoning.WriteString(r)
				out.Reasoning = r
			}
			if d.Content != nil && *d.Content != "" {
				content.WriteString(*d.Content)
				gotContent = true
				out.Content = *d.Content
			}
			if d.Refusal != nil && *d.Refusal != "" {
				refusal.WriteString(*d.Refusal)
				gotRefusal = true
			}
			if out != (Delta{}) {
				emit(out)
			}
			for _, tc := range d.ToolCalls {
				t := calls[tc.Index]
				if t == nil {
					t = &ToolCall{Type: "function"}
					calls[tc.Index] = t
				}
				if tc.ID != "" {
					t.ID = tc.ID
				}
				t.Function.Name += tc.Function.Name
				t.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, retryable{fmt.Errorf("读取模型的流式响应失败：%w", err)}
	}

	m := apiMessage{Reasoning: reasoning.String()}
	if gotContent {
		s := content.String()
		m.Content = &s
	}
	if gotRefusal {
		s := refusal.String()
		m.Refusal = &s
	}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		m.ToolCalls = append(m.ToolCalls, *calls[i])
	}
	if finish == "" && !sawDone {
		return nil, retryable{errors.New("模型的流式响应中途断开了")}
	}
	return toResult(m, finish, usage)
}

func toResult(m apiMessage, finish string, u *apiUsage) (*Result, error) {
	r := &Result{Usage: u.usage(), Reasoning: m.Reasoning}
	if r.Reasoning == "" {
		r.Reasoning = m.ReasoningContent
	}
	if m.Content != nil {
		r.Content = *m.Content
	}

	switch {
	case len(m.ToolCalls) > 0:
		// 输出被截断时，最后一个调用的参数很可能不完整，不能执行
		if finish == "length" {
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
	case finish == "content_filter" || (m.Refusal != nil && *m.Refusal != "" && r.Content == ""):
		r.Reason = ReasonRefused
		if m.Refusal != nil {
			r.Content = *m.Refusal
		}
	default:
		r.Reason = ReasonFinal
		r.Truncated = finish == "length"
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
