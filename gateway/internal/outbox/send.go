package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/media"
)

// run 是投递协程：按入队顺序给每个聊天窗口挑下一条能发的，交给发送协程。
func (o *Outbox) run() {
	defer o.wg.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait := o.scan()
		o.refreshHealth()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-o.ctx.Done():
			return
		case <-o.wake:
		case <-timer.C:
		}
	}
}

// scan 在持有 o.mu 时决定谁该发；返回下一次最迟什么时候再看。
//
// 一个聊天窗口里按入队顺序找第一条能发的：
//   - 正在发的，后面的都等；
//   - 连续失败不到 3 次的，没到时间或没落盘就卡住后面的；
//   - 连续失败 3 次以上的，到时间就重试，没到时间就让后面的先发（它继续按退避重试）。
func (o *Outbox) scan() time.Duration {
	now := o.deps.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	next := o.tn.tick
	soon := func(d time.Duration) {
		if d < time.Millisecond {
			d = time.Millisecond
		}
		if d < next {
			next = d
		}
	}
	for _, t := range o.targets {
		for _, e := range append([]*entry(nil), t.q...) {
			if e.inflight || !e.durable {
				continue
			}
			if age := now.Sub(e.at); age >= o.tn.expiry {
				o.settleLocked(e, now, "dropped", "expired")
				o.kickWriter()
			} else {
				soon(o.tn.expiry - age)
			}
		}
	}
	for _, t := range o.targets {
		if inflight(t) {
			continue // 同一个窗口一次只发一条：队列里任何一条在发，整个窗口都等它发完
		}
		for _, e := range t.q {
			if e.fails >= passAfter {
				if e.durable && !now.Before(e.next) {
					if !o.canLaunchLocked(e) {
						break // 渠道的发送位满了：不排期，等某次发送结束时叫醒
					}
					o.launchLocked(e)
					break
				}
				soon(e.next.Sub(now))
				continue
			}
			if !e.durable {
				break // 等落盘；写协程写完会叫醒
			}
			if now.Before(e.next) {
				soon(e.next.Sub(now))
				break
			}
			if e.headAt.IsZero() {
				e.headAt = now
			}
			if !e.delayDone {
				if w := e.headAt.Add(e.item.Delay).Sub(now); w > 0 {
					soon(w)
					break
				}
				e.delayDone = true
			}
			if o.canLaunchLocked(e) {
				o.launchLocked(e)
			}
			break
		}
	}
	return next
}

func inflight(t *target) bool {
	for _, e := range t.q {
		if e.inflight {
			return true
		}
	}
	return false
}

func (o *Outbox) canLaunchLocked(e *entry) bool { return o.busy[e.item.Channel] < sendsPerChan }

func (o *Outbox) launchLocked(e *entry) {
	e.inflight = true
	o.busy[e.item.Channel]++
	o.wg.Add(1)
	go o.send(e)
}

type outcome struct {
	kind  string // sent | gone | media_missing | absent | limited | failed | canceled
	err   error
	after time.Duration
}

func (o *Outbox) send(e *entry) {
	defer o.wg.Done()
	res := o.deliver(e)
	now := o.deps.Now() // 不在锁里调 Deps
	o.mu.Lock()
	e.inflight = false
	o.busy[e.item.Channel]--
	if !e.settled {
		switch res.kind {
		case "sent":
			o.settleLocked(e, now, "sent", "")
		case "gone", "media_missing":
			if res.err != nil {
				e.lastErr = res.err.Error()
			}
			o.settleLocked(e, now, "dropped", res.kind)
		case "absent":
			e.next = now.Add(o.tn.absentWait) // 渠道暂时不在：不算失败，等到过期
		case "limited":
			e.next = now.Add(res.after) // 限流：不计入失败次数，按平台给的时间等
			e.lastErr = res.err.Error()
			o.queueLocked(rec{T: recAttempt, ID: e.id, Fails: e.fails, Err: e.lastErr, Next: e.next.UnixMilli()})
		case "failed":
			e.fails++
			e.lastErr = res.err.Error()
			e.next = now.Add(o.backoff(e.fails))
			o.queueLocked(rec{T: recAttempt, ID: e.id, Fails: e.fails, Err: e.lastErr, Next: e.next.UnixMilli()})
			o.recentLocked(Record{ID: e.id, Channel: e.item.Channel, Chat: e.item.Chat, Kind: e.item.Kind, State: "failed", Error: e.lastErr, Fails: e.fails, At: now})
			o.log.Warn("出站发送失败，稍后重试", "id", e.id, "channel", e.item.Channel, "fails", e.fails, "err", res.err)
		}
	}
	o.mu.Unlock()
	o.kickWriter()
	o.refreshHealth()
	o.poke()
}

func (o *Outbox) backoff(fails int) time.Duration {
	d := o.tn.backoffBase
	for i := 1; i < fails && d < o.tn.backoffMax; i++ {
		d *= 2
	}
	return min(d, o.tn.backoffMax)
}

func classify(err error) outcome {
	if errors.Is(err, channel.ErrGone) {
		return outcome{kind: "gone", err: err}
	}
	if d, ok := channel.RetryAfter(err); ok {
		return outcome{kind: "limited", err: err, after: d}
	}
	return outcome{kind: "failed", err: err}
}

// deliver 发一条消息，不持有 o.mu。文字从没发出去的那段接着发。
func (o *Outbox) deliver(e *entry) outcome {
	if o.ctx.Err() != nil {
		return outcome{kind: "canceled"}
	}
	ch := o.deps.Channel(e.item.Channel)
	if ch == nil {
		return outcome{kind: "absent"}
	}
	it := e.item
	if it.Media != nil {
		return o.deliverMedia(e, ch)
	}
	o.mu.Lock()
	from := e.sent
	o.mu.Unlock()
	for i := from; i < len(it.Parts); i++ {
		reply := ""
		if i == 0 {
			reply = it.ReplyTo // 引用回复只用于第一段
		}
		ctx, cancel := context.WithTimeout(o.ctx, o.tn.sendTimeout)
		var err error
		if m, ok := ch.(channel.Markdowner); ok && !it.Plain && it.Markdown {
			err = m.SendMarkdown(ctx, it.Chat, it.Parts[i], reply)
		} else {
			err = ch.Send(ctx, it.Chat, it.Parts[i], reply)
		}
		cancel()
		if err != nil {
			return o.afterError(err)
		}
		o.mu.Lock()
		e.sent = i + 1
		o.queueLocked(rec{T: recProgress, ID: e.id, Sent: e.sent})
		o.mu.Unlock()
		o.kickWriter()
	}
	return outcome{kind: "sent"}
}

func (o *Outbox) afterError(err error) outcome {
	if o.ctx.Err() != nil {
		return outcome{kind: "canceled"}
	}
	return classify(err)
}

// deliverMedia 发一个附件，用户看到的和以前一样，不会反复收到说明：
//   - 渠道不能发文件：只发说明；
//   - 附件发送失败，属于 ErrGone：丢弃，不发说明；限流：按 RetryAfter 等待后重试附件，不发说明；
//   - 其他错误：发一条说明，说明发出去就算这条已发，并在 Recent 里记一次附件失败；
//     说明也发不出去，就按说明的错误分类重试，下次仍然先试附件。
func (o *Outbox) deliverMedia(e *entry, ch channel.Channel) outcome {
	it := e.item
	att := *it.Media
	send := o.deps.SendMedia
	if send == nil {
		if ms, ok := ch.(channel.MediaSender); ok {
			send = func(ctx context.Context, _ channel.Channel, chat string, a media.Attachment, data []byte) error {
				return ms.SendMedia(ctx, chat, a, data)
			}
		}
	}
	sendNote := func(cause error) outcome {
		ctx, cancel := context.WithTimeout(o.ctx, o.tn.sendTimeout)
		defer cancel()
		if err := ch.Send(ctx, it.Chat, o.mediaNote(att, cause), ""); err != nil {
			return o.afterError(err)
		}
		return outcome{kind: "sent"}
	}
	if send == nil {
		return sendNote(nil)
	}
	var data []byte
	var err error
	if o.deps.Media != nil {
		data, err = o.deps.Media(att.ID)
	} else {
		err = errors.New("没有媒体库")
	}
	if err != nil {
		return outcome{kind: "media_missing", err: err}
	}
	ctx, cancel := context.WithTimeout(o.ctx, o.tn.sendTimeout)
	err = send(ctx, ch, it.Chat, att, data)
	cancel()
	if err == nil {
		return outcome{kind: "sent"}
	}
	if res := o.afterError(err); res.kind != "failed" {
		return res // canceled、gone、限流
	}
	res := sendNote(err)
	if res.kind == "sent" {
		now := o.deps.Now()
		o.mu.Lock()
		o.recentLocked(Record{ID: e.id, Channel: it.Channel, Chat: it.Chat, Kind: it.Kind, State: "failed", Error: "附件没发出去：" + err.Error(), Fails: e.fails + 1, At: now})
		o.mu.Unlock()
	}
	return res
}

func (o *Outbox) mediaNote(att media.Attachment, err error) string {
	if o.deps.MediaNote != nil {
		return o.deps.MediaNote(att, err)
	}
	if err == nil {
		return fmt.Sprintf("［%s：%s］", att.MIME, att.Name)
	}
	return fmt.Sprintf("［%s：%s，没发出去：%s］", att.MIME, att.Name, err)
}
