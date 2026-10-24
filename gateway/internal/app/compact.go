package app

import (
	"context"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

func defaultContext() config.Context { return config.DefaultContext() }
func (a *App) contextConfig() (float64, float64, int) {
	c := a.cfg.Context
	if c.CompactAt == 0 {
		c = defaultContext()
	}
	return c.CompactAt, c.CompactTo, max(1, c.KeepTurns)
}
func (a *App) compactLimits(sid string) (int, int, int) {
	at, to, keep := a.contextConfigFor(sid)
	_, _, _, window := a.contextParts(sid)
	high, low := int(float64(window)*at), int(float64(window)*to)
	m, _, _ := a.modelOf(sid)
	limit := m.MaxHistory
	if limit == 0 && a.models == nil {
		limit = a.cfg.LLM.MaxHistoryTokens
	}
	if limit > 0 && limit < high {
		high = limit
		ratio := a.cfg.LLM.TrimTo
		if ratio <= 0 || ratio >= 1 {
			ratio = .5
		}
		low = int(float64(limit) * ratio)
	}
	return high, low, keep
}

// summaryChunks 把要整理的历史切成块：每块按序列化后的请求大小算，不超过 budget；工具输出先截到 2KB 再切
func summaryChunks(h []session.Msg, budget int) [][]llm.Message {
	msgs := session.BuildMessages("", "", h, false, nil)
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			msgs[i].Content += "\n[工具调用] " + tc.Function.Name + " " + cutUTF8(tc.Function.Arguments, 2048)
		}
		msgs[i].ToolCalls = nil
	}
	var out [][]llm.Message
	var chunk []llm.Message
	for _, m := range msgs {
		if m.Role == "tool" && len(m.Content) > 2048 {
			m.Content = cutUTF8(m.Content, 2048) + " [输出已截短]"
		}
		for {
			part := m
			part.ToolCalls = nil
			// JSON 会转义内容，所以量序列化后的请求，不量文本字节数
			remaining := m.Content
			if len(remaining) > max(64, budget*2) {
				part.Content = cutUTF8(remaining, max(64, budget*2))
			}
			if len(part.Content) == len(remaining) {
				part.ToolCalls = m.ToolCalls
			}
			trial := append(append([]llm.Message(nil), chunk...), part)
			if requestTokens(llm.SummaryRequest("", trial)) > budget && len(chunk) > 0 {
				out = append(out, chunk)
				chunk = nil
				continue
			}
			for requestTokens(llm.SummaryRequest("", []llm.Message{part})) > budget && len(part.Content) > 64 {
				part.Content = cutUTF8(part.Content, len(part.Content)/2)
				part.ToolCalls = nil
			}
			chunk = append(chunk, part)
			m.Content = remaining[len(part.Content):]
			if m.Content == "" {
				break
			}
			out = append(out, chunk)
			chunk = nil
		}
	}
	if len(chunk) > 0 {
		out = append(out, chunk)
	}
	return out
}
func requestTokens(req llm.Request) int { b, _ := json.Marshal(req.Messages); return len(b)/3 + 8 }
func cutUTF8(s string, n int) string {
	n = min(n, len(s))
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
func (a *App) compact(s *sess, high, low int) { a.beginCompact(s, high, low, "", false) }
func (a *App) beginCompact(s *sess, high, low int, focus string, background bool) {
	if s.compacting {
		return
	}
	_, _, keep := a.compactLimits(s.sid)
	before := a.contextInfo(s.sid).Total
	if err := a.store.Elide(s.sid, keep); err != nil {
		a.log.Warn("保存省略表失败", "err", err)
		if !background {
			a.generateModel(s)
		}
		return
	}
	h := a.store.EffectiveHistory(s.sid)
	overhead := a.contextInfo(s.sid)
	budget := max(0, low-overhead.Persona-overhead.Tools-max(2048, overhead.Summary))
	cut := session.PlanCut(h, budget, keep)
	if cut == 0 {
		if !background {
			a.generateModel(s)
		}
		return
	}
	s.compacting = true
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	s.compactCancel = cancel
	r := s.run
	if r != nil {
		r.cancelLLM = cancel
	}
	previous := a.store.Settings(s.sid).Summary
	_, _, _, window := a.contextParts(s.sid)
	chunks := summaryChunks(h[:cut], max(512, window/3-2500))
	client, cerr := a.clientFor(s.sid)
	thinking := ""
	if slices.Contains(a.levelsOf(s.sid), "none") {
		thinking = "none"
	}
	note := "正在整理上下文…"
	if r != nil {
		for _, v := range r.views() {
			v.Note(note)
		}
	}
	a.publish(events.Event{T: "say", SID: s.sid, Text: note})
	a.publish(events.Event{T: "context", SID: s.sid, Kind: "start"})
	go func() {
		summary := previous
		err := cerr
		for _, chunk := range chunks {
			if err != nil || ctx.Err() != nil {
				break
			}
			req := llm.SummaryRequest(summary, chunk)
			for requestTokens(req) > window/3 && len(summary) > 64 {
				summary = cutUTF8(summary, len(summary)/2)
				req = llm.SummaryRequest(summary, chunk)
			}
			req.Thinking = thinking
			if focus != "" {
				req.Messages[1].Content += "\n用户特别希望保留的重点：" + cutUTF8(focus, 2048)
			}
			a.mu.Lock()
			a.totals.ModelCalls++
			if r != nil {
				r.calls++
			}
			a.mu.Unlock()
			var res *llm.Result
			res, err = client.Chat(ctx, req, nil)
			if err == nil {
				a.mu.Lock()
				if r != nil {
					a.addUsage(r, res.Usage)
				} else {
					a.totals.add(res.Usage)
				}
				a.mu.Unlock()
				if res.Reason != llm.ReasonFinal || res.Truncated || strings.TrimSpace(res.Content) == "" {
					err = fmt.Errorf("未得到完整摘要（%s）", res.Reason)
				} else {
					summary = strings.TrimSpace(res.Content)
				}
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		ctxErr := ctx.Err()
		stale := s.compactCancel == nil || ctx.Err() == context.Canceled || (!background && s.run != r)
		cancel()
		if stale {
			return
		}
		s.compacting = false
		s.compactCancel = nil
		if r != nil {
			r.cancelLLM = nil
		}
		if err != nil || ctxErr == context.DeadlineExceeded {
			a.log.Warn("上下文摘要失败，保留省略表", "sid", s.sid, "err", err)
			summary = ""
			if a.contextInfo(s.sid).Total <= high {
				cut = 0
			}
		}
		if saveErr := a.store.CommitCompact(s.sid, cut, summary, a.now()); saveErr != nil {
			a.log.Warn("保存上下文摘要失败", "err", saveErr)
			if r != nil {
				a.hub.Send(r.node, proto.Msg{T: proto.TTurn, SID: s.sid, Reason: proto.ReasonError, Detail: "保存上下文摘要：" + saveErr.Error()})
			} else {
				a.startRun(s)
			}
			return
		}
		text := fmt.Sprintf("已整理上下文：%d → %d", before, a.contextInfo(s.sid).Total)
		if r != nil {
			for _, v := range r.views() {
				v.Note(text)
			}
		}
		a.publish(events.Event{T: "say", SID: s.sid, Text: text})
		a.publish(events.Event{T: "context", SID: s.sid, Kind: "end"})
		a.publishSession(s.sid)
		if background {
			for _, ep := range a.endpointsOf(s.sid) {
				if ep.Channel != "web" && a.showOf(parseEndpoint(ep.Key)) != "简洁" {
					a.deliver(parseEndpoint(ep.Key), text, "")
				}
			}
			a.startRun(s)
		} else {
			a.generateModel(s)
		}
	}()
}
func (a *App) addUsage(r *run, u llm.Usage) {
	r.usage.PromptTokens += u.PromptTokens
	r.usage.CachedTokens += u.CachedTokens
	r.usage.CompletionTokens += u.CompletionTokens
	r.usage.ReasoningTokens += u.ReasoningTokens
	a.totals.add(u)
}
func (a *App) maybeCompact(s *sess, idle bool) {
	if s.run != nil || s.compacting || len(s.queue) > 0 {
		return
	}
	high, low, _ := a.compactLimits(s.sid)
	threshold := int(float64(high) * .8)
	if idle {
		c := a.cfg.Context
		if c.CompactAt == 0 {
			c = defaultContext()
		}
		d, err := time.ParseDuration(c.IdleCompact)
		if err != nil || d <= 0 || s.idleCompacted || s.lastActivity.IsZero() || a.now().Sub(s.lastActivity) < d {
			return
		}
		_, _, _, window := a.contextParts(s.sid)
		threshold = int(float64(window) * c.IdleCompactMin)
	}
	if a.contextInfo(s.sid).Total > threshold {
		if idle {
			s.idleCompacted = true
		}
		low = min(low, a.contextInfo(s.sid).Total/2)
		a.beginCompact(s, high, low, "", true)
	}
}
func (a *App) maintenance() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
			a.Maintain()
		}
	}
}
func (a *App) Maintain() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sid := range a.sessionIDs() {
		if isCronSID(sid) {
			continue
		} // 下次任务会归档并清空，无需空闲摘要
		s := a.sessOf(sid)
		if s.lastActivity.IsZero() {
			for _, m := range a.store.History(sid) {
				if ts := time.Unix(m.TS, 0); ts.After(s.lastActivity) {
					s.lastActivity = ts
				}
			}
		}
		a.maybeCompact(s, true)
	}
	if a.lastArchiveClean.IsZero() || a.now().Sub(a.lastArchiveClean) >= 24*time.Hour {
		days := a.cfg.Context.ArchiveDays
		if a.cfg.Context.CompactAt == 0 {
			days = 90
		}
		if err := a.store.PruneArchives(a.now(), days); err != nil {
			a.log.Warn("清理归档失败", "err", err)
		}
		a.lastArchiveClean = a.now()
	}
}
func (a *App) cmdCompact(c in, focus string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能整理上下文")
		return
	}
	if c.s.run != nil || c.s.compacting {
		a.answer(c, "任务正在进行，结束后再整理")
		return
	}
	high, low, _ := a.compactLimits(c.s.sid)
	low = min(low, a.contextInfo(c.s.sid).Total/2)
	a.beginCompact(c.s, high, low, focus, true)
	if !c.s.compacting {
		a.answer(c, "没有可以整理的旧轮次，最近的对话原样保留")
	}
}
