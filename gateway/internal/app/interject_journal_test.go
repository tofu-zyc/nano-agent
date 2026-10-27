package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	recs, err := readJournal(a.journalPath(sid))
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
	if !states[0].included || states[0].settled == false {
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
