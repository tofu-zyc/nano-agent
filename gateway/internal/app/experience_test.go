package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// This model deliberately repeats any apparently unfinished request outside a
// separated, closed experience section. It makes the original accident structural.
type experienceGuardModel struct {
	mu   sync.Mutex
	last llm.Request
}

func (g *experienceGuardModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	g.mu.Lock()
	g.last = req
	g.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	res := &llm.Result{Reason: llm.ReasonFinal, Content: "[不做]", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10}}
	if last.Role == "tool" {
		res.Content = "整理好了，记了两条记忆。"
		return res, nil
	}
	text := last.Content
	if i := strings.Index(text, "【最近的经历】"); i >= 0 {
		text = text[:i]
	}
	if strings.Contains(text, "整理一下上下文") {
		res.Reason = llm.ReasonToolUse
		for _, title := range []string{"主人的工作习惯", "nano-agent 的进展"} {
			args, _ := json.Marshal(map[string]string{"action": "write", "title": title, "type": "事实", "body": "已确认的结论", "visible": "admin"})
			res.Calls = append(res.Calls, llm.ToolCall{ID: title, Type: "function", Function: llm.FunctionCall{Name: "Knowledge", Arguments: string(args)}})
		}
	}
	return res, nil
}
func (g *experienceGuardModel) request() llm.Request { g.mu.Lock(); defer g.mu.Unlock(); return g.last }

func TestP6bContextMemoryResetDoesNotWakeOrRepeat(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	setNow(e, base)
	g := &experienceGuardModel{}
	e.a.mu.Lock()
	e.a.llm = g
	e.a.lifeOf("default").Next = base.Add(4 * time.Hour).Unix()
	e.a.mind("default").SetNextCheck(base.Add(4 * time.Hour))
	e.a.mu.Unlock()
	e.say("整理一下上下文，我要重置会话了")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	turn := e.k.expect(proto.TTurn)
	if len(turn.Calls) != 2 {
		t.Fatalf("fixture did not request both memories: %+v", turn)
	}
	for _, c := range turn.Calls {
		e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: c.ID, Name: c.Name, Args: c.Args})
		result := e.k.expect(proto.TExecResult)
		if result.OK == nil || !*result.OK {
			t.Fatalf("memory write failed: %+v", result)
		}
		e.k.send(proto.Msg{T: proto.TResult, SID: mainSID, ID: c.ID, OK: result.OK, Content: result.Content})
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	waitIdle(t, e, mainSID)
	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	setNow(e, base.Add(11*time.Minute))
	e.a.autonomyTick()
	e.k.none()
	if err := e.a.Wake("default"); err != nil {
		t.Fatal(err)
	}
	sid := lifeSID("default")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	turn = e.k.expect(proto.TTurn)
	if len(turn.Calls) != 0 || turn.Reason != proto.ReasonFinal {
		t.Fatalf("repeated completed work: %+v", turn)
	}
	req := g.request()
	brief := lastUser(req.Messages)
	for _, want := range []string{"【要处理的】", "【最近的经历】", "已结束", "清空了聊天上下文", "主人的工作习惯", "nano-agent 的进展"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief missing %q: %s", want, brief)
		}
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	if err := e.a.Wake("default"); err != nil {
		t.Fatal(err)
	}
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	second := g.request()
	if len(second.Messages) != 2 || second.Messages[0].Content != req.Messages[0].Content {
		t.Fatalf("episode carries old dialog/prefix changed: %+v", second.Messages)
	}
	if !strings.Contains(lastUser(second.Messages), "[life]") {
		t.Fatal("new episode lacks previous episode experience")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	e.a.mu.Lock()
	if len(e.a.store.Archive(sid)) != 4 {
		t.Fatal("raw episode history lost")
	}
	note := e.a.experienceNote(mainSID, "fake:u1")
	if strings.Contains(note, "整理一下上下文") || strings.Contains(note, "清空了聊天上下文") {
		t.Fatal("reset chat was rehydrated through experience")
	}
	e.a.mu.Unlock()
}

func TestBranchRunLeavesAnAdminOnlyExperience(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	setNow(e, base)
	meta := session.BranchMeta{SID: "br-exp", ParentSID: mainSID, Title: "一件事", Status: session.BranchPreparing, Delivery: session.BranchDeliverySilent}
	if err := e.a.store.BeginBranch(meta); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	s := e.a.sessOf("br-exp")
	r := &run{runID: "r-branch", from: "fake:u1", asked: "把这件事接着做完", start: base}
	e.a.recordRunExperience(s, r, proto.StatusOK, "分支里做完了，结论放在这里")
	rows := e.a.experienceOf("default").Recent(knowledge.Viewer{Admin: true}, "", 10, base)
	public := e.a.experienceOf("default").Recent(knowledge.Viewer{Who: "fake:u9"}, "", 10, base)
	e.a.mu.Unlock()
	found := false
	for _, row := range rows {
		if row.Kind == "branch" && row.SID == "br-exp" && strings.Contains(row.Said, "结论") {
			found = true
		}
	}
	if !found {
		t.Fatalf("分支没有留下经历：%+v", rows)
	}
	for _, row := range public {
		if row.SID == "br-exp" {
			t.Fatal("分支经历漏给了非主人", row)
		}
	}
	// 什么都没说的分支轮次不记，避免给每次空跑都留一条
	e.a.mu.Lock()
	e.a.recordRunExperience(s, &run{runID: "r-empty", from: "fake:u1", start: base}, proto.StatusOK, "[不回复]")
	empty := e.a.experienceOf("default").Recent(knowledge.Viewer{Admin: true}, "", 10, base)
	e.a.mu.Unlock()
	for _, row := range empty {
		if row.RunID == "r-empty" {
			t.Fatal("空的分支轮次也记了经历", row)
		}
	}
}
