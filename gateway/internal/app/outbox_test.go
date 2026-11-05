package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/outbox"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

// platCh 是一个外部平台渠道：记录每次发送的时间，可以让前几次失败。
type platCh struct {
	name string
	mu   sync.Mutex
	fail int // 还要失败几次
	got  []string
	at   []time.Time
}

func (p *platCh) Name() string                                     { return p.name }
func (p *platCh) Run(ctx context.Context, _ channel.Handler) error { <-ctx.Done(); return nil }
func (p *platCh) Send(_ context.Context, _, text, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail > 0 {
		p.fail--
		return errors.New("临时错误")
	}
	p.got = append(p.got, text)
	p.at = append(p.at, time.Now())
	return nil
}
func (p *platCh) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

func (e *env) deliverTo(ch string, text string) bool {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.enqueue(endpoint{ch: ch, chat: "c1"}, outItem{text: text})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) outboxPending() int {
	n := 0
	for _, c := range e.a.outbox.Stats().Channels {
		n += c.Pending
	}
	return n
}

func TestOutboxRetriesPlatformChannelUntilDelivered(t *testing.T) {
	e := setup(t)
	ch := &platCh{name: "tg", fail: 1}
	e.a.AddChannel(ch)
	if !e.deliverTo("tg", "你好") {
		t.Fatal("应受理")
	}
	waitUntil(t, "先失败后成功，消息最终送达", func() bool { return len(ch.texts()) == 1 })
	if ch.texts()[0] != "你好" {
		t.Fatalf("%v", ch.texts())
	}
	waitUntil(t, "回执", func() bool { return e.outboxPending() == 0 })
}

// 新建一个 App，数据目录用 dir 里已有的（模拟重启）。
func newAppAt(t *testing.T, dir string, buf *bytes.Buffer) *App {
	t.Helper()
	cfg := &config.Config{
		LLM:       config.LLM{Provider: "mock", MaxHistoryTokens: 10000, TrimTo: 0.5},
		Persona:   config.Persona{Dir: filepath.Join(dir, "personas")},
		Skills:    config.Skills{Dir: filepath.Join(dir, "skills")},
		Workspace: config.Workspace{Mode: "ask"},
		Dir:       filepath.Join(dir, "conf"),
	}
	store, err := session.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = io.Discard
	if buf != nil {
		w = buf
	}
	log := slog.New(slog.NewTextHandler(w, nil))
	ctx, cancel := context.WithCancel(context.Background())
	a := New(ctx, cfg, &recLLM{}, store, persona.New(cfg.Persona.Dir, log), log)
	t.Cleanup(func() { cancel(); a.Wait() })
	return a
}

func TestOutboxDeliversPendingAfterAppRestart(t *testing.T) {
	e := setup(t)
	if !e.deliverTo("fake", "x") { // fake 渠道在线：先清掉，保证只留下下面这条
		t.Fatal("应受理")
	}
	waitUntil(t, "先清空", func() bool { return e.outboxPending() == 0 })
	absent := &platCh{name: "tg"}
	e.a.AddChannel(absent)
	absent.mu.Lock()
	absent.fail = 1 << 30 // 一直失败：消息留在待发里
	absent.mu.Unlock()
	e.deliverTo("tg", "重启前入队")
	waitUntil(t, "落盘", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.dir, "data", "runtime", "outbox.jsonl"))
		return strings.Contains(string(b), "重启前入队")
	})
	e.a.outbox.Close() // 旧 App 的出站停下，日志留在磁盘上

	a2 := newAppAt(t, e.dir, nil)
	ch2 := &platCh{name: "tg"}
	a2.AddChannel(ch2)
	waitUntil(t, "重启后送达", func() bool { return len(ch2.texts()) == 1 })
	if ch2.texts()[0] != "重启前入队" {
		t.Fatalf("%v", ch2.texts())
	}
}

// 群聊分条的停顿不变：第二条之前停 0.3 到 3 秒，和现在一样。
func TestOutboxKeepsGroupPartDelay(t *testing.T) {
	e := setup(t)
	ch := &platCh{name: "tg"}
	e.a.AddChannel(ch)
	e.a.mu.Lock()
	e.a.sendParts(endpoint{ch: "tg", chat: "g1"}, []string{"第一条", "第二条"})
	e.a.mu.Unlock()
	waitUntil(t, "两条都到", func() bool { return len(ch.texts()) == 2 })
	ch.mu.Lock()
	gap := ch.at[1].Sub(ch.at[0])
	ch.mu.Unlock()
	if gap < 280*time.Millisecond {
		t.Fatalf("分条之间的停顿丢了: %v", gap)
	}
}

func TestOutboxFullReturnsFalseAndWarns(t *testing.T) {
	e := setup(t)
	var buf bytes.Buffer
	e.a.mu.Lock()
	e.a.log = slog.New(slog.NewTextHandler(&buf, nil))
	e.a.mu.Unlock()
	ch := &platCh{name: "tg", fail: 1 << 30}
	e.a.AddChannel(ch)
	ok := 0
	var last bool
	for i := 0; i < 300; i++ {
		last = e.deliverTo("tg", "m")
		if last {
			ok++
		}
	}
	if last || ok != 256 {
		t.Fatalf("单个窗口最多 256 条，满了 enqueue 返回 false: 受理 %d，最后一次 %v", ok, last)
	}
	if !strings.Contains(buf.String(), "出站队列已满") {
		t.Fatalf("应记警告: %s", buf.String())
	}
	waitUntil(t, "健康 degraded", func() bool { return outboxHealth(e.a).State == health.Degraded })
}

func outboxHealth(a *App) health.Entry {
	for _, h := range a.Status().Health {
		if h.Name == "outbox" {
			return h
		}
	}
	return health.Entry{}
}

func TestOutboxStorageBadFallsBackToMemory(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "data", "runtime"), 0o755)
	bad := []byte("不是 JSON\n")
	path := filepath.Join(dir, "data", "runtime", "outbox.jsonl")
	os.WriteFile(path, bad, 0o600)
	var buf bytes.Buffer
	a := newAppAt(t, dir, &buf)
	ch := &platCh{name: "tg"}
	a.AddChannel(ch)
	a.mu.Lock()
	ok := a.enqueue(endpoint{ch: "tg", chat: "c1"}, outItem{text: "照样送达"})
	a.mu.Unlock()
	if !ok {
		t.Fatal("退回内存直发应受理")
	}
	waitUntil(t, "内存直发送达", func() bool { return len(ch.texts()) == 1 })
	if outboxHealth(a).State != health.Error {
		t.Fatalf("健康 error: %+v", outboxHealth(a))
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, bad) {
		t.Fatal("原文件必须保持不动")
	}
	for _, is := range a.store.StorageIssues() {
		if is.Scope == "outbox" {
			t.Fatalf("不往存储问题里重复记: %+v", is)
		}
	}
	if !strings.Contains(buf.String(), "出站存储不可用") {
		t.Fatalf("应记日志: %s", buf.String())
	}
}

func TestCliAndWebBypassOutbox(t *testing.T) {
	e := setup(t)
	for _, name := range []string{"cli", "web"} {
		ch := &platCh{name: name}
		e.a.AddChannel(ch)
		if !e.deliverTo(name, "本机视图") {
			t.Fatalf("%s 应受理", name)
		}
		waitUntil(t, name+" 送达", func() bool { return len(ch.texts()) == 1 })
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "data", "runtime", "outbox.jsonl"))
	if strings.Contains(string(b), "本机视图") || e.outboxPending() != 0 {
		t.Fatalf("cli 和 web 不进出站: %s", b)
	}
	// 其他渠道（包括以后新加的平台）一律进出站
	ch := &platCh{name: "newplat"}
	e.a.AddChannel(ch)
	e.deliverTo("newplat", "新平台")
	waitUntil(t, "新平台送达", func() bool { return len(ch.texts()) == 1 })
	waitUntil(t, "新平台进了日志", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.dir, "data", "runtime", "outbox.jsonl"))
		return strings.Contains(string(b), "新平台")
	})
}

func TestDeleteBotDropsPendingMessages(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.ReconcileChannels(e.a.cfg)
	v, err := e.a.CreateBot(agents.KindTelegram, BotPatch{Name: strp("临时"), Token: strp("1:x"), Enabled: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	name := "tg." + v.ID
	if _, err := e.a.outbox.Enqueue(outbox.Item{Channel: name, Chat: "c1", Parts: []string{"待发"}, Plain: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.outbox.Enqueue(outbox.Item{Channel: "other", Chat: "c1", Parts: []string{"别的"}, Plain: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.a.DeleteBot(v.ID); err != nil {
		t.Fatal(err)
	}
	st := e.a.outbox.Stats()
	if st.Dropped24h["channel_removed"] != 1 || st.Channels[name].Pending != 0 || st.Channels["other"].Pending != 1 {
		t.Fatalf("删除机器人后它的待发以 channel_removed 结局: %+v", st)
	}
}

func TestStatusReportsOutbox(t *testing.T) {
	e := setup(t)
	ch := &platCh{name: "tg", fail: 1 << 30}
	e.a.AddChannel(ch)
	e.deliverTo("tg", "卡住")
	waitUntil(t, "有待发", func() bool { return e.a.Status().Outbox.Channels["tg"].Pending == 1 })
	st := e.a.Status()
	if s := st.Outbox.Summary(time.Now()); !strings.Contains(s, "待发 1 条") {
		t.Fatalf("概括: %q", s)
	}
	if s := (OutboxInfo{}).Summary(time.Now()); s != "" {
		t.Fatalf("没事不打印: %q", s)
	}
}

// 渠道停用：从渠道表和快照里去掉，待发消息留在出站里等；重新启用后一条不丢、按顺序发出。
func TestDisabledChannelKeepsPendingAndResumesInOrder(t *testing.T) {
	e := setup(t)
	e.a.ReconcileChannels(e.a.cfg) // 初始化 runs
	ch1 := &platCh{name: "tg"}
	e.a.chMu.Lock()
	e.a.startChannel("telegram", ch1, nil)
	e.a.chMu.Unlock()
	if !e.deliverTo("tg", "第0条") {
		t.Fatal("渠道在时应该受理")
	}
	waitUntil(t, "第0条发出", func() bool { return len(ch1.texts()) == 1 })

	e.a.chMu.Lock()
	e.a.stopChannel("telegram")
	e.a.chMu.Unlock()
	e.a.mu.Lock()
	_, inChans := e.a.chans["tg"]
	e.a.mu.Unlock()
	if snap := e.a.chSnap.Load(); inChans || (snap != nil && (*snap)["tg"] != nil) {
		t.Fatal("停用的渠道应该从渠道表和快照里去掉")
	}
	for i := 1; i <= 3; i++ {
		if !e.deliverTo("tg", "第"+string(rune('0'+i))+"条") {
			t.Fatalf("渠道停用期间第 %d 条也应该受理（等渠道回来）", i)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.outboxPending(); n != 3 {
		t.Fatalf("停用期间待发应该留着：%d", n)
	}
	if got := ch1.texts(); len(got) != 1 {
		t.Fatalf("停用的渠道对象不该再发：%v", got)
	}
	if e.a.outbox.Stats().Dropped24h["channel_removed"] != 0 {
		t.Fatal("停用不是删除，不该丢弃")
	}

	ch2 := &platCh{name: "tg"}
	e.a.chMu.Lock()
	e.a.startChannel("telegram", ch2, nil)
	e.a.chMu.Unlock()
	waitUntil(t, "停用期间的消息发出", func() bool { return len(ch2.texts()) == 3 })
	if got := strings.Join(ch2.texts(), ","); got != "第1条,第2条,第3条" {
		t.Fatalf("顺序不对：%s", got)
	}
	if e.outboxPending() != 0 {
		t.Fatal("发完应该清空")
	}
}

// 删除机器人的顺序：先停渠道、再忘掉并丢弃待发。之后发给它的消息直接拒绝，出站里没有残留。
func TestDeleteBotAfterStopRejectsNewMessages(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.ReconcileChannels(e.a.cfg)
	v, err := e.a.CreateBot(agents.KindTelegram, BotPatch{Name: strp("临时"), Token: strp("1:x"), Enabled: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	name := "tg." + v.ID
	ch := &platCh{name: name, fail: 1 << 30} // 一直发不出去：消息留在出站里
	e.a.chMu.Lock()
	e.a.startChannel(name, ch, nil) // 渠道在跑，DeleteBot 里的 reconcile 会停掉它（停用时记进 chRetired）
	e.a.chMu.Unlock()
	if !e.deliverTo(name, "待发") {
		t.Fatal("渠道在时应该受理")
	}
	if err := e.a.DeleteBot(v.ID); err != nil {
		t.Fatal(err)
	}
	st := e.a.outbox.Stats()
	if st.Dropped24h["channel_removed"] != 1 || e.outboxPending() != 0 {
		t.Fatalf("待发应按 channel_removed 丢弃: %+v", st)
	}
	if e.deliverTo(name, "删除之后") {
		t.Fatal("删除之后发给它的消息应该直接拒绝")
	}
	if e.outboxPending() != 0 {
		t.Fatal("出站里不该有残留")
	}
}
