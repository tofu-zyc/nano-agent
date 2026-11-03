package app

import (
	"path/filepath"

	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/ticket"
)

// 工单（设计 §16.1）：团队里名单上的人点名 agent 派的一件活。记录在 data/runtime/tickets.jsonl。

// openTickets 打开工单记录。打不开就报到健康条目 tickets，Ticket 工具返回错误，其他一切照旧。
func (a *App) openTickets() {
	path := filepath.Join(a.store.Dir(), "runtime", "tickets.jsonl")
	s, err := ticket.Open(path)
	if err != nil {
		a.ticketsErr = err
		a.log.Error("工单记录打不开，保留现场", "err", err)
		a.hTicket.Set(health.Error, "工单记录打不开："+err.Error())
		return
	}
	s.Now = a.now
	a.tickets = s
}

// ticketHealth 记下工单记录写不进去（健康条目变 degraded）；写成功时恢复。
func (a *App) ticketHealth(err error) {
	if a.hTicket == nil {
		return
	}
	if err != nil {
		a.hTicket.Set(health.Degraded, "工单记录写不进去："+err.Error())
		return
	}
	a.hTicket.Set(health.OK, "")
}
