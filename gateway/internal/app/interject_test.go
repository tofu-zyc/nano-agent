package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
)

// gatedLLM 把每次模型请求卡在这里：用来模拟“正在生成”的那段时间，测试在这中间发插话。
type gatedLLM struct {
	mu   sync.Mutex
	reqs []llm.Request
	gate chan *llm.Result
}

func newGatedLLM() *gatedLLM {
	return &gatedLLM{gate: make(chan *llm.Result)}
}

func (g *gatedLLM) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Result, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	g.mu.Unlock()
	select {
	case res := <-g.gate:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release 放行第 n 次（从 1 开始）请求，返回它看到的请求。
func (g *gatedLLM) release(t *testing.T, res *llm.Result) llm.Request {
	t.Helper()
	g.gate <- res
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reqs[len(g.reqs)-1]
}

// waitCalls 等累计第 n 次请求真的进到模型这边
func (g *gatedLLM) waitCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		got := len(g.reqs)
		g.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("模型请求没有来（等到累计第 %d 次）", n)
}

func (g *gatedLLM) last() llm.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reqs) == 0 {
		return llm.Request{}
	}
	return g.reqs[len(g.reqs)-1]
}

// 模型正在生成时收到插话：等这次生成完整结束、用量只算一次，然后注入并重新请求；
// 这批还没交给内核的工具调用不再派发。
func TestInterjectionWhileGeneratingReplansWithoutDispatching(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.Features = map[string]int{"user_interjection": 1}
	e.a.mu.Unlock()
	g := newGatedLLM()
	e.a.SetLLM(g)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	g.waitCalls(t, 1)
	// 生成还没结束，这时候来了一条补充
	e.say("补充：只改 Linux", msgID("m2"))
	e.ch.wait(t, "已收到插话，会等这次生成结束")
	e.k.none() // 不能因为插话就提前结束这次生成，也不能开第二个请求
	// 第一次生成结束：它提出了一批工具调用
	g.release(t, &llm.Result{Reason: llm.ReasonToolUse, Content: "我先看看",
		Calls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "echo", Arguments: "{}"}}}})
	// 网关不应该把这批调用交给内核，而是先插入插话、把重新请求交给内核
	y := e.k.expect(proto.TModelYield)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: y.TurnID + 1})
	g.waitCalls(t, 2)
	req := g.last()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "[用户插话]") || !strings.Contains(last.Content, "只改 Linux") {
		t.Fatalf("重新规划时没有看到插话：%+v", req.Messages)
	}
	// 未接受的 tool_calls 不写进正式历史，也不会留下孤立的工具调用
	for _, m := range e.a.store.History(e.sid) {
		if len(m.ToolCalls) > 0 {
			t.Fatalf("未接受的工具调用进了历史：%+v", m)
		}
	}
	// 第二次生成是最终回答：这次照常交给内核
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "好的，只改 Linux"})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonFinal {
		t.Fatalf("turn: %+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "好的，只改 Linux")
}

// 模型生成最终回答时还有插话：正文保留、run 不结束，下一次请求能看到带标记的补充。
func TestInterjectionOnFinalAnswerKeepsAnswerAndContinues(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.Features = map[string]int{"user_interjection": 1}
	e.a.mu.Unlock()
	g := newGatedLLM()
	e.a.SetLLM(g)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	g.waitCalls(t, 1)
	e.say("再补一句", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "第一次的结论"})
	y := e.k.expect(proto.TModelYield)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: y.TurnID + 1})
	g.waitCalls(t, 2)
	req := g.last()
	if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "再补一句") {
		t.Fatalf("最终回答之后没有注入插话：%+v", last)
	}
	var sawAnswer bool
	for _, m := range req.Messages {
		if m.Role == "assistant" && strings.Contains(m.Content, "第一次的结论") {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Fatalf("已经展示过的正文没有留在上下文里：%+v", req.Messages)
	}
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "结合补充的结论"})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "结合补充的结论")
}

// 这一轮异常收尾时，还没插入的补充放回会话队列，交给后续 run，不丢。
func TestInterjectionRequeuedWhenRunEnds(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	// 这一轮直接异常结束（比如内核断了），插话还没到边界
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusError, Detail: "内核出错"})
	e.ch.wait(t, "没有处理你后来的 1 条补充")
	in := e.k.expect(proto.TInput)
	if in.SID != e.sid {
		t.Fatalf("input: %+v", in)
	}
	msgs := e.roundTrip(t)
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, "补充") {
		t.Fatalf("补充没有在下一轮处理：%+v", last)
	}
}

// 另一位操作者的消息不进本轮收件箱，仍按原来的排队规则。
func TestInterjectionOnlyForInitiator(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("别人的话", msgID("m2"), from("u2", "李四"))
	e.k.none()
	e.a.mu.Lock()
	pending := len(e.a.sessions[e.sid].run.inbox)
	queued := len(e.a.sessions[e.sid].queue)
	e.a.mu.Unlock()
	if pending != 0 || queued != 1 {
		t.Fatalf("pending=%d queued=%d", pending, queued)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	if in := e.k.expect(proto.TInput); in.From != "fake:u2" {
		t.Fatalf("input: %+v", in)
	}
}

// 收件箱有界：满了明确回执未受理，不静默截断。
func TestInterjectionInboxIsBounded(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m0"))
	e.k.expect(proto.TInput)
	for i := 0; i < maxInterjections; i++ {
		e.say("补充", msgID("x"+string(rune('a'+i))))
	}
	e.ch.wait(t, "已收到插话")
	e.say("再来一条", msgID("overflow"))
	e.ch.wait(t, "没有受理")
	e.a.mu.Lock()
	n := len(e.a.sessions[e.sid].run.inbox)
	e.a.mu.Unlock()
	if n != maxInterjections {
		t.Fatalf("收件箱条数：%d", n)
	}
}

// 已经注入的插话被撤回时不截断仍有待返回工具结果的历史，按更正说明处理。
func TestRecallInjectedInterjectionIsCorrection(t *testing.T) {
	e := setup(t)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn) // 已经写进历史
	// 平台上撤回了这条补充：不截断仍有待返回工具结果的历史，只说明
	e.say("", func(m *channel.Message) { m.Text, m.MsgID, m.SenderID, m.Action = "", "m2", "", channel.ActionRecall })
	e.ch.wait(t, "已经插进正在进行的任务")
}

// 内核认 model_yield 时，插话后的重新请求由内核发起（步数预算归内核管），网关不自己再开一次。
func TestInterjectionYieldGoesThroughKernel(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.Features = map[string]int{"user_interjection": 1}
	e.a.mu.Unlock()
	g := newGatedLLM()
	e.a.SetLLM(g)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	g.waitCalls(t, 1)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "第一次的结论"})
	y := e.k.expect(proto.TModelYield)
	if y.SID != e.sid || y.TurnID != 1 {
		t.Fatalf("model_yield 的身份：%+v", y)
	}
	// 网关没有自己再请求一次模型：等内核决定
	time.Sleep(100 * time.Millisecond)
	g.mu.Lock()
	n := len(g.reqs)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("网关不该自己重新请求模型：%d 次", n)
	}
	// 内核按自己的预算再要一次模型
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 2})
	g.waitCalls(t, 2)
	req := g.last()
	if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "补充") {
		t.Fatalf("第二次请求没有看到插话：%+v", last)
	}
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "结合补充的结论"})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "结合补充的结论")
}

// 完整调用身份：别的 run 的 done 不能结束当前 run，上一轮的迟到结果也不能写进历史。
func TestStaleKernelMessagesIgnored(t *testing.T) {
	e := setup(t)
	e.say("第一轮", msgID("m1"))
	in := e.k.expect(proto.TInput)
	if in.RunID == "" {
		t.Fatal("input 没有带 run_id")
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, RunID: in.RunID, TurnID: 1})
	turn := e.k.expect(proto.TTurn)
	if turn.RunID != in.RunID || turn.TurnID != 1 {
		t.Fatalf("turn 没带完整身份：%+v", turn)
	}
	// 另一个 run 的 done：当前这一轮必须还活着
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, RunID: "another-run", Status: proto.StatusOK})
	time.Sleep(80 * time.Millisecond)
	e.a.mu.Lock()
	alive := e.a.sessions[e.sid].run != nil
	e.a.mu.Unlock()
	if !alive {
		t.Fatal("别的 run 的 done 结束了当前 run")
	}
	// 上一轮的迟到结果：不写进历史
	e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, RunID: in.RunID, TurnID: 7, ID: "late",
		OK: proto.Bool(true), Content: "迟到的结果"})
	time.Sleep(80 * time.Millisecond)
	for _, m := range e.a.store.History(e.sid) {
		if m.Role == "tool" && strings.Contains(m.Content, "迟到的结果") {
			t.Fatalf("迟到的结果进了历史：%+v", m)
		}
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, RunID: in.RunID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：第一轮")
}

// 旧节点不认识 model_yield：不能在网关里自己重开模型请求（那会绕过内核的步数预算），
// 退回原来的行为——补充放回会话队列，这一轮按正常结果收尾。
func TestInterjectionOldNodeFallsBackToQueue(t *testing.T) {
	e := setup(t)
	g := newGatedLLM()
	e.a.SetLLM(g)
	e.say("任务", msgID("m1"))
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	g.waitCalls(t, 1)
	e.say("补充", msgID("m2"))
	e.ch.wait(t, "已收到插话")
	g.release(t, &llm.Result{Reason: llm.ReasonFinal, Content: "第一次的结论"})
	// 没有 model_yield，这一轮照常交给内核
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonFinal {
		t.Fatalf("turn: %+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "已经放回待处理")
	// 补充作为下一轮的用户消息重新开始（不丢）
	in := e.k.expect(proto.TInput)
	if in.SID != e.sid {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid, TurnID: 1})
	g.waitCalls(t, 2)
	if last := g.last().Messages[len(g.last().Messages)-1]; !strings.Contains(last.Content, "补充") {
		t.Fatalf("补充没有在下一轮处理：%+v", last)
	}
}
