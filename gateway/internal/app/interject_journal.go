package app

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"nano-agent/gateway/internal/session"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
)

// ---- 插话的持久日志 ----
//
// data/interjections/<编码后的 sid>.jsonl，只追加、单写者（App.mu 下）。
// 每条插话有稳定 interjection_id（贯穿受理、恢复、注入、请求和终态），
// 另有一条去重键（sid|渠道|聊天|平台消息 ID）。平台消息 ID 与内部 ID 分开保存。
//
// 状态不是"收到→注入→读过"那么简单：读过（included_in_request）不代表请求成功，
// 更不代表工具副作用发生过。所以请求结局（outcome）和撤回终态（withdrawn）各自独立记录：
//
//	received            已可靠受理，还没进正式历史
//	injected            已经写进正式历史（带稳定 history_message_id）
//	included_in_request 已经被某个模型请求的快照包含（关联 request_id 与 revision）
//	outcome             结局：ok / error / budget / timeout / cancelled 是"结束了"；
//	                    interrupted 是"没有结局、还需要人处理"。
//	withdrawn           用户撤回、停止、重置、删除导致这条补充失效：重启后不能复活
//
// 检查点除了未完成条目，还要保留两个"跨压缩必须活着"的东西：
//   - 接收序号高水位（head.seq）：没有它，重启后无平台 ID 的新消息会分到旧 ID；
//   - 有界的去重窗口（dedup 记录）：已完成/撤回的消息在一段时间内仍要能挡住渠道重发。

const (
	journalVersion      = 1
	journalCheckpointAt = 512 // 记录数超过这个就重写成检查点
	journalDedupKeep    = 256 // 去重窗口：最近这么多个去重键跨检查点保留

	jrecHead      = "head"
	jrecDedup     = "dedup"
	jrecReceived  = "received"
	jrecInjected  = "injected"
	jrecIncluded  = "included"
	jrecOutcome   = "outcome"
	jrecWithdrawn = "withdrawn"
	jrecDiscard   = "discard"

	outcomeOK          = "ok"
	outcomeError       = "error"
	outcomeBudget      = "budget"
	outcomeTimeout     = "timeout"
	outcomeCancelled   = "cancelled"
	outcomeInterrupted = "interrupted" // 请求没有拿到结局：不是"处理完"
)

// definitiveOutcome 报告这个结局是不是"这件事算结束了"。
// interrupted 不算：它表示请求发出去就没有下文，必须留着让人能继续。
func definitiveOutcome(outcome string) bool {
	switch outcome {
	case outcomeOK, outcomeError, outcomeBudget, outcomeTimeout, outcomeCancelled:
		return true
	}
	return false
}

// journalRec 是日志里的一行。字段都有界；正文上限由收件箱（64 KiB）保证。
type journalRec struct {
	V          int                `json:"v"`
	Kind       string             `json:"kind"`
	Seq        int64              `json:"seq,omitempty"`    // 接收序号（head 里是高水位）
	ID         string             `json:"id,omitempty"`     // 稳定 interjection_id
	MsgID      string             `json:"msg_id,omitempty"` // 平台原始消息 ID（和内部 ID 分开）
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
	count int   // 已经写进去多少条记录（用来决定什么时候重写检查点）
	good  int64 // 最后一个完整提交的记录之后的文件偏移
	// highSeq 是接收序号高水位：检查点把它写进 head，重启后接着往上分配
	highSeq int64
	// dedup 是去重索引（去重键 → interjection_id），dedupOrder 记插入顺序用于有界裁剪
	dedup      map[string]string
	dedupOrder []string
	// broken 非空表示这份日志有中间损坏 / 版本不认识 / 写坏过又修不好：
	// 保留现场，不能再往里追加并回执"已收到"。
	broken error
}

func (a *App) journalDir() string {
	return filepath.Join(a.store.Dir(), "interjections")
}

func (a *App) journalPath(sid string) string {
	return session.AuxiliaryFile(a.journalDir(), sid, ".jsonl")
}

// openJournal 打开（必要时创建）会话日志。调用方持有锁。
//
// 打开时先读一遍：末尾没写完的半条会被截掉（否则下一次追加会接到半条后面，
// 把整条新记录拼坏）；中间损坏则把日志标成 broken，后续追加一律失败，
// 由调用方明确告诉发送者"没有受理"，而不是假装受理成功。
func (a *App) openJournal(sid string) (*interjectionJournal, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	p, _, pathErr := a.store.AuxiliaryPath("interjections", a.journalDir(), ".jsonl", sid, true)
	if pathErr != nil {
		return nil, pathErr
	}
	if j := a.journals[sid]; j != nil {
		return j, nil
	}
	if err := os.MkdirAll(a.journalDir(), 0o700); err != nil {
		return nil, err
	}
	j := &interjectionJournal{path: p, dedup: map[string]string{}}
	recs, good, err := readJournal(p)
	if err != nil {
		j.broken = err
		a.log.Error("插话日志损坏，保留现场并停止受理", "sid", sid, "err", err)
	} else {
		j.count, j.good = len(recs), good
		for _, rec := range recs {
			if rec.Kind == jrecHead && rec.Seq > j.highSeq {
				j.highSeq = rec.Seq
			}
			if rec.Seq > j.highSeq {
				j.highSeq = rec.Seq
			}
			if rec.Dedup != "" && rec.ID != "" && (rec.Kind == jrecReceived || rec.Kind == jrecDedup) {
				if _, seen := j.dedup[rec.Dedup]; !seen {
					j.dedupOrder = append(j.dedupOrder, rec.Dedup)
				}
				j.dedup[rec.Dedup] = rec.ID
			}
		}
		j.trimDedup()
		// 截掉没提交的尾部
		if st, serr := os.Stat(p); serr == nil && st.Size() > good {
			if terr := os.Truncate(p, good); terr != nil {
				j.broken = fmt.Errorf("截去插话日志尾部失败：%w", terr)
				a.log.Error("截不去插话日志尾部，停止受理", "sid", sid, "err", terr)
			} else {
				a.log.Warn("截去了插话日志未提交的尾部", "sid", sid, "bytes", st.Size()-good)
			}
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j.file = f
	if a.journals == nil {
		a.journals = map[string]*interjectionJournal{}
	}
	a.journals[sid] = j
	return j, nil
}

// trimDedup 只保留最近 journalDedupKeep 个去重键。
func (j *interjectionJournal) trimDedup() {
	for len(j.dedupOrder) > journalDedupKeep {
		old := j.dedupOrder[0]
		j.dedupOrder = j.dedupOrder[1:]
		delete(j.dedup, old)
	}
}

// append 写一条记录并 fsync：返回 nil 之前，这条记录已经在磁盘上。
func (j *interjectionJournal) append(rec journalRec) error {
	if j.broken != nil {
		return j.broken
	}
	rec.V = journalVersion
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	buf := append(line, '\n')
	if _, err := j.file.Write(buf); err != nil {
		return j.afterWriteError(err)
	}
	if err := j.file.Sync(); err != nil {
		return j.afterWriteError(err)
	}
	j.count++
	j.good += int64(len(buf))
	return nil
}

// afterWriteError 处理"写了一半或不知道写没写进去"的情况。
//
// 关键：这条记录没有提交，但文件尾部可能已经被写坏了。先把文件截回上一个
// 完整提交的偏移并确认成功，之后还能继续受理；截不回去就冻结这份日志，
// 让调用方向用户回"没有受理"，不能等下一次重启才修。
func (j *interjectionJournal) afterWriteError(cause error) error {
	j.close()
	if err := os.Truncate(j.path, j.good); err != nil {
		j.broken = fmt.Errorf("插话日志尾部修不回去（%v），已停止受理：%w", cause, err)
		return j.broken
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		j.broken = fmt.Errorf("插话日志重新打开失败（%v）：%w", cause, err)
		return j.broken
	}
	j.file = f
	// 尾部已经回到上一次完整提交的位置：这次仍然算失败（记录没落地），但日志可用
	return cause
}

func (j *interjectionJournal) close() {
	if j != nil && j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
}

// readJournal 读一个日志文件，返回记录和"最后一个完整提交的字节偏移"。
// 末尾没换行的那半条不算提交，偏移停在上一条之后；中间坏行、版本过新、
// 读不出来都返回错误（调用方据此保留现场）。
func readJournal(path string) ([]journalRec, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var out []journalRec
	var good int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			body := line[:len(line)-1]
			if len(body) > 1<<20 {
				return out, good, fmt.Errorf("插话日志有一行太长（%d 字节）", len(body))
			}
			if len(body) > 0 {
				var rec journalRec
				if jerr := json.Unmarshal(body, &rec); jerr != nil {
					return out, good, fmt.Errorf("插话日志损坏（第 %d 条）：%w", len(out)+1, jerr)
				}
				if rec.V > journalVersion {
					return out, good, fmt.Errorf("插话日志版本 %d 比程序新", rec.V)
				}
				out = append(out, rec)
			}
			good += int64(len(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, good, nil // 尾部的半条：偏移停在它前面
			}
			return out, good, err
		}
	}
}

// interjState 是把日志折叠之后的单条插话状态。
type interjState struct {
	ID         string
	Seq        int64
	MsgID      string
	Dedup      string
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
	settled    bool // 只有 definitiveOutcome 的结局才算
	withdrawn  bool
	HistoryID  string
	RunID      string
	RequestID  string
	Revision   int
	Outcome    string
	Note       string
}

// foldJournal 把日志折叠成每条插话的状态。generation 是当前会话世代：
// 别的世代的记录不算数（重置/删除之后不能让旧补充复活）。
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
		if rec.Kind == jrecHead || rec.Kind == jrecDedup || rec.ID == "" || rec.Kind == jrecDiscard {
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
			s.MsgID, s.Dedup = rec.MsgID, rec.Dedup
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
			s.RequestID, s.Revision = rec.RequestID, rec.Revision
			if rec.RunID != "" {
				s.RunID = rec.RunID
			}
		case jrecOutcome:
			s.Outcome, s.Note = rec.Outcome, rec.Note
			s.settled = definitiveOutcome(rec.Outcome) // interrupted 不算结束
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
	if rec.Seq > j.highSeq {
		j.highSeq = rec.Seq
	}
	if rec.Dedup != "" && rec.ID != "" && rec.Kind == jrecReceived {
		if _, seen := j.dedup[rec.Dedup]; !seen {
			j.dedupOrder = append(j.dedupOrder, rec.Dedup)
		}
		j.dedup[rec.Dedup] = rec.ID
		j.trimDedup()
	}
	if j.count > journalCheckpointAt {
		if err := a.checkpointJournal(sid); err != nil {
			a.log.Warn("重写插话检查点失败（旧日志继续可用）", "sid", sid, "err", err)
		}
	}
	return nil
}

// checkpointJournal 把日志重写成小检查点：保留所有还没有明确结束的条目，
// 连身份、去重键、稳定 ID、世代、请求与 revision 一起原样带过去；
// 另外单独保存接收序号高水位和一小段去重窗口，避免"压缩之后重发又被受理"。
// 先写临时文件、逐条检查错误、再 rename：任何一步失败都保留旧日志。
func (a *App) checkpointJournal(sid string) error {
	j := a.journals[sid]
	if j == nil || j.broken != nil {
		return nil
	}
	gen := a.store.Settings(sid).SessionGeneration
	recs, _, err := readJournal(j.path)
	if err != nil {
		return err // 损坏的日志保留现场，不覆盖
	}
	states := foldJournal(recs, gen)
	high := j.highSeq
	for _, s := range states {
		if s.Seq > high {
			high = s.Seq
		}
	}
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	nj := &interjectionJournal{path: tmp, file: f, highSeq: high, dedup: map[string]string{}}
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := nj.append(journalRec{Kind: jrecHead, Seq: high, Generation: gen, Note: "checkpoint"}); err != nil {
		return fail(err)
	}
	// 去重窗口：最近 journalDedupKeep 个键（不管条目是否已经结束）
	keep := j.dedupOrder
	if len(keep) > journalDedupKeep {
		keep = keep[len(keep)-journalDedupKeep:]
	}
	for _, key := range keep {
		id := j.dedup[key]
		if id == "" {
			continue
		}
		if err := nj.append(journalRec{Kind: jrecDedup, ID: id, Dedup: key, Generation: gen}); err != nil {
			return fail(err)
		}
		nj.dedup[key] = id
		nj.dedupOrder = append(nj.dedupOrder, key)
	}
	for _, s := range states {
		if s.withdrawn || s.settled {
			continue // 有明确终态的才回收
		}
		if err := nj.append(journalRec{Kind: jrecReceived, ID: s.ID, Seq: s.Seq, MsgID: s.MsgID, Dedup: s.Dedup,
			Text: s.Text, Images: s.Images, Who: s.Who, Agent: s.Agent, Channel: s.Channel, Chat: s.Chat,
			At: s.At, Generation: s.Generation}); err != nil {
			return fail(err)
		}
		if s.Dedup != "" {
			if _, seen := nj.dedup[s.Dedup]; !seen {
				nj.dedup[s.Dedup] = s.ID
				nj.dedupOrder = append(nj.dedupOrder, s.Dedup)
			}
		}
		if s.injected {
			if err := nj.append(journalRec{Kind: jrecInjected, ID: s.ID, HistoryID: s.HistoryID, RunID: s.RunID,
				Generation: s.Generation}); err != nil {
				return fail(err)
			}
		}
		if s.included {
			if err := nj.append(journalRec{Kind: jrecIncluded, ID: s.ID, RequestID: s.RequestID, Revision: s.Revision,
				RunID: s.RunID, Generation: s.Generation}); err != nil {
				return fail(err)
			}
		}
		if s.Outcome != "" && !s.settled {
			// interrupted：留着，让它跨检查点和重启一直可见
			if err := nj.append(journalRec{Kind: jrecOutcome, ID: s.ID, Outcome: s.Outcome, Note: s.Note,
				Generation: s.Generation}); err != nil {
				return fail(err)
			}
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		os.Remove(tmp)
		return err
	}
	j.close()
	f2, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		j.broken = err // 替换后打不开：停止受理，别把状态当成正常
		return err
	}
	// 内存索引必须和新文件一致：去重窗口与高水位都跟着走
	j.file, j.count, j.dedup, j.dedupOrder = f2, nj.count, nj.dedup, nj.dedupOrder
	j.highSeq = high
	if st, err := os.Stat(j.path); err == nil {
		j.good = st.Size()
	}
	return nil
}

// newGeneration 把会话世代加一：重置前后的插话不能混在一起。
func (a *App) newGeneration(s *sess) error {
	st := a.store.Settings(s.sid)
	st.SessionGeneration++
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		a.log.Warn("保存会话世代失败", "sid", s.sid, "err", err)
		return fmt.Errorf("保存会话世代失败：%w", err)
	}
	// 留一条 discard 记录，日志重写和排障时能看出这里清过一次
	return a.journalAppend(s.sid, journalRec{Kind: jrecDiscard, Note: "reset"})
}

// dropJournal 删除会话时把日志一起清掉。
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
	if err != nil || j == nil || key == "" || j.broken != nil {
		return journalRec{}, false
	}
	id, ok := j.dedup[key]
	if !ok {
		return journalRec{}, false
	}
	return journalRec{Kind: jrecReceived, ID: id, Dedup: key}, true
}

// withdrawInterjections 把还没处理的补充记成持久终态：重启后不能复活。
// 返回错误时调用方不能假装撤回成功。调用方持有锁。
func (a *App) withdrawInterjections(sid string, items []interjection, reason, note string) error {
	var firstErr error
	for _, it := range items {
		if it.id == "" {
			continue
		}
		rec := journalRec{Kind: jrecWithdrawn, ID: it.id, Outcome: reason, Note: note}
		if err := a.journalAppend(sid, rec); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// discardPending 把"待处理"和"中断未完成"的补充统一作废：停止、重置共用。
// 写失败时内存状态保持不动——不能一边说已停止、一边假装已经清干净。
func (a *App) discardPending(s *sess, inbox []interjection, reason, note string) error {
	var firstErr error
	// 收件箱里这几条也要一起作废：只清内存就会在重启后复活
	if len(inbox) > 0 {
		firstErr = a.withdrawInterjections(s.sid, inbox, reason, note)
	}
	if err := a.withdrawInterjections(s.sid, s.pending, reason, note); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, it := range s.interrupted {
		if it.id == "" {
			continue
		}
		if err := a.journalAppend(s.sid, journalRec{Kind: jrecOutcome, ID: it.id,
			Outcome: outcomeCancelled, Note: note}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		s.pending, s.pendingBytes = nil, 0
		s.interrupted = nil
	}
	return firstErr
}

// settleInterjections 给这一轮处理过的补充记上结局。
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

// restoreInterjections 在会话第一次被访问时，把日志里还没处理完的补充恢复出来。
// 调用方持有锁；重复调用是幂等的。
//
// 必须一次跑完所有状态转换：补记 injected 的同一次里也要判成 interrupted，
// 不能要求用户再重启一次才看得到"可以继续"。
func (a *App) restoreInterjections(s *sess) {
	if s == nil || s.sid == "" || s.restored || a.store == nil {
		return
	}
	s.restored = true
	j, jerr := a.openJournal(s.sid)
	if jerr != nil {
		a.log.Error("插话日志打不开，保留现场等人工处理", "sid", s.sid, "err", jerr)
		return
	}
	recs, _, err := readJournal(a.journalPath(s.sid))
	if err != nil {
		a.log.Error("插话日志读不出来，保留现场等人工处理", "sid", s.sid, "err", err)
		return
	}
	// 接收序号高水位：重启后接着往上分配，无平台 ID 的消息不会撞旧 ID
	if j.highSeq > s.interjectSeq {
		s.interjectSeq = j.highSeq
	}
	if len(recs) == 0 {
		return
	}
	gen := a.store.Settings(s.sid).SessionGeneration
	states := foldJournal(recs, gen)
	var pending []*interjState
	var interrupted []*interjState
	markInterrupted := func(st *interjState, note string) {
		if err := a.journalAppend(s.sid, journalRec{Kind: jrecOutcome, ID: st.ID,
			Outcome: outcomeInterrupted, Note: note}); err != nil {
			a.log.Warn("补记插话结局失败", "sid", s.sid, "err", err)
		}
		st.Outcome = outcomeInterrupted
		interrupted = append(interrupted, st)
	}
	for _, st := range states {
		if st.Seq > s.interjectSeq {
			s.interjectSeq = st.Seq
		}
		if st.withdrawn || !st.received {
			continue
		}
		// 身份切换：这条补充是发给原来的 agent 的，不能交给别人
		if st.Agent != "" && st.Agent != a.execAgentID(s.sid) {
			_ = a.journalAppend(s.sid, journalRec{Kind: jrecWithdrawn, ID: st.ID, Note: "会话换成别的 agent 了"})
			continue
		}
		if st.injected || st.included {
			if !st.settled {
				if st.Outcome == outcomeInterrupted {
					interrupted = append(interrupted, st) // 已经是 interrupted：保持可见
					continue
				}
				note := "进程重启前已写入历史，但没有发起模型请求"
				if st.included {
					note = "进程重启前已经进入模型请求，但没有拿到结局（请求 " + st.RequestID + "）"
				}
				markInterrupted(st, note)
			}
			continue
		}
		// 写进历史了但没记 injected（历史成功、日志失败就崩了）：按历史对账补记。
		// 历史可能很长，不能用"最近 N 条"当边界：这里查全部（历史本来就在内存里）。
		if msg, ok := a.store.FindMessageBySource(s.sid, st.ID, 0); ok {
			if err := a.journalAppend(s.sid, journalRec{Kind: jrecInjected, ID: st.ID, HistoryID: msg.ID}); err != nil {
				a.log.Warn("补记 injected 失败", "sid", s.sid, "id", st.ID, "err", err)
				continue
			}
			// 同一次恢复里就要判成 interrupted：请求有没有完成是未知的
			markInterrupted(st, "进程重启前已写入历史，但没有发起模型请求")
			continue
		}
		if st.settled || st.included {
			continue
		}
		pending = append(pending, st)
	}
	var dropped int
	for _, st := range pending {
		if len(s.pending) >= maxInterjections || s.pendingBytes+len(st.Text) > maxInterjectionBytes {
			dropped++
			_ = a.journalAppend(s.sid, journalRec{Kind: jrecWithdrawn, ID: st.ID, Note: "恢复时超过待处理上限"})
			continue
		}
		s.pendingBytes += len(st.Text)
		s.pending = append(s.pending, interjection{
			id:   st.ID, // 稳定 ID 必须一路带着：它是对账的唯一依据
			seq:  int(st.Seq),
			msg:  channel.Message{ChatID: st.Chat, SenderID: st.Who, Text: st.Text, MsgID: st.MsgID, Images: st.Images},
			ep:   endpoint{ch: st.Channel, chat: st.Chat},
			who:  st.Who,
			text: st.Text,
			at:   time.UnixMilli(st.At),
		})
	}
	for _, st := range interrupted {
		s.interrupted = append(s.interrupted, interjection{
			id:   st.ID,
			seq:  int(st.Seq),
			msg:  channel.Message{ChatID: st.Chat, SenderID: st.Who, Text: st.Text, MsgID: st.MsgID},
			ep:   endpoint{ch: st.Channel, chat: st.Chat},
			who:  st.Who,
			text: st.Text,
			at:   time.UnixMilli(st.At),
		})
	}
	if dropped > 0 {
		a.log.Warn("恢复时丢掉了超额的插话", "sid", s.sid, "dropped", dropped)
	}
	if n := len(s.pending); n > 0 {
		a.log.Info("恢复了上次没处理完的用户补充", "sid", s.sid, "count", n)
		a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "restored",
			Data: map[string]any{"count": n}})
	}
	if n := len(s.interrupted); n > 0 {
		a.log.Info("有上次中断、还没有结局的补充", "sid", s.sid, "count", n)
		a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "interrupted",
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
