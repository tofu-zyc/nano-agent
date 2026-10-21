package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// compact 只在历史超限时做一次摘要；平时不改变请求前缀。
// 当前用户这一轮留在历史里，摘要生成过程中仍可停止或清空会话。
func (a *App) compact(s *sess, high, low int) {
	r := s.run
	h := a.store.History(s.sid)
	cut := session.CutPoint(h, low)
	req := llm.SummaryRequest(a.store.Settings(s.sid).Summary, session.BuildMessages("", "", h[:cut], false))
	if slices.Contains(a.cfg.LLM.ThinkingLevels, "none") {
		req.Thinking = "none"
	}
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	r.cancelLLM = cancel
	r.calls++
	a.totals.ModelCalls++
	note := "正在整理较早的对话…"
	for _, v := range r.views() {
		v.Note(note)
	}
	a.publish(events.Event{T: "say", SID: s.sid, Text: note})
	a.publish(events.Event{T: "model", SID: s.sid, Kind: "start", Data: map[string]any{"purpose": "summary"}})
	client := a.llm
	go func() {
		start := time.Now()
		res, err := client.Chat(ctx, req, nil)
		a.mu.Lock()
		defer a.mu.Unlock()
		stale := s.run != r || ctx.Err() == context.Canceled
		cancel()
		if stale {
			return
		}
		r.cancelLLM = nil
		data := map[string]any{"purpose": "summary", "ms": time.Since(start).Milliseconds()}
		var summary string
		if err == nil {
			a.addUsage(r, res.Usage)
			data["usage"] = res.Usage
			summary = strings.TrimSpace(res.Content)
			if res.Reason != llm.ReasonFinal || res.Truncated || summary == "" {
				err = fmt.Errorf("未得到完整摘要（%s）", res.Reason)
			}
		}
		text := llm.ReasonFinal
		if err != nil {
			summary = ""
			text = "error: " + err.Error()
			a.log.Warn("上下文摘要失败，沿用裁剪", "sid", s.sid, "err", err)
		}
		a.publish(events.Event{T: "model", SID: s.sid, Kind: "end", Text: text, Data: data})
		n, saveErr := a.store.Compact(s.sid, high, low, summary)
		if saveErr != nil {
			a.log.Warn("保存上下文摘要失败", "sid", s.sid, "err", saveErr)
			a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, Reason: proto.ReasonError, Detail: "保存上下文摘要：" + saveErr.Error()})
			return
		}
		a.log.Info("上下文已整理", "sid", s.sid, "messages", n, "summarized", summary != "")
		a.generateModel(s)
	}()
}

func (a *App) addUsage(r *run, u llm.Usage) {
	r.usage.PromptTokens += u.PromptTokens
	r.usage.CachedTokens += u.CachedTokens
	r.usage.CompletionTokens += u.CompletionTokens
	r.usage.ReasoningTokens += u.ReasoningTokens
	a.totals.add(u)
}
