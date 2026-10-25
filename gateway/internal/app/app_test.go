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
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
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
	mu    sync.Mutex
	last  llm.Request
	fixed string // 不为空时模型就回这句（测试“只回 [不回复]”之类）
}

func (r *recLLM) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Result, error) {
	r.mu.Lock()
	r.last = req
	fixed := r.fixed
	r.mu.Unlock()
	if fixed != "" {
		return &llm.Result{Reason: llm.ReasonFinal, Content: fixed, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10}}, nil
	}
	return r.Mock.Chat(ctx, req, onDelta)
}

func (r *recLLM) say(text string) {
	r.mu.Lock()
	r.fixed = text
	r.mu.Unlock()
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
	a     *App
	ch    *fakeCh
	k     *fakeKernel
	llm   *recLLM
	hosts *fakeHosts
	dir   string
	sid   string // 测试用的窗口 fake:c1 接着的会话：管理员默认接在主会话上
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
		Skills:    config.Skills{Dir: filepath.Join(dir, "skills")},
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
	hosts := newFakeHosts()
	a.SetHosts(hosts)
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
	waitNode(a, "test", true)
	t.Cleanup(func() { cancel(); conn.Close() })
	return &env{a: a, ch: ch, k: k, llm: rec, hosts: hosts, dir: dir, sid: mainSID}
}

// waitNode 等调度层知道内核 name 上线（或下线）了
func waitNode(a *App, name string, online bool) {
	for i := 0; i < 3000; i++ {
		a.mu.Lock()
		ok := (a.nodes[name] != nil) == online
		a.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	panic("等不到内核 " + name)
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
	// 没有工作目录：HOME 模式，工具仍然给；管理员还有 Gateway / GatewayAdmin
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "你是测试机器人" || len(req.Tools) != 5 ||
		req.Tools[0].Function.Name != "Gateway" || req.Tools[1].Function.Name != "GatewayAdmin" || req.Tools[2].Function.Name != "Message" ||
		req.Tools[3].Function.Name != "Schedule" || req.Tools[4].Function.Name != "echo" {
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

func TestHomeWithoutWorkspace(t *testing.T) {
	e := setup(t)
	// 换一套带读写工具的真实工具列表；没有工作目录时工具都给，写入靠 elevated 审批
	e.k.send(proto.Msg{T: proto.TTools, Tools: []proto.ToolSpec{
		{Name: "Read", Description: "读", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "Grep", Description: "搜", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "Write", Description: "写", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "Shell", Description: "命令", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}})
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Tools) == 4 })
	e.say("看看")
	in := e.k.expect(proto.TInput)
	if in.Mode != proto.ModeAsk || !in.Elevate || len(in.Workspaces) != 1 || in.Workspaces[0].Name != "HOME" || !in.Workspaces[0].Home {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	var names []string
	for _, tool := range e.llm.lastReq().Tools {
		names = append(names, tool.Function.Name)
	}
	for _, want := range []string{"Grep", "Read", "Shell", "Write"} {
		if !slices.Contains(names, want) {
			t.Fatalf("HOME 模式缺工具 %s：%v", want, names)
		}
	}
}

func TestToolsOnlyWithWorkdir(t *testing.T) {
	e := setup(t)
	e.useDir(t, "work")
	e.say("看看目录")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk || len(in.Workspaces) != 1 {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	e.k.expect(proto.TTurn)
	if n := len(e.llm.lastReq().Tools); n != 5 {
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

// btnCh 是能用按钮审批的渠道（像 Telegram、飞书）
type btnCh struct {
	fakeCh
	mu       sync.Mutex
	cards    []channel.ApprovalCard
	resolved map[string]string // ref → 结果
}

func (b *btnCh) SendApproval(_ context.Context, _ string, c channel.ApprovalCard) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cards = append(b.cards, c)
	return fmt.Sprintf("ref%d", len(b.cards)), nil
}

func (b *btnCh) ResolveApproval(_ context.Context, _, ref string, _ channel.ApprovalCard, result string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolved[ref] = result
	return nil
}

func (b *btnCh) state() ([]channel.ApprovalCard, map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := map[string]string{}
	for k, v := range b.resolved {
		r[k] = v
	}
	return append([]channel.ApprovalCard(nil), b.cards...), r
}

func TestApprovalButtons(t *testing.T) {
	e := setup(t)
	b := &btnCh{fakeCh: fakeCh{name: "btn", sent: make(chan string, 100)}, resolved: map[string]string{}}
	e.a.AddChannel(b)
	press := func(sender, n string, ok bool) {
		act := channel.ActionDeny
		if ok {
			act = channel.ActionApprove
		}
		e.a.OnMessage(b, channel.Message{ChatID: "c1", SenderID: sender, SenderName: "张三", Text: n, Mentioned: true, Action: act})
	}
	e.a.cfg.Access.Admins = append(e.a.cfg.Access.Admins, "btn:u1")
	e.a.OnMessage(b, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: "执行命令", Mentioned: true})
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: mainSID, ID: "c9", Name: "Shell", Args: json.RawMessage(`{"command":"make test","workspace":"gpu:~/x"}`), From: "btn:u1"})
	waitFor(t, func() bool { c, _ := b.state(); return len(c) == 1 })
	cards, _ := b.state()
	if c := cards[0]; c.N != 1 || c.Title != "执行命令（在 gpu:~/x 上）" || c.Detail != "make test" || c.From != "btn:u1" {
		t.Fatalf("审批卡片：%+v", c)
	}
	// 不是管理员点了按钮：不算数
	press("u2", "1", true)
	b.wait(t, "只有管理员能审批")
	e.k.none()
	// 管理员点“批准”：交给内核，审批消息改成结果
	press("u1", "1", true)
	if ap := e.k.expect(proto.TApprove); ap.ID != "c9" || ap.OK == nil || !*ap.OK || ap.From != "btn:u1" {
		t.Fatalf("approve: %+v", ap)
	}
	waitFor(t, func() bool { _, r := b.state(); return r["ref1"] != "" })
	if _, r := b.state(); r["ref1"] != "✅ 已批准 · 张三" {
		t.Fatalf("结果：%v", r)
	}

	// 第二个审批在网页上拒绝：按钮消息也改成结果
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: mainSID, ID: "c10", Name: "Write", Args: json.RawMessage(`{"path":"a","content":"x"}`), From: "btn:u1"})
	waitFor(t, func() bool { c, _ := b.state(); return len(c) == 2 })
	if err := e.a.Approve(2, false); err != nil {
		t.Fatal(err)
	}
	e.k.expect(proto.TApprove)
	waitFor(t, func() bool { _, r := b.state(); return r["ref2"] != "" })
	if _, r := b.state(); r["ref2"] != "❌ 已拒绝 · 网页" {
		t.Fatalf("结果：%v", r)
	}
	// 第三个没人处理、这一轮结束了
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: mainSID, ID: "c11", Name: "Shell", Args: json.RawMessage(`{"command":"x"}`), From: "btn:u1"})
	waitFor(t, func() bool { c, _ := b.state(); return len(c) == 3 })
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	waitFor(t, func() bool { _, r := b.state(); return r["ref3"] != "" })
	if _, r := b.state(); !strings.Contains(r["ref3"], "这一轮已经结束") {
		t.Fatalf("结果：%v", r)
	}
}

// 放行模式：内核照样按自动模式跑，要审批的调用由网关当场批准，聊天里不发审批消息；拟人型没设置时默认放行
func TestFullModeAutoApproves(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	e.say("帮我截个屏")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAuto {
		t.Fatalf("发给内核的模式：%q", in.Mode)
	}
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: e.sid, ID: "c1", Name: "Shell", Args: json.RawMessage(`{"command":"screenshot.ps1"}`), From: "fake:u1"})
	if ap := e.k.expect(proto.TApprove); ap.ID != "c1" || ap.OK == nil || !*ap.OK {
		t.Fatalf("approve: %+v", ap)
	}
	e.k.send(proto.Msg{T: proto.TNotice, SID: e.sid, Text: "已批准，开始执行 Shell"})
	e.ch.none(t)
	e.a.mu.Lock()
	n := len(e.a.approvals)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("不该留下审批单：%d", n)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	waitFor(t, func() bool { e.a.mu.Lock(); defer e.a.mu.Unlock(); return e.a.sessions[e.sid].run == nil })

	// 自己设了审批模式的拟人型：照常审批
	e.say("/mode ask")
	e.ch.wait(t, "权限模式已切换为审批")
	e.say("再截一次")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: e.sid, ID: "c2", Name: "Shell", Args: json.RawMessage(`{"command":"x"}`), From: "fake:u1"})
	e.ch.wait(t, "需要审批 #")
	e.k.none()
}

func TestKernelDownAbortsRuns(t *testing.T) {
	e := setup(t)
	e.say("任务")
	e.k.expect(proto.TInput)
	e.k.conn.Close()
	e.ch.wait(t, "（test 断开了，这一轮已中止）")
	e.say("还在吗")
	e.ch.wait(t, "没有在线的内核")
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

	// 没挂工作目录的管理员：HOME 模式，工具都给，写外面要 elevated
	e.say("你好")
	in := e.k.expect(proto.TInput)
	if in.Mode != proto.ModeAsk || !in.Elevate || len(in.Workspaces) != 1 || in.Workspaces[0].Name != "HOME" || !in.Workspaces[0].Home || in.Workspaces[0].Local != true ||
		len(in.Workspaces[0].Dirs) != 0 || len(in.Workspaces[0].RO) != 1 || in.Workspaces[0].RO[0] != "/" {
		t.Fatalf("input: %+v", in)
	}
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 这个会话没有挂载工作目录，工作目录是 HOME") || !strings.HasSuffix(got, "\n"+stamp+" 你好") {
		t.Fatalf("user content: %q", got)
	}

	e.say("/dir add " + work)
	e.ch.wait(t, "挂上了 本机:"+work)
	e.say("/mode auto")
	e.ch.wait(t, "已切换为自动")
	e.say("看看目录")
	in = e.k.expect(proto.TInput)
	if in.Mode != proto.ModeAuto || len(in.Workspaces) != 1 {
		t.Fatalf("input: %+v", in)
	}
	if w := in.Workspaces[0]; w.Name != "本机:"+work || !w.Local || len(w.Dirs) != 1 || w.Dirs[0] != work {
		t.Fatalf("本机的目录由本机的内核直接执行：%+v", w)
	}
	msgs := e.roundTrip(t)
	want := "[系统] 在 本机:" + work + " 上工作（网关所在的机器）。相对路径和命令都从这个目录开始。权限模式：自动，"
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

	// 再挂一个：同一台机器上的工作目录互相都能读写，模型要用 workspace 参数指明
	other := filepath.Join(e.dir, "docs")
	os.MkdirAll(other, 0o755)
	e.say("/dir add " + other)
	e.ch.wait(t, "挂上了 本机:"+other)
	e.say("两个一起看")
	in = e.k.expect(proto.TInput)
	if len(in.Workspaces) != 2 || in.Workspaces[1].Name != "本机:"+other || len(in.Workspaces[0].Dirs) != 2 || in.Workspaces[0].Dirs[1] != other {
		t.Fatalf("两个工作目录：%+v", in.Workspaces)
	}
	if got := lastUser(e.roundTrip(t)); !strings.Contains(got, "用 workspace 参数写明") || !strings.Contains(got, "\n- 本机:"+other+"（网关所在的机器）") {
		t.Fatalf("env note: %q", got)
	}
	e.say("/dir")
	if msg := e.ch.wait(t, "工作目录列表"); !strings.Contains(msg, "1. 本机:"+work+"（已挂载）") || !strings.Contains(msg, "2. 本机:"+other+"（已挂载）") {
		t.Fatalf("list: %q", msg)
	}
	e.say("/dir -2")
	e.ch.wait(t, "取下了 本机:"+other)
	e.say("/dir default 2")
	e.ch.wait(t, "新会话会默认挂上 本机:"+other)
	e.say("/dir none")
	e.ch.wait(t, "全部取下")
	if info, _ := e.a.Session(mainSID); len(info.Workspaces) != 0 {
		t.Fatalf("只聊天：%+v", info)
	}
	e.say("/dir rm 2")
	e.ch.wait(t, "已从列表里删掉 本机:"+other)
	if ws := e.a.Workspaces(); len(ws) != 1 || ws[0].Path != work || ws[0].Default {
		t.Fatalf("workspaces: %+v", ws)
	}
	e.say("/dir 1")
	e.ch.wait(t, "挂上了")

	// /reset 之后历史是空的，环境说明要重新给一次
	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	e.say("新的开始")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 在 本机:"+work+" 上工作") {
		t.Fatalf("after reset: %q", got)
	}
}

func TestWorkspaceGuards(t *testing.T) {
	e := setup(t)
	e.say("/dir add relative/path")
	e.ch.wait(t, "绝对路径")
	e.say("/dir add " + e.dir) // 包含网关的配置和数据目录
	e.ch.wait(t, "不能把")
	e.say("/dir add nowhere:/tmp")
	e.ch.wait(t, "没有叫 nowhere 的机器")
	e.say("/dir add " + e.dir + "/missing")
	e.ch.wait(t, "用不了")
	e.say("/mode yolo")
	e.ch.wait(t, "模式只能是")
	e.say("/dir add /tmp", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能修改工作目录")
	e.say("/mode auto", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能切换权限模式")
	e.say("/dir 9")
	e.ch.wait(t, "没有第 9 个工作目录")
}

func TestGuestMode(t *testing.T) {
	e := setup(t)
	e.sid = "fake:c1" // 不是管理员：有自己的会话
	e.useDir(t, "work")
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
	want := "end,tool,result,content,end,note,close,"
	if kinds != want {
		t.Fatalf("events:\n got %s\nwant %s\n%+v", kinds, want, ev)
	}
	if ev[1].text != "Shell ls -la" || ev[2].text != "true a.txt（共 2 行）" || ev[3].text != "（mock）工具结果：a.txt\nb.txt\n" {
		t.Fatalf("details: %+v", ev)
	}
	if !strings.Contains(ev[5].text, "2 次模型调用") || !strings.Contains(ev[5].text, "缓存命中 90%") {
		t.Fatalf("usage: %q", ev[5].text)
	}
	// 正文已经流式显示过，不会再用 Send 发一遍
	select {
	case m := <-sc.sent:
		t.Fatalf("不该再发：%q", m)
	case <-time.After(200 * time.Millisecond):
	}
}

type finalOnlyLLM struct{}

func (finalOnlyLLM) Chat(context.Context, llm.Request, func(llm.Delta)) (*llm.Result, error) {
	return &llm.Result{Reason: llm.ReasonRefused, Content: "这次无法回答"}, nil
}

func TestStreamingFinalWithoutContentDelta(t *testing.T) {
	e, sc := setupStream(t)
	e.a.SetLLM(finalOnlyLLM{})
	e.sayOn(sc, "问题")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: e.sid})
	if turn := e.k.expect(proto.TTurn); turn.Reason != proto.ReasonRefused {
		t.Fatal(turn)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusRefused})
	waitFor(t, func() bool {
		events := sc.snapshot()
		return len(events) > 0 && events[len(events)-1].kind == "close"
	})
	n := 0
	for _, event := range sc.snapshot() {
		if event.kind == "content" && event.text == "这次无法回答" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("正文应显示一次，实际 %d 次", n)
	}
	select {
	case text := <-sc.sent:
		t.Fatalf("不应再发送正文：%s", text)
	default:
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
	e.a.cfg.Context = defaultContext()
	e.a.cfg.Context.KeepTurns = 2
	e.llm.AccurateUsage = true
	long := strings.Repeat("长", 100) // 约 100 token
	var prev []llm.Message
	compactions := 0
	for i := 0; i < 12; i++ {
		e.say(long)
		e.k.expect(proto.TInput)
		msgs := e.roundTrip(t)
		if prev != nil && !reflect.DeepEqual(msgs[:min(len(prev), len(msgs))], prev[:min(len(prev), len(msgs))]) {
			compactions++
			if !strings.HasPrefix(msgs[1].Content, "[系统] 更早对话的摘要：") {
				t.Fatalf("裁剪后缺少摘要：%q", msgs[1].Content)
			}
		}
		prev = msgs
	}
	// 每轮约 230 token，上限 600、裁到 300：12 轮里应该只裁几次，而不是每轮都裁
	if compactions == 0 || compactions > 10 {
		t.Fatalf("裁剪次数：%d", compactions)
	}
}

func TestGatewayToolsOnlyForAdmin(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	admin := e.a.toolsFor(mainSID, e.a.nodes["test"], false, true)
	guest := e.a.toolsFor(mainSID, e.a.nodes["test"], false, false)
	e.a.mu.Unlock()
	names := func(ts []llm.Tool) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Function.Name)
		}
		return out
	}
	if a := names(admin); !slices.Contains(a, "Gateway") || !slices.Contains(a, "GatewayAdmin") {
		t.Fatalf("管理员没有网关工具：%v", a)
	}
	if g := names(guest); slices.Contains(g, "Gateway") || slices.Contains(g, "GatewayAdmin") {
		t.Fatalf("访客看到了网关工具：%v", g)
	}
}

func TestGatewayAdminApprovalCardMasksSecrets(t *testing.T) {
	title, body := callCard("GatewayAdmin", json.RawMessage(`{"action":"ssh_setup","host":"10.0.0.1","name":"gpu","password":"hunter2-secret","api_key":"sk-very-secret"}`))
	if !strings.Contains(title, "提权") || !strings.Contains(title, "ssh_setup") {
		t.Fatalf("标题没有提权/动作：%q", title)
	}
	for _, leak := range []string{"hunter2-secret", "sk-very-secret"} {
		if strings.Contains(body, leak) {
			t.Fatalf("审批卡片泄露了密钥：%s", body)
		}
	}
	if !strings.Contains(body, "10.0.0.1") || !strings.Contains(body, "***") {
		t.Fatalf("审批卡片内容不对：%s", body)
	}
}

func TestGatewayAdminNonAdminAndWorkspaceAdd(t *testing.T) {
	e := setup(t)
	var ok bool
	var text string
	e.a.gatewayAdminCall(mainSID, "fake:u2", `{"action":"workspace_add","host":"本机","path":"/tmp"}`, func(o bool, s string) { ok, text = o, s })
	if ok || !strings.Contains(text, "管理员") {
		t.Fatalf("访客调用了管理工具：%v %q", ok, text)
	}

	dir := filepath.Join(e.dir, "gw-ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.a.gatewayAdminCall(mainSID, "fake:u1", `{"action":"workspace_add","host":"本机","path":"`+dir+`"}`, func(o bool, s string) { ok, text = o, s })
	if !ok || !strings.Contains(text, "工作目录已添加") {
		t.Fatalf("添加失败：%v %q", ok, text)
	}
	found := false
	for _, w := range e.a.Workspaces() {
		if w.Path == dir {
			found = true
		}
	}
	if !found {
		t.Fatalf("列表里没有：%+v", e.a.Workspaces())
	}
}

type fakeAdminHosts struct {
	*fakeHosts
	mu       sync.Mutex
	infos    []host.Info
	saves    int
	connects int
}

func (f *fakeAdminHosts) setInfos(xs []host.Info) { f.mu.Lock(); f.infos = xs; f.mu.Unlock() }

func (f *fakeAdminHosts) Save(c host.Connection) (string, error) {
	f.saves++
	if c.ID == "" {
		c.ID = "id-" + c.Name
	}
	return c.ID, nil
}
func (f *fakeAdminHosts) Delete(string) error     { return nil }
func (f *fakeAdminHosts) Connect(string) error    { f.connects++; return nil }
func (f *fakeAdminHosts) Disconnect(string) error { return nil }
func (f *fakeAdminHosts) Infos() []host.Info      { f.mu.Lock(); defer f.mu.Unlock(); return f.infos }

func TestSSHSetupStates(t *testing.T) {
	old := sshSetupTimeout
	sshSetupTimeout = 80 * time.Millisecond
	defer func() { sshSetupTimeout = old }()
	e := setup(t)
	nilInfos := func() []host.Info { return nil }
	f := &fakeAdminHosts{fakeHosts: e.hosts}
	f.setInfos(nilInfos())
	e.a.SetHosts(f)
	if _, err := e.a.sshSetup(mainSID, gatewayArgs{Name: "gpu", Host: "10.0.0.1"}); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时：%v", err)
	}
	f.setInfos([]host.Info{{ID: "id-gpu", Name: "gpu", Ready: false, State: "error", Detail: "认证失败"}})
	if _, err := e.a.sshSetup(mainSID, gatewayArgs{Name: "gpu", Host: "10.0.0.1"}); err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("认证失败：%v", err)
	}
	f.setInfos([]host.Info{{ID: "id-gpu", Name: "gpu", Ready: true, State: "online"}})
	res, err := e.a.sshSetup(mainSID, gatewayArgs{Name: "gpu", Host: "10.0.0.1"})
	if err != nil || !strings.Contains(res, "在线") {
		t.Fatalf("在线：%q %v", res, err)
	}
}

func TestGatewayModelsQueryDoesNotLeakKey(t *testing.T) {
	e := setup(t)
	ms, err := models.Open(t.TempDir(), &config.Config{LLM: config.LLM{Provider: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.SaveEndpoint(models.Endpoint{Name: "x", Kind: "openai", BaseURL: "https://x/v1", APIKey: "sk-should-not-leak"}); err != nil {
		t.Fatal(err)
	}
	e.a.SetModels(ms, nil)
	var out string
	e.a.gatewayQuery(mainSID, `{"what":"models"}`, func(ok bool, s string) {
		if !ok {
			t.Fatal(s)
		}
		out = s
	})
	if strings.Contains(out, "sk-should-not-leak") {
		t.Fatalf("查询泄露了 key：%s", out)
	}
	if !strings.Contains(out, "has_key") {
		t.Fatalf("没显示 has_key：%s", out)
	}
}

func TestGatewayAdminErrorRedacted(t *testing.T) {
	e := setup(t)
	var ok bool
	var text string
	e.a.gatewayAdminCall(mainSID, "fake:u1", `{"action":"sk-12345678901234567890"}`, func(o bool, s string) { ok, text = o, s })
	if ok || strings.Contains(text, "sk-12345678901234567890") {
		t.Fatalf("错误里泄露了密钥：%v %q", ok, text)
	}
}

// 每条消息的上限由网关带给内核：0 表示不限（发给内核是 -1）；到了上限要说清楚怎么接着做、在哪里改
func TestInputCarriesAgentLimits(t *testing.T) {
	e := setup(t)
	zero := 0
	e.a.mu.Lock()
	e.a.cfg.Agent = config.Agent{MaxSteps: &zero, RunTimeout: "2h", ApprovalTimeout: "0", MaxOutputKB: 128}
	e.a.mu.Unlock()
	e.say("干活")
	in := e.k.expect(proto.TInput)
	if in.MaxSteps != -1 || in.RunTimeoutMs != 7200000 || in.ApprovalTimeoutMs != -1 || in.MaxOutput != 128<<10 {
		t.Fatalf("input limits: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusBudget})
	e.ch.wait(t, "发「继续」接着做")

	// 没设置过：默认 200 次、不限时长、等审批 30 分钟、64 KB
	e.a.mu.Lock()
	e.a.cfg.Agent = config.Agent{}
	e.a.mu.Unlock()
	waitFor(t, func() bool { e.a.mu.Lock(); defer e.a.mu.Unlock(); return e.a.sessions[in.SID].run == nil })
	e.say("再来")
	in = e.k.expect(proto.TInput)
	if in.MaxSteps != 200 || in.RunTimeoutMs != -1 || in.ApprovalTimeoutMs != 1800000 || in.MaxOutput != 64<<10 {
		t.Fatalf("default limits: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusTimeout})
	e.ch.wait(t, "时限在网页拓扑图的 Agent 节点里改")
}
