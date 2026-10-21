package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// ---- 测试替身 ----

type fakeCh struct {
	name string
	sent chan string
}

func (f *fakeCh) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}
func (f *fakeCh) Run(ctx context.Context, _ channel.Handler) error { <-ctx.Done(); return nil }
func (f *fakeCh) Send(_ context.Context, _, text, _ string) error  { f.sent <- text; return nil }

func (f *fakeCh) wait(t *testing.T, substr string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case s := <-f.sent:
			if strings.Contains(s, substr) {
				return s
			}
		case <-deadline:
			t.Fatalf("没等到包含 %q 的回复", substr)
		}
	}
}

// recLLM 包一层 Mock，记下最后一次请求
type recLLM struct {
	llm.Mock
	mu   sync.Mutex
	last llm.Request
}

func (r *recLLM) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Result, error) {
	r.mu.Lock()
	r.last = req
	r.mu.Unlock()
	return r.Mock.Chat(ctx, req, onDelta)
}

func (r *recLLM) lastReq() llm.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

type fakeKernel struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
}

func (k *fakeKernel) send(m proto.Msg) {
	b, _ := json.Marshal(m)
	k.conn.Write(append(b, '\n'))
}

func (k *fakeKernel) recv(d time.Duration) (proto.Msg, bool) {
	k.conn.SetReadDeadline(time.Now().Add(d))
	if !k.sc.Scan() {
		return proto.Msg{}, false
	}
	var m proto.Msg
	json.Unmarshal(k.sc.Bytes(), &m)
	return m, true
}

func (k *fakeKernel) expect(typ string) proto.Msg {
	k.t.Helper()
	m, ok := k.recv(3 * time.Second)
	if !ok || m.T != typ {
		k.t.Fatalf("期望内核收到 %s，实际 %+v（ok=%v）", typ, m, ok)
	}
	return m
}

func (k *fakeKernel) none() {
	k.t.Helper()
	if m, ok := k.recv(200 * time.Millisecond); ok {
		k.t.Fatalf("不该收到消息：%+v", m)
	}
	// 读超时会让 Scanner 进入错误状态，换一个新的
	k.conn.SetReadDeadline(time.Time{})
	k.sc = bufio.NewScanner(k.conn)
}

type env struct {
	a   *App
	ch  *fakeCh
	k   *fakeKernel
	llm *recLLM
	dir string
	sid string // 测试用的窗口 fake:c1 接着的会话：管理员默认接在主会话上
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "personas"), 0o755)
	os.WriteFile(filepath.Join(dir, "personas", "default.md"), []byte("你是测试机器人"), 0o644)
	sock := filepath.Join(dir, "k.sock")
	cfg := &config.Config{
		Kernel:    config.Kernel{Listen: "unix:" + sock, Token: "tok"},
		LLM:       config.LLM{Provider: "mock", MaxHistoryTokens: 10000, TrimTo: 0.5},
		Persona:   config.Persona{Dir: filepath.Join(dir, "personas")},
		Access:    config.Access{Admins: []string{"fake:u1"}, Guest: "none"},
		Workspace: config.Workspace{Mode: "ask"},
		Dir:       filepath.Join(dir, "conf"),
	}
	store, err := session.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &recLLM{}
	a := New(ctx, cfg, rec, store, persona.New(cfg.Persona.Dir, log), log)
	a.now = func() time.Time { return time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC) }
	a.loc = time.UTC
	hub := kernel.NewHub("tok", a, log)
	a.SetHub(hub)
	ln, err := kernel.Listen(cfg.Kernel.Listen)
	if err != nil {
		t.Fatal(err)
	}
	go hub.Serve(ctx, ln)
	ch := &fakeCh{sent: make(chan string, 100)}
	a.AddChannel(ch)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{t: t, conn: conn, sc: bufio.NewScanner(conn)}
	k.send(proto.Msg{T: proto.THello, V: proto.Version, Token: "tok", Node: "test",
		Tools: []proto.ToolSpec{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	k.expect(proto.TWelcome)
	for !hub.Connected() {
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() { cancel(); conn.Close() })
	return &env{a: a, ch: ch, k: k, llm: rec, dir: dir, sid: mainSID}
}

func (e *env) say(text string, mod ...func(*channel.Message)) {
	m := channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: text, MsgID: "m1", Mentioned: true}
	for _, f := range mod {
		f(&m)
	}
	e.a.OnMessage(e.ch, m)
}

func group(mentioned bool) func(*channel.Message) {
	return func(m *channel.Message) { m.Group, m.Mentioned = true, mentioned }
}

func from(id, name string) func(*channel.Message) {
	return func(m *channel.Message) { m.SenderID, m.SenderName = id, name }
}

// ---- 用例 ----

const stamp = "[2026-10-21 周三 15:08 · fake]"

func TestFinalAnswer(t *testing.T) {
	e := setup(t)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	if in.SID != mainSID || in.From != "fake:u1" {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonFinal {
		t.Fatalf("turn: %+v", turn)
	}
	req := e.llm.lastReq()
	// 没有工作目录：只聊天，不给模型工具
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "你是测试机器人" || len(req.Tools) != 0 {
		t.Fatalf("request: %+v", req)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：你好")
}

func TestToolFlowWritesResultsIntoContext(t *testing.T) {
	e := setup(t)
	e.say(`!call echo {"a":1}`)
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	turn := e.k.expect(proto.TTurn)
	if turn.Reason != proto.ReasonToolUse || len(turn.Calls) != 1 || string(turn.Calls[0].Args) != `{"a":1}` {
		t.Fatalf("turn: %+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, ID: turn.Calls[0].ID, OK: proto.Bool(true), Content: "回显成功"})
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	msgs := e.llm.lastReq().Messages
	if last := msgs[len(msgs)-1]; last.Role != "tool" || last.ToolCallID != turn.Calls[0].ID || last.Content != "回显成功" {
		t.Fatalf("tool result not in context: %+v", last)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "工具结果：回显成功")
}

func TestInvalidToolArgsAreForwardedAsString(t *testing.T) {
	e := setup(t)
	e.say(`!call echo {broken`)
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	turn := e.k.expect(proto.TTurn)
	var s string
	if json.Unmarshal(turn.Calls[0].Args, &s) != nil || s != "{broken" {
		t.Fatalf("args: %s", turn.Calls[0].Args)
	}
}

func TestGroupIgnored(t *testing.T) {
	e := setup(t)
	e.say("随便聊聊", group(false))
	e.say("@机器人 几点了", group(true))
	e.ch.wait(t, "暂时只支持私聊")
	e.say("@机器人 在吗", group(true)) // 只提醒一次
	e.k.none()
	select {
	case m := <-e.ch.sent:
		t.Fatalf("不该再回复：%q", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestToolsOnlyWithWorkdir(t *testing.T) {
	e := setup(t)
	e.a.cfg.Workspace.Dirs = []string{e.dir}
	e.say("看看目录")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk || len(in.Dirs) != 1 {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	if n := len(e.llm.lastReq().Tools); n != 1 {
		t.Fatalf("有工作目录时应该提供工具：%d", n)
	}
}

func TestQueuedMessagesFromSameSenderAreMerged(t *testing.T) {
	e := setup(t)
	e.say("第一条")
	e.k.expect(proto.TInput)
	e.say("第二条")
	e.say("第三条")
	e.say("别人的", from("u2", "李四"))
	e.k.none() // 上一轮没结束，不能开新 run
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	in := e.k.expect(proto.TInput)
	if in.From != "fake:u1" {
		t.Fatalf("second run from: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	msgs := e.llm.lastReq().Messages
	if got := msgs[len(msgs)-1].Content; got != stamp+" 第二条\n第三条" {
		t.Fatalf("merged: %q", got)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	if in := e.k.expect(proto.TInput); in.From != "fake:u2" {
		t.Fatalf("third run from: %+v", in)
	}
}

func TestStopAndReset(t *testing.T) {
	e := setup(t)
	e.say("/stop")
	e.ch.wait(t, "没有进行中的任务")

	e.say("一个长任务")
	e.k.expect(proto.TInput)
	e.say("/stop")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "（已停止）")

	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	if h := e.a.store.History(e.sid); len(h) != 0 {
		t.Fatalf("history not cleared: %+v", h)
	}
}

func TestResetWhileRunning(t *testing.T) {
	e := setup(t)
	e.say("任务")
	e.k.expect(proto.TInput)
	e.say("/reset")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "上下文已清空")
	if h := e.a.store.History(e.sid); len(h) != 0 {
		t.Fatalf("history not cleared: %+v", h)
	}
}

func TestApproval(t *testing.T) {
	e := setup(t)
	e.say("帮我执行命令")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: e.sid, ID: "call_9", Name: "Shell",
		Args: json.RawMessage(`{"command":"ls -la"}`), From: "fake:u1"})
	msg := e.ch.wait(t, "需要审批 #1")
	if !strings.Contains(msg, "执行命令：\nls -la") || !strings.Contains(msg, "/approve 1") {
		t.Fatalf("approval prompt: %s", msg)
	}
	e.say("/approve 2")
	e.ch.wait(t, "没有这个编号")
	e.say("/approve #1", from("u2", "李四")) // 不是管理员：网关直接拒绝，审批单仍然有效
	e.ch.wait(t, "只有管理员能审批")
	e.k.none()
	e.say("/approve #1")
	ap := e.k.expect(proto.TApprove)
	if ap.ID != "call_9" || ap.From != "fake:u1" || ap.OK == nil || !*ap.OK {
		t.Fatalf("approve: %+v", ap)
	}
	e.say("/deny 1")
	if d := e.k.expect(proto.TApprove); d.OK == nil || *d.OK {
		t.Fatalf("deny: %+v", d)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, ID: "call_9", OK: proto.Bool(false), Content: "管理员拒绝执行"})
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.say("/approve 1")
	e.ch.wait(t, "没有这个编号") // 已经有结果了，审批单作废
}

func TestKernelDownAbortsRuns(t *testing.T) {
	e := setup(t)
	e.say("任务")
	e.k.expect(proto.TInput)
	e.k.conn.Close()
	e.ch.wait(t, "内核连接断开")
	e.say("还在吗")
	e.ch.wait(t, "内核没有连接")
}

func TestWhoamiAndAllowList(t *testing.T) {
	e := setup(t)
	e.say("/whoami")
	if msg := e.ch.wait(t, "你的身份"); !strings.Contains(msg, "fake:u1") || !strings.Contains(msg, "fake:c1") {
		t.Fatalf("whoami: %s", msg)
	}
	e.a.cfg.Access.AllowChats = []string{"fake:other"}
	e.say("你好")
	e.k.none()
	e.say("/whoami") // 不在白名单也能查身份，方便配置
	e.ch.wait(t, "你的身份")
}

func lastUser(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// 跑完一轮：内核要模型，模型回复，内核结束
func (e *env) roundTrip(t *testing.T) []llm.Message {
	t.Helper()
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	msgs := e.llm.lastReq().Messages
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）") // 等回复发出来，确认这一轮已经结束
	return msgs
}

func TestWorkspaceAndModes(t *testing.T) {
	e := setup(t)
	work := filepath.Join(e.dir, "work")
	os.MkdirAll(work, 0o755)

	// 没设置工作目录的纯聊天会话：不加环境说明
	e.say("你好")
	in := e.k.expect(proto.TInput)
	if in.Mode != proto.ModeNone || len(in.Dirs) != 0 { // 没有工作目录：不能用工具
		t.Fatalf("input: %+v", in)
	}
	// 没有工作目录：告诉模型它没有工具
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 这个会话没有工作目录") || !strings.HasSuffix(got, "\n"+stamp+" 你好") {
		t.Fatalf("user content: %q", got)
	}

	e.say("/dir add " + work)
	e.ch.wait(t, "1. "+work)
	e.say("/mode auto")
	e.ch.wait(t, "已切换为自动")
	e.say("看看目录")
	in = e.k.expect(proto.TInput)
	if in.Mode != proto.ModeAuto || len(in.Dirs) != 1 || in.Dirs[0] != work {
		t.Fatalf("input: %+v", in)
	}
	msgs := e.roundTrip(t)
	want := "[系统] 工作目录：" + work + "（默认，相对路径和命令都从这里开始）。权限模式：自动，"
	if got := lastUser(msgs); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\n"+stamp+" 看看目录") {
		t.Fatalf("env note: %q", got)
	}
	// 系统提示词里不放会变的东西：前缀缓存不会因为换目录而失效
	if msgs[0].Role != "system" || msgs[0].Content != "你是测试机器人" {
		t.Fatalf("system prompt changed: %q", msgs[0].Content)
	}

	// 环境没变就不重复说明
	e.say("再看看")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); got != stamp+" 再看看" {
		t.Fatalf("user content: %q", got)
	}

	// 只读目录、换默认目录
	other := filepath.Join(e.dir, "docs")
	e.say("/dir add " + other + " ro")
	e.ch.wait(t, other+"（只读）")
	e.say("/dir")
	e.ch.wait(t, "权限模式：自动")
	e.say("继续")
	in = e.k.expect(proto.TInput)
	if !slices.Contains(in.RO, other) {
		t.Fatalf("ro: %+v", in.RO)
	}
	if got := lastUser(e.roundTrip(t)); !strings.Contains(got, "只读目录："+other) {
		t.Fatalf("env note: %q", got)
	}

	// /reset 之后历史是空的，环境说明要重新给一次
	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	e.say("新的开始")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 工作目录：") {
		t.Fatalf("after reset: %q", got)
	}
}

func TestWorkspaceGuards(t *testing.T) {
	e := setup(t)
	e.say("/dir add relative/path")
	e.ch.wait(t, "绝对路径")
	e.say("/dir add " + e.dir) // 包含网关的配置和数据目录
	e.ch.wait(t, "不能把")
	e.say("/mode yolo")
	e.ch.wait(t, "模式只能是")
	e.say("/dir add /tmp", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能修改工作目录")
	e.say("/mode auto", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能切换权限模式")
	e.say("/dir rm /nope")
	e.ch.wait(t, "不在工作目录列表里")
}

func TestGuestMode(t *testing.T) {
	e := setup(t)
	e.sid = "fake:c1" // 不是管理员：有自己的会话
	e.a.cfg.Workspace.Dirs = []string{e.dir}
	e.say("你好", from("u2", "李四"))
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeNone || in.SID != e.sid {
		t.Fatalf("guest mode: %+v", in)
	}
	e.roundTrip(t)
	e.a.cfg.Access.Guest = "readonly"
	e.say("你好", from("u2", "李四"))
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeReadonly {
		t.Fatalf("guest mode: %+v", in)
	}
}

// ---- 流式显示 ----

type viewEvent struct{ kind, text string }

type streamCh struct {
	fakeCh
	mu     sync.Mutex
	events []viewEvent
}

func (c *streamCh) OpenRun(_, _ string) channel.RunView { return &recView{c: c} }

func (c *streamCh) log(kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.events); n > 0 && c.events[n-1].kind == kind && (kind == "content" || kind == "reasoning") {
		c.events[n-1].text += text // 相邻的增量合在一起，方便断言
		return
	}
	c.events = append(c.events, viewEvent{kind, text})
}

func (c *streamCh) snapshot() []viewEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

type recView struct{ c *streamCh }

func (v *recView) Reasoning(t string) { v.c.log("reasoning", t) }
func (v *recView) Content(t string)   { v.c.log("content", t) }
func (v *recView) EndMessage()        { v.c.log("end", "") }
func (v *recView) Tool(n, d string)   { v.c.log("tool", n+" "+d) }
func (v *recView) ToolResult(ok bool, d string) {
	v.c.log("result", fmt.Sprint(ok)+" "+d)
}
func (v *recView) Note(t string) { v.c.log("note", t) }
func (v *recView) Close()        { v.c.log("close", "") }

func setupStream(t *testing.T) (*env, *streamCh) {
	e := setup(t)
	sc := &streamCh{fakeCh: fakeCh{sent: make(chan string, 100)}}
	e.a.AddChannel(sc)
	return e, sc
}

func (e *env) sayOn(ch channel.Channel, text string) {
	e.a.OnMessage(ch, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: text, MsgID: "m1", Mentioned: true})
}

func TestStreamingView(t *testing.T) {
	e, sc := setupStream(t)
	e.a.cfg.LLM.ThinkingLevels = []string{"none", "high"}
	e.sayOn(sc, "/think high")
	sc.wait(t, "已切换为 high")

	e.sayOn(sc, `!call Shell {"command":"ls -la"}`)
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	turn := e.k.expect(proto.TTurn)
	if e.llm.lastReq().Thinking != "high" {
		t.Fatalf("thinking: %q", e.llm.lastReq().Thinking)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: e.sid, ID: turn.Calls[0].ID, OK: proto.Bool(true), Content: "a.txt\nb.txt\n"})
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusOK})

	deadline := time.Now().Add(3 * time.Second)
	for {
		ev := sc.snapshot()
		if n := len(ev); n > 0 && ev[n-1].kind == "close" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没等到 close：%+v", sc.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev := sc.snapshot()
	kinds := ""
	for _, x := range ev {
		kinds += x.kind + ","
	}
	want := "reasoning,end,tool,result,reasoning,content,end,note,close,"
	if kinds != want {
		t.Fatalf("events:\n got %s\nwant %s\n%+v", kinds, want, ev)
	}
	if ev[2].text != "Shell ls -la" || ev[3].text != "true a.txt（共 2 行）" || ev[5].text != "（mock）工具结果：a.txt\nb.txt\n" {
		t.Fatalf("details: %+v", ev)
	}
	if !strings.Contains(ev[7].text, "2 次模型调用") || !strings.Contains(ev[7].text, "缓存命中 90%") {
		t.Fatalf("usage: %q", ev[7].text)
	}
	// 正文已经流式显示过，不会再用 Send 发一遍
	select {
	case m := <-sc.sent:
		t.Fatalf("不该再发：%q", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestThinkCommand(t *testing.T) {
	e := setup(t)
	e.a.cfg.LLM.ThinkingLevels = []string{"none", "low", "high"}
	e.say("/think")
	e.ch.wait(t, "思考强度：模型默认")
	e.say("/think max")
	e.ch.wait(t, "只能是 none、low、high")
	e.say("/think low", from("u2", "李四"))
	e.ch.wait(t, "只有管理员")
	e.say("/think low")
	e.ch.wait(t, "已切换为 low")
	e.say("问题")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	if got := e.llm.lastReq().Thinking; got != "low" {
		t.Fatalf("thinking: %q", got)
	}
}

func TestStamp(t *testing.T) {
	e := setup(t)
	if got := e.a.stamp("fake"); got != stamp {
		t.Fatalf("stamp: %q", got)
	}
	off := false
	e.a.cfg.Session.Timestamps = &off
	e.say("你好")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); !strings.HasSuffix(got, "\n你好") || strings.Contains(got, "[2026") {
		t.Fatalf("timestamps off: %q", got)
	}
}

// 历史超过上限：一次裁掉一大段，之后的请求又是只追加
func TestCompactionKeepsRequestsAppendOnly(t *testing.T) {
	e := setup(t)
	e.a.cfg.LLM.MaxHistoryTokens = 600
	long := strings.Repeat("长", 100) // 约 100 token
	var prev []llm.Message
	compactions := 0
	for i := 0; i < 12; i++ {
		e.say(long)
		e.k.expect(proto.TInput)
		msgs := e.roundTrip(t)
		if prev != nil && !reflect.DeepEqual(msgs[:min(len(prev), len(msgs))], prev[:min(len(prev), len(msgs))]) {
			compactions++
			if !strings.HasPrefix(msgs[1].Content, "[系统] 更早的对话因为太长已经省略。") {
				t.Fatalf("裁剪后缺少说明：%q", msgs[1].Content[:60])
			}
		}
		prev = msgs
	}
	// 每轮约 230 token，上限 600、裁到 300：12 轮里应该只裁几次，而不是每轮都裁
	if compactions == 0 || compactions > 5 {
		t.Fatalf("裁剪次数：%d", compactions)
	}
}
