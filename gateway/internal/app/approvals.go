package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
)

// 审批：内核说某个工具调用要管理员批准（approve_req）。能显示按钮的渠道（Telegram、飞书）发一张带
// “批准 / 拒绝”按钮的审批消息，其他窗口发文字（回复 /approve 编号）。不管在哪儿批（按钮、命令、网页），
// 所有窗口里的审批消息都改成处理结果。

type approval struct {
	sid, id, name, detail, from string
	at                          time.Time
	card                        channel.ApprovalCard
	refs                        []approvalRef // 发出去的带按钮的审批消息
	result                      string        // 处理结果，空表示还在等
	// resolve 不为空表示这不是内核要执行的工具（比如 MCP 在网关这边跑），
	// 批不批由这里交回去，不用发 TApprove 给内核
	resolve func(ok bool)
}

type approvalRef struct {
	ep  endpoint
	ref string
}

// askApproval 处理内核的 approve_req。调用方持有锁。
func (a *App) askApproval(s *sess, m *proto.Msg) {
	a.nextAppr++
	n := a.nextAppr
	title, body := callCard(m.Name, m.Args)
	desc := title + "：\n" + body
	ap := &approval{sid: s.sid, id: m.ID, name: m.Name, detail: desc, from: m.From, at: a.now(),
		card: channel.ApprovalCard{N: n, Title: title, Detail: body, From: m.From}}
	a.approvals[n] = ap
	a.publish(events.Event{T: "approval", SID: s.sid, N: n, ID: m.ID, Name: m.Name, Text: desc, Data: map[string]any{"from": m.From}})
	text := fmt.Sprintf("⚠️ 需要审批 #%d\n%s\n发起人：%s\n管理员回复 /approve %d 批准，/deny %d 拒绝", n, desc, m.From, n, n)
	a.publish(events.Event{T: "say", SID: s.sid, Text: text})
	for _, o := range s.run.outs {
		ch := a.chans[o.ep.ch]
		ap2, ok := ch.(channel.Approver)
		if !ok {
			a.deliver(o.ep, text, "")
			continue
		}
		ep := o.ep
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
			ref, err := ap2.SendApproval(ctx, ep.chat, ap.card)
			cancel()
			a.mu.Lock()
			defer a.mu.Unlock()
			if err != nil {
				a.log.Warn("发送审批卡片失败，改发文字", "chat", ep.key(), "err", err)
				a.deliver(ep, text, "")
				return
			}
			ap.refs = append(ap.refs, approvalRef{ep: ep, ref: ref})
			if ap.result != "" { // 发出去之前已经处理了
				a.updateApprovalCard(ap, approvalRef{ep: ep, ref: ref})
			}
		}()
	}
}

// autoApprove：放行模式下内核要审批的调用，网关当场批准，不发审批消息，只在网页上记一笔。调用方持有锁。
func (a *App) autoApprove(s *sess, m *proto.Msg) {
	title, _ := callCard(m.Name, m.Args)
	if !a.hub.Send(s.run.node, proto.Msg{T: proto.TApprove, SID: s.sid, ID: m.ID, OK: proto.Bool(true), From: "auto"}) {
		return
	}
	a.log.Info("放行模式：自动批准", "sid", s.sid, "tool", m.Name)
	a.publish(events.Event{T: "say", SID: s.sid, Text: "✅ 放行模式，自动批准：" + title})
}

// resolveApproval 记下处理结果，把发出去的审批消息都改成结果（去掉按钮）。调用方持有锁。
func (a *App) resolveApproval(ap *approval, result string) {
	if ap.result != "" {
		return
	}
	ap.result = result
	for _, r := range ap.refs {
		a.updateApprovalCard(ap, r)
	}
}

func (a *App) updateApprovalCard(ap *approval, r approvalRef) {
	ch, ok := a.chans[r.ep.ch].(channel.Approver)
	if !ok {
		return
	}
	card, result := ap.card, ap.result
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()
		if err := ch.ResolveApproval(ctx, r.ep.chat, r.ref, card, result); err != nil {
			a.log.Warn("更新审批卡片失败", "chat", r.ep.key(), "err", err)
		}
	}()
}

// dropApproval 审批不再需要了（工具有了结果、这一轮结束了）。调用方持有锁。
func (a *App) dropApproval(n int, why string) {
	ap := a.approvals[n]
	if ap == nil {
		return
	}
	delete(a.approvals, n)
	a.resolveApproval(ap, why)
	a.publish(events.Event{T: "approval_done", SID: ap.sid, N: n})
}

// decided 是审批结果的说明：谁批的（who 是身份或者名字）
func (a *App) decided(ok bool, who string) string {
	verb := "❌ 已拒绝"
	if ok {
		verb = "✅ 已批准"
	}
	switch {
	case strings.HasPrefix(who, "web:"):
		who = "网页"
	case strings.HasPrefix(who, "cli:"):
		who = "终端"
	}
	return verb + " · " + who
}

// canApproveFrom：审批单属于它自己的会话；定时任务的审批单属于 cron 会话，但发到任务的投递窗口，在那里也能批
func (a *App) canApproveFrom(c in, ap *approval) bool {
	s := a.sessions[ap.sid]
	if s == nil || s.run == nil {
		return false
	}
	if ap.sid == c.s.sid {
		return true
	}
	if s.run.scheduled != nil {
		for _, o := range s.run.outs {
			if o.ep == c.ep {
				return true
			}
		}
	}
	return false
}
