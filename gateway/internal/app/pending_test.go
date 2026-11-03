package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// 等我处理（设计 §15.3）：列表、“知道了”、回答、主人在别处处理过的算看过。

func ownerBranch(t *testing.T, e *env, reqID string) string {
	t.Helper()
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "来源会话的上下文"}); err != nil {
		t.Fatal(err)
	}
	br, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "查竞品", RequestID: reqID, By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	return br.BranchSID
}

func setBranch(t *testing.T, e *env, sid string, f func(*session.BranchMeta)) {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if err := e.a.store.SetBranch(sid, f); err != nil {
		t.Fatal(err)
	}
}

func appendRec(t *testing.T, e *env, r commit.CommitRec) commit.CommitRec {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	out, err := e.a.commits.Append(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pendingOf(e *env, kind, id string) *PendingView {
	for _, p := range e.a.Pending() {
		if p.Kind == kind && p.ID == id {
			return &p
		}
	}
	return nil
}

// 主人建的分支做完一轮、同一轮 Report ready：两条说的是同一件事，点一次“知道了”就都不再列。
func TestPendingSeenOnceCoversHostReady(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-covered")
	now := e.a.now().Unix()
	setBranch(t, e, sid, func(m *session.BranchMeta) {
		m.Status, m.Progress, m.ProgressAt = session.BranchCompleted, "做完了一轮", now
	})
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusReady, Text: "竞品有三家"})
	p := pendingOf(e, commit.KindBranch, sid)
	if p == nil || p.Phase != commit.Ready || p.Why != "竞品有三家" || p.Mark != fmt.Sprintf("r:%d", r.Seq) || p.Number != fmt.Sprint(r.Seq) {
		t.Fatalf("汇报的就绪：%+v", p)
	}
	if p.Covered != fmt.Sprintf("p:%d", now) {
		t.Fatalf("被盖住的宿主就绪要留下 Mark：%q", p.Covered)
	}
	if err := e.a.PendingSeen(p.Kind, p.ID, p.Mark, "web:admin"); err != nil {
		t.Fatal(err)
	}
	if p := pendingOf(e, commit.KindBranch, sid); p != nil {
		t.Fatalf("点一次就不再列：%+v", p)
	}
	e.a.mu.Lock()
	seenReport, seenHost := e.a.commits.Seen(commit.KindBranch, sid, fmt.Sprintf("r:%d", r.Seq)), e.a.commits.Seen(commit.KindBranch, sid, fmt.Sprintf("p:%d", now))
	e.a.mu.Unlock()
	if !seenReport || !seenHost {
		t.Fatalf("两个 Mark 都要记 seen：%v %v", seenReport, seenHost)
	}
	// 同一个 Mark 再点：已经不在了
	if err := e.a.PendingSeen(commit.KindBranch, sid, fmt.Sprintf("r:%d", r.Seq), "web:admin"); err == nil {
		t.Fatal("已经处理过的不能再点")
	}
}

// 受阻和就绪互相盖住时是两件事：知道了只记决定阶段的那一条，另一条再显出来。
func TestPendingSeenBlockedDoesNotCoverReady(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-blocked")
	now := e.a.now().Unix()
	setBranch(t, e, sid, func(m *session.BranchMeta) {
		m.Status, m.Progress, m.ProgressAt = session.BranchCompleted, "做完了一轮", now
	})
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "缺一个密钥"})
	p := pendingOf(e, commit.KindBranch, sid)
	if p == nil || p.Phase != commit.Blocked || p.Covered != "" {
		t.Fatalf("受阻不盖住就绪：%+v", p)
	}
	if err := e.a.PendingSeen(p.Kind, p.ID, p.Mark, "web:admin"); err != nil {
		t.Fatal(err)
	}
	p = pendingOf(e, commit.KindBranch, sid)
	if p == nil || p.Phase != commit.Ready || p.Mark != fmt.Sprintf("p:%d", now) || p.Mark == fmt.Sprintf("r:%d", r.Seq) {
		t.Fatalf("宿主的就绪是另一件事，要再显出来：%+v", p)
	}
}

func TestPendingListNewestFirstAndOnlyBlockedOrReady(t *testing.T) {
	e := setup(t)
	a := ownerBranch(t, e, "pending-a")
	b := ownerBranch(t, e, "pending-b")
	appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: a, Agent: "default", Status: commit.StatusProgress, Text: "只是进展"})
	e.a.mu.Lock()
	e.a.commits.Now = func() time.Time { return time.Unix(2000, 0) }
	e.a.mu.Unlock()
	appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: a, Agent: "default", Status: commit.StatusBlocked, Text: "先到的"})
	e.a.mu.Lock()
	e.a.commits.Now = func() time.Time { return time.Unix(3000, 0) }
	e.a.mu.Unlock()
	appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: b, Agent: "default", Status: commit.StatusReady, Text: "后到的"})
	var got []string
	for _, p := range e.a.Pending() {
		got = append(got, p.ID+":"+p.Phase)
	}
	if strings.Join(got, ",") != b+":ready,"+a+":blocked" {
		t.Fatalf("最新的在前，只列受阻和就绪：%v", got)
	}
}

func TestPendingAnswerBranchResumesIt(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-answer")
	setBranch(t, e, sid, func(m *session.BranchMeta) { m.Status = session.BranchCompleted })
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "两个方案", Question: "用哪个？"})
	mark := fmt.Sprintf("r:%d", r.Seq)
	for _, bad := range []string{"", "   ", strings.Repeat("字", maxAnswerRunes+1)} {
		if err := e.a.PendingAnswer(commit.KindBranch, sid, mark, bad, "web:admin"); err == nil {
			t.Fatalf("空的或超过 %d 字的回答要拒绝：%d", maxAnswerRunes, len(bad))
		}
	}
	if err := e.a.PendingAnswer(commit.KindBranch, sid, "r:999", "第一个", "web:admin"); err == nil {
		t.Fatal("Mark 对不上要拒绝")
	}
	if err := e.a.PendingAnswer(commit.KindBranch, sid, mark, "第一个", "web:admin"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	ans, ok := e.a.commits.Answer(commit.KindBranch, sid, mark)
	queued := e.a.sessions[sid]
	var text string
	if queued != nil {
		for _, q := range queued.queue {
			text += q.m.Text
		}
		if queued.run != nil {
			text += "|running"
		}
	}
	e.a.mu.Unlock()
	if !ok || ans.Answer != "第一个" || ans.By != "web:admin" {
		t.Fatalf("先写进承诺记录：%+v", ans)
	}
	if queued == nil || (text == "" && queued.run == nil) {
		t.Fatal("回答要送进分支")
	}
	if p := pendingOf(e, commit.KindBranch, sid); p != nil && p.Question != "" {
		t.Fatalf("回答过的问题不再算：%+v", p)
	}
}

func TestPendingAnswerRefusedForArchivedBranchBeforeAnySideEffect(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-archived")
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "x", Question: "用哪个？"})
	setBranch(t, e, sid, func(m *session.BranchMeta) { m.Status = session.BranchCompleted })
	// 归档之前它在列表里
	p := pendingOf(e, commit.KindBranch, sid)
	if p == nil {
		t.Fatal("应该在列表里")
	}
	if _, err := e.a.SessionBranchAction("archive", sid, "", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	// 归档后，之前的汇报都算看过：不再列
	if p := pendingOf(e, commit.KindBranch, sid); p != nil {
		t.Fatalf("归档后不再列：%+v", p)
	}
	if err := e.a.PendingAnswer(commit.KindBranch, sid, fmt.Sprintf("r:%d", r.Seq), "第一个", "web:admin"); err == nil {
		t.Fatal("已归档的分支拒绝回答")
	}
	e.a.mu.Lock()
	_, wrote := e.a.commits.Answer(commit.KindBranch, sid, fmt.Sprintf("r:%d", r.Seq))
	e.a.mu.Unlock()
	if wrote {
		t.Fatal("拒绝要发生在写记录之前")
	}
}

func TestPendingAnswerWakesAgendaAndMovesItToNow(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	now := e.a.now().In(e.a.loc)
	e.a.mu.Lock()
	item, err := e.a.mind("default").AddAgenda(autonomy.AgendaItem{Title: "整理文档", Kind: autonomy.KindCommitment, At: now.Add(3 * time.Hour).Unix()}, now)
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindAgenda, ID: item.ID, Agent: "default", Status: commit.StatusBlocked, Text: "两份重复", Question: "保留哪一份？"})
	if err := e.a.PendingAnswer(commit.KindAgenda, item.ID, fmt.Sprintf("r:%d", r.Seq), "留新的", "web:admin"); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	st := e.a.mind("default")
	got, _ := st.AgendaByID(item.ID)
	var ev *autonomy.Event
	for i := range st.Events {
		if st.Events[i].AgendaID == item.ID {
			ev = &st.Events[i]
		}
	}
	e.a.mu.Unlock()
	if ev == nil || !strings.Contains(ev.Text, "留新的") || !strings.Contains(ev.Text, "保留哪一份？") {
		t.Fatalf("心智状态里要有带回答的事件：%+v", ev)
	}
	if got.At > now.Unix()+5 {
		t.Fatalf("还没开始的议程项时间改成现在：%d vs %d", got.At, now.Unix())
	}
}

// 主人在别处自己动手也算处理过。
func TestHandledByOwner(t *testing.T) {
	e := setup(t)
	appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindSchedule, ID: "other", Agent: "default", Status: commit.StatusProgress, Text: "占一个序号"})
	r1 := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindSchedule, ID: "t1", Agent: "default", Status: commit.StatusBlocked, Text: "失败了"})
	// 下一轮开始时的序号是 r1：之前的汇报在下一次成功后算看过
	e.a.mu.Lock()
	e.a.handledByOwner(commit.KindSchedule, "t1", r1.Seq-1) // 汇报比开始时晚：不算
	still := e.a.commits.Seen(commit.KindSchedule, "t1", fmt.Sprintf("r:%d", r1.Seq))
	e.a.handledByOwner(commit.KindSchedule, "t1", r1.Seq)
	now := e.a.commits.Seen(commit.KindSchedule, "t1", fmt.Sprintf("r:%d", r1.Seq))
	e.a.mu.Unlock()
	if still || !now {
		t.Fatalf("upTo 之内才算：%v %v", still, now)
	}
}

func TestPendingChatCommands(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-chat")
	setBranch(t, e, sid, func(m *session.BranchMeta) { m.Status = session.BranchCompleted })
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "两个方案", Question: "用哪个？"})
	e.say("/pending")
	e.ch.wait(t, fmt.Sprintf("#%d", r.Seq))
	e.say("/answer")
	e.ch.wait(t, "用法：/answer")
	e.say(fmt.Sprintf("/answer %d 第一个", r.Seq))
	e.ch.wait(t, "已经把回答交给")
	e.say("/pending")
	e.ch.wait(t, "没有等你处理的事")
	// /seen 对就绪的事
	sid2 := ownerBranch(t, e, "pending-chat-2")
	setBranch(t, e, sid2, func(m *session.BranchMeta) { m.Status = session.BranchCompleted })
	r2 := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid2, Agent: "default", Status: commit.StatusReady, Text: "做好了"})
	e.say(fmt.Sprintf("/seen %d", r2.Seq))
	e.ch.wait(t, "知道了")
	e.say("/seen 12345")
	e.ch.wait(t, "没有这个编号")
}

// 问题在这一轮还在跑的时候就问出来了：照通知回 /answer 要答得进去，回答排进下一个安全边界。
func TestPendingAnswerWhileTheRunIsStillGoing(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "pending-running")
	if _, err := e.a.SessionBranchAction("continue", sid, "查一下", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	if in.SID != sid {
		t.Fatalf("在分支里跑：%s", in.SID)
	}
	e.a.mu.Lock()
	s := e.a.sessions[sid]
	_, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "blocked", "text": "两个方案", "question": "用哪个？"}))
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	p := pendingOf(e, commit.KindBranch, sid)
	if p == nil || p.Question != "用哪个？" || p.Phase != commit.Running {
		t.Fatalf("这一轮还在跑，问题也要列进来：%+v", p)
	}
	got := e.ch.wait(t, "需要你回答")
	if !strings.Contains(got, "/answer "+p.Number+" ") {
		t.Fatalf("通知里的编号要能用：%q / %s", got, p.Number)
	}
	e.say("/answer " + p.Number + " 第一个")
	e.ch.wait(t, "已经把回答交给")
	e.a.mu.Lock()
	var queued string
	for _, q := range e.a.sessions[sid].queue {
		queued += q.m.Text
	}
	e.a.mu.Unlock()
	if !strings.Contains(queued, "[主人的回答]") || !strings.Contains(queued, "第一个") {
		t.Fatalf("回答排在队列里：%q", queued)
	}
	if p := pendingOf(e, commit.KindBranch, sid); p != nil && p.Question != "" {
		t.Fatalf("回答过的问题不再列：%+v", p)
	}
	// 这一轮结束后，下一轮在分支里带着回答跑
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	in2 := e.k.expect(proto.TInput)
	if in2.SID != sid {
		t.Fatalf("下一轮在分支里：%s", in2.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	var sentToModel string
	for _, m := range e.llm.lastReq().Messages {
		sentToModel += m.Content
	}
	if !strings.Contains(sentToModel, "[主人的回答]") || !strings.Contains(sentToModel, "第一个") {
		t.Fatalf("下一轮的模型请求带着回答：%q", sentToModel)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

// 结束了的承诺上的问题不列（比如分支已经归档）。
func TestPendingQuestionOnEndedBranchIsNotListed(t *testing.T) {
	e := setup(t)
	sid := ownerBranch(t, e, "pending-ended")
	appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "x", Question: "用哪个？"})
	setBranch(t, e, sid, func(m *session.BranchMeta) { m.Status = session.BranchArchived; m.Note = "归档了" })
	if p := pendingOf(e, commit.KindBranch, sid); p != nil {
		t.Fatalf("结束了的不列：%+v", p)
	}
}

// 定时任务、Hook 的汇报不能带问题，没有人接着问：回答它们一律拒绝。
func TestPendingAnswerRefusedForScheduleAndHook(t *testing.T) {
	e := setup(t)
	for _, kind := range []string{commit.KindSchedule, commit.KindHook} {
		r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: kind, ID: "x1", Agent: "default", Status: commit.StatusBlocked, Text: "x", Question: "?"})
		if err := e.a.PendingAnswer(kind, "x1", fmt.Sprintf("r:%d", r.Seq), "好", "web:admin"); err == nil {
			t.Fatalf("%s 不接受回答", kind)
		}
	}
}

// 宿主记下出错时，通知里的编号取这件事在“等我处理”里的编号：这一轮里还有没处理的汇报受阻，决定阶段的是汇报那条。
func TestAttentionNumberFollowsTheDecidingFact(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-number")
	r := appendRec(t, e, commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindBranch, ID: sid, Agent: "default", Status: commit.StatusBlocked, Text: "缺密钥"})
	drainSent(e)
	e.a.mu.Lock()
	s := e.a.sessOf(sid)
	e.a.branchDone(s, &run{outcome: "error", final: "内核掉线了", asked: "查一下"})
	e.a.mu.Unlock()
	got := e.ch.wait(t, "内核掉线了")
	want := fmt.Sprintf("/seen %d", r.Seq)
	if !strings.Contains(got, want) {
		t.Fatalf("编号要是决定阶段的汇报那条 %q：%q", want, got)
	}
	e.say(want)
	e.ch.wait(t, "知道了")
}
