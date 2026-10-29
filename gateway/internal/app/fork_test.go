package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// 空闲时 Fork：直接 ready，快照保存了分叉点的上下文，分支带着它继续。
func TestForkIdleCreatesReadyBranch(t *testing.T) {
	e := setup(t)
	e.say("第一问", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：第一问")

	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "自己研究", RequestID: "req-1", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != session.BranchReady {
		t.Fatalf("状态：%s", res.Status)
	}
	if res.SnapshotID == "" || res.OperationID == "" {
		t.Fatalf("结果：%+v", res)
	}
	if info, ok := e.a.store.Snapshot(res.SnapshotID); !ok || info.Messages == 0 {
		t.Fatalf("快照：%+v %v", info, ok)
	}
	// 分支继承分叉点的有效上下文
	parent := e.a.store.History(e.sid)
	branch := e.a.store.History(res.BranchSID)
	if len(branch) != len(parent) {
		t.Fatalf("分支历史 %d 条，父会话 %d 条", len(branch), len(parent))
	}
	// 父会话继续追加，分支不受影响
	if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "父会话后面的话"}); err != nil {
		t.Fatal(err)
	}
	if got := len(e.a.store.History(res.BranchSID)); got != len(parent) {
		t.Fatalf("父会话追加影响到了分支：%d", got)
	}
	// 分支自己的追加也不写回父会话
	if err := e.a.store.Append(res.BranchSID, session.Msg{Role: "user", Content: "分支自己的话"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range e.a.store.History(e.sid) {
		if m.Content == "分支自己的话" {
			t.Fatal("分支写回了父会话")
		}
	}
}

// 同一个 request_id 重试：返回同一个操作和分支（同进程 + 重启）。
func TestForkSameRequestIsIdempotent(t *testing.T) {
	e := setup(t)
	e.say("起个题目", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：起个题目")

	a, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "A", RequestID: "req-dup", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "B", RequestID: "req-dup", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.BranchSID != b.BranchSID || a.OperationID != b.OperationID {
		t.Fatalf("重复请求建了两个分支：%+v / %+v", a, b)
	}
	if n := len(e.a.store.Children(e.sid)); n != 1 {
		t.Fatalf("分支数：%d", n)
	}
	// 重启后同样的 request_id 还是同一个分支
	a2 := reopen(t, e)
	c, err := a2.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "C", RequestID: "req-dup", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.BranchSID != a.BranchSID {
		t.Fatalf("重启后又建了一个：%s vs %s", c.BranchSID, a.BranchSID)
	}
}

// 生成中 Fork：先 pending_boundary，这一轮结束后才 ready。
func TestForkWhileGeneratingWaitsForBoundary(t *testing.T) {
	e := setup(t)
	e.say("正在做的事", msgID("m1"))
	e.k.expect(proto.TInput)
	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "等边界", RequestID: "req-b1", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != session.BranchPendingBoundary {
		t.Fatalf("生成中应该等边界：%s", res.Status)
	}
	// 这一轮结束：边界到了，分支就绪
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：正在做的事")
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.a.mu.Lock()
		m, ok := e.a.store.Branch(res.BranchSID)
		e.a.mu.Unlock()
		if ok && m.Status == session.BranchReady {
			if m.SnapshotID == "" {
				t.Fatal("ready 了却没有快照")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("边界之后没有 ready：%+v", m)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 同一批工具里调用 Session 工具 fork：先拿到 pending_boundary，批次闭合后 ready，不死锁。
func TestForkInsideToolBatch(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	// 网关执行 Session 工具（和 Schedule 等网关工具一样，工具调用直接进来）
	e.k.send(proto.Msg{T: proto.TExec, SID: e.sid, ID: "c1", Name: "Session",
		Args: jsonRaw(`{"action":"fork","title":"工具里建的分支","request_id":"req-tool-1"}`)})
	got := e.k.expect(proto.TExecResult)
	if got.OK == nil || !*got.OK {
		t.Fatalf("工具失败：%s", got.Content)
	}
	if !strings.Contains(got.Content, "pending_boundary") {
		t.Fatalf("同批工具里应该先返回 pending_boundary：%s", got.Content)
	}
	// 内核把结果交回来，这一批闭合
	e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, ID: "c1", OK: proto.Bool(true), Content: got.Content})
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	waitFor(t, func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return e.a.sessions[e.sid].run == nil
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := e.a.Branches()
		if len(list) == 1 && list[0].Status == session.BranchReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("批次闭合后没有 ready：%+v", list)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 父会话重置 / 删除之后，分支仍然保留分叉时的上下文。
func TestBranchSurvivesParentResetAndDelete(t *testing.T) {
	e := setup(t)
	e.say("共同的背景", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：共同的背景")
	before := len(e.a.store.History(e.sid))

	res, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "幸存者", RequestID: "req-survive", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	// 父会话清空
	if err := e.a.store.Reset(e.sid); err != nil {
		t.Fatal(err)
	}
	if got := len(e.a.store.History(e.sid)); got != 0 {
		t.Fatalf("父会话没清空：%d", got)
	}
	if got := len(e.a.store.History(res.BranchSID)); got != before {
		t.Fatalf("父会话重置影响了分支：%d vs %d", got, before)
	}
}

// 同一点分出来的多个分支共用同一个快照；删掉一个不影响另一个。
func TestBranchesShareSnapshotAndDeleteKeepsOthers(t *testing.T) {
	e := setup(t)
	e.say("起点", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：起点")

	a, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "A", RequestID: "req-a", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "B", RequestID: "req-b", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.SnapshotID != b.SnapshotID {
		t.Fatalf("同一点应该共用快照：%s vs %s", a.SnapshotID, b.SnapshotID)
	}
	if refs := e.a.store.SnapshotRefs(a.SnapshotID); refs != 2 {
		t.Fatalf("引用数：%d", refs)
	}
	if err := e.a.store.DeleteBranch(a.BranchSID); err != nil {
		t.Fatal(err)
	}
	if got := len(e.a.store.History(b.BranchSID)); got == 0 {
		t.Fatal("删掉一个分支破坏了另一个")
	}
	if refs := e.a.store.SnapshotRefs(a.SnapshotID); refs != 1 {
		t.Fatalf("引用数：%d", refs)
	}
	// 全部删完，快照可以被回收
	if err := e.a.store.DeleteBranch(b.BranchSID); err != nil {
		t.Fatal(err)
	}
	if gone := e.a.store.GCSnapshots(); len(gone) != 1 {
		t.Fatalf("快照没回收：%v", gone)
	}
}

// 列出 100 个分支：只看元信息，不把历史都加载进来。
func TestBranchListIsLightweight(t *testing.T) {
	e := setup(t)
	e.say("种子", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到：种子")

	first, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "0", RequestID: "req-l0", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := e.a.store.Snapshot(first.SnapshotID)
	for i := 1; i < 100; i++ {
		sid := "br-test-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		meta := session.BranchMeta{SID: sid, ParentSID: e.sid, Title: "闲置", Status: session.BranchPreparing,
			Delivery: session.BranchDeliverySilent}
		if err := e.a.store.BeginBranch(meta); err != nil {
			t.Fatal(err)
		}
		if err := e.a.store.ReadyBranch(sid, snap, session.BranchReady); err != nil {
			t.Fatal(err)
		}
	}
	e.a.store.UnloadHistory(first.BranchSID)
	list := e.a.Branches()
	if len(list) != 100 {
		t.Fatalf("分支数：%d", len(list))
	}
	for _, b := range list {
		if b.Status != session.BranchReady || b.SnapshotID == "" {
			t.Fatalf("列表项：%+v", b)
		}
	}
}

func jsonRaw(s string) []byte { return []byte(s) }

type messageAttemptModel struct{ asked int }

func (m *messageAttemptModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.asked++
	if m.asked > 1 {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "结论留在分支里"}, nil
	}
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "say-main", Type: "function",
		Function: llm.FunctionCall{Name: "Message", Arguments: `{"to":"chat:main","text":"分支里想对外说的一句话"}`}}}}, nil
}

// §12.5：安静分支里的 Message 不向来源聊天发声，结论留在分支历史里。
func TestSilentBranchMessageNeverReachesTheSourceChat(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "来源会话的上下文"}); err != nil {
		t.Fatal(err)
	}
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "安静", RequestID: "silent-msg", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.llm = &messageAttemptModel{}
	e.a.mu.Unlock()
	if _, err = e.a.SessionBranchAction("continue", branch.BranchSID, "在分支里准备结论", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	if !in.Silent || in.SID != branch.BranchSID {
		t.Fatalf("分支这一轮不是安静的：%+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	turn := e.k.expect(proto.TTurn)
	if len(turn.Calls) != 1 || turn.Calls[0].Name != "Message" {
		t.Fatalf("测试模型没有发出 Message：%+v", turn.Calls)
	}
	e.k.send(proto.Msg{T: proto.TExec, SID: in.SID, ID: turn.Calls[0].ID, Name: "Message", Args: turn.Calls[0].Args})
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK || !strings.Contains(res.Content, "安静分支") {
		t.Fatalf("安静分支的 Message 没有被拒绝：%+v", res)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: in.SID, ID: turn.Calls[0].ID, OK: proto.Bool(false), Content: res.Content})
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
	select {
	case text := <-e.ch.sent:
		t.Fatal("安静分支把消息发给来源聊天了", text)
	default:
	}
	main := e.a.store.History(mainSID)
	for _, m := range main {
		if strings.Contains(m.Content, "分支里想对外说的一句话") || strings.Contains(m.Content, "结论留在分支里") {
			t.Fatalf("安静分支的产出漏进来源会话：%+v", m)
		}
	}
	inBranch := false
	for _, m := range e.a.store.History(branch.BranchSID) {
		if strings.Contains(m.Content, "结论留在分支里") {
			inBranch = true
		}
	}
	if !inBranch {
		t.Fatal("结论没有留在分支历史里")
	}
}

// 评审 §24 R2：自主分支不能绕过暂停和当天预算；主人手动继续不受影响。
func TestAutonomousBranchRespectsPauseAndBudget(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	budget := 10
	makeLife(t, e, store, func(ag *agents.Agent) { ag.LifeBudget = &budget })
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "种子上下文"}); err != nil {
		t.Fatal(err)
	}
	fork := func(id string) SessionForkResult {
		out, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: id, RequestID: id, By: "life:default"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// 1) 暂停之后，agent 自己继续分支被拒绝，也没有新的 input
	paused := fork("pause-branch")
	e.a.mu.Lock()
	e.a.mind("default").SetPaused(true, "测试暂停", e.a.now())
	e.a.mu.Unlock()
	if _, err := e.a.SessionBranchAction("continue", paused.BranchSID, "继续做点事", "life:default"); err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatalf("暂停后仍然启动了分支：%v", err)
	}
	e.k.none()
	// 暂停时主人手动继续仍然可以（用户交互语义不变）
	e.a.mu.Lock()
	e.a.mind("default").SetPaused(false, "", e.a.now())
	e.a.mu.Unlock()
	if _, err := e.a.SessionBranchAction("continue", paused.BranchSID, "主人让它继续", "fake:u1"); err != nil {
		t.Fatalf("主人手动继续被挡住了：%v", err)
	}
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: paused.BranchSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: paused.BranchSID, Status: proto.StatusOK})
	waitIdle(t, e, paused.BranchSID)
	// 2) 预算用完之后，agent 自己继续同样被拒绝
	broke := fork("broke-branch")
	e.a.mu.Lock()
	e.a.mind("default").Spend(10)
	e.a.mind("default").SetBudgetLimit(10)
	e.a.mu.Unlock()
	if _, err := e.a.SessionBranchAction("continue", broke.BranchSID, "再试一次", "life:default"); err == nil || !strings.Contains(err.Error(), "预算") {
		t.Fatalf("预算用完之后仍然启动了分支：%v", err)
	}
	e.k.none()
}

// 分支的"最近进展"：从真实执行状态生成、可持久化，重启后还在；旧记录没有进展也能正常显示。
func TestBranchRecentProgressComesFromRealState(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "种子"}); err != nil {
		t.Fatal(err)
	}
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "有进展", RequestID: "progress-1", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.SessionBranchAction("continue", branch.BranchSID, "看看日志", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	e.k.expect(proto.TInput)
	// 开始跑：先记下"运行中：…"
	info := e.a.Branches()
	if len(info) != 1 || !strings.Contains(info[0].Progress, "运行中") {
		t.Fatalf("开始之后没有进展：%+v", info)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: branch.BranchSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: branch.BranchSID, Status: proto.StatusOK})
	waitIdle(t, e, branch.BranchSID)
	info = e.a.Branches()
	if len(info) != 1 || !strings.Contains(info[0].Progress, "完成") || info[0].ProgressAt == 0 {
		t.Fatalf("结束之后没有进展：%+v", info)
	}
	// 持久化：重新打开存储也能读到
	reopened, err := session.New(e.a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Branch(branch.BranchSID)
	if !ok || got.Progress != info[0].Progress || got.ProgressAt != info[0].ProgressAt {
		t.Fatalf("进展没有落盘：%+v", got)
	}
}

// 评审 §26 G2：父会话忙时连续引入两份报告，两份都要保留各自的卡片；混排的普通文字不能被卡片藏起来。
func TestQueuedBranchReportsKeepEveryStructuredCard(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "起点"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, title, conclusion string) SessionForkResult {
		out, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: title, RequestID: id, By: "fake:u1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.a.store.Append(out.BranchSID, session.Msg{Role: "assistant", Content: conclusion}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	a1 := mk("card-1", "第一份", "结论一：按天轮转")
	b1 := mk("card-2", "第二份", "结论二：保留七天")
	// 主会话正在跑一轮：两份报告都进入队列
	e.say("先聊着")
	e.k.expect(proto.TInput)
	if _, err := e.a.introduceBranchReportLocked(a1.BranchSID, "fake:u1", endpoint{ch: "web", chat: mainSID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.introduceBranchReportLocked(b1.BranchSID, "fake:u1", endpoint{ch: "web", chat: mainSID}); err != nil {
		t.Fatal(err)
	}
	// 这一轮结束后，队列里的报告作为下一轮进来
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.k.expect(proto.TInput) // 报告那一轮
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	waitIdle(t, e, mainSID)
	e.a.mu.Lock()
	hist := e.a.store.History(mainSID)
	e.a.mu.Unlock()
	var cards []session.Report
	for _, m := range hist {
		if m.Report != nil {
			cards = append(cards, *m.Report)
		}
	}
	if len(cards) != 2 {
		t.Fatalf("两份报告只剩 %d 份结构：%+v", len(cards), cards)
	}
	if cards[0].BranchSID != a1.BranchSID || cards[1].BranchSID != b1.BranchSID {
		t.Fatalf("报告顺序或归属不对：%+v", cards)
	}
	for _, card := range cards {
		if !strings.Contains(card.Conclusion, "结论") {
			t.Fatalf("报告的结论丢了：%+v", card)
		}
	}
}

// 评审 §26 G3：归档正在跑的分支之后，取消回执不能把 archived 改回 paused。
func TestArchivingRunningBranchRemainsArchivedAfterCancel(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "起点"}); err != nil {
		t.Fatal(err)
	}
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "归档", RequestID: "archive-running", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.SessionBranchAction("continue", branch.BranchSID, "先跑着", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	e.k.expect(proto.TInput)
	// 归档：发 CANCEL，然后内核回一个 cancelled 的 DONE
	if _, err := e.a.SessionBranchAction("archive", branch.BranchSID, "", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.a.store.Branch(branch.BranchSID); !ok || m.Status != session.BranchArchived {
		t.Fatalf("归档没有生效：%+v", m)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: branch.BranchSID, Status: proto.StatusCancelled})
	waitIdle(t, e, branch.BranchSID)
	got, ok := e.a.store.Branch(branch.BranchSID)
	if !ok || got.Status != session.BranchArchived {
		t.Fatalf("取消回执把归档改掉了：%+v", got)
	}
	if !strings.Contains(got.Progress, "取消") {
		t.Fatalf("进展没有记录这次取消：%q", got.Progress)
	}
	// 重启后仍然是归档，继续仍然被拒绝
	reopened, err := session.New(e.a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	again, ok := reopened.Branch(branch.BranchSID)
	if !ok || again.Status != session.BranchArchived {
		t.Fatalf("重启后归档状态丢了：%+v", again)
	}
	if _, err := e.a.SessionBranchAction("continue", branch.BranchSID, "再跑", "fake:u1"); err == nil {
		t.Fatal("归档的分支还能继续")
	}
}

// 评审 §27：拆分报告不能用原始正文顶替准备好的说明（交接、环境、记忆），交接被消费就必须进历史。
func TestQueuedBranchReportsKeepPreparedContext(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "起点"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, conclusion string) SessionForkResult {
		out, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: id, RequestID: id, By: "fake:u1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.a.store.Append(out.BranchSID, session.Msg{Role: "assistant", Content: conclusion}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	a1 := mk("ctx-1", "结论一")
	b1 := mk("ctx-2", "结论二")
	e.say("先聊着")
	e.k.expect(proto.TInput)
	// 会话正在跑的时候才写入交接说明：它必须留给下一轮（报告那一轮），不能被这一轮吃掉
	e.a.mu.Lock()
	st := e.a.store.Settings(mainSID)
	st.Handoff = "[交接] 上一个 agent 留下的说明：先看任务清单"
	if err := e.a.store.SaveSettings(mainSID, st); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	if _, err := e.a.introduceBranchReportLocked(a1.BranchSID, "fake:u1", endpoint{ch: "web", chat: mainSID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.introduceBranchReportLocked(b1.BranchSID, "fake:u1", endpoint{ch: "web", chat: mainSID}); err != nil {
		t.Fatal(err)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.k.expect(proto.TInput)
	// 模型这一轮看到的用户消息里必须有交接说明（消费了就要真的用上）
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	req := e.llm.lastReq()
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	waitIdle(t, e, mainSID)
	saw := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "[交接] 上一个 agent 留下的说明") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("交接说明没有进入模型上下文")
	}
	e.a.mu.Lock()
	left := e.a.store.Settings(mainSID).Handoff
	hist := e.a.store.History(mainSID)
	e.a.mu.Unlock()
	if left != "" {
		t.Fatalf("交接说明已经用掉却没清空：%q", left)
	}
	cards := 0
	for _, m := range hist {
		if m.Report != nil {
			cards++
			if !strings.Contains(m.Content, "结论") && m.Kind != "branch_report" {
				t.Fatalf("报告的正文丢了：%+v", m)
			}
		}
	}
	if cards != 2 {
		t.Fatalf("两份报告的结构：%d", cards)
	}
}
