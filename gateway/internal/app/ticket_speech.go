package app

import (
	"errors"
	"fmt"
	"strings"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/ticket"
)

// CancelTicket 是网页上叫停一张工单（登录的就是管理员）。
func (a *App) CancelTicket(id, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tickets == nil {
		return errors.New("工单记录现在不可用")
	}
	t, ok := a.tickets.Get(strings.TrimSpace(id))
	if !ok {
		return errors.New("没有这张工单")
	}
	return a.cancelTicketLocked(t, reason)
}

// ticketPush 把问题或结果发回原消息所在的对话，作为对原消息的回复（设计 §16.1 的说话纪律）。
// 原对话里只会有三样：建工单那一轮 agent 自己的确认、要人回答的问题、最终结果；进展、工具输出、分支里的回复都不发。
// state 是 ready、blocked、failed。和 attention 一样先在承诺记录里写 notify 再入队，去重，每个工单每小时最多 3 条。
// 5a 里还没有来源（要等 5b 的分支和验收）：这里先把规则做好，测试直接调用。调用方持有锁。
func (a *App) ticketPush(id, state, text, question string) {
	if a.tickets == nil {
		return
	}
	t, ok := a.tickets.Get(id)
	if !ok || ticket.IsEnded(t.Status) && state != "ready" {
		return
	}
	if !a.notifyGate(commit.KindTicket, id, t.Agent, state, text+"\x00"+question) {
		return
	}
	body := ticketBody(t, state, text, question)
	src := t.Auth.Source
	a.publish(events.Event{T: "say", SID: t.Auth.Scene, Text: body})
	a.deliver(endpoint{ch: src.Channel, chat: src.Chat}, body, src.MsgID)
}

func ticketBody(t ticket.Ticket, state, text, question string) string {
	label := map[string]string{"ready": "就绪", "blocked": "受阻", "failed": "失败"}[state]
	if question != "" {
		label = "需要你回答"
	}
	s := fmt.Sprintf("[工单 %s「%s」· %s] %s", t.ID, t.Title, label, text)
	if question != "" {
		s += "\n问：" + question
	}
	return s
}
