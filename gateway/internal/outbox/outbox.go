// Package outbox 是发往外部聊天平台的可靠出站：消息先落盘再发送，重启后继续，
// 同一个聊天窗口严格按入队顺序发，发送结果有回执（已发，或已丢弃并附原因）。
// 保证“至少一次”，不承诺恰好一次：结果不明（超时）和崩溃在回执写入之前，都会再发一次。
//
// 命令行、网页、Job 卡片、审批卡片、Hook 通知和流式回复不经过这里。
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/media"
)

var (
	ErrFull     = errors.New("出站队列已满")
	ErrTooLarge = errors.New("消息超过出站日志的单条上限")
	ErrStorage  = errors.New("出站存储不可用")
	ErrClosed   = errors.New("出站已关闭")
)

const (
	recordLimit  = 1 << 20  // 单条记录上限；超过的消息不能丢，由调用方走内存直发
	fileLimit    = 32 << 20 // 日志文件硬上限（待发正文最多 8 MiB，留足余量）
	compactBytes = 4 << 20  // 超过这么大且明显有可回收的内容时压缩
	maxItems     = 2048     // 待发条数
	maxBytes     = 8 << 20  // 待发正文字节数
	maxPerTarget = 256      // 单个聊天窗口待发条数
	sendsPerChan = 4        // 每个渠道同时在发的聊天窗口数
	passAfter    = 3        // 队头连续失败这么多次后，后面的消息才放行
	historyKeep  = 500      // 已结局的记录保留条数
	historyAge   = 7 * 24 * time.Hour
	recentKeep   = 100
	healthWindow = 24 * time.Hour
)

// Item 是一条要发的消息：文字（Parts）或一个附件（Media）。
type Item struct {
	Channel  string            `json:"channel"` // 渠道名，发送时按名字取当前的渠道对象
	Chat     string            `json:"chat"`
	Parts    []string          `json:"parts,omitempty"` // 已按渠道实际的发法切好的段；媒体消息为空
	Markdown bool              `json:"markdown,omitempty"`
	Plain    bool              `json:"plain,omitempty"`    // Parts 的结果：逐段用 Send 发；否则用 SendMarkdown 发
	ReplyTo  string            `json:"reply_to,omitempty"` // 只用于第一段
	Delay    time.Duration     `json:"delay,omitempty"`    // 第一段之前的停顿，保留群聊和私聊分条的节奏
	Media    *media.Attachment `json:"media,omitempty"`    // 只存引用，发送时从媒体库读
	SID      string            `json:"sid,omitempty"`      // 来源会话，只用于展示
	Kind     string            `json:"kind,omitempty"`     // reply | notice | proactive | command，只用于统计
}

type Deps struct {
	Channel   func(name string) channel.Channel // 当前的渠道对象；nil 表示暂时没有。不能去拿 App 的总锁
	Media     func(id string) ([]byte, error)
	SendMedia func(ctx context.Context, ch channel.Channel, chat string, att media.Attachment, data []byte) error // 可选：只发附件本身；默认用渠道的 MediaSender
	MediaNote func(att media.Attachment, err error) string                                                        // 说明的文字；err 为 nil 表示渠道不能发文件
	Now       func() time.Time
	Log       *slog.Logger
	Health    *health.Handle
}

// Record 是一次失败或丢弃，供网页和 /status 展示。
type Record struct {
	ID      string    `json:"id"`
	Channel string    `json:"channel"`
	Chat    string    `json:"chat"`
	Kind    string    `json:"kind,omitempty"`
	State   string    `json:"state"`            // failed | dropped
	Reason  string    `json:"reason,omitempty"` // 丢弃的原因：gone | expired | media_missing | channel_removed
	Error   string    `json:"error,omitempty"`
	Fails   int       `json:"fails,omitempty"`
	At      time.Time `json:"at"`
}

type ChannelStats struct {
	Pending int       `json:"pending"`
	Oldest  time.Time `json:"oldest"`  // 最老一条待发的入队时间
	Failing int       `json:"failing"` // 连续失败中的条数
}

type Stats struct {
	Channels    map[string]ChannelStats `json:"channels"`
	Dropped24h  map[string]int          `json:"dropped_24h"`       // 原因 → 条数
	Full24h     int                     `json:"full_24h"`          // 队列满而拒绝的次数
	TooLarge24h int                     `json:"too_large_24h"`     // 超过单条上限而拒绝的次数
	Storage     string                  `json:"storage,omitempty"` // 存储失败的原因；空表示正常
}

type tune struct {
	backoffBase, backoffMax time.Duration
	expiry                  time.Duration
	tick                    time.Duration // 没有别的事时最久多久检查一次
	absentWait              time.Duration // 渠道暂时不在时多久再看
	sendTimeout             time.Duration
}

var defaultTune = tune{5 * time.Second, 5 * time.Minute, 24 * time.Hour, time.Minute, 5 * time.Second, 2 * time.Minute}

type entry struct {
	id        string
	n         uint64
	item      Item
	at        time.Time
	size      int
	durable   bool // enqueue 记录已落盘；没落盘之前绝不发送
	sent      int  // 已发的段数
	fails     int  // 连续失败次数
	next      time.Time
	lastErr   string
	inflight  bool
	headAt    time.Time
	delayDone bool
	settled   bool
}

type target struct{ q []*entry }

type settled struct {
	id     string
	n      uint64
	item   Item // 只留展示用的字段
	at     time.Time
	result string
	reason string
	setAt  time.Time
}

type event struct {
	at     time.Time
	kind   string // drop | full | large
	reason string
}

type Outbox struct {
	deps Deps
	tn   tune
	ctx  context.Context
	stop context.CancelFunc
	log  *slog.Logger

	hmu sync.Mutex // 串行化健康条目的写入；顺序 hmu → mu，不在 mu 里调用 Deps

	mu         sync.Mutex
	closed     bool
	storageErr error
	seq        uint64
	targets    map[string]*target
	all        map[string]*entry
	count      int
	bytes      int
	busy       map[string]int
	history    []settled
	events     []event
	recent     []Record
	wq         []wrec
	wstop      bool
	settledN   int // 上次压缩以来的结局数

	wwake chan struct{}
	wake  chan struct{}
	wdone chan struct{}
	wg    sync.WaitGroup

	failWrite atomic.Bool // 测试用：让之后的写盘失败
}

// Open 打开日志、重放并开始投递。存储损坏时仍返回一个拒绝受理的 *Outbox（Enqueue 返回
// ErrStorage，健康条目报 error，原文件保持不动）和对应的错误，调用方退回内存直发。
func Open(ctx context.Context, path string, deps Deps) (*Outbox, error) {
	return open(ctx, path, deps, defaultTune)
}

func open(ctx context.Context, path string, deps Deps, tn tune) (*Outbox, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	cctx, cancel := context.WithCancel(ctx)
	o := &Outbox{deps: deps, tn: tn, ctx: cctx, stop: cancel, log: deps.Log,
		targets: map[string]*target{}, all: map[string]*entry{}, busy: map[string]int{},
		wwake: make(chan struct{}, 1), wake: make(chan struct{}, 1), wdone: make(chan struct{})}
	j, err := o.load(path)
	if err != nil {
		o.storageErr = err
		o.log.Error("出站存储不可用，消息改为内存直发，原文件保持不动", "err", err)
		close(o.wdone)
		o.refreshHealth()
		return o, err
	}
	o.refreshHealth()
	go o.writer(j)
	o.wg.Add(1)
	go o.run()
	return o, nil
}

func (o *Outbox) poke() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func targetKey(it Item) string { return it.Channel + "\x00" + it.Chat }

func itemSize(it Item) int {
	n := 0
	for _, p := range it.Parts {
		n += len(p)
	}
	return n
}

// Enqueue 把消息交给出站。只做内存操作：不等磁盘、不阻塞，可以在 App 的总锁下调用。
// 没有被接受时返回错误：
//   - ErrFull 是明确拒绝，不是退回：调用方记一条警告，这条消息就不发了；
//   - ErrTooLarge、ErrStorage（和 ErrClosed）：调用方退回内存直发。
func (o *Outbox) Enqueue(it Item) (string, error) {
	size := itemSize(it)
	// 记录的 JSON 比正文略大（转义、字段名）；按最坏情况留余量，真正的上限由日志再把关
	enc, err := encodeRec(rec{T: recEnqueue, ID: "o00000000000000000000", At: 1 << 60, Item: &it})
	if err == nil && len(enc) > recordLimit {
		err = ErrTooLarge
	}
	now := o.deps.Now()
	o.mu.Lock()
	switch {
	case o.closed:
		o.mu.Unlock()
		return "", ErrClosed
	case o.storageErr != nil:
		o.mu.Unlock()
		return "", ErrStorage
	case err != nil:
		o.noteLocked(now, "large", "")
		o.mu.Unlock()
		o.refreshHealth()
		if errors.Is(err, ErrTooLarge) {
			return "", ErrTooLarge
		}
		return "", err
	}
	t := o.targets[targetKey(it)]
	if o.count >= maxItems || o.bytes+size > maxBytes || (t != nil && len(t.q) >= maxPerTarget) {
		o.noteLocked(now, "full", "")
		o.mu.Unlock()
		o.refreshHealth()
		return "", ErrFull
	}
	o.seq++
	e := &entry{id: fmt.Sprintf("o%d", o.seq), n: o.seq, item: it, at: now, size: size}
	o.addLocked(e)
	r := rec{T: recEnqueue, ID: e.id, At: now.UnixMilli(), Item: &it}
	o.wq = append(o.wq, wrec{r: r, e: e})
	o.mu.Unlock()
	o.kickWriter()
	return e.id, nil
}

func (o *Outbox) addLocked(e *entry) {
	k := targetKey(e.item)
	t := o.targets[k]
	if t == nil {
		t = &target{}
		o.targets[k] = t
	}
	t.q = append(t.q, e)
	o.all[e.id] = e
	o.count++
	o.bytes += e.size
}

func (o *Outbox) noteLocked(now time.Time, kind, reason string) {
	o.events = append(o.events, event{now, kind, reason})
	if len(o.events) > 4096 {
		o.events = o.events[len(o.events)-2048:]
	}
}

// settleLocked 给一条消息记结局（sent 或 dropped），并把它从待发里拿掉。
func (o *Outbox) settleLocked(e *entry, now time.Time, result, reason string) {
	if e.settled {
		return
	}
	e.settled = true
	k := targetKey(e.item)
	if t := o.targets[k]; t != nil {
		for i, x := range t.q {
			if x == e {
				t.q = append(t.q[:i], t.q[i+1:]...)
				break
			}
		}
		if len(t.q) == 0 {
			delete(o.targets, k)
		}
	}
	delete(o.all, e.id)
	o.count--
	o.bytes -= e.size
	o.addHistoryLocked(settled{id: e.id, n: e.n, item: brief(e.item), at: e.at, result: result, reason: reason, setAt: now})
	if result == "dropped" {
		o.noteLocked(now, "drop", reason)
		o.recentLocked(Record{ID: e.id, Channel: e.item.Channel, Chat: e.item.Chat, Kind: e.item.Kind, State: "dropped", Reason: reason, Error: e.lastErr, Fails: e.fails, At: now})
		o.log.Warn("出站丢弃一条消息", "id", e.id, "channel", e.item.Channel, "reason", reason, "err", e.lastErr)
	}
	o.settledN++
	o.wq = append(o.wq, wrec{r: rec{T: recSettle, ID: e.id, At: now.UnixMilli(), Result: result, Reason: reason}})
}

func brief(it Item) Item {
	return Item{Channel: it.Channel, Chat: it.Chat, Kind: it.Kind, SID: it.SID, Markdown: it.Markdown}
}

func (o *Outbox) addHistoryLocked(s settled) {
	o.history = append(o.history, s)
	if len(o.history) > historyKeep+64 { // 攒一点再整理，不每条都挪
		o.trimHistoryLocked(s.setAt)
	}
}

func (o *Outbox) trimHistoryLocked(now time.Time) {
	keep := o.history[:0]
	for _, h := range o.history {
		if now.Sub(h.setAt) <= historyAge {
			keep = append(keep, h)
		}
	}
	if len(keep) > historyKeep {
		keep = keep[len(keep)-historyKeep:]
	}
	o.history = keep
}

func (o *Outbox) recentLocked(r Record) {
	o.recent = append(o.recent, r)
	if len(o.recent) > recentKeep {
		o.recent = o.recent[len(o.recent)-recentKeep:]
	}
}

// DropChannel：机器人被删除，它的待发消息以 channel_removed 结局。
func (o *Outbox) DropChannel(name string) {
	now := o.deps.Now()
	o.mu.Lock()
	var hit []*entry
	for _, e := range o.all {
		if e.item.Channel == name {
			hit = append(hit, e)
		}
	}
	sort.Slice(hit, func(i, j int) bool { return hit[i].n < hit[j].n })
	for _, e := range hit {
		o.settleLocked(e, now, "dropped", "channel_removed")
	}
	o.mu.Unlock()
	if len(hit) > 0 {
		o.kickWriter()
		o.refreshHealth()
		o.poke()
	}
}

func (o *Outbox) Stats() Stats {
	now := o.deps.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Stats{Channels: map[string]ChannelStats{}, Dropped24h: map[string]int{}}
	for _, e := range o.all {
		c := st.Channels[e.item.Channel]
		c.Pending++
		if c.Oldest.IsZero() || e.at.Before(c.Oldest) {
			c.Oldest = e.at
		}
		if e.fails > 0 {
			c.Failing++
		}
		st.Channels[e.item.Channel] = c
	}
	for _, ev := range o.events {
		if now.Sub(ev.at) > healthWindow {
			continue
		}
		switch ev.kind {
		case "drop":
			st.Dropped24h[ev.reason]++
		case "full":
			st.Full24h++
		case "large":
			st.TooLarge24h++
		}
	}
	if o.storageErr != nil {
		st.Storage = o.storageErr.Error()
	}
	return st
}

// Recent 是最近 n 次失败和丢弃，新的在前。
func (o *Outbox) Recent(n int) []Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Record, 0, n)
	for i := len(o.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, o.recent[i])
	}
	return out
}

// refreshHealth 按当前情况写健康条目：存储失败为 error；有条目连续失败 3 次以上、
// 24 小时内有丢弃或队列满、有消息超过单条上限为 degraded。不在 o.mu 里调用 Deps。
func (o *Outbox) refreshHealth() {
	if o.deps.Health == nil {
		return
	}
	now := o.deps.Now()
	o.hmu.Lock()
	defer o.hmu.Unlock()
	o.mu.Lock()
	state, detail := health.OK, ""
	if o.storageErr != nil {
		state, detail = health.Error, "存储不可用，消息改为内存直发："+o.storageErr.Error()
	} else {
		var why []string
		failing := 0
		for _, e := range o.all {
			if e.fails >= passAfter {
				failing++
			}
		}
		if failing > 0 {
			why = append(why, fmt.Sprintf("%d 条连续失败", failing))
		}
		drops := map[string]int{}
		full, large := 0, 0
		for _, ev := range o.events {
			if now.Sub(ev.at) > healthWindow {
				continue
			}
			switch ev.kind {
			case "drop":
				if ev.reason != "channel_removed" { // 删除机器人是有意的
					drops[ev.reason]++
				}
			case "full":
				full++
			case "large":
				large++
			}
		}
		if len(drops) > 0 {
			var parts []string
			total := 0
			for r, n := range drops {
				parts = append(parts, fmt.Sprintf("%s %d", r, n))
				total += n
			}
			sort.Strings(parts)
			why = append(why, fmt.Sprintf("24 小时内丢弃 %d 条（%s）", total, strings.Join(parts, "、")))
		}
		if full > 0 {
			why = append(why, fmt.Sprintf("队列满拒绝了 %d 次", full))
		}
		if large > 0 {
			why = append(why, fmt.Sprintf("%d 条消息超过单条上限，已改内存直发", large))
		}
		if len(why) > 0 {
			state, detail = health.Degraded, strings.Join(why, "；")
		}
	}
	o.mu.Unlock()
	o.deps.Health.Set(state, detail)
}

// Close 停止受理，取消在途发送，等写协程把已受理的记录写完。
func (o *Outbox) Close() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	o.mu.Unlock()
	o.stop()
	o.wg.Wait()
	o.mu.Lock()
	o.wstop = true
	o.mu.Unlock()
	o.kickWriter()
	<-o.wdone
}
