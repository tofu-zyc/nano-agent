package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/media"
)

var fastTune = tune{backoffBase: 5 * time.Millisecond, backoffMax: 40 * time.Millisecond, expiry: time.Hour,
	tick: 10 * time.Millisecond, absentWait: 10 * time.Millisecond, sendTimeout: 5 * time.Second}

type call struct {
	chat, text, reply string
	md                bool
}

// fakeCh 记录收到的调用；fail 可以按调用决定成败。
type fakeCh struct {
	name string
	mu   sync.Mutex
	got  []call
	n    int
	fail func(n int, c call) error // n 是第几次调用（从 1 起，含失败的）
	hook func(c call)              // 成功之后、返回之前调用
	gate chan struct{}             // 非 nil 时每次发送先等它
}

func (f *fakeCh) Name() string { return f.name }
func (f *fakeCh) Run(ctx context.Context, _ channel.Handler) error {
	<-ctx.Done()
	return nil
}
func (f *fakeCh) record(ctx context.Context, chat, text, reply string, md bool) error {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.n++
	c := call{chat, text, reply, md}
	if f.fail != nil {
		if err := f.fail(f.n, c); err != nil {
			f.mu.Unlock()
			return err
		}
	}
	f.got = append(f.got, c)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(c)
	}
	return nil
}
func (f *fakeCh) Send(ctx context.Context, chat, text, reply string) error {
	return f.record(ctx, chat, text, reply, false)
}
func (f *fakeCh) calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.got...)
}

type mdCh struct{ fakeCh }

func (f *mdCh) SendMarkdown(ctx context.Context, chat, text, reply string) error {
	return f.record(ctx, chat, text, reply, true)
}

type rig struct {
	t    *testing.T
	o    *Outbox
	path string
	mu   sync.Mutex
	chs  map[string]channel.Channel
	now  atomic.Int64 // 时间偏移（纳秒）
	hb   *health.Board
	h    *health.Handle
	data map[string][]byte
}

func (r *rig) clock() time.Time { return time.Now().Add(time.Duration(r.now.Load())) }

func (r *rig) deps() Deps {
	return Deps{
		Channel: func(n string) channel.Channel { r.mu.Lock(); defer r.mu.Unlock(); return r.chs[n] },
		Media: func(id string) ([]byte, error) {
			if b, ok := r.data[id]; ok {
				return b, nil
			}
			return nil, os.ErrNotExist
		},
		Now: r.clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Health: r.h,
	}
}

func (r *rig) setCh(name string, ch channel.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch == nil {
		delete(r.chs, name)
		return
	}
	r.chs[name] = ch
}

func newRig(t *testing.T, tn tune, chs ...channel.Channel) *rig {
	t.Helper()
	r := &rig{t: t, path: filepath.Join(t.TempDir(), "runtime", "outbox.jsonl"), chs: map[string]channel.Channel{}, hb: health.NewBoard(), data: map[string][]byte{}}
	r.h, _ = r.hb.Claim("outbox")
	for _, c := range chs {
		r.chs[c.Name()] = c
	}
	o, err := open(context.Background(), r.path, r.deps(), tn)
	if err != nil {
		t.Fatal(err)
	}
	r.o = o
	t.Cleanup(func() { r.o.Close() })
	return r
}

func (r *rig) health() health.Entry { return r.hb.Snapshot()[0] }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func text(ch, chat string, parts ...string) Item {
	return Item{Channel: ch, Chat: chat, Parts: parts, Plain: true, Kind: "reply"}
}

func (r *rig) mustEnqueue(it Item) string {
	r.t.Helper()
	id, err := r.o.Enqueue(it)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func texts(cs []call) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.text
	}
	return out
}

func (r *rig) queued() int { r.o.mu.Lock(); defer r.o.mu.Unlock(); return len(r.o.all) }

func (r *rig) pending() int {
	n := 0
	for _, c := range r.o.Stats().Channels {
		n += c.Pending
	}
	return n
}

func TestSendsInOrderAndSettles(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune, ch)
	for i := 0; i < 20; i++ {
		r.mustEnqueue(text("tg", "c1", fmt.Sprint(i)))
	}
	waitFor(t, "全部发完", func() bool { return len(ch.calls()) == 20 && r.pending() == 0 })
	for i, c := range ch.calls() {
		if c.text != fmt.Sprint(i) {
			t.Fatalf("顺序: %v", texts(ch.calls()))
		}
	}
	r.o.Close()
	// 重启后没有待发：回执已写
	r2, _ := open(context.Background(), r.path, r.deps(), fastTune)
	defer r2.Close()
	if n := len(r2.all); n != 0 {
		t.Fatalf("已发的不该再发: %d", n)
	}
}

// 崩溃恢复：用“日志的当时快照”模拟不正常关闭，在新的出站里重新打开。
func TestCrashRecovery(t *testing.T) {
	snapshot := func(r *rig) string {
		b, err := os.ReadFile(r.path)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "outbox.jsonl")
		os.WriteFile(p, b, 0o600)
		return p
	}
	reopen := func(p string, ch *fakeCh) *Outbox {
		rr := &rig{chs: map[string]channel.Channel{"tg": ch}, data: map[string][]byte{}}
		o, err := open(context.Background(), p, rr.deps(), fastTune)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		return o
	}

	t.Run("落盘之后、发送之前", func(t *testing.T) {
		r := newRig(t, fastTune) // 渠道不在：消息落盘后一直等
		id := r.mustEnqueue(text("tg", "c1", "a"))
		waitFor(t, "落盘", func() bool { b, _ := os.ReadFile(r.path); return strings.Contains(string(b), `"id":"`+id+`"`) })
		ch := &fakeCh{name: "tg"}
		reopen(snapshot(r), ch)
		waitFor(t, "重启后发出", func() bool { return len(ch.calls()) == 1 })
	})
	t.Run("发送中", func(t *testing.T) {
		gate := make(chan struct{})
		ch := &fakeCh{name: "tg", gate: gate}
		r := newRig(t, fastTune, ch)
		id := r.mustEnqueue(text("tg", "c1", "a"))
		waitFor(t, "落盘", func() bool { b, _ := os.ReadFile(r.path); return strings.Contains(string(b), `"id":"`+id+`"`) })
		waitFor(t, "在途", func() bool { r.o.mu.Lock(); defer r.o.mu.Unlock(); return r.o.all[id].inflight })
		ch2 := &fakeCh{name: "tg"}
		reopen(snapshot(r), ch2)
		waitFor(t, "重启后发出", func() bool { return len(ch2.calls()) == 1 })
		close(gate)
	})
	t.Run("发送成功但回执未写", func(t *testing.T) {
		var snap atomic.Value
		var rr *rig
		ch := &fakeCh{name: "tg"}
		ch.hook = func(call) { snap.Store(snapshot(rr)) } // 返回之前：settle 记录还没写
		rr = newRig(t, fastTune, ch)
		rr.mustEnqueue(text("tg", "c1", "a"))
		waitFor(t, "发出", func() bool { return len(ch.calls()) == 1 && snap.Load() != nil })
		ch2 := &fakeCh{name: "tg"}
		reopen(snap.Load().(string), ch2)
		waitFor(t, "至少一次：再发一次", func() bool { return len(ch2.calls()) == 1 })
	})
	t.Run("已写回执的不再发", func(t *testing.T) {
		ch := &fakeCh{name: "tg"}
		r := newRig(t, fastTune, ch)
		r.mustEnqueue(text("tg", "c1", "a"))
		waitFor(t, "发完", func() bool { return len(ch.calls()) == 1 && r.pending() == 0 })
		r.o.Close()
		ch2 := &fakeCh{name: "tg"}
		o := reopen(r.path, ch2)
		time.Sleep(50 * time.Millisecond)
		if len(ch2.calls()) != 0 || len(o.all) != 0 {
			t.Fatalf("不该再发: %v", ch2.calls())
		}
	})
}

func TestNeverSendsBeforeEnqueueRecordIsOnDisk(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune)
	var onDisk atomic.Bool
	ch.hook = func(c call) {}
	// 每次发送时检查：这条的 enqueue 记录必须已经在文件里
	var bad atomic.Value
	ch.fail = func(n int, c call) error {
		b, _ := os.ReadFile(r.path)
		if !strings.Contains(string(b), `"`+c.text+`"`) {
			bad.Store(c.text)
		}
		onDisk.Store(true)
		return nil
	}
	r.setCh("tg", ch)
	for i := 0; i < 30; i++ {
		r.mustEnqueue(text("tg", "c1", fmt.Sprintf("m%02d", i)))
	}
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 30 })
	if v := bad.Load(); v != nil {
		t.Fatalf("%v 在落盘之前就发了", v)
	}
}

func TestOrderSurvivesInjectedFailures(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	fails := map[string]int{"m10": 1, "m11": 2, "m40": 2, "m99": 1} // 都不到 3 次
	ch.fail = func(n int, c call) error {
		if fails[c.text] > 0 {
			fails[c.text]--
			return errors.New("临时错误")
		}
		return nil
	}
	r := newRig(t, fastTune, ch)
	for i := 0; i < 100; i++ {
		r.mustEnqueue(text("tg", "c1", fmt.Sprintf("m%02d", i)))
	}
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 100 })
	for i, c := range ch.calls() {
		if c.text != fmt.Sprintf("m%02d", i) {
			t.Fatalf("第 %d 条顺序错: %v", i, texts(ch.calls()))
		}
	}
}

func TestHeadFailingThreeTimesLetsLaterOnesPass(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	var tries atomic.Int32
	ch.fail = func(n int, c call) error {
		if c.text == "bad" && tries.Add(1) <= 6 {
			return errors.New("坏消息")
		}
		return nil
	}
	r := newRig(t, tune{backoffBase: 30 * time.Millisecond, backoffMax: 60 * time.Millisecond, expiry: time.Hour, tick: 10 * time.Millisecond, absentWait: 10 * time.Millisecond, sendTimeout: time.Second}, ch)
	r.mustEnqueue(text("tg", "c1", "bad"))
	for _, s := range []string{"x1", "x2", "x3"} {
		r.mustEnqueue(text("tg", "c1", s))
	}
	waitFor(t, "后面的放行", func() bool { return len(ch.calls()) >= 3 })
	got := texts(ch.calls())
	if got[0] != "x1" || got[1] != "x2" || got[2] != "x3" {
		t.Fatalf("队头失败 3 次后后面的才先发: %v", got)
	}
	if e := r.health(); e.State != health.Degraded || !strings.Contains(e.Detail, "连续失败") {
		t.Fatalf("失败中的条目让健康变 degraded: %+v", e)
	}
	waitFor(t, "队头最终发出", func() bool { return len(ch.calls()) == 4 })
	if ch.calls()[3].text != "bad" {
		t.Fatalf("队头继续按退避重试直到发出: %v", texts(ch.calls()))
	}
	waitFor(t, "健康恢复", func() bool { return r.health().State == health.OK })
}

func TestFewerThanThreeFailuresBlockLaterOnes(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	var tries atomic.Int32
	ch.fail = func(n int, c call) error {
		if c.text == "bad" && tries.Add(1) <= 2 {
			return errors.New("坏消息")
		}
		return nil
	}
	r := newRig(t, fastTune, ch)
	r.mustEnqueue(text("tg", "c1", "bad"))
	r.mustEnqueue(text("tg", "c1", "next"))
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 2 })
	if got := texts(ch.calls()); got[0] != "bad" || got[1] != "next" {
		t.Fatalf("失败不到 3 次不放行: %v", got)
	}
}

func TestRateLimitWaitsWithoutCountingFailures(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	var limited atomic.Int32
	ch.fail = func(n int, c call) error {
		if c.text == "a" && limited.Add(1) <= 5 {
			return &limitErr{40 * time.Millisecond}
		}
		return nil
	}
	r := newRig(t, fastTune, ch)
	start := time.Now()
	r.mustEnqueue(text("tg", "c1", "a"))
	r.mustEnqueue(text("tg", "c1", "b"))
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 2 })
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("限流要按平台给的时间等: %v", time.Since(start))
	}
	if got := texts(ch.calls()); got[0] != "a" || got[1] != "b" {
		t.Fatalf("限流不放行后面的: %v", got)
	}
	// 5 次限流都不算失败：健康一直是 ok
	if r.health().State != health.OK {
		t.Fatalf("限流不让健康变 degraded: %+v", r.health())
	}
	if recent := r.o.Recent(10); len(recent) != 0 {
		t.Fatalf("限流不记失败: %+v", recent)
	}
}

type limitErr struct{ d time.Duration }

func (e *limitErr) Error() string             { return "429" }
func (e *limitErr) RetryDelay() time.Duration { return e.d }

func TestPartialSendResumesFromUnsentPart(t *testing.T) {
	ch := &mdCh{fakeCh{name: "feishu"}}
	var tries atomic.Int32
	ch.fail = func(n int, c call) error {
		if c.text == "p2" && tries.Add(1) == 1 {
			return errors.New("第 2 段失败")
		}
		return nil
	}
	r := newRig(t, fastTune, ch)
	it := Item{Channel: "feishu", Chat: "c1", Parts: []string{"p1", "p2", "p3"}, Markdown: true, Plain: false, ReplyTo: "m-7"}
	r.mustEnqueue(it)
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 3 })
	got := ch.calls()
	if got[0].text != "p1" || got[1].text != "p2" || got[2].text != "p3" {
		t.Fatalf("重试不重发第 1 段: %v", texts(got))
	}
	for i, c := range got {
		if !c.md {
			t.Fatalf("Plain=false 用 SendMarkdown: %+v", c)
		}
		if (i == 0) != (c.reply == "m-7") {
			t.Fatalf("引用回复只用于第一段: %+v", got)
		}
	}
	// Plain=true 即使渠道能发 Markdown 也用 Send
	ch2 := &mdCh{fakeCh{name: "wx"}}
	r.setCh("wx", ch2)
	r.mustEnqueue(Item{Channel: "wx", Chat: "c1", Parts: []string{"a"}, Markdown: true, Plain: true})
	waitFor(t, "wx 发出", func() bool { return len(ch2.calls()) == 1 })
	if ch2.calls()[0].md {
		t.Fatal("Plain=true 用 Send")
	}
	// 进度进了日志：重启后接着发
	r.o.Close()
}

func TestPartialProgressSurvivesRestart(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	var once atomic.Bool
	ch.fail = func(n int, c call) error {
		if c.text == "p2" && !once.Swap(true) {
			return errors.New("失败一次")
		}
		return nil
	}
	r := newRig(t, tune{backoffBase: 250 * time.Millisecond, backoffMax: time.Second, expiry: time.Hour, tick: 10 * time.Millisecond, absentWait: 10 * time.Millisecond, sendTimeout: time.Second}, ch)
	r.mustEnqueue(text("tg", "c1", "p1", "p2", "p3"))
	waitFor(t, "第 1 段发出且第 2 段失败过", func() bool { return once.Load() })
	waitFor(t, "进度落盘", func() bool {
		b, _ := os.ReadFile(r.path)
		return strings.Contains(string(b), `"t":"progress"`) && strings.Contains(string(b), `"t":"attempt"`)
	})
	r.o.Close()
	ch2 := &fakeCh{name: "tg"}
	r.setCh("tg", ch2)
	o2, err := open(context.Background(), r.path, r.deps(), fastTune)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	waitFor(t, "接着发（退避时间也记在日志里）", func() bool { return len(ch2.calls()) == 2 })
	if got := texts(ch2.calls()); got[0] != "p2" || got[1] != "p3" {
		t.Fatalf("重启后从没发出的段接着发: %v", got)
	}
}

func TestPermanentFailureDropsAsGone(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	ch.fail = func(n int, c call) error {
		if c.text == "x" {
			return fmt.Errorf("telegram：%w", channel.ErrGone)
		}
		return nil
	}
	r := newRig(t, fastTune, ch)
	r.mustEnqueue(text("tg", "c1", "x"))
	r.mustEnqueue(text("tg", "c1", "y"))
	waitFor(t, "丢弃后后面的照发", func() bool { return len(ch.calls()) == 1 && ch.calls()[0].text == "y" && r.pending() == 0 })
	if st := r.o.Stats(); st.Dropped24h["gone"] != 1 {
		t.Fatalf("丢弃要记原因: %+v", st)
	}
	rec := r.o.Recent(5)
	if len(rec) != 1 || rec[0].State != "dropped" || rec[0].Reason != "gone" {
		t.Fatalf("Recent: %+v", rec)
	}
	waitFor(t, "健康 degraded", func() bool {
		e := r.health()
		return e.State == health.Degraded && strings.Contains(e.Detail, "gone 1")
	})
}

func TestExpiresAfter24Hours(t *testing.T) {
	r := newRig(t, fastTune) // 渠道一直不在
	r.mustEnqueue(text("tg", "c1", "a"))
	time.Sleep(50 * time.Millisecond)
	if r.pending() != 1 {
		t.Fatal("渠道不在时消息等待")
	}
	r.now.Store(int64(25 * time.Hour))
	r.o.poke()
	waitFor(t, "过期丢弃", func() bool { return r.pending() == 0 })
	if st := r.o.Stats(); st.Dropped24h["expired"] != 1 {
		t.Fatalf("expired: %+v", st)
	}
}

func TestChannelAbsentWaitsThenSends(t *testing.T) {
	r := newRig(t, fastTune)
	r.mustEnqueue(text("tg", "c1", "a"))
	time.Sleep(60 * time.Millisecond)
	if st := r.o.Stats(); st.Channels["tg"].Failing != 0 || r.health().State != health.OK {
		t.Fatalf("渠道不在不算失败: %+v %+v", st, r.health())
	}
	ch := &fakeCh{name: "tg"}
	r.setCh("tg", ch)
	waitFor(t, "渠道回来后发出", func() bool { return len(ch.calls()) == 1 })
}

func TestDropChannel(t *testing.T) {
	r := newRig(t, fastTune)
	r.mustEnqueue(text("tg.b1", "c1", "a"))
	r.mustEnqueue(text("tg.b1", "c2", "b"))
	r.mustEnqueue(text("tg.keep", "c1", "k"))
	r.o.DropChannel("tg.b1")
	if st := r.o.Stats(); st.Channels["tg.b1"].Pending != 0 || st.Channels["tg.keep"].Pending != 1 || st.Dropped24h["channel_removed"] != 2 {
		t.Fatalf("DropChannel: %+v", st)
	}
	if r.health().State != health.OK {
		t.Fatalf("删除机器人是有意的，不让健康变 degraded: %+v", r.health())
	}
	r.o.Close()
	r2, _ := open(context.Background(), r.path, r.deps(), fastTune)
	defer r2.Close()
	if len(r2.all) != 1 {
		t.Fatalf("重启后只剩没删的: %d", len(r2.all))
	}
}

func TestCapacityFull(t *testing.T) {
	r := newRig(t, fastTune) // 渠道不在：全留着
	for i := 0; i < maxPerTarget; i++ {
		r.mustEnqueue(text("tg", "c1", "x"))
	}
	if _, err := r.o.Enqueue(text("tg", "c1", "x")); !errors.Is(err, ErrFull) {
		t.Fatalf("单个目标满: %v", err)
	}
	if _, err := r.o.Enqueue(text("tg", "c2", "x")); err != nil {
		t.Fatalf("别的目标不受影响: %v", err)
	}
	waitFor(t, "健康 degraded", func() bool { return r.health().State == health.Degraded })
	// 总数上限
	r2 := newRig(t, fastTune)
	for i := 0; i < maxItems; i++ {
		r2.mustEnqueue(text("tg", fmt.Sprint("c", i), "x"))
	}
	if _, err := r2.o.Enqueue(text("tg", "new", "x")); !errors.Is(err, ErrFull) {
		t.Fatalf("总数满: %v", err)
	}
	// 总字节数上限
	r3 := newRig(t, fastTune)
	big := strings.Repeat("字", 300<<10) // 约 900 KB，低于单条上限
	var err error
	for i := 0; err == nil && i < 20; i++ {
		_, err = r3.o.Enqueue(text("tg", fmt.Sprint("c", i), big))
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("总字节满: %v", err)
	}
	if st := r3.o.Stats(); st.Full24h == 0 {
		t.Fatalf("记下队列满: %+v", st)
	}
}

func TestOversizedMessageIsRejectedNotLost(t *testing.T) {
	r := newRig(t, fastTune)
	huge := strings.Repeat("a", recordLimit+1)
	if _, err := r.o.Enqueue(text("tg", "c1", huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过单条上限: %v", err)
	}
	if _, err := r.o.Enqueue(text("tg", "c1", "ok")); err != nil {
		t.Fatalf("之后的消息不受影响: %v", err)
	}
	if e := r.health(); e.State != health.Degraded || !strings.Contains(e.Detail, "单条上限") {
		t.Fatalf("健康 degraded: %+v", e)
	}
	if st := r.o.Stats(); st.TooLarge24h != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestStorageCorruptOnOpenFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.jsonl")
	bad := "不是 JSON\n"
	os.WriteFile(path, []byte(bad), 0o600)
	hb := health.NewBoard()
	h, _ := hb.Claim("outbox")
	o, err := open(context.Background(), path, Deps{Channel: func(string) channel.Channel { return nil }, Health: h, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fastTune)
	if err == nil || o == nil {
		t.Fatalf("损坏应报错且仍返回可用的 Outbox: %v %v", o, err)
	}
	defer o.Close()
	if _, err := o.Enqueue(text("tg", "c1", "a")); !errors.Is(err, ErrStorage) {
		t.Fatalf("停止受理: %v", err)
	}
	if e := hb.Snapshot()[0]; e.State != health.Error {
		t.Fatalf("健康 error: %+v", e)
	}
	if b, _ := os.ReadFile(path); string(b) != bad {
		t.Fatal("原文件必须保持不动")
	}
	if st := o.Stats(); st.Storage == "" {
		t.Fatal("Stats 带存储错误")
	}
}

func TestStorageFailureAtRuntime(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune, ch)
	r.mustEnqueue(text("tg", "c1", "before"))
	waitFor(t, "第一条发出", func() bool { return len(ch.calls()) == 1 })
	// 让下一次写盘失败：关掉底层日志文件
	r.o.mu.Lock()
	r.o.mu.Unlock()
	breakJournal(r.o)
	id := r.mustEnqueue(text("tg", "c1", "during"))
	_ = id
	waitFor(t, "存储失败后停止受理", func() bool {
		_, err := r.o.Enqueue(text("tg", "c1", "after"))
		return errors.Is(err, ErrStorage)
	})
	waitFor(t, "健康 error", func() bool { return r.health().State == health.Error })
	waitFor(t, "已受理的照发", func() bool {
		for _, c := range ch.calls() {
			if c.text == "during" {
				return true
			}
		}
		return false
	})
}

func TestDelayStartsWhenItemBecomesHead(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune, ch)
	var at [3]time.Time
	var atMu sync.Mutex
	ch.hook = func(c call) {
		atMu.Lock()
		defer atMu.Unlock()
		switch c.text {
		case "a":
			at[0] = time.Now()
		case "b":
			at[1] = time.Now()
		case "c":
			at[2] = time.Now()
		}
	}
	for _, s := range []string{"a", "b", "c"} {
		it := text("tg", "c1", s)
		it.Delay = 80 * time.Millisecond
		r.mustEnqueue(it)
	}
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 3 })
	atMu.Lock()
	defer atMu.Unlock()
	if d := at[1].Sub(at[0]); d < 70*time.Millisecond {
		t.Fatalf("b 成为队头后才开始等: %v", d)
	}
	if d := at[2].Sub(at[1]); d < 70*time.Millisecond {
		t.Fatalf("c 成为队头后才开始等: %v", d)
	}
	// 重启后不再等
	r.o.Close()
	ch2 := &fakeCh{name: "tg"}
	r2 := newRig(t, fastTune)
	r2.setCh("tg", nil)
	it := text("tg", "c1", "slow")
	it.Delay = 10 * time.Second
	r2.mustEnqueue(it)
	waitFor(t, "落盘", func() bool { b, _ := os.ReadFile(r2.path); return strings.Contains(string(b), "slow") })
	r2.o.Close()
	rr := &rig{chs: map[string]channel.Channel{"tg": ch2}, data: map[string][]byte{}}
	o3, err := open(context.Background(), r2.path, rr.deps(), fastTune)
	if err != nil {
		t.Fatal(err)
	}
	defer o3.Close()
	waitFor(t, "重启后立即发", func() bool { return len(ch2.calls()) == 1 })
}

func TestTargetsAreIndependentAndChannelConcurrencyIsBounded(t *testing.T) {
	gate := make(chan struct{})
	ch := &fakeCh{name: "tg", gate: gate}
	// 用 gate 卡住发送，数同时在途的个数
	r := newRig(t, fastTune, ch)
	for i := 0; i < 8; i++ {
		r.mustEnqueue(text("tg", fmt.Sprint("chat", i), "x"))
	}
	waitFor(t, "4 个在途", func() bool {
		r.o.mu.Lock()
		defer r.o.mu.Unlock()
		return r.o.busy["tg"] == sendsPerChan
	})
	time.Sleep(50 * time.Millisecond)
	r.o.mu.Lock()
	n := r.o.busy["tg"]
	r.o.mu.Unlock()
	if n != sendsPerChan {
		t.Fatalf("每个渠道同时最多 %d 个目标在发: %d", sendsPerChan, n)
	}
	close(gate)
	waitFor(t, "全部发完", func() bool { return len(ch.calls()) == 8 })
}

func TestSameTargetSendsOneAtATime(t *testing.T) {
	var cur, peak atomic.Int32
	ch := &fakeCh{name: "tg"}
	ch.fail = func(n int, c call) error {
		v := cur.Add(1)
		for {
			p := peak.Load()
			if v <= p || peak.CompareAndSwap(p, v) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond)
		cur.Add(-1)
		return nil
	}
	r := newRig(t, fastTune, ch)
	for i := 0; i < 15; i++ {
		r.mustEnqueue(text("tg", "c1", fmt.Sprint(i)))
	}
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 15 })
	if peak.Load() != 1 {
		t.Fatalf("同一目标一次只发一条: %d", peak.Load())
	}
}

func TestMediaItems(t *testing.T) {
	var got []string
	var mu sync.Mutex
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune, ch)
	r.data["img1"] = []byte("PNG")
	r.o.deps.SendMedia = func(ctx context.Context, c channel.Channel, chat string, att media.Attachment, data []byte) error {
		mu.Lock()
		got = append(got, att.ID+":"+string(data))
		mu.Unlock()
		return nil
	}
	r.mustEnqueue(Item{Channel: "tg", Chat: "c1", Media: &media.Attachment{ID: "img1", MIME: "image/png"}})
	r.mustEnqueue(Item{Channel: "tg", Chat: "c1", Media: &media.Attachment{ID: "gone", MIME: "image/png"}})
	r.mustEnqueue(text("tg", "c1", "after"))
	waitFor(t, "发完", func() bool { return len(ch.calls()) == 1 && r.pending() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "img1:PNG" {
		t.Fatalf("附件: %v", got)
	}
	if st := r.o.Stats(); st.Dropped24h["media_missing"] != 1 {
		t.Fatalf("附件读不到要丢弃并记原因: %+v", st)
	}
}

func TestCompactionKeepsPendingAndBoundedHistory(t *testing.T) {
	ch := &fakeCh{name: "tg"}
	r := newRig(t, fastTune, ch)
	r.setCh("keep", nil)
	r.mustEnqueue(text("keep", "c1", "waiting")) // 渠道不在，一直待发
	for i := 0; i < 2500; i++ {
		for r.queued() > 1500 { // 别超过待发上限
			time.Sleep(time.Millisecond)
		}
		r.mustEnqueue(text("tg", fmt.Sprint("c", i%100), fmt.Sprint(i)))
	}
	waitFor(t, "全部发完", func() bool { return len(ch.calls()) == 2500 })
	waitFor(t, "压缩过", func() bool {
		b, _ := os.ReadFile(r.path)
		return strings.Count(string(b), "\n") < 6000 // 不压缩的话有 7500 多行
	})
	r.o.Close()
	ch2 := &fakeCh{name: "keep"}
	r.setCh("keep", ch2)
	o2, err := open(context.Background(), r.path, r.deps(), fastTune)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	waitFor(t, "压缩后待发的还在", func() bool { return len(ch2.calls()) == 1 })
	o2.mu.Lock()
	h := len(o2.history)
	o2.mu.Unlock()
	if h > historyKeep+64 {
		t.Fatalf("已结局的记录有界: %d", h)
	}
	if id := r.o.seq; id < 2501 {
		t.Fatalf("序号不回退: %d", id)
	}
}

func TestEnqueueDoesNotBlockOnDiskOrSend(t *testing.T) {
	gate := make(chan struct{})
	ch := &fakeCh{name: "tg", gate: gate}
	r := newRig(t, fastTune, ch)
	start := time.Now()
	for i := 0; i < 200; i++ {
		r.mustEnqueue(text("tg", fmt.Sprint("c", i%50), "x"))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Enqueue 不等发送: %v", d)
	}
	close(gate)
}

func TestCloseDrainsAndRejects(t *testing.T) {
	r := newRig(t, fastTune) // 渠道不在
	for i := 0; i < 50; i++ {
		r.mustEnqueue(text("tg", "c1", fmt.Sprint(i)))
	}
	r.o.Close()
	if _, err := r.o.Enqueue(text("tg", "c1", "late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后拒绝: %v", err)
	}
	r.o.Close() // 可重复
	r2, _ := open(context.Background(), r.path, r.deps(), fastTune)
	defer r2.Close()
	if len(r2.all) != 50 {
		t.Fatalf("Close 要把已受理的记录写完: %d", len(r2.all))
	}
}

// blockCh：H 立刻失败；A、B 开头的一直阻塞到 release；其余直接成功。统计每个窗口同时在发的最大条数。
type blockCh struct {
	name    string
	release chan struct{}
	mu      sync.Mutex
	cur     map[string]int
	peak    map[string]int
	got     []call
}

func newBlockCh(name string) *blockCh {
	return &blockCh{name: name, release: make(chan struct{}), cur: map[string]int{}, peak: map[string]int{}}
}
func (b *blockCh) Name() string                                     { return b.name }
func (b *blockCh) Run(ctx context.Context, _ channel.Handler) error { <-ctx.Done(); return nil }
func (b *blockCh) Send(ctx context.Context, chat, text, reply string) error {
	b.mu.Lock()
	b.cur[chat]++
	b.peak[chat] = max(b.peak[chat], b.cur[chat])
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.cur[chat]--; b.mu.Unlock() }()
	switch {
	case text == "H":
		return errors.New("H 总是失败")
	case strings.HasPrefix(text, "A"), strings.HasPrefix(text, "B"):
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	b.got = append(b.got, call{chat: chat, text: text})
	b.mu.Unlock()
	return nil
}
func (b *blockCh) peakOf(chat string) int { b.mu.Lock(); defer b.mu.Unlock(); return b.peak[chat] }

// 回归：队头失败 3 次被放行后，后面一条在发的时候，队头的重试不能同时启动。
func TestOneInFlightPerTargetWhileHeadRetries(t *testing.T) {
	ch := newBlockCh("tg")
	r := newRig(t, fastTune)
	r.setCh("tg", ch)
	r.mustEnqueue(text("tg", "w", "H"))
	r.mustEnqueue(text("tg", "w", "A"))
	time.Sleep(400 * time.Millisecond)
	if p := ch.peakOf("w"); p != 1 {
		t.Fatalf("同一个窗口同时在发 %d 条", p)
	}
	close(ch.release)
}

// 回归：渠道的发送位占满时，到点的队头不能让扫描空转。
func TestNoBusyLoopWhenChannelSlotsAreFull(t *testing.T) {
	ch := newBlockCh("tg")
	var nows atomic.Int64
	r := &rig{t: t, path: filepath.Join(t.TempDir(), "outbox.jsonl"), chs: map[string]channel.Channel{"tg": ch}, data: map[string][]byte{}}
	d := r.deps()
	d.Now = func() time.Time { nows.Add(1); return time.Now() }
	tn := fastTune
	tn.tick = time.Hour
	o, err := open(context.Background(), r.path, d, tn)
	if err != nil {
		t.Fatal(err)
	}
	r.o = o
	t.Cleanup(o.Close)
	r.mustEnqueue(text("tg", "w", "H"))
	waitFor(t, "H 失败满 3 次", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		e := o.all["o1"]
		return e != nil && e.fails >= passAfter
	})
	for i := 0; i < sendsPerChan; i++ {
		r.mustEnqueue(text("tg", fmt.Sprint("b", i), "B"))
	}
	waitFor(t, "发送位占满", func() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.busy["tg"] == sendsPerChan })
	time.Sleep(100 * time.Millisecond) // 让 H 的退避到点
	before := nows.Load()
	time.Sleep(300 * time.Millisecond)
	if n := nows.Load() - before; n > 20 {
		t.Fatalf("渠道占满期间扫描空转：300ms 里 Now 被调用 %d 次", n)
	}
	close(ch.release)
	waitFor(t, "位置空出后 H 仍会重试", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.all["o1"] != nil && o.all["o1"].fails > passAfter
	})
}

type mediaCh struct {
	fakeCh
	sendMedia func() error
	files     int
}

func (m *mediaCh) SendMedia(ctx context.Context, chat string, att media.Attachment, data []byte) error {
	m.mu.Lock()
	m.files++
	m.mu.Unlock()
	return m.sendMedia()
}

func TestMediaFailureRules(t *testing.T) {
	att := &media.Attachment{ID: "img", MIME: "image/png", Name: "a.png"}
	newM := func(f func() error) (*mediaCh, *rig) {
		m := &mediaCh{fakeCh: fakeCh{name: "tg"}, sendMedia: f}
		r := newRig(t, fastTune, m)
		r.data["img"] = []byte("PNG")
		r.o.deps.MediaNote = func(a media.Attachment, err error) string {
			if err == nil {
				return "NOTE-NOFILE"
			}
			return "NOTE-FAIL:" + err.Error()
		}
		return m, r
	}
	t.Run("渠道不能发文件：只发说明", func(t *testing.T) {
		ch := &fakeCh{name: "tg"}
		r := newRig(t, fastTune, ch)
		r.data["img"] = []byte("PNG")
		r.o.deps.MediaNote = func(a media.Attachment, err error) string { return fmt.Sprint("NOTE:", err) }
		r.mustEnqueue(Item{Channel: "tg", Chat: "c", Media: att})
		waitFor(t, "说明", func() bool { return len(ch.calls()) == 1 })
		if ch.calls()[0].text != "NOTE:<nil>" {
			t.Fatalf("%v", ch.calls())
		}
	})
	t.Run("ErrGone：丢弃，不发说明", func(t *testing.T) {
		m, r := newM(func() error { return fmt.Errorf("x: %w", channel.ErrGone) })
		r.mustEnqueue(Item{Channel: "tg", Chat: "c", Media: att})
		waitFor(t, "丢弃", func() bool { return r.o.Stats().Dropped24h["gone"] == 1 })
		if len(m.calls()) != 0 {
			t.Fatalf("不该发说明: %v", m.calls())
		}
	})
	t.Run("限流：等待后重试附件，不发说明", func(t *testing.T) {
		var n atomic.Int32
		m, r := newM(func() error {
			if n.Add(1) <= 2 {
				return &limitErr{20 * time.Millisecond}
			}
			return nil
		})
		r.mustEnqueue(Item{Channel: "tg", Chat: "c", Media: att})
		waitFor(t, "附件发出", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.files == 3 && r.pending() == 0 })
		if len(m.calls()) != 0 || len(r.o.Recent(5)) != 0 {
			t.Fatalf("限流不发说明、不记失败: %v %v", m.calls(), r.o.Recent(5))
		}
	})
	t.Run("其他错误：发一条说明就算已发，Recent 记一次", func(t *testing.T) {
		m, r := newM(func() error { return errors.New("上传失败") })
		r.mustEnqueue(Item{Channel: "tg", Chat: "c", Media: att})
		waitFor(t, "说明发出", func() bool { return len(m.calls()) == 1 && r.pending() == 0 })
		time.Sleep(60 * time.Millisecond)
		if got := m.calls(); len(got) != 1 || got[0].text != "NOTE-FAIL:上传失败" {
			t.Fatalf("只发一条说明，不重复: %v", got)
		}
		if rec := r.o.Recent(5); len(rec) != 1 || !strings.Contains(rec[0].Error, "附件没发出去") {
			t.Fatalf("Recent: %+v", rec)
		}
	})
	t.Run("说明也发不出去：重试，下次仍先试附件", func(t *testing.T) {
		var notes atomic.Int32
		m, r := newM(func() error { return errors.New("上传失败") })
		m.fail = func(n int, c call) error {
			if notes.Add(1) == 1 {
				return errors.New("说明也失败")
			}
			return nil
		}
		r.mustEnqueue(Item{Channel: "tg", Chat: "c", Media: att})
		waitFor(t, "说明最终发出", func() bool { return len(m.calls()) == 1 && r.pending() == 0 })
		m.mu.Lock()
		files := m.files
		m.mu.Unlock()
		if files != 2 {
			t.Fatalf("每次重试都先试附件: %d", files)
		}
	})
}

// 压缩后的第一条记录带最大 ID：历史全部过期也不会让 ID 从头开始。
func TestCompactionKeepsSequence(t *testing.T) {
	r := newRig(t, fastTune)
	r.o.mu.Lock()
	r.o.seq = 777
	recs := r.o.compactRecordsLocked()
	r.o.mu.Unlock()
	first, ok := recs[0].(rec)
	if !ok || first.T != recSeq || first.ID != "o777" {
		t.Fatalf("第一条: %+v", recs[0])
	}
	r.o.Close()
	path := filepath.Join(t.TempDir(), "o.jsonl")
	j, err := journalOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Rewrite(recs); err != nil {
		t.Fatal(err)
	}
	j.Close()
	rr := &rig{chs: map[string]channel.Channel{}, data: map[string][]byte{}}
	o, err := open(context.Background(), path, rr.deps(), fastTune)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	id, err := o.Enqueue(text("tg", "c", "x"))
	if err != nil || id != "o778" {
		t.Fatalf("重启后 ID 接着往后: %q %v", id, err)
	}
}
