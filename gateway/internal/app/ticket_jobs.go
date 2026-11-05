package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/ticket"
)

// 工单里的后台任务（设计 §16.4）：分支开的后台 Shell 和 External 都是 Job。
//   - 分支还有没结束的 Job 时，Report ready 被拒，写明是哪几个；
//   - 一轮结束时还有在跑的 Job，不算“没汇报”：记下在等哪几个（Ticket.WaitJobs），等它们结束叫醒分支；
//   - 叫醒只在“一轮结束时还有没结束的 Job、分支在等它们”时有：最后一个结束时叫一次，把这段时间结束的几个合成一条；
//     分支正在跑时结束的 Job 不另外叫，这一轮结束时照常按有没有汇报处理；
//   - 每个 Job 只叫一次：先把叫的 Job 记进 Ticket.Woken（并清掉 WaitJobs）再排这一轮，重启后不重复；
//     记了却没排成的（崩在中间），重启后的“接着做”会接上，不漏；
//   - 叫停、放弃、结束时，停掉分支开的、还没结束的 Job。

// liveJobsOf 是源会话 sid 开的、还没结束的 Job。调用方持有锁。
func (a *App) liveJobsOf(sid string) []eventlog.JobRecord {
	if a.jobEvents == nil {
		return nil
	}
	var out []eventlog.JobRecord
	for _, j := range a.jobEvents.JobsBySession(sid) {
		if !finalJob(j.Receipt.Status) {
			out = append(out, j)
		}
	}
	return out
}

func jobIDs(jobs []eventlog.JobRecord) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.Receipt.JobID
	}
	return ids
}

// liveJobsText 是 Report ready 被拒时列出的没结束的 Job。
func liveJobsText(jobs []eventlog.JobRecord) string {
	parts := make([]string, len(jobs))
	for i, j := range jobs {
		parts[i] = fmt.Sprintf("%s（%s）", j.Receipt.JobID, j.Receipt.Status)
	}
	return strings.Join(parts, "、")
}

// ticketWaitJobs 记下分支这一轮结束时还在跑的 Job：不算“没汇报”，它们都结束了叫醒分支。调用方持有锁。
func (a *App) ticketWaitJobs(t ticket.Ticket, live []eventlog.JobRecord) {
	ids := jobIDs(live)
	if slices.Equal(t.WaitJobs, ids) {
		return
	}
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) { t.WaitJobs = ids }); err != nil {
		a.ticketHealth(err)
	}
}

// ticketJobsSettled 是分支开的 Job 结束了、或者重启后看一眼：分支在等 Job（WaitJobs）、空闲、工单在 working、
// 分支开的 Job 都结束了，就叫醒分支一次，把结束的几个合成一条。分支正在跑时不叫：这一轮结束时照常处理。调用方持有锁。
func (a *App) ticketJobsSettled(sid string) {
	t, ok := a.ticketOfSession(sid)
	if !ok || t.Status != ticket.StatusWorking || len(t.WaitJobs) == 0 {
		return
	}
	if s := a.sessions[sid]; s != nil && (s.run != nil || len(s.queue) > 0) {
		return
	}
	if len(a.liveJobsOf(sid)) > 0 {
		return // 还有没结束的：最后一个结束时才叫
	}
	status := map[string]string{}
	for _, j := range a.jobEvents.JobsBySession(sid) {
		status[j.Receipt.JobID] = j.Receipt.Status
	}
	var fresh, parts []string
	for _, id := range t.WaitJobs {
		if slices.Contains(t.Woken, id) {
			continue
		}
		fresh = append(fresh, id)
		st := status[id]
		if st == "" {
			st = "记录已经不在了"
		}
		parts = append(parts, fmt.Sprintf("%s（%s）", id, st))
	}
	// 先记再说：叫过的记下来、不再等，写成功才排这一轮
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
		t.WaitJobs = nil
		t.Woken = append(t.Woken, fresh...)
		if n := len(t.Woken) - ticket.MaxWoken; n > 0 {
			t.Woken = slices.Clone(t.Woken[n:])
		}
	}); err != nil {
		a.ticketHealth(err)
		return
	}
	if len(fresh) == 0 { // 等的都叫过了（不该有）：当作这一轮刚结束，照常看有没有改动
		go a.settleSilentRun(t.ID)
		return
	}
	msg := "[后台任务] " + strings.Join(parts, "、") + " 结束了。用 Job 看一下输出和结果，接着做；做完用 Report ready 汇报。"
	// 算这一轮的延续：不算续上（Renewals），也不算被重启打断（Resumes）
	if err := a.startBranchRunLocked(t.RunSID, msg, ticketIdentityPrefix+t.ID); err != nil {
		a.ticketBlock(t.ID, "error", "后台任务都结束了，但是没法让分支接着做："+err.Error(), "", true)
	}
}

// stopTicketJobs 停掉分支开的、还没结束的 Job（工单叫停、放弃、结束时），再等它们结束（最多 10 秒），然后才删 worktree：
// 外部 agent 还在往那里写。不持有锁。
func (a *App) stopTicketJobs(t ticket.Ticket, live []eventlog.JobRecord) {
	if len(live) == 0 || t.RunSID == "" {
		return
	}
	for _, j := range live {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		_, err := a.JobAction(ctx, ticketIdentityPrefix+t.ID, t.RunSID, JobRequest{Action: "cancel", Node: j.SourceID, JobID: j.Receipt.JobID})
		cancel()
		if err != nil {
			a.log.Warn("停工单分支的后台任务失败", "ticket", t.ID, "job", j.Receipt.JobID, "err", err)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && a.ctx.Err() == nil; time.Sleep(100 * time.Millisecond) {
		a.mu.Lock()
		n := len(a.liveJobsOf(t.RunSID))
		a.mu.Unlock()
		if n == 0 {
			return
		}
	}
}
