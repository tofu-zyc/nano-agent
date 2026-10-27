package app

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
)

// ---- 插话的持久日志（二轮验收 B） ----
//
// data/interjections/<编码后的 sid>.jsonl，只追加、单写者（App.mu 下）。
// 每条插话有稳定 interjection_id，另有一条去重键（sid|渠道|聊天|消息 ID）。
//
// 状态不是"收到→注入→读过"那么简单：读过（included_in_request）不代表请求成功，
// 更不代表工具副作用发生过。所以请求结局（outcome）和撤回终态（withdrawn）各自独立记录：
//
//	received            已可靠受理，还没进正式历史
//	injected            已经写进正式历史（带稳定 history_message_id）
//	included_in_request 已经被某个模型请求的快照包含（关联 request_id 与 revision）
//	outcome             这一次请求有明确结局（ok / budget / timeout / cancelled / error / interrupted）
//	withdrawn           用户撤回、停止、重置、删除导致这条补充失效：重启后不能再复活
//
// 恢复时按 ID 和历史对账：写进历史但没记 injected 的补记一笔，不会重复追加正文；
// 已经 included 但没有 outcome 的标成 interrupted，保留可见的继续处理入口，不自动重放旧工具。

const (
	journalVersion = 1
	journalCheckpointAt = 512 // 记录数超过这个就重写成检查点

	jrecReceived  = "received"
	jrecInjected  = "injected"
	jrecIncluded  = "included"
	jrecOutcome   = "outcome"
	jrecWithdrawn = "withdrawn"
	jrecDiscard   = "discard"
	jrecHead      = "head"

	// 请求结局
	outcomeOK          = "ok"
	outcomeError       = "error"
	outcomeBudget      = "budget"
	outcomeTimeout     = "timeout"
	outcomeCancelled   = "cancelled"
	outcomeInterrupted = "interrupted"
	outcomeRequeued   = "requeued" // 放回会话队列，按普通消息在后续 run 处理
)

// journalRec 是日志里的一行。字段都有界；正文上限由收件箱（64 KiB）保证。
type journalRec struct {
	V          int                `json:"v"`
	Kind       string             `json:"kind"`
	Seq        int64              `json:"seq,omitempty"`
	ID         string             `json:"id,omitempty"`
	Dedup      string             `json:"dedup,omitempty"`
	Text       string             `json:"text,omitempty"`
	Images     []media.Attachment `json:"images,omitempty"`
	Who        string             `json:"who,omitempty"`
	Agent      string             `json:"agent,omitempty"` // 受理时是哪个 agent，切换后不能再交给新身份
	Channel    string             `json:"channel,omitempty"`
	Chat       string             `json:"chat,omitempty"`
	At         int64              `json:"at,omitempty"` // 接收时间（毫秒）：和历史的秒不是一回事
	Generation int64              `json:"generation,omitempty"`
	RunID      string             `json:"run_id,omitempty"`
	RequestID  string             `json:"request_id,omitempty"`
	Revision   int                `json:"revision,omitempty"`
	HistoryID  string             `json:"history_id,omitempty"`
	Outcome    string             `json:"outcome,omitempty"`
	Note       string             `json:"note,omitempty"`
}

// interjectionJournal 是一个会话的日志文件句柄。
type interjectionJournal struct {
	path  string
	file  *os.File
	count int               // 已经写进去多少条记录（用来决定什么时候重写检查点）
	dedup map[string]string // 去重键 → interjection_id（打开时从日志建好）
}

func (a *App) journalDir() string {
	return filepath.Join(a.store.Dir(), "interjections")
}

func (a *App) journalPath(sid string) string {
	return filepath.Join(a.journalDir(), url.PathEscape(sid)+".jsonl")
}

// openJournal 打开（必要时创建）会话日志。调用方持有锁。
func (a *App) openJournal(sid string) (*interjectionJournal, error) {
	if j := a.journals[sid]; j != nil {
		return j, nil
	}
	if err := os.MkdirAll(a.journalDir(), 0o700); err != nil {
		return nil, err
	}
	p := a.journalPath(sid)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j := &interjectionJournal{path: p, file: f, dedup: map[string]string{}}
	// 已有记录数：用于决定何时重写检查点（不必精确）；顺便建好去重索引
	if recs, err := readJournal(p); err == nil {
		j.count = len(recs)
		for _, rec := range recs {
			if rec.Kind == jrecReceived && rec.Dedup != "" {
				j.dedup[rec.Dedup] = rec.ID
			}
		}
	}
	if a.journals == nil {
		a.journals = map[string]*interjectionJournal{}
	}
	a.journals[sid] = j
	return j, nil
}

// append 写一条记录并 fsync：返回 nil 之前，这条记录已经在磁盘上。
func (j *interjectionJournal) append(rec journalRec) error {
	rec.V = journalVersion
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := j.file.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.count++
	return nil
}

func (j *interjectionJournal) close() {
	if j != nil && j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
}

// readJournal 读一个日志文件。末尾半条（没有换行）视为没提交的尾部，直接忽略；
// 中间坏行不能静默跳过——返回错误，调用方据此保留现场并报告。
func readJournal(path string) ([]journalRec, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var out []journalRec
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			body := line[:len(line)-1]
			if len(body) > 0 {
				if len(body) > 1<<20 {
					return out, fmt.Errorf("插话日志有一行太长（%d 字节）", len(body))
				}
				var rec journalRec
				if err := json.Unmarshal(body, &rec); err != nil {
					return out, fmt.Errorf("插话日志损坏（第 %d 条）：%w", len(out)+1, err)
				}
				if rec.V > journalVersion {
					return out, fmt.Errorf("插话日志版本 %d 比程序新", rec.V)
				}
				out = append(out, rec)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil // 末尾没有换行的那条被丢掉（没提交的尾部）
			}
			return out, err
		}
	}
}

// interjState 是把日志折叠之后的单条插话状态。
type interjState struct {
	ID         string
	Seq        int64
	Text       string
	Images     []media.Attachment
	Who        string
	Channel    string
	Chat       string
	At         int64
	Agent      string
	Generation int64
	received   bool
	injected   bool
	included   bool
	settled    bool
	withdrawn  bool
	HistoryID  string
	RunID      string
	RequestID  string
	Outcome    string
	Note       string
}

// foldJournal 把日志折叠成每条插话的状态。generation 是当前会话世代：
// 更早世代和 discard 之前的记录都不算数（重置/删除之后不能让旧补充复活）。
func foldJournal(recs []journalRec, generation int64) []*interjState {
	order := []string{}
	states := map[string]*interjState{}
	get := func(id string) *interjState {
		if s, ok := states[id]; ok {
			return s
		}
		s := &interjState{ID: id}
		states[id] = s
		order = append(order, id)
		return s
	}
	for _, rec := range recs {
		if rec.Kind == jrecHead || rec.ID == "" || rec.Kind == jrecDiscard {
			continue
		}
		if rec.Generation != generation {
			continue // 别的世代：重置/删除之前的
		}
		s := get(rec.ID)
		switch rec.Kind {
		case jrecReceived:
			s.received = true
			s.Seq, s.Text, s.Images = rec.Seq, rec.Text, rec.Images
			s.Who, s.Channel, s.Chat = rec.Who, rec.Channel, rec.Chat
			s.At, s.Generation, s.Agent = rec.At, rec.Generation, rec.Agent
		case jrecInjected:
			s.injected = true
			if rec.HistoryID != "" {
				s.HistoryID = rec.HistoryID
			}
			if rec.RunID != "" {
				s.RunID = rec.RunID
			}
		case jrecIncluded:
			s.included = true
			s.RequestID = rec.RequestID
			if rec.RunID != "" {
				s.RunID = rec.RunID
			}
		case jrecOutcome:
			s.settled = true
			s.Outcome, s.Note = rec.Outcome, rec.Note
		case jrecWithdrawn:
			s.withdrawn = true
			s.Note = rec.Note
		}
	}
	out := make([]*interjState, 0, len(order))
	for _, id := range order {
		out = append(out, states[id])
	}
	return out
}

// journalAppend 写一条记录；失败返回错误（调用方决定能不能回执）。
func (a *App) journalAppend(sid string, rec journalRec) error {
	j, err := a.openJournal(sid)
	if err != nil {
		return err
	}
	if rec.Generation == 0 {
		rec.Generation = a.store.Settings(sid).SessionGeneration
	}
	if err := j.append(rec); err != nil {
		return err
	}
	if rec.Kind == jrecReceived && rec.Dedup != "" {
		j.dedup[rec.Dedup] = rec.ID
	}
	if j.count > journalCheckpointAt {
		if err := a.checkpointJournal(sid); err != nil {
			a.log.Warn("重写插话检查点失败", "sid", sid, "err", err)
		}
	}
	return nil
}

// checkpointJournal 把日志重写成“只有还没结局的条目”的小检查点。
// 先写临时文件再 rename：中途崩掉只会留下旧日志，不会丢已受理的补充。
func (a *App) checkpointJournal(sid string) error {
	j := a.journals[sid]
	if j == nil {
		return nil
	}
	gen := a.store.Settings(sid).SessionGeneration
	recs, err := readJournal(j.path)
	if err != nil {
		return err // 损坏的日志保留现场，不覆盖
	}
	states := foldJournal(recs, gen)
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	nj := &interjectionJournal{path: tmp, file: f}
	_ = nj.append(journalRec{Kind: jrecHead, Generation: gen, Note: "checkpoint"})
	for _, s := range states {
		if s.withdrawn || s.settled {
			continue // 有明确终态的不再保留
		}
		_ = nj.append(journalRec{Kind: jrecReceived, ID: s.ID, Seq: s.Seq, Text: s.Text, Images: s.Images,
			Who: s.Who, Channel: s.Channel, Chat: s.Chat, At: s.At, Generation: s.Generation})
		if s.injected {
			_ = nj.append(journalRec{Kind: jrecInjected, ID: s.ID, HistoryID: s.HistoryID, RunID: s.RunID,
				Generation: s.Generation})
		}
		if s.included {
			_ = nj.append(journalRec{Kind: jrecIncluded, ID: s.ID, RequestID: s.RequestID, RunID: s.RunID,
				Generation: s.Generation})
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	j.close()
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	f2, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	j.file, j.count = f2, nj.count
	return nil
}

// newGeneration 把会话世代加一：重置前后的插话不能混在一起。
func (a *App) newGeneration(s *sess) {
	st := a.store.Settings(s.sid)
	st.SessionGeneration++
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		a.log.Warn("保存会话世代失败", "sid", s.sid, "err", err)
	}
	// 留一条 discard 记录，日志重写和排障时能看出这里清过一次
	_ = a.journalAppend(s.sid, journalRec{Kind: jrecDiscard, Note: "reset"})
}

// dropJournal 删除会话时把日志一起清掉（已经落过 discard，不留悬空文件）。
func (a *App) dropJournal(sid string) {
	if j := a.journals[sid]; j != nil {
		j.close()
		delete(a.journals, sid)
	}
	_ = os.Remove(a.journalPath(sid))
}

// journalByDedup 查这条平台消息是不是已经受理过（渠道重发、进程重启都靠它去重）。
func (a *App) journalByDedup(sid, key string) (journalRec, bool) {
	j, err := a.openJournal(sid)
	if err != nil || j == nil || key == "" {
		return journalRec{}, false
	}
	id, ok := j.dedup[key]
	if !ok {
		return journalRec{}, false
	}
	return journalRec{Kind: jrecReceived, ID: id, Dedup: key}, true
}

// withdrawInterjections 把还没处理的补充记成持久终态：重启后不能复活。调用方持有锁。
func (a *App) withdrawInterjections(sid string, items []interjection, reason, note string) {
	for _, it := range items {
		if it.id == "" {
			continue
		}
		if err := a.journalAppend(sid, journalRec{Kind: jrecWithdrawn, ID: it.id, Outcome: reason, Note: note}); err != nil {
			a.log.Warn("记录插话撤回失败", "sid", sid, "id", it.id, "err", err)
		}
	}
}

// settleInterjections 给这一轮处理过的补充记上结局。结局未知（请求发出去但没结果、
// 进程重启）用 interrupted：它表示"需要人看"，不是"已经处理完"。
func (a *App) settleInterjections(sid string, ids []string, outcome, note string) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := a.journalAppend(sid, journalRec{Kind: jrecOutcome, ID: id, Outcome: outcome, Note: note}); err != nil {
			a.log.Warn("记录插话结局失败", "sid", sid, "id", id, "err", err)
		}
	}
}

// interjectionOutcome 把 run 的终态换成插话的请求结局。
func interjectionOutcome(status string) string {
	switch status {
	case proto.StatusOK:
		return outcomeOK
	case proto.StatusBudget:
		return outcomeBudget
	case proto.StatusTimeout:
		return outcomeTimeout
	case proto.StatusCancelled:
		return outcomeCancelled
	case proto.StatusRefused:
		return outcomeOK // 模型明确拒绝：请求本身是有结局的
	default:
		return outcomeError
	}
}

// restoreInterjections 在会话第一次被访问时，把日志里还没处理完的补充恢复进收件箱。
// 调用方持有锁；重复调用是幂等的（only once per sess）。
func (a *App) restoreInterjections(s *sess) {
	if s == nil || s.sid == "" || s.restored || a.store == nil {
		return
	}
	s.restored = true
	recs, err := readJournal(a.journalPath(s.sid))
	if err != nil {
		a.log.Error("插话日志读不出来，保留现场等人工处理", "sid", s.sid, "err", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	gen := a.store.Settings(s.sid).SessionGeneration
	states := foldJournal(recs, gen)
	var pending []*interjState
	for _, st := range states {
		if st.withdrawn || !st.received {
			continue
		}
		// 身份切换：这条补充是发给原来的 agent 的，不能交给别人
		if st.Agent != "" && st.Agent != a.execAgentID(s.sid) {
			_ = a.journalAppend(s.sid, journalRec{Kind: jrecWithdrawn, ID: st.ID, Note: "会话换成别的 agent 了"})
			continue
		}
		if st.injected || st.included {
			// 已经写进历史：不再追加正文。但没有明确结局的必须标成 interrupted：
			// 「读过」不等于「处理完」，请求可能发出去就崩了，要留一个可以继续处理的入口。
			if !st.settled {
				note := "进程重启前已写入历史，但没有发起模型请求"
				if st.included {
					note = "进程重启前已经进入模型请求，但没有拿到结局（请求 " + st.RequestID + "）"
				}
				if err := a.journalAppend(s.sid, journalRec{Kind: jrecOutcome, ID: st.ID,
					Outcome: outcomeInterrupted, Note: note}); err != nil {
					a.log.Warn("补记插话结局失败", "sid", s.sid, "err", err)
				}
			}
			continue
		}
		// 写进历史了但没记 injected（历史成功、日志失败就崩了）：按历史对账补记
		if msg, ok := a.store.FindMessageBySource(s.sid, st.ID, 400); ok {
			_ = a.journalAppend(s.sid, journalRec{Kind: jrecInjected, ID: st.ID, HistoryID: msg.ID})
			continue
		}
		if st.settled || st.included {
			continue
		}
		pending = append(pending, st)
	}
	if len(pending) == 0 {
		return
	}
	var bytes int
	var dropped int
	for _, st := range pending {
		if len(s.pending) >= maxInterjections || bytes+len(st.Text) > maxInterjectionBytes {
			dropped++
			_ = a.journalAppend(s.sid, journalRec{Kind: jrecWithdrawn, ID: st.ID, Note: "恢复时超过待处理上限"})
			continue
		}
		bytes += len(st.Text)
		s.pending = append(s.pending, interjection{
			seq: int(st.Seq),
			msg: channel.Message{ChatID: st.Chat, SenderID: st.Who, Text: st.Text, MsgID: st.ID, Images: st.Images},
			ep:  endpoint{ch: st.Channel, chat: st.Chat},
			who: st.Who,
			text: st.Text,
			at:   time.UnixMilli(st.At),
		})
		if st.Seq > s.interjectSeq {
			s.interjectSeq = st.Seq
		}
	}
	if dropped > 0 {
		a.log.Warn("恢复时丢掉了超额的插话", "sid", s.sid, "dropped", dropped)
	}
	if n := len(s.pending); n > 0 {
		a.log.Info("恢复了上次没处理完的用户补充", "sid", s.sid, "count", n)
		a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "restored",
			Data: map[string]any{"count": n}})
	}
}

func interjectionID(dedup string) string {
	sum := sha1.Sum([]byte(dedup))
	return "ij-" + hex.EncodeToString(sum[:])[:16]
}

func interjectionHistoryID(ids []string) string {
	sum := sha1.Sum([]byte(strings.Join(ids, "|")))
	return "ijb-" + hex.EncodeToString(sum[:])[:16]
}

// dedupKey 是平台消息的去重键。原始消息 ID 在不同窗口之间不一定唯一，
// 所以把会话、渠道、聊天窗口都带上。
func dedupKey(sid, ch, chat, msgID string) string {
	if msgID == "" {
		return ""
	}
	return sid + "|" + ch + "|" + chat + "|" + msgID
}
