package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// reopen 在同一份数据目录上重建一个 App：模拟进程重启。
func reopen(t *testing.T, e *env) *App {
	t.Helper()
	store, err := session.New(filepath.Join(e.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := e.a.cfg
	a := New(ctx, cfg, &recLLM{}, store, persona.New(cfg.Persona.Dir, log), log)
	a.now = func() time.Time { return time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC) }
	a.loc = time.UTC
	a.SetHosts(newFakeHosts())
	return a
}

func journalRecs(t *testing.T, a *App, sid string) []journalRec {
	t.Helper()
	recs, _, err := readJournal(a.journalPath(sid))
	if err != nil {
		t.Fatalf("读日志：%v", err)
	}
	return recs
}

// 受理回执必须以持久提交为前提：写不进日志就不能说“已收到”。
func TestInterjectionReceiptNeedsDurableWrite(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	// 用目录占住日志文件的位置，让写入必然失败
	if err := os.MkdirAll(e.a.journalPath(e.sid), 0o700); err != nil {
		t.Fatal(err)
	}
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "没能保存下来")
	e.a.mu.Lock()
	n := len(e.a.sessions[e.sid].run.inbox)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("没落盘的补充不该进收件箱：%d 条", n)
	}
}

// 受理之后、插入历史之前崩溃：重启后补充还在，而且只恢复一次。
func TestJournalRestoresPendingAfterRestart(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 1 {
		t.Fatalf("恢复的待处理条数：%d", len(s2.pending))
	}
	if s2.pending[0].text != "补充" {
		t.Fatalf("恢复的内容：%+v", s2.pending[0])
	}
	// 再重启一次：同一条不能变两条
	a3 := reopen(t, e)
	if got := len(a3.sessOf(e.sid).pending); got != 1 {
		t.Fatalf("第二次恢复：%d 条", got)
	}
}

// 历史写成功、injected 没来得及写：重启后按历史 ID 对账，正文只出现一次。
func TestJournalHistoryWithoutInjectedNotDuplicated(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	id := interjectionID(dedupKey(e.sid, "fake", "c1", "m2"))
	histID := interjectionHistoryID([]string{id})
	if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "合并后的正文",
		Kind: "user_interjection", ID: histID, SourceIDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	before := len(e.a.store.History(e.sid))

	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 0 {
		t.Fatalf("已经写进历史的不该再进收件箱：%d 条", len(s2.pending))
	}
	if got := len(a2.store.History(e.sid)); got != before {
		t.Fatalf("历史被重复写了：%d → %d", before, got)
	}
	// 日志里补记了 injected
	var injected bool
	for _, rec := range journalRecs(t, a2, e.sid) {
		if rec.Kind == jrecInjected && rec.ID == id && rec.HistoryID == histID {
			injected = true
		}
	}
	if !injected {
		t.Fatal("没有补记 injected")
	}
}

// 已经进入模型请求、但没有结局（请求发出去就崩溃）：不能当成处理完，标成 interrupted。
func TestJournalIncludedWithoutOutcomeIsInterrupted(t *testing.T) {
	e := setup(t)
	id := "ij-included-1"
	for _, rec := range []journalRec{
		{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"},
		{Kind: jrecInjected, ID: id, HistoryID: "h1"},
		{Kind: jrecIncluded, ID: id, RequestID: "r1-t1", Revision: 1},
	} {
		if err := e.a.journalAppend(e.sid, rec); err != nil {
			t.Fatal(err)
		}
	}
	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 0 {
		t.Fatalf("已经进了请求的不该回到收件箱：%d", len(s2.pending))
	}
	states := foldJournal(journalRecs(t, a2, e.sid), 0)
	if len(states) != 1 {
		t.Fatalf("状态：%+v", states)
	}
	if states[0].Outcome != outcomeInterrupted {
		t.Fatalf("没有标成 interrupted：%+v", states[0])
	}
	// interrupted 不是"结束"：settled 必须为 false，才能跨检查点保留
	if !states[0].included || states[0].settled {
		t.Fatalf("状态字段：%+v", states[0])
	}
}

// 撤回过的补充重启后不能复活。
func TestJournalWithdrawnNeverRestored(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	// 用户在平台上撤回
	e.say("", func(m *channel.Message) {
		m.Text, m.MsgID, m.SenderID, m.Action = "", "m2", "", channel.ActionRecall
	})
	e.ch.wait(t, "已撤回还没处理的消息")

	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("撤回的补充又回来了：%d 条", got)
	}
}

// 会话重置之后世代加一：重置前受理的补充全部作废。
func TestJournalGenerationDiscardsOldPending(t *testing.T) {
	e := setup(t)
	id := "ij-old-gen"
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: id, Seq: 1, Text: "旧补充"}); err != nil {
		t.Fatal(err)
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 1 {
		t.Fatalf("同一世代应该恢复：%d", got)
	}
	// 重置：世代 +1
	e.a.mu.Lock()
	e.a.newGeneration(e.a.sessOf(e.sid))
	e.a.mu.Unlock()
	a3 := reopen(t, e)
	if got := len(a3.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("重置前的补充不该恢复：%d", got)
	}
}

// 渠道重发同一条消息：只受理一次。
func TestJournalDedupOnResend(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	e.say("补充", msgID("m2")) // 渠道重发
	time.Sleep(20 * time.Millisecond)
	var received int
	for _, rec := range journalRecs(t, e.a, e.sid) {
		if rec.Kind == jrecReceived {
			received++
		}
	}
	if received != 1 {
		t.Fatalf("受理记录：%d 条", received)
	}
	e.a.mu.Lock()
	n := len(e.a.sessions[e.sid].run.inbox)
	e.a.mu.Unlock()
	if n != 1 {
		t.Fatalf("收件箱：%d 条", n)
	}
}

// 日志尾部半条（写到一半崩了）视为未提交，前面已提交的记录照常可用。
func TestJournalIgnoresTruncatedTail(t *testing.T) {
	e := setup(t)
	id := "ij-tail"
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(e.a.journalPath(e.sid), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"v":1,"kind":"received","id":"half"`) // 没有换行
	f.Close()
	recs, _, err := readJournal(e.a.journalPath(e.sid))
	if err != nil {
		t.Fatalf("尾部半条不该报错：%v", err)
	}
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("记录：%+v", recs)
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 1 {
		t.Fatalf("恢复：%d", got)
	}
}

// 会话换了 agent：原来受理的补充不能再交给新身份。
func TestJournalAgentSwitchDiscards(t *testing.T) {
	e := setup(t)
	id := "ij-agent"
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: id, Seq: 1, Text: "旧补充", Agent: "other-agent"}); err != nil {
		t.Fatal(err)
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("换了 agent 还恢复：%d", got)
	}
}

// B1：恢复出来的补充必须带着稳定 ID；处理完成再重启不会再次出现。
func TestRecoveredInterjectionProcessedThenRestartDoesNotReturn(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 1 {
		t.Fatalf("恢复：%d", len(s2.pending))
	}
	it := s2.pending[0]
	if it.id == "" {
		t.Fatal("恢复出来的补充丢了稳定 ID")
	}
	// 按正常 drain 的步骤：写历史（带稳定 ID）→ 记 injected → 记结局
	histID := interjectionHistoryID([]string{it.id})
	if err := a2.store.Append(e.sid, session.Msg{Role: "user", Content: "合并后的正文", Kind: "user_interjection",
		ID: histID, SourceIDs: []string{it.id}}); err != nil {
		t.Fatal(err)
	}
	if err := a2.journalAppend(e.sid, journalRec{Kind: jrecInjected, ID: it.id, HistoryID: histID}); err != nil {
		t.Fatal(err)
	}
	if err := a2.journalAppend(e.sid, journalRec{Kind: jrecOutcome, ID: it.id, Outcome: outcomeOK}); err != nil {
		t.Fatal(err)
	}
	a3 := reopen(t, e)
	if got := len(a3.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("处理完的补充又回来了：%d", got)
	}
	if got := len(a3.store.History(e.sid)); got != 2 {
		t.Fatalf("历史条数：%d", got)
	}
}

// B1：不同恢复批次不会算出同一个历史 ID。
func TestRecoveredBatchesGetDistinctHistoryIDs(t *testing.T) {
	if interjectionHistoryID([]string{"ij-a"}) == interjectionHistoryID([]string{"ij-b"}) {
		t.Fatal("不同批次的历史 ID 撞了")
	}
}

// B2：尾部半条要截掉，之后追加新记录并重启仍然可读。
func TestTruncatedJournalThenAppendThenRestart(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	// 模拟写到一半崩了：追加半条没有换行的 JSON
	f, err := os.OpenFile(e.a.journalPath(e.sid), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"v":1,"kind":"received","id":"half`)
	f.Close()

	// 重启：截掉半条，恢复出原来的补充
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 1 {
		t.Fatalf("恢复：%d", got)
	}
	// 再受理一条新的
	if err := a2.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: "ij-new", Seq: 99, Text: "新补充"}); err != nil {
		t.Fatalf("截尾后还写不进去：%v", err)
	}
	// 再重启：两条都在，而且文件能正常解析
	a3 := reopen(t, e)
	if got := len(a3.sessOf(e.sid).pending); got != 2 {
		t.Fatalf("重启后待处理：%d", got)
	}
	if _, _, err := readJournal(a3.journalPath(e.sid)); err != nil {
		t.Fatalf("日志还是坏的：%v", err)
	}
}

// B2：中间损坏时不能再假装受理成功。
func TestCorruptJournalRefusesNewReceipt(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	// 在中间插一条坏 JSON（带换行）
	f, err := os.OpenFile(e.a.journalPath(e.sid), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{这不是 JSON}\n")
	f.Close()

	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 0 {
		t.Fatalf("损坏的日志不该恢复出待处理：%d", len(s2.pending))
	}
	if err := a2.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: "ij-x", Seq: 5, Text: "x"}); err == nil {
		t.Fatal("日志损坏还允许追加并返回成功")
	}
}

// B3：重新排队后、下一轮写历史之前崩溃，补充不能丢。
func TestRequeuedInterjectionSurvivesCrashBeforeNextRun(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	e.a.mu.Lock()
	s := e.a.sessions[e.sid]
	left := e.a.requeueInterjections(s, s.run)
	e.a.mu.Unlock()
	if len(left) != 1 {
		t.Fatalf("重新排队：%d", len(left))
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 1 {
		t.Fatalf("崩溃后待处理：%d（应该还在）", got)
	}
}

// B4：检查点必须保留身份与去重信息。
func TestCheckpointPreservesAgentAndDedupAcrossRestart(t *testing.T) {
	e := setup(t)
	key := dedupKey(e.sid, "fake", "c1", "m-checkpoint")
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: "ij-cp", Seq: 1, MsgID: "m-checkpoint",
		Dedup: key, Text: "补充", Agent: "other-agent"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	if err := e.a.checkpointJournal(e.sid); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	a2 := reopen(t, e)
	if _, ok := a2.journalByDedup(e.sid, key); !ok {
		t.Fatal("检查点丢了去重键")
	}
	// 身份是 other-agent：不能交给现在的 agent
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("换了 agent 还恢复：%d", got)
	}
}

// B5：普通撤回 / 网页撤回要写持久终态，重启不复活。
func TestUndoInterjectionDoesNotReturnAfterRestart(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	e.a.mu.Lock()
	_, note, _, err := e.a.undoLatest(e.a.sessOf(e.sid))
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "已撤回") {
		t.Fatalf("说明：%q", note)
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("撤回的补充又回来了：%d", got)
	}
}

// B6：interrupted 跨检查点和重启仍然可见，不被回收。
func TestInterruptedInterjectionRemainsVisibleAfterCheckpointAndRestart(t *testing.T) {
	e := setup(t)
	id := "ij-int"
	for _, rec := range []journalRec{
		{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"},
		{Kind: jrecInjected, ID: id, HistoryID: "h1"},
		{Kind: jrecIncluded, ID: id, RequestID: "r1-t1", Revision: 1},
	} {
		if err := e.a.journalAppend(e.sid, rec); err != nil {
			t.Fatal(err)
		}
	}
	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.interrupted) != 1 {
		t.Fatalf("没有可见的中断状态：%d", len(s2.interrupted))
	}
	a2.mu.Lock()
	if err := a2.checkpointJournal(e.sid); err != nil {
		t.Fatal(err)
	}
	a2.mu.Unlock()
	a3 := reopen(t, e)
	s3 := a3.sessOf(e.sid)
	if len(s3.interrupted) != 1 {
		t.Fatalf("检查点把中断状态回收了：%d", len(s3.interrupted))
	}
	if len(s3.pending) != 0 {
		t.Fatalf("已经进历史的不能回到待处理：%d", len(s3.pending))
	}
}
