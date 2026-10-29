package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/redact"
)

type jobHint struct {
	info     JobInfo
	started  time.Time
	notified bool
}
type jobPresentation struct {
	key                           string
	info                          JobInfo
	ep                            endpoint
	ch                            channel.Channel
	summary, ref, hint            string
	tokens                        map[string]string
	accepted                      map[string]bool
	working                       map[string]bool
	expires, removeAt, nextUpdate time.Time
	dirty, sending, expired       bool
	failures                      int
}
type jobButtonAction struct{ key, action string }

func jobCardKey(info JobInfo, ep endpoint) string {
	raw, _ := json.Marshal([]string{info.Node, info.JobID, ep.key()})
	return string(raw)
}
func jobStateLabel(status string) string {
	labels := map[string]string{"preparing": "正在移交", "queued": "排队中", "starting": "正在启动", "running": "运行中",
		"cancelling": "正在取消", "succeeded": "已完成", "failed": "失败", "cancelled": "已取消", "timed_out": "已超时",
		"interrupted": "重启后中断", "unknown": "状态未知"}
	if text := labels[status]; text != "" {
		return text
	}
	return status
}

// All asynchronous Job replies use the original channel/agent scope. Binding
// changes may revoke a delivery; they never retarget it.
func (a *App) jobEndpointValid(ch channel.Channel, ep endpoint, agent, sid string) bool {
	if a.ctx.Err() != nil || ch == nil || a.chans[ep.ch] != ch || !a.allowed(ep.key()) ||
		!a.agentExists(agent) || a.execAgentID(sid) != agent {
		return false
	}
	if record := a.eps[ep.key()]; record != nil && a.execAgentID(record.Session) != agent {
		return false
	}
	if ep.ch != "cli" && ep.ch != "web" && a.agentForChannel(ep.ch) != agent && !a.hasExplicitAgent(sid) {
		return false
	}
	return true
}
func (a *App) jobReply(ch channel.Channel, ep endpoint, agent, sid, text string) bool {
	if !a.jobEndpointValid(ch, ep, agent, sid) {
		return false
	}
	if redact.Needed(a.cfg.Security.Redact, false, ep.ch != "web" && ep.ch != "cli") {
		text = redact.ToolResult(text, a.knownSecrets())
	}
	a.tell(sid, ep, text)
	return true
}
func (a *App) jobCardOrigin(p *jobPresentation) bool {
	return !p.info.Silent && !a.branchSilent(p.info.SID) && a.jobEndpointValid(p.ch, p.ep, p.info.Agent, p.info.SID)
}
func (a *App) removeJobCard(p *jobPresentation) {
	for _, token := range p.tokens {
		delete(a.jobButtons, token)
	}
	delete(a.jobCards, p.key)
}
func (a *App) jobCard(p *jobPresentation) channel.JobCard {
	info := p.info
	card := channel.JobCard{JobID: info.JobID, Node: info.Node, Status: jobStateLabel(info.Status),
		Summary: p.summary, Background: info.Background, Finished: finalJob(info.Status), Detail: info.Error}
	if card.Detail == "" && !info.Background && !card.Finished {
		card.Detail = p.hint
	}
	if info.Status == "unknown" {
		card.Detail = "执行结果未知，执行名额和资源占用仍保留。"
	}
	if runes := []rune(card.Detail); len(runes) > 1000 {
		card.Detail = string(runes[:1000]) + "…"
	}
	if p.expired {
		card.Detail += "\n按钮已过期，请使用任务命令。"
		return card
	}
	actions := []struct{ action, label string }{{"detach", "转入后台"}, {"output", "查看输出"}, {"cancel", "停止命令"}}
	for _, button := range actions {
		token := p.tokens[button.action]
		if token == "" {
			continue
		}
		card.Buttons = append(card.Buttons, channel.JobButton{Label: button.label, Token: token})
	}
	return card
}

// The existing bounded per-channel sender owns all platform I/O. A state
// change while a card request is in flight is coalesced by the next tick.
func (a *App) queueJobCard(p *jobPresentation) {
	if p.sending || !p.dirty || a.now().Before(p.nextUpdate) || !a.jobCardOrigin(p) {
		return
	}
	presenter, ok := p.ch.(channel.JobPresenter)
	if !ok {
		return
	}
	p.sending = true
	if !a.enqueue(p.ep, outItem{job: func(ctx context.Context) {
		a.mu.Lock()
		if a.jobCards[p.key] != p || !a.jobCardOrigin(p) {
			p.sending = false
			a.mu.Unlock()
			return
		}
		card, ref, revision := a.jobCard(p), p.ref, p.info.Revision
		a.mu.Unlock()
		var err error
		if ref == "" {
			ref, err = presenter.SendJob(ctx, p.ep.chat, card)
		} else {
			err = presenter.UpdateJob(ctx, p.ep.chat, ref, card)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.jobCards[p.key] != p {
			return
		}
		p.sending = false
		p.nextUpdate = a.now().Add(time.Second)
		if err != nil {
			a.log.Warn("任务卡片请求失败", "job", p.info.JobID, "chat", p.ep.key(), "err", err)
			if p.ref == "" {
				// Sending may have succeeded remotely: no blind resend of the
				// original card. A plain command fallback remains usable.
				if a.jobCardOrigin(p) {
					a.deliver(p.ep, channel.JobCardText(card), "")
				}
				a.removeJobCard(p)
				return
			}
			p.failures++
			p.nextUpdate = a.now().Add(time.Duration(p.failures) * 5 * time.Second)
			if p.failures >= 3 {
				a.removeJobCard(p)
			}
			return
		}
		p.ref = ref
		p.failures = 0
		p.dirty = p.info.Revision != revision || p.expired && len(card.Buttons) > 0
	}}) {
		p.sending = false
	}
}
func (a *App) observeJobPresentation(info JobInfo) {
	key := jobPersistKey(info.Node, info.JobID)
	for _, p := range a.jobCards {
		if p.info.Node != info.Node || p.info.JobID != info.JobID || info.Revision < p.info.Revision {
			continue
		}
		changed := info.Revision != p.info.Revision || info.Online != p.info.Online
		p.info = info
		if changed {
			p.dirty = true
		}
		for action, token := range p.tokens {
			if action != "output" && (finalJob(info.Status) || action == "detach" && info.Background) {
				delete(a.jobButtons, token)
				delete(p.tokens, action)
				p.dirty = true
			}
		}
		if finalJob(info.Status) && p.removeAt.IsZero() {
			p.removeAt = a.now().Add(30 * time.Minute)
		}
	}
	if finalJob(info.Status) || info.Background || info.Silent {
		delete(a.jobHints, key)
		return
	}
	if info.Status != "running" && info.Status != "cancelling" {
		return
	}
	if watch := a.jobHints[key]; watch != nil {
		watch.info = info
		return
	}
	source := a.sessions[info.SID]
	if source == nil || source.run == nil || source.run.runID != info.RunID || source.run.inner || a.branchSilent(info.SID) {
		return
	}
	if len(a.jobHints) >= 1024 {
		return
	}
	a.jobHints[key] = &jobHint{info: info, started: a.now()}
}
func (a *App) jobHintTick() {
	if a.ctx.Err() != nil {
		return
	}
	now := a.now()
	for _, p := range a.jobCards {
		if !a.jobCardOrigin(p) {
			a.removeJobCard(p)
			continue
		}
		if !p.removeAt.IsZero() && !now.Before(p.removeAt) {
			a.removeJobCard(p)
			continue
		}
		if !p.expired && !now.Before(p.expires) {
			for _, token := range p.tokens {
				delete(a.jobButtons, token)
			}
			p.tokens = map[string]string{}
			p.expired, p.dirty = true, true
			p.removeAt = now.Add(5 * time.Minute)
		}
		a.queueJobCard(p)
	}
	for key, watch := range a.jobHints {
		info := watch.info
		s := a.sessions[info.SID]
		if s == nil || s.run == nil || s.run.runID != info.RunID || s.run.turnID != info.TurnID ||
			s.run.inner || info.Silent || a.branchSilent(info.SID) {
			delete(a.jobHints, key)
			continue
		}
		if watch.notified || info.Status != "running" || now.Sub(watch.started) < time.Duration(a.runtimeLimits.LongCallHintSeconds)*time.Second {
			continue
		}
		watch.notified = true
		summary := "Shell 命令"
		var args struct {
			Command string `json:"command"`
		}
		if call, ok := s.run.callArgs[info.CallID]; ok && json.Unmarshal(call.Args, &args) == nil && args.Command != "" {
			summary = redact.ToolResult(args.Command, a.knownSecrets())
			if runes := []rune(summary); len(runes) > 400 {
				summary = string(runes[:400]) + "…"
			}
		}
		card := channel.JobCard{JobID: info.JobID, Node: info.Node, Status: "运行中", Summary: summary,
			Detail: fmt.Sprintf("命令已运行至少 %d 秒，仍在执行。", a.runtimeLimits.LongCallHintSeconds)}
		text := channel.JobCardText(card)
		a.publish(events.Event{T: "say", SID: info.SID, RunID: info.RunID, TurnID: info.TurnID, ID: info.CallID, Text: text})
		seen := map[string]bool{}
		for _, out := range s.run.outs {
			if len(seen) >= 8 {
				break
			}
			if seen[out.ep.key()] {
				continue
			}
			seen[out.ep.key()] = true
			ch := a.chans[out.ep.ch]
			p := &jobPresentation{key: jobCardKey(info, out.ep), info: info, ep: out.ep, ch: ch, summary: summary,
				hint: card.Detail, tokens: map[string]string{}, accepted: map[string]bool{}, working: map[string]bool{}, expires: now.Add(24 * time.Hour), dirty: true}
			if ch == nil || !a.jobCardOrigin(p) {
				continue
			}
			if _, ok := ch.(channel.JobPresenter); !ok || len(a.jobCards) >= 256 {
				a.deliver(out.ep, text, "")
				continue
			}
			a.jobCards[p.key] = p
			for _, action := range []string{"detach", "output", "cancel"} {
				token := channel.JobTokenPrefix + randHex(12)
				p.tokens[action] = token
				a.jobButtons[token] = jobButtonAction{key: p.key, action: action}
			}
			a.queueJobCard(p)
		}
	}
}
func (a *App) jobButton(ch channel.Channel, ep endpoint, m channel.Message, by string) {
	deny := func(text string) { a.deliver(ep, text, "") }
	action, ok := a.jobButtons[m.Text]
	p := a.jobCards[action.key]
	if !ok || p == nil || p.ch != ch || p.ep != ep || p.expired || !a.now().Before(p.expires) || !a.jobCardOrigin(p) ||
		p.ref == "" || m.MsgID != p.ref {
		deny("任务按钮已过期或来源已改变，请用 /jobs 查看。")
		return
	}
	req := JobRequest{Action: action.action, Node: p.info.Node, JobID: p.info.JobID, MaxBytes: 8192}
	record, err := a.jobRecord(req, by, p.info.SID)
	if err != nil {
		deny("没有这个任务的操作权限。")
		return
	}
	// Re-authorize each click, including repeat/cached actions.
	if action.action == "detach" && (!a.isAdmin(by) || a.workspaceOf(p.info.SID).mode == "readonly" || a.workspaceOf(p.info.SID).mode == "none") {
		deny("当前权限不能把任务转后台。")
		return
	}
	if record.Silent {
		deny("安静任务只能在它自己的工作入口操作。")
		return
	}
	if p.working[action.action] {
		deny("这项任务操作正在处理，请稍后查询状态。")
		return
	}
	if p.accepted[action.action] {
		deny("该操作已经受理，请查看同一任务的最终状态。")
		return
	}
	p.working[action.action] = true
	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Second)
	go func() {
		defer cancel()
		result, err := a.JobAction(ctx, by, p.info.SID, req)
		a.mu.Lock()
		defer a.mu.Unlock()
		p.working[action.action] = false
		if err == nil && result.Accepted && action.action != "output" {
			p.accepted[action.action] = true
		}
		if a.jobCards[p.key] != p || !a.jobCardOrigin(p) {
			return
		}
		latest, accessErr := a.jobRecord(req, by, p.info.SID)
		if accessErr != nil || latest.Silent {
			return
		}
		text := ""
		if err != nil {
			text = "任务操作：" + err.Error()
		} else {
			text = renderJobResponse(result)
		}
		// An explicit click returns to its original endpoint/session. It does
		// not attach a window, write model history, or select another chat.
		a.jobReply(p.ch, p.ep, p.info.Agent, p.info.SID, strings.TrimSpace(text))
	}()
}
