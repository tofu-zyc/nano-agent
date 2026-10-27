package app

import (
	"context"
	"errors"
	"fmt"
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
	// 用目录占住日志文件的位置，让写入必然失败（先丢掉缓存的句柄，否则还在写旧文件）
	if j := e.a.journals[e.sid]; j != nil {
		j.close()
		delete(e.a.journals, e.sid)
	}
	_ = os.Remove(e.a.journalPath(e.sid))
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

// B7：检查点之后，原进程重发同一条平台消息不能再次受理。
func TestAutomaticCheckpointPreservesDedupWithoutRestart(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	e.a.mu.Lock()
	if err := e.a.checkpointJournal(e.sid); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	// 同一进程内重发
	e.say("补充", msgID("m2"))
	time.Sleep(30 * time.Millisecond)
	e.a.mu.Lock()
	n := len(e.a.sessions[e.sid].run.inbox)
	e.a.mu.Unlock()
	if n != 1 {
		t.Fatalf("检查点之后重发又被受理了：收件箱 %d 条", n)
	}
	if _, ok := e.a.journalByDedup(e.sid, dedupKey(e.sid, "fake", "c1", "m2")); !ok {
		t.Fatal("检查点之后内存去重索引丢了")
	}
}

// B7：所有旧补充都完成、被检查点回收之后，接收序号高水位仍要保住。
func TestCheckpointPreservesReceiveHighWaterAfterAllSettled(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	// 无平台 ID 的补充：ID 由会话内序号分配
	e.say("补充甲", func(m *channel.Message) { m.MsgID = "" })
	e.ch.wait(t, "已收到插话")
	e.a.mu.Lock()
	s := e.a.sessions[e.sid]
	first := s.run.inbox[0]
	// 真实 drain + 结局
	if !e.a.drainInterjections(s, s.run) {
		e.a.mu.Unlock()
		t.Fatal("drain 失败")
	}
	e.a.settleInterjections(e.sid, []string{first.id}, outcomeOK, "测试")
	if err := e.a.checkpointJournal(e.sid); err != nil {
		e.a.mu.Unlock()
		t.Fatal(err)
	}
	e.a.mu.Unlock()

	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if s2.interjectSeq < int64(first.seq) {
		t.Fatalf("接收序号倒退：%d < %d", s2.interjectSeq, first.seq)
	}
	// 新的一条无 ID 补充不能拿到旧 ID
	newID := interjectionID(fmt.Sprintf("%s|seq-%d", e.sid, s2.interjectSeq+1))
	if newID == first.id {
		t.Fatal("新消息拿到了旧的 interjection_id")
	}
}

// B8：写了一半失败后，同进程重试不能拼坏日志。
func TestPartialJournalWriteThenRetryWithoutRestart(t *testing.T) {
	e := setup(t)
	j, err := e.a.openJournal(e.sid)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟写出 20 字节后失败（EFBIG 之类）
	if _, err := j.file.WriteString(`{"v":1,"kind":"rece`); err != nil {
		t.Fatal(err)
	}
	if err := j.afterWriteError(errors.New("EFBIG")); err == nil {
		t.Fatal("部分写入失败却返回成功")
	}
	// 同进程继续追加：必须能成功，而且日志整体可读
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: "ij-a", Seq: 1, Text: "补充"}); err != nil {
		t.Fatalf("修复尾部后还写不进去：%v", err)
	}
	recs, _, err := readJournal(e.a.journalPath(e.sid))
	if err != nil {
		t.Fatalf("日志被拼坏了：%v", err)
	}
	if len(recs) != 1 || recs[0].ID != "ij-a" {
		t.Fatalf("记录：%+v", recs)
	}
}

// B9：历史变长之后对账仍然成立（不能只看最近 N 条）。
func TestHistoryReconciliationBeyondRecentWindow(t *testing.T) {
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
	// 后面再接 401 条短消息，把那条挤出"最近 400 条"
	for i := 0; i < 401; i++ {
		if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "闲聊"}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.a.store.History(e.sid))
	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 0 {
		t.Fatalf("历史里的补充被当成待处理了：%d", len(s2.pending))
	}
	if got := len(a2.store.History(e.sid)); got != before {
		t.Fatalf("历史被重复写：%d → %d", before, got)
	}
}

// B9：补记 injected 的同一次恢复里，就要能看到"可以继续"。
func TestRecoveredHistoryWithoutInjectedIsImmediatelyContinuable(t *testing.T) {
	e := setup(t)
	id := "ij-hist-first"
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"}); err != nil {
		t.Fatal(err)
	}
	histID := interjectionHistoryID([]string{id})
	if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "正文", Kind: "user_interjection",
		ID: histID, SourceIDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.interrupted) != 1 {
		t.Fatalf("第一次恢复就应该能看到中断状态：%d", len(s2.interrupted))
	}
	if len(s2.pending) != 0 {
		t.Fatalf("已经写进历史的不该回到待处理：%d", len(s2.pending))
	}
	if got := a2.interruptedCount(e.sid); got != 1 {
		t.Fatalf("可见计数：%d", got)
	}
}

// B10：没有活跃 run 时，/stop 也要清掉恢复的待处理。
func TestStopRestoredPendingWithoutRun(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	a2 := reopen(t, e)
	a2.AddChannel(&fakeCh{sent: make(chan string, 10)})
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 1 {
		t.Fatalf("恢复：%d", len(s2.pending))
	}
	a2.mu.Lock()
	a2.stop(in{s: s2, ep: endpoint{ch: "fake", chat: "c1"}, who: "fake:u1"})
	a2.mu.Unlock()
	if len(s2.pending) != 0 {
		t.Fatalf("停止之后还有待处理：%d", len(s2.pending))
	}
	a3 := reopen(t, e)
	if got := len(drainPending(a3.sessOf(e.sid))); got != 0 {
		t.Fatalf("重启后又活了：%d", got)
	}
}

// B10：终态写失败时必须说清楚，不能回"已经停干净了"。
func TestStopReportsJournalFailureAndDoesNotClaimDurableDiscard(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")

	a2 := reopen(t, e)
	ch := &fakeCh{sent: make(chan string, 10)}
	a2.AddChannel(ch)
	s2 := a2.sessOf(e.sid)
	// 让日志写不进去：丢掉句柄，用目录占位
	if j := a2.journals[e.sid]; j != nil {
		j.close()
		delete(a2.journals, e.sid)
	}
	_ = os.Remove(a2.journalPath(e.sid))
	if err := os.MkdirAll(a2.journalPath(e.sid), 0o700); err != nil {
		t.Fatal(err)
	}
	a2.mu.Lock()
	a2.stop(in{s: s2, ep: endpoint{ch: "fake", chat: "c1"}, who: "fake:u1"})
	a2.mu.Unlock()
	ch.wait(t, "没能保存下来")
	if len(s2.pending) != 1 {
		t.Fatalf("写失败却把待处理清了：%d", len(s2.pending))
	}
}

// B10：重置之后，中断提示和字节计数要立刻消失，不用等重启。
func TestResetClearsInterruptedAndPendingBytesImmediately(t *testing.T) {
	e := setup(t)
	id := "ij-reset-int"
	for _, rec := range []journalRec{
		{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"},
		{Kind: jrecInjected, ID: id, HistoryID: "h1"},
		{Kind: jrecIncluded, ID: id, RequestID: "r1-t1", Revision: 1},
	} {
		if err := e.a.journalAppend(e.sid, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: "ij-reset-pending", Seq: 2, Text: "待处理"}); err != nil {
		t.Fatal(err)
	}
	a2 := reopen(t, e)
	ch := &fakeCh{sent: make(chan string, 10)}
	a2.AddChannel(ch)
	s2 := a2.sessOf(e.sid)
	if len(s2.interrupted) != 1 || len(s2.pending) != 1 {
		t.Fatalf("恢复：interrupted=%d pending=%d", len(s2.interrupted), len(s2.pending))
	}
	before := s2.pendingBytes
	if before == 0 {
		t.Fatal("字节计数没恢复")
	}
	a2.mu.Lock()
	a2.reset(in{s: s2, ep: endpoint{ch: "fake", chat: "c1"}, who: "fake:u1"})
	a2.mu.Unlock()
	if len(s2.interrupted) != 0 || s2.pendingBytes != 0 || len(s2.pending) != 0 {
		t.Fatalf("重置后没清干净：interrupted=%d bytes=%d pending=%d",
			len(s2.interrupted), s2.pendingBytes, len(s2.pending))
	}
	if got := a2.interruptedCount(e.sid); got != 0 {
		t.Fatalf("可见计数没清：%d", got)
	}
}

// drainPending 是测试里把恢复的待处理拿出来看的辅助。
func drainPending(s *sess) []interjection { return s.pending }

// B10：运行中的 /stop 必须把收件箱里的补充也持久作废。
func TestActiveStopDiscardsInterjectionAcrossRestart(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	e.say("/stop", msgID("m3"))
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "已停止")
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("停止后重启又活了：%d", got)
	}
}

// B10：运行中的 /reset 必须换世代，重启不能恢复旧补充。
func TestActiveResetAdvancesGenerationAndDoesNotRestoreInterjections(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	e.say("/reset", msgID("m3"))
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "上下文已清空")
	if gen := e.a.store.Settings(e.sid).SessionGeneration; gen == 0 {
		t.Fatalf("重置没有换世代：%d", gen)
	}
	a2 := reopen(t, e)
	s2 := a2.sessOf(e.sid)
	if len(s2.pending) != 0 {
		t.Fatalf("重置后重启又恢复：%d", len(s2.pending))
	}
	if got := len(a2.store.History(e.sid)); got != 0 {
		t.Fatalf("重置后历史还在：%d 条", got)
	}
}

// B9：压缩（Base 推进）之后，已经提交过的插入仍然要能按 ID 查到，不能重复注入。
func TestCompactedHistoryInterjectionIsNotReplayedAfterRestart(t *testing.T) {
	e := setup(t)
	id := "ij-compact"
	if err := e.a.journalAppend(e.sid, journalRec{Kind: jrecReceived, ID: id, Seq: 1, Text: "补充"}); err != nil {
		t.Fatal(err)
	}
	histID := interjectionHistoryID([]string{id})
	if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "第一问"}); err != nil {
		t.Fatal(err)
	}
	if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "正文", Kind: "user_interjection",
		ID: histID, SourceIDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := e.a.store.Append(e.sid, session.Msg{Role: "user", Content: strings.Repeat("后面的话", 40)}); err != nil {
			t.Fatal(err)
		}
		if err := e.a.store.Append(e.sid, session.Msg{Role: "assistant", Content: "嗯"}); err != nil {
			t.Fatal(err)
		}
	}
	e.a.store.Settings(e.sid)
	if _, err := e.a.store.Compact(e.sid, 1, 40, "摘要"); err != nil {
		t.Fatal(err)
	}
	if base := e.a.store.Settings(e.sid).Base; base == 0 {
		t.Fatalf("没有真的压缩")
	}
	// 压缩之后，那条历史消息不在有效窗口里了，但仍然是已提交的
	if _, ok := e.a.store.FindMessageBySource(e.sid, id, 0); !ok {
		t.Fatal("压缩后查不到已提交的历史")
	}
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("压缩后把已提交的补充当成待处理：%d", got)
	}
	// 原始记录（含 Base 之前）里这条历史只出现一次
	var times int
	for _, m := range a2.store.Archive(e.sid) {
		if m.ID == histID {
			times++
		}
	}
	if times != 1 {
		t.Fatalf("历史 ID 出现 %d 次", times)
	}
}

// B10：平台撤回写失败时要保留待处理与字节数，修好之后能直接重试。
func TestPlatformRecallWriteFailurePreservesInboxAndCanRetry(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	// 让日志写不进去
	if j := e.a.journals[e.sid]; j != nil {
		j.close()
		delete(e.a.journals, e.sid)
	}
	_ = os.Remove(e.a.journalPath(e.sid))
	if err := os.MkdirAll(e.a.journalPath(e.sid), 0o700); err != nil {
		t.Fatal(err)
	}
	before := len(e.a.sessions[e.sid].run.inbox)
	beforeBytes := e.a.sessions[e.sid].run.inboxBytes
	e.say("", func(m *channel.Message) {
		m.Text, m.MsgID, m.SenderID, m.Action = "", "m2", "", channel.ActionRecall
	})
	e.ch.wait(t, "还在队列里")
	if got := len(e.a.sessions[e.sid].run.inbox); got != before {
		t.Fatalf("写失败后收件箱被改了：%d → %d", before, got)
	}
	if got := e.a.sessions[e.sid].run.inboxBytes; got != beforeBytes {
		t.Fatalf("字节数没还原：%d → %d", beforeBytes, got)
	}
	// 修好日志，同进程直接重试
	if err := os.RemoveAll(e.a.journalPath(e.sid)); err != nil {
		t.Fatal(err)
	}
	e.say("", func(m *channel.Message) {
		m.Text, m.MsgID, m.SenderID, m.Action = "", "m2", "", channel.ActionRecall
	})
	e.ch.wait(t, "已撤回还没处理的消息")
	a2 := reopen(t, e)
	if got := len(a2.sessOf(e.sid).pending); got != 0 {
		t.Fatalf("撤回后重启又复活：%d", got)
	}
}
