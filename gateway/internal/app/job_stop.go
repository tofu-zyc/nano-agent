package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// stop all cancels the current foreground run and Jobs actually created by
// this actor in this source session. Admin status never broadens "本人" scope.
func (a *App) cmdStopAllJobs(c in) {
	a.stop(c)
	if a.jobEvents == nil {
		a.answer(c, "可靠任务索引不可用，后台任务未确认停止。")
		return
	}
	key := jobPersistKey(c.who, c.s.sid)
	if a.jobStopGroups[key] {
		a.answer(c, "本人在此会话的任务取消仍在处理，请稍后查看 /jobs。")
		return
	}
	if len(a.jobStopGroups) >= 16 {
		a.answer(c, "批量取消请求过多；前台已请求停止，后台请用 /job <job_id> cancel。")
		return
	}
	var requests []JobRequest
	cursor := ""
	for {
		rows, next := a.jobEvents.Jobs(cursor, 100)
		for _, j := range rows {
			if j.Creator != c.who || j.Receipt.OriginSID != c.s.sid || finalJob(j.Receipt.Status) || !a.canAccessJob(c.who, c.s.sid, j) {
				continue
			}
			requests = append(requests, JobRequest{Action: "cancel", Node: j.SourceID, JobID: j.Receipt.JobID})
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(requests) == 0 {
		a.answer(c, "此会话没有本人发起、仍未结束的已登记任务。")
		return
	}
	a.jobStopGroups[key] = true
	ch, agent := a.chans[c.ep.ch], a.execAgentID(c.s.sid)
	a.answer(c, fmt.Sprintf("正在请求停止本人在此会话发起的 %d 个任务；最终状态请以 /jobs 为准。", len(requests)))
	// At most 16 groups, four workers each. Queries remain independent of the
	// run being stopped, and no network/disk wait holds App.mu.
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
		defer cancel()
		queue := make(chan JobRequest)
		var wg sync.WaitGroup
		var resultMu sync.Mutex
		accepted, failed := 0, 0
		var errors []string
		for range min(4, len(requests)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for req := range queue {
					out, err := a.JobAction(ctx, c.who, c.s.sid, req)
					resultMu.Lock()
					if err == nil && out.Accepted {
						accepted++
					} else {
						failed++
						if len(errors) < 3 {
							detail := "没有取得取消受理回执"
							if err != nil {
								detail = err.Error()
							}
							errors = append(errors, req.JobID+"："+detail)
						}
					}
					resultMu.Unlock()
				}
			}()
		}
		for _, req := range requests {
			queue <- req
		}
		close(queue)
		wg.Wait()
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.jobStopGroups, key)
		if !a.jobEndpointValid(ch, c.ep, agent, c.s.sid) {
			return
		}
		text := fmt.Sprintf("任务取消：%d 个已受理，%d 个未确认。受理不表示进程已退出，请用 /jobs 查看原任务的最终状态。", accepted, failed)
		if len(errors) > 0 {
			text += "\n" + strings.Join(errors, "\n")
		}
		a.jobReply(ch, c.ep, agent, c.s.sid, text)
	}()
}
