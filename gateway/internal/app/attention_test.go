package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// attention 投递档（设计 §15.3）。

// drainSent 取走窗口里已经发出的消息（等一小会儿让出站队列送完）。
func drainSent(e *env) []string {
	time.Sleep(150 * time.Millisecond)
	var out []string
	for {
		select {
		case s := <-e.ch.sent:
			out = append(out, s)
		default:
			return out
		}
	}
}

func attentionBranch(t *testing.T, e *env, reqID string) string {
	t.Helper()
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "来源会话的上下文"}); err != nil {
		t.Fatal(err)
	}
	br, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "查竞品", RequestID: reqID, By: "fake:u1", Delivery: session.BranchDeliveryAttention})
	if err != nil {
		t.Fatal(err)
	}
	return br.BranchSID
}

func TestAttentionOnlyOwnerCanSetIt(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-owner")
	e.a.mu.Lock()
	d, silent := e.a.branchDelivery(sid), e.a.branchSilent(sid)
	e.a.mu.Unlock()
	if d != session.BranchDeliveryAttention || !silent {
		t.Fatalf("主人要求的 attention：%q 安静=%v", d, silent)
	}
	// 自主活动分出的（life、Hook、整理记忆）设不成
	for _, by := range []string{"life:default", "hook:r1", "dream:default"} {
		br, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "自动", RequestID: "att-auto-" + by, By: by, Delivery: session.BranchDeliveryAttention})
		if err != nil {
			continue // 创建本身被拒绝也算设不成
		}
		e.a.mu.Lock()
		d := e.a.branchDelivery(br.BranchSID)
		e.a.mu.Unlock()
		if d != session.BranchDeliverySilent {
			t.Fatalf("%s 分出的只能是 silent：%q", by, d)
		}
	}
	// 没要求 attention 的还是 silent；report 的行为不变
	br, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "默认", RequestID: "att-default", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	d = e.a.branchDelivery(br.BranchSID)
	e.a.mu.Unlock()
	if d != session.BranchDeliverySilent {
		t.Fatalf("默认 silent：%q", d)
	}
}

func TestAttentionPushesReadyBlockedAndHostFailureOnly(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-push")
	before := len(e.a.store.History(mainSID))
	drainSent(e)
	e.a.mu.Lock()
	e.a.attentionPush(sid, "ready", "竞品有三家", "", "7")
	e.a.mu.Unlock()
	e.ch.wait(t, "[分支「查竞品」· 就绪] 竞品有三家")
	e.a.mu.Lock()
	e.a.attentionPush(sid, "blocked", "有两个方案", "用哪个？", "8")
	e.a.mu.Unlock()
	got := e.ch.wait(t, "· 需要你回答] 有两个方案")
	if !strings.Contains(got, "问：用哪个？") || !strings.Contains(got, "/answer 8 <你的回答>") || !strings.Contains(got, "“等我处理”") {
		t.Fatalf("格式：%q", got)
	}
	// 通知不写进父会话的历史
	if n := len(e.a.store.History(mainSID)); n != before {
		t.Fatalf("父会话历史不该变：%d → %d", before, n)
	}
}

func TestAttentionReportToolPushesOnlyWhatMatters(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-tool")
	if _, err := e.a.SessionBranchAction("continue", sid, "查一下", "fake:u1"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	if in.SID != sid {
		t.Fatalf("在分支里跑：%s", in.SID)
	}
	e.a.mu.Lock()
	s := e.a.sessions[sid]
	if _, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "progress", "text": "开始查了"})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "waiting", "text": "在等网页"})); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	if sent := drainSent(e); len(sent) != 0 {
		t.Fatalf("progress、waiting 和普通回复不发：%v", sent)
	}
	e.a.mu.Lock()
	r, err := e.a.reportCall(s, reportArgs(map[string]any{"status": "blocked", "text": "缺一个密钥", "question": "密钥放哪？"}))
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_ = r
	got := e.ch.wait(t, "需要你回答")
	if !strings.Contains(got, "密钥放哪？") {
		t.Fatalf("通知：%q", got)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
}

func TestAttentionHostFailureCountsAsBlocked(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-host")
	e.a.mu.Lock()
	s := e.a.sessOf(sid)
	e.a.branchDone(s, &run{outcome: "error", final: "内核掉线了", asked: "查一下"})
	e.a.mu.Unlock()
	got := e.ch.wait(t, "· 受阻]")
	if !strings.Contains(got, "内核掉线了") || !strings.Contains(got, "/seen p:") {
		t.Fatalf("宿主记下的失败当作受阻发一条：%q", got)
	}
	// 编号就是“等我处理”里的编号
	p := pendingOf(e, commit.KindBranch, sid)
	if p == nil || !strings.Contains(got, "/seen "+p.Number) {
		t.Fatalf("编号对不上：%+v / %q", p, got)
	}
}

func TestAttentionBoundsDuplicatesRateAndQuietParent(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-bounds")
	push := func(state, text string) {
		e.a.mu.Lock()
		e.a.attentionPush(sid, state, text, "", "1")
		e.a.mu.Unlock()
	}
	push("ready", "第一版")
	push("ready", "第一版") // 和上一条一样：不发
	if sent := drainSent(e); len(sent) != 1 {
		t.Fatalf("相同的汇报不重复发：%v", sent)
	}
	push("blocked", "卡住了")
	push("ready", "第二版")
	push("blocked", "又卡住了") // 这一小时的第 4 条：不发
	push("ready", "第三版")
	sent := drainSent(e)
	if len(sent) != 2 {
		t.Fatalf("每小时最多 3 条（前面已经发了 1 条，这里再发 2 条）：%v", sent)
	}
	// 一小时以后又可以发
	e.a.mu.Lock()
	e.a.commits.Now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	e.a.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	e.a.mu.Unlock()
	push("ready", "过了一小时")
	if sent := drainSent(e); len(sent) != 1 {
		t.Fatalf("过了一小时可以再发：%v", sent)
	}
	// 记录先于入队：推送记录里有这些条目
	e.a.mu.Lock()
	n := len(e.a.commits.Notes(commit.KindBranch, sid))
	e.a.mu.Unlock()
	if n != commit.MaxNotes {
		t.Fatalf("推送记录留最近 %d 条：%d", commit.MaxNotes, n)
	}
}

func TestAttentionQuietParentGetsNothing(t *testing.T) {
	e := setup(t)
	parent := attentionBranch(t, e, "att-quiet-parent") // 父会话本身是一个 attention（安静）分支
	e.a.mu.Lock()
	e.a.eps["fake:c2"] = &epRec{Session: parent, Who: "fake:u1"}
	e.a.mu.Unlock()
	child, err := e.a.SessionFork(SessionForkRequest{SourceSID: parent, Title: "孙分支", RequestID: "att-grand", By: "fake:u1", Delivery: session.BranchDeliveryAttention})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.attentionPush(child.BranchSID, "ready", "好了", "", "1")
	notes := len(e.a.commits.Notes(commit.KindBranch, child.BranchSID))
	e.a.mu.Unlock()
	if sent := drainSent(e); len(sent) != 0 || notes != 0 {
		t.Fatalf("父会话是安静的：不发、也不记：%v %d", sent, notes)
	}
}

func TestAttentionNotePresentOnlyForAttentionBranches(t *testing.T) {
	e := setup(t)
	sid := attentionBranch(t, e, "att-note")
	plain, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "普通", RequestID: "att-note-plain", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	withNote, _, _, _ := e.a.contextParts(sid)
	without, _, _, _ := e.a.contextParts(plain.BranchSID)
	e.a.mu.Unlock()
	if !strings.Contains(withNote, "Report blocked 问一个具体问题") || strings.Contains(without, "Report blocked 问一个具体问题") {
		t.Fatalf("attention 分支多一句说明，其他不变：%v %v", strings.Contains(withNote, "Report blocked"), strings.Contains(without, "Report blocked"))
	}
	_ = fmt.Sprint
}

func TestSessionForkCommandAttention(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	e.say("/session fork --attention 查竞品")
	e.ch.wait(t, "会通知这个聊天")
	var got *session.BranchMeta
	for _, m := range e.a.store.Branches() {
		if m.Title == "查竞品" {
			m := m
			got = &m
		}
	}
	if got == nil || got.Delivery != session.BranchDeliveryAttention {
		t.Fatalf("/session fork --attention：%+v", got)
	}
}

// 模型通过 Session 工具只能要 silent 或 attention；report 一律当 silent，非管理员要 attention 也只得到 silent。
func TestAttentionToolDeliveryIsRestricted(t *testing.T) {
	if toolDelivery("report") != session.BranchDeliverySilent || toolDelivery("") != session.BranchDeliverySilent ||
		toolDelivery("attention") != session.BranchDeliveryAttention {
		t.Fatal("toolDelivery 的映射不对")
	}
	e := setup(t)
	withAgents(t, e)
	br, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "访客", RequestID: "att-guest", By: "fake:guest", Delivery: session.BranchDeliveryAttention})
	if err != nil {
		return // 访客根本分不了也算
	}
	e.a.mu.Lock()
	d := e.a.branchDelivery(br.BranchSID)
	e.a.mu.Unlock()
	if d != session.BranchDeliverySilent {
		t.Fatalf("非管理员只能是 silent：%q", d)
	}
}
