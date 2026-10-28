package app

import (
	"strings"
	"testing"
	"time"

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
