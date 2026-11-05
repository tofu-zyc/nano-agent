package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/ticket"
)

// 5d-3：工单里的 Job（设计 §16.4）——Report ready 要等分支的 Job 结束、一轮结束时还有在跑的 Job 不算没汇报、
// Job 结束叫醒分支（只叫一次，重启后不重复）、叫停时停掉分支开的 Job。

// branchJob 登记一个分支开的后台 Job（状态是 status），返回它的回执；之后用 finishBranchJob 让它结束。
func (g *gitEnv) branchJob(t *testing.T, sid, id, status string) proto.JobSnapshot {
	t.Helper()
	origin := proto.LeaseSpec{SID: sid, RunID: "branch-run", TurnID: 1, CallID: "call-" + id, Agent: "default", CreatedBy: "ticket:tk-1", Name: "Shell"}
	q := proto.JobSnapshot{Version: 1, JobID: "j_boot-" + id, NodeEpoch: "boot", OwnerAgent: "default", CreatedBy: "ticket:tk-1",
		OriginSID: sid, OriginRunID: origin.RunID, OriginCallID: origin.CallID, Origin: origin,
		Status: status, Execution: "background", Background: true, Revision: 1, CreatedAt: 100, TimeoutSeconds: 1800}
	g.a.mu.Lock()
	n := g.a.nodes["test"]
	n.info.NodeEpoch = "boot"
	n.info.Features = map[string]int{"hook_check": 1, "job_manager": 1, "job_stream": 1, "machine_leases": 1, "lease_detach": 1}
	g.a.mu.Unlock()
	j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
	if err := g.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &j}); err != nil {
		t.Fatal(err)
	}
	return q
}

// finishBranchJob 让 Job 结束。viaGateway：走和真内核一样的路径（terminal 回执发给网关，网关记下并处理；确认由调用方收）；
// 否则只改记录（终态和事件一起提交），不经过网关的处理，模拟“终态已经记下，但网关在叫醒之前崩了”。
func (g *gitEnv) finishBranchJob(t *testing.T, q proto.JobSnapshot, status string, viaGateway bool) proto.JobSnapshot {
	t.Helper()
	q.Status = status
	q.Revision++
	q.EndedAt = 200
	q.LastEventSeq = 1
	q.Result = &proto.JobResult{OK: status == "succeeded", Content: "done"}
	if viaGateway {
		g.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: q.JobID, Kind: "terminal", EventSeq: 1, Job: &q})
		return q
	}
	j := eventlog.Job{SourceID: "test", Creator: q.CreatedBy, Receipt: q}
	if err := g.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Job: &j, Event: jobCompletedEvent(j, 200)}); err != nil {
		t.Fatal(err)
	}
	return q
}

// finishedBranchJob 登记一个已经结束的 Job。
func (g *gitEnv) finishedBranchJob(t *testing.T, sid, id string) {
	t.Helper()
	q := g.branchJob(t, sid, id, "running")
	g.finishBranchJob(t, q, "succeeded", false)
}

func (g *gitEnv) endBranchRun(t *testing.T, sid string) {
	t.Helper()
	g.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, g.env, sid)
}

// historyOf 是会话历史里所有消息的正文。
func (g *gitEnv) historyOf(sid string) string {
	g.a.mu.Lock()
	defer g.a.mu.Unlock()
	var b strings.Builder
	for _, m := range g.a.store.History(sid) {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

// Report ready：分支还有没结束的 Job（后台 Shell、External 都算）时被拒，写明是哪几个；都结束了才收。
func TestTicketReportReadyRefusedWhileBranchJobsLive(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	j1 := g.branchJob(t, in.SID, "1", "running")
	j2 := g.branchJob(t, in.SID, "2", "queued")
	g.finishedBranchJob(t, in.SID, "0") // 结束了的不算
	report := func() (string, error) {
		g.a.mu.Lock()
		defer g.a.mu.Unlock()
		s := g.a.sessions[in.SID]
		s.run.toolEvidence = map[string]autonomy.ToolEvidence{"w1": {ID: "w1", Name: "Write", OK: true, Verified: true}}
		return g.a.reportCall(s, []byte(`{"status":"ready","text":"改好了","evidence":["w1"]}`))
	}
	_, err := report()
	if err == nil || !strings.Contains(err.Error(), j1.JobID+"（running）") || !strings.Contains(err.Error(), j2.JobID+"（queued）") ||
		strings.Contains(err.Error(), "j_boot-0") || !strings.Contains(err.Error(), "2 个没结束的后台任务") {
		t.Fatalf("ready 被拒，写明是哪几个：%v", err)
	}
	if tk := g.tk("tk-1"); tk.WantCheck || tk.Status != ticket.StatusWorking {
		t.Fatalf("没记下要验收：%+v", tk)
	}
	// 别的汇报（blocked、progress）不受影响
	g.a.mu.Lock()
	out, err2 := g.a.reportCall(g.a.sessions[in.SID], []byte(`{"status":"progress","text":"在等 agent"}`))
	g.a.mu.Unlock()
	if err2 != nil {
		t.Fatalf("progress 照常：%q %v", out, err2)
	}
	g.finishBranchJob(t, j1, "succeeded", true)
	g.k.expect(proto.TEventsAck)
	if _, err := report(); err == nil || !strings.Contains(err.Error(), j2.JobID) {
		t.Fatalf("还剩一个：%v", err)
	}
	g.finishBranchJob(t, j2, "failed", true)
	g.k.expect(proto.TEventsAck)
	if out, err := report(); err != nil || !strings.Contains(out, "ready") {
		t.Fatalf("都结束了才收：%q %v", out, err)
	}
	if tk := g.tk("tk-1"); !tk.WantCheck {
		t.Fatalf("记下要验收：%+v", tk)
	}
}

// 一轮结束时还有在跑的 Job：不算“没汇报”（工单不受阻、不验收），记下在等哪几个；Job 结束叫醒分支一次，
// 把这段时间结束的几个合成一条；不算续上，也不算被重启打断。
func TestTicketWakesTheBranchOnceWhenTheLastJobEnds(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	j1 := g.branchJob(t, in.SID, "1", "running")
	j2 := g.branchJob(t, in.SID, "2", "running")
	g.endBranchRun(t, in.SID) // 这一轮结束了，没汇报，但还有两个 Job 在跑
	if tk := g.tk("tk-1"); tk.Status != ticket.StatusWorking || len(tk.WaitJobs) != 2 {
		t.Fatalf("不算没汇报，记下在等哪几个：%+v", tk)
	}
	g.k.none()
	// 第一个结束：还有一个没结束，不叫
	g.finishBranchJob(t, j1, "succeeded", true)
	g.k.expect(proto.TEventsAck)
	g.k.none()
	if tk := g.tk("tk-1"); len(tk.WaitJobs) != 2 || len(tk.Woken) != 0 {
		t.Fatalf("最后一个结束才叫：%+v", tk)
	}
	// 最后一个结束：叫醒一次，两个合成一条
	end2 := g.finishBranchJob(t, j2, "failed", true)
	wake := g.k.expect(proto.TInput)
	if wake.SID != in.SID || wake.From != "ticket:tk-1" {
		t.Fatalf("叫醒的是分支，身份还是工单：%+v", wake)
	}
	g.k.expect(proto.TEventsAck)
	seen := g.historyOf(in.SID)
	if !strings.Contains(seen, "[后台任务] "+j1.JobID+"（succeeded）、"+j2.JobID+"（failed） 结束了") || strings.Count(seen, "[后台任务]") != 1 {
		t.Fatalf("两个合成一条：%s", seen)
	}
	tk := g.tk("tk-1")
	if len(tk.WaitJobs) != 0 || len(tk.Woken) != 2 || tk.Renewals != 0 || tk.Resumes != 0 || tk.Status != ticket.StatusWorking {
		t.Fatalf("先记叫过的；不算续上，也不算被重启打断：%+v", tk)
	}
	// 同一个终态重发、重启后再看一眼：不再叫
	g.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", JobID: j2.JobID, Kind: "terminal", EventSeq: 1, Job: &end2})
	g.k.expect(proto.TEventsAck)
	g.a.mu.Lock()
	g.a.ticketJobsSettled(in.SID)
	g.a.mu.Unlock()
	g.endBranchRun(t, in.SID)
	g.a.resumeTickets()
	if strings.Count(g.historyOf(in.SID), "[后台任务]") != 1 {
		t.Fatalf("每个 Job 只叫一次：%s", g.historyOf(in.SID))
	}
}

// 分支正在跑时结束的 Job 不另外叫：这一轮结束时照常按有没有汇报处理。
func TestTicketJobEndingDuringARunDoesNotWake(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	j1 := g.branchJob(t, in.SID, "1", "running")
	g.finishBranchJob(t, j1, "succeeded", true) // 这一轮还在跑
	g.k.expect(proto.TEventsAck)
	g.k.none()
	if tk := g.tk("tk-1"); len(tk.WaitJobs) != 0 || len(tk.Woken) != 0 {
		t.Fatalf("没有叫：%+v", tk)
	}
	// 这一轮结束：Job 都结束了，照常按有没有汇报处理（没汇报、没改动：受阻）
	g.endBranchRun(t, in.SID)
	eventually(t, "照常受阻", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := g.tk("tk-1"); tk.BlockedBy != "silent" {
		t.Fatalf("没汇报、没改动：%+v", tk)
	}
}

// 重启：分支在等的 Job 还在跑就继续等（不算被重启打断）；终态已经记下、叫醒之前崩了，重启后叫醒一次，不漏。
func TestTicketWaitingForJobsSurvivesRestart(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	j1 := g.branchJob(t, in.SID, "1", "running")
	g.endBranchRun(t, in.SID)
	if tk := g.tk("tk-1"); len(tk.WaitJobs) != 1 {
		t.Fatalf("在等：%+v", tk)
	}
	g.a.resumeWorking("tk-1") // 重启后：Job 还在跑，继续等
	g.k.none()
	if tk := g.tk("tk-1"); tk.Resumes != 0 || tk.Status != ticket.StatusWorking || len(tk.WaitJobs) != 1 {
		t.Fatalf("继续等，不算被打断：%+v", tk)
	}
	g.finishBranchJob(t, j1, "succeeded", false) // 终态记下了，网关在叫醒之前崩了
	g.k.none()
	g.a.resumeWorking("tk-1")
	wake := g.k.expect(proto.TInput)
	if wake.SID != in.SID {
		t.Fatalf("重启后叫醒：%+v", wake)
	}
	if tk := g.tk("tk-1"); tk.Resumes != 0 || len(tk.WaitJobs) != 0 || len(tk.Woken) != 1 {
		t.Fatalf("叫醒不算被打断，叫过的记下：%+v", tk)
	}
	if !strings.Contains(g.historyOf(in.SID), "[后台任务] "+j1.JobID+"（succeeded）") {
		t.Fatalf("叫醒的话：%s", g.historyOf(in.SID))
	}
	// 再重启一次：不再叫（这一轮自己结束后 working 的工单走原来的“接着做”，那不是 Job 的叫醒）
	g.a.resumeWorking("tk-1")
	g.k.none()
	if strings.Count(g.historyOf(in.SID), "[后台任务]") != 1 {
		t.Fatalf("只叫一次：%s", g.historyOf(in.SID))
	}
}

// 叫停时停掉分支开的、还没结束的 Job（结束了的不动）。
func TestTicketCancelStopsTheBranchsLiveJobs(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	live := g.branchJob(t, in.SID, "1", "running")
	g.finishedBranchJob(t, in.SID, "0")
	mention(g.env, "@机器人 算了", "g101", "u2", "李四")
	g.k.expect(proto.TInput)
	if out, err := callTicket(g.env, sid, `{"action":"cancel","id":"tk-1","text":"不要了"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	var control proto.Msg
	for { // 先收到的可能是取消这一轮之类的别的消息
		m, ok := g.k.recv(3 * time.Second)
		if !ok {
			t.Fatal("等不到停 Job 的控制消息")
		}
		if m.T == proto.TJobControl {
			control = m
			break
		}
	}
	if control.Action != "cancel" || control.JobID != live.JobID {
		t.Fatalf("停没结束的那个：%+v", control)
	}
	g.k.send(proto.Msg{T: proto.TJobState, NodeEpoch: "boot", RequestID: control.RequestID, JobID: live.JobID, OK: proto.Bool(true)})
	g.finishBranchJob(t, live, "cancelled", true)
	finishGroupRun(t, g.env, sid)
	eventually(t, "worktree 清掉", func() bool { _, gone := statErr(g.r.worktree("tk-1")); return gone })
	// 结束了的那个没有再收到控制消息
	for {
		m, ok := g.k.recv(300 * time.Millisecond)
		if !ok {
			break
		}
		if m.T == proto.TJobControl {
			t.Fatalf("结束了的不用停：%+v", m)
		}
	}
	if tk := g.tk("tk-1"); tk.Status != ticket.StatusStopped {
		t.Fatalf("叫停：%+v", tk)
	}
}

// statErr 是路径已经不在了。
func statErr(path string) (struct{}, bool) {
	_, err := os.Stat(path)
	return struct{}{}, os.IsNotExist(err)
}

// 分支在等 Job 时来了名单里的人的补充，补充开了新的一轮；这一轮里 Job 结束：不另外叫醒（这一轮自己看得到），
// 这一轮结束时照常按有没有汇报处理（等的记录清掉）。
func TestTicketJobsEndingWhileANoteRunsDoNotWake(t *testing.T) {
	g := newGitEnv(t)
	sid := g.teamGroupOnRepo(t, "true")
	in := g.startWork(t, sid)
	j1 := g.branchJob(t, in.SID, "1", "running")
	g.endBranchRun(t, in.SID)
	if tk := g.tk("tk-1"); len(tk.WaitJobs) != 1 {
		t.Fatalf("在等：%+v", tk)
	}
	mention(g.env, "@机器人 补充一下", "g102", "u2", "李四")
	g.k.expect(proto.TInput)
	if out, err := callTicket(g.env, sid, `{"action":"note","id":"tk-1","text":"注意手机端"}`); err != nil {
		t.Fatalf("%q %v", out, err)
	}
	finishGroupRun(t, g.env, sid)
	if note := g.k.expect(proto.TInput); note.SID != in.SID { // 补充送进分支，开了新的一轮
		t.Fatalf("补充送进分支：%+v", note)
	}
	g.finishBranchJob(t, j1, "succeeded", true) // 这一轮还在跑
	g.k.expect(proto.TEventsAck)
	g.k.none()
	if tk := g.tk("tk-1"); len(tk.Woken) != 0 || strings.Contains(g.historyOf(in.SID), "[后台任务]") {
		t.Fatalf("分支正在跑时结束的 Job 不另外叫：%+v", tk)
	}
	g.endBranchRun(t, in.SID)
	g.k.none() // 也没有排上第二轮
	eventually(t, "这一轮结束照常处理", func() bool { return g.tk("tk-1").Status == ticket.StatusBlocked })
	if tk := g.tk("tk-1"); len(tk.WaitJobs) != 0 || tk.BlockedBy != "silent" {
		t.Fatalf("等的记录清掉，没汇报、没改动：%+v", tk)
	}
}
