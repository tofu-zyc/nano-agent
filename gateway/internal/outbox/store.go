package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"nano-agent/gateway/internal/journal"
)

const (
	recEnqueue  = "enqueue"
	recProgress = "progress"
	recAttempt  = "attempt"
	recSettle   = "settle"
	recSeq      = "seq" // 压缩时写在第一条，只带当前最大的 ID
)

// rec 是日志里的一条记录。四种：enqueue（整条 Item）、progress（已发段数）、
// attempt（失败次数、最后的错误、下次时间）、settle（sent 或 dropped、原因、时间）。
type rec struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	At     int64  `json:"at,omitempty"` // 毫秒
	Item   *Item  `json:"item,omitempty"`
	Sent   int    `json:"sent,omitempty"`
	Fails  int    `json:"fails,omitempty"`
	Err    string `json:"err,omitempty"`
	Next   int64  `json:"next,omitempty"`
	Result string `json:"result,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func encodeRec(r rec) ([]byte, error) { return json.Marshal(r) }

type wrec struct {
	r rec
	e *entry // enqueue 记录写成功后这条才能发送
}

func idNum(id string) uint64 {
	n, _ := strconv.ParseUint(id[1:], 10, 64)
	return n
}

// load 打开日志并重放：恢复待发集合和最近的结局记录。
func (o *Outbox) load(path string) (*journal.Journal, error) {
	pending := map[string]*entry{}
	var hist []settled
	j, err := journal.Open(path, 1, journal.Limits{Record: recordLimit, File: fileLimit}, func(_ uint64, data json.RawMessage) error {
		var r rec
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.ID) < 2 || r.ID[0] != 'o' || idNum(r.ID) == 0 {
			return fmt.Errorf("记录的 ID 不对：%q", r.ID)
		}
		n := idNum(r.ID)
		if n > o.seq {
			o.seq = n
		}
		switch r.T {
		case recEnqueue:
			if r.Item == nil || pending[r.ID] != nil {
				return nil
			}
			pending[r.ID] = &entry{id: r.ID, n: n, item: *r.Item, at: time.UnixMilli(r.At), size: itemSize(*r.Item)}
		case recProgress:
			if e := pending[r.ID]; e != nil {
				e.sent = r.Sent
			}
		case recAttempt:
			if e := pending[r.ID]; e != nil {
				e.fails, e.lastErr = r.Fails, r.Err
				if r.Next > 0 {
					e.next = time.UnixMilli(r.Next)
				}
			}
		case recSettle:
			// 压缩可能已经把更早的 enqueue 丢掉；对不上的结局记录直接忽略
			if e := pending[r.ID]; e != nil {
				delete(pending, r.ID)
				hist = append(hist, settled{id: e.id, n: e.n, item: brief(e.item), at: e.at, result: r.Result, reason: r.Reason, setAt: time.UnixMilli(r.At)})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	now := o.deps.Now()
	list := make([]*entry, 0, len(pending))
	for _, e := range pending {
		list = append(list, e)
	}
	sort.Slice(list, func(i, k int) bool { return list[i].n < list[k].n })
	for _, e := range list {
		e.durable, e.delayDone = true, true // 已经落盘；重启后不再等 Delay
		o.addLocked(e)
	}
	o.history = hist
	o.trimHistoryLocked(now)
	for _, h := range o.history {
		if h.result == "dropped" && now.Sub(h.setAt) <= healthWindow {
			o.events = append(o.events, event{h.setAt, "drop", h.reason})
			o.recent = append(o.recent, Record{ID: h.id, Channel: h.item.Channel, Chat: h.item.Chat, Kind: h.item.Kind, State: "dropped", Reason: h.reason, At: h.setAt})
		}
	}
	if len(list) > 0 {
		o.log.Info("出站恢复了待发消息", "pending", len(list))
	}
	return j, nil
}

func (o *Outbox) kickWriter() {
	select {
	case o.wwake <- struct{}{}:
	default:
	}
}

// queueRec 记一条不影响发送资格的记录（progress、attempt）。调用方持有 o.mu。
func (o *Outbox) queueLocked(r rec) { o.wq = append(o.wq, wrec{r: r}) }

// writer 是日志的唯一写协程：按入队顺序把记录写盘，enqueue 写成功后放行发送。
func (o *Outbox) writer(j *journal.Journal) {
	defer close(o.wdone)
	for {
		o.mu.Lock()
		batch, stop := o.wq, o.wstop
		o.wq = nil
		o.mu.Unlock()
		if len(batch) == 0 {
			if stop {
				break
			}
			<-o.wwake
			continue
		}
		for _, w := range batch {
			if j != nil {
				err := j.Append(w.r)
				if o.failWrite.Load() {
					err = errors.New("注入的写盘错误")
				}
				if err != nil {
					o.failStorage(err)
					j.Close()
					j = nil
				}
			}
			if w.e != nil {
				o.mu.Lock()
				w.e.durable = true // 存储失败后退回内存模式：不再有落盘保证，但已受理的消息照发
				o.mu.Unlock()
			}
		}
		o.poke()
		if j != nil {
			o.maybeCompact(j)
		}
	}
	if j != nil {
		j.Close()
	}
}

func (o *Outbox) failStorage(err error) {
	o.mu.Lock()
	if o.storageErr == nil {
		o.storageErr = err
	}
	o.mu.Unlock()
	o.log.Error("出站写盘失败，停止受理，消息改为内存直发；已受理的照常发送", "err", err)
	o.refreshHealth()
}

// maybeCompact：文件超过 4 MiB 且明显有可回收的内容，或已结局的记录多过待发的，就改写。
// 已结局的只留最近 500 条或 7 天，供展示。
func (o *Outbox) maybeCompact(j *journal.Journal) {
	now := o.deps.Now()
	o.mu.Lock()
	live := int64(o.bytes) + int64(len(o.all))*256
	need := (j.Size() > compactBytes && j.Size() > 2*live) || (o.settledN >= 1000 && o.settledN > len(o.all))
	if !need {
		o.mu.Unlock()
		return
	}
	o.trimHistoryLocked(now)
	recs := o.compactRecordsLocked()
	o.settledN = 0
	o.mu.Unlock()
	if err := j.Rewrite(recs); err != nil {
		o.log.Warn("出站日志压缩失败", "err", err)
	}
}

// compactRecordsLocked 是改写后的日志内容。第一条只带当前最大的 ID：历史全部过期之后
// 再压缩，重启时 ID 也不会从头开始、和用过的重复。
func (o *Outbox) compactRecordsLocked() []any {
	recs := []any{rec{T: recSeq, ID: fmt.Sprintf("o%d", o.seq)}}
	for _, h := range o.history {
		it := h.item
		recs = append(recs, rec{T: recEnqueue, ID: h.id, At: h.at.UnixMilli(), Item: &it},
			rec{T: recSettle, ID: h.id, At: h.setAt.UnixMilli(), Result: h.result, Reason: h.reason})
	}
	live2 := make([]*entry, 0, len(o.all))
	for _, e := range o.all {
		if e.durable { // 还在写队列里的 enqueue 稍后会正常追加
			live2 = append(live2, e)
		}
	}
	sort.Slice(live2, func(i, k int) bool { return live2[i].n < live2[k].n })
	for _, e := range live2 {
		it := e.item
		recs = append(recs, rec{T: recEnqueue, ID: e.id, At: e.at.UnixMilli(), Item: &it})
		if e.sent > 0 {
			recs = append(recs, rec{T: recProgress, ID: e.id, Sent: e.sent})
		}
		if e.fails > 0 || !e.next.IsZero() {
			recs = append(recs, rec{T: recAttempt, ID: e.id, Fails: e.fails, Err: e.lastErr, Next: e.next.UnixMilli()})
		}
	}
	return recs
}
