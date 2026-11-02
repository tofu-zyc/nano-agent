package app

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/journal"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/session"
)

// 剧场（设计 §14.1）：开了 stage 的内部群是一场戏。场内的事不进成员的记忆，场外的事进不来，
// 每一句话、每一轮、每次模型调用、导演的操作都按顺序记进 data/runtime/stage/<群 ID>.jsonl。
//
// 隔离和记录都按 sceneOf(sid) 取到的规则判断：没有 stage 的会话走到这里什么都不做。
// 群记录只有 roomLogWrite 一个写入口：先写场景记录，再写群记录，保证 Line 和群记录的位置一一对应。

const (
	stageRecMax   = 64 << 10
	stageFileMax  = 64 << 20
	stageInnerMax = 200  // 心里话最多多少字
	stageThinkMax = 8000 // 推理最多多少字
	stageOutMax   = 20000
)

// StageRec 是场景记录里的一条。
type StageRec struct {
	Seq  uint64 `json:"seq,omitempty"` // 读出来时才有：文件里的序号
	Kind string `json:"kind"`          // line | cursors | run | model | prefix | op | fork
	TS   int64  `json:"ts"`            // 毫秒

	// line：群记录里的一句；cursors：这一句送达后每个成员会话的消息数
	Line    int            `json:"line,omitempty"` // 群记录里的第几句（从 1 起，和群记录逐条对应）
	From    string         `json:"from,omitempty"` // agent ID；人、旁白、系统为空
	Name    string         `json:"name,omitempty"`
	Say     string         `json:"say,omitempty"` // say | narrate | cue | system
	Channel string         `json:"channel,omitempty"`
	To      string         `json:"to,omitempty"`
	Text    string         `json:"text,omitempty"`
	Cursors map[string]int `json:"cursors,omitempty"`

	// run：成员的一轮结束；model：一次模型调用
	Run       string     `json:"run,omitempty"`
	Member    string     `json:"member,omitempty"`
	Status    string     `json:"status,omitempty"`
	Inner     string     `json:"inner,omitempty"`     // 心里话（最多 200 字）
	Reasoning string     `json:"reasoning,omitempty"` // 模型的推理，最多 8000 字
	Usage     *llm.Usage `json:"usage,omitempty"`
	Said      int        `json:"said,omitempty"` // 这一轮说出口的是第几句；没说为 0
	Model     *ModelRec  `json:"model,omitempty"`

	// prefix：某个前缀第一次出现时记一次全文；op：导演的操作；fork：从哪一场的第几句分出
	Hash   string          `json:"hash,omitempty"`
	Len    int             `json:"len,omitempty"` // 读出来时 prefix 只给哈希和长度
	Body   string          `json:"body,omitempty"`
	Op     string          `json:"op,omitempty"` // pause | resume | mute | rules | reset
	Detail json.RawMessage `json:"detail,omitempty"`
}

// ModelRec 是一次模型调用。
type ModelRec struct {
	Endpoint string         `json:"endpoint"` // 模型接口的名字
	Model    string         `json:"model"`
	Params   map[string]any `json:"params"` // 实际发出的参数：thinking、max_tokens
	Prefix   string         `json:"prefix"` // 系统提示加工具定义的 SHA-256，全文在 prefix 记录里
	Input    string         `json:"input"`  // 这次请求全部消息的 SHA-256
	Msgs     [2]int         `json:"msgs"`   // 用到的成员会话消息：[Base, 长度)
	Tail     []string       `json:"tail"`   // 只放在这次请求里的内容（轮到你、频道提示等）
	Out      string         `json:"out"`    // 回复正文
	Calls    []llm.ToolCall `json:"calls,omitempty"`
	Reason   string         `json:"reason"` // 结束原因；调用失败是 error
	Err      string         `json:"err,omitempty"`
}

type stageLog struct {
	j        *journal.Journal
	path     string
	seq      uint64
	lines    int             // 群记录里最后一句的序号
	prefixes map[string]bool // 已经记过全文的前缀
	lastSaid map[string]int  // 成员 → 它这一轮说出口的那一句
	openErr  error
	h        *health.Handle
}

func (a *App) stagePath(roomID string) string {
	return filepath.Join(a.observeDir, "runtime", "stage", roomID+".jsonl")
}

// stageFor 是会话所在的场景的场景记录；这个场景没开 stage 就是 nil。调用方持有锁。
func (a *App) stageFor(sid string) (*stageLog, rooms.Room) {
	sc, ok := a.sceneOf(sid)
	if !ok || !sc.Rules.Stage || sc.Room == nil {
		return nil, rooms.Room{}
	}
	room := *sc.Room
	if a.stages == nil {
		a.stages = map[string]*stageLog{}
	}
	if st := a.stages[room.ID]; st != nil {
		return st, room
	}
	st := &stageLog{prefixes: map[string]bool{}, lastSaid: map[string]int{}, path: a.stagePath(room.ID)}
	if a.observeDir == "" {
		st.openErr = errors.New("没有数据目录")
	} else {
		j, err := journal.Open(st.path, 1, journal.Limits{Record: stageRecMax, File: stageFileMax}, func(seq uint64, data json.RawMessage) error {
			var rec StageRec
			if err := json.Unmarshal(data, &rec); err != nil {
				return err
			}
			st.seq = seq
			switch rec.Kind {
			case "line":
				st.lines = max(st.lines, rec.Line)
			case "prefix":
				st.prefixes[rec.Hash] = true
			}
			return nil
		})
		st.j, st.openErr = j, err
	}
	a.stages[room.ID] = st
	return st, room
}

// stagePut 写一条记录。失败时健康状态记为 degraded。调用方持有锁。
func (a *App) stagePut(st *stageLog, room rooms.Room, rec StageRec) error {
	rec.TS = a.now().UnixMilli()
	err := st.openErr
	if err == nil {
		err = st.j.Append(rec)
	}
	if err != nil {
		if st.h == nil {
			st.h, _ = a.health.Claim("stage")
		}
		if st.h != nil {
			st.h.Set(health.Degraded, "场景记录写不进去（群 "+room.Name+"）："+err.Error())
		}
		return err
	}
	st.seq++
	if st.h != nil {
		st.h.Set(health.OK, "")
	}
	a.publish(events.Event{T: "stage", SID: roomLogSID(room.ID), N: int(st.seq), Data: map[string]any{"room": room.ID, "seq": st.seq}})
	return nil
}

// stageBroken 是场景记录写不进去：这一场暂停，原因只写群记录（再写场景记录也会失败）。调用方持有锁。
func (a *App) stageBroken(room rooms.Room, err error) {
	a.log.Warn("场景记录写入失败，暂停这一场", "room", room.ID, "err", err)
	a.pauseRoomLogOnly(room, "场景记录写不进去（"+err.Error()+"），先暂停")
}

// roomLogWrite 是群记录唯一的写入口（人和旁白的话、agent 的话、cue、系统提示都从这里写）：
// 开了 stage 的先写场景记录再写群记录；写不进去就不写群记录，返回 false，调用方不发这一句。
// 返回这一句在群记录里的序号（没开 stage 也照样计数）。调用方持有锁。
func (a *App) roomLogWrite(room rooms.Room, msg session.Msg, rec StageRec) (int, bool) {
	log := roomLogSID(room.ID)
	st, _ := a.stageFor(log)
	line := 0
	if st != nil {
		rec.Kind, rec.Line = "line", st.lines+1
		if err := a.stagePut(st, room, rec); err != nil {
			a.stageBroken(room, err)
			return 0, false
		}
		st.lines++
		line = st.lines
		if rec.From != "" && rec.Say == lineSay {
			st.lastSaid[rec.From] = line
		}
	}
	if err := a.store.Append(log, msg); err != nil {
		a.log.Warn("保存房间记录失败", "room", room.ID, "err", err)
	}
	return line, true
}

// stageCursors 记下这一句送达之后每个成员会话的消息数（3b 从这一句分出平行世界要靠它）。
// 必须在这一句已经写进所有听得到的成员会话之后调用。调用方持有锁。
func (a *App) stageCursors(room rooms.Room, line int) {
	st, _ := a.stageFor(roomLogSID(room.ID))
	if st == nil || line == 0 {
		return
	}
	cur := map[string]int{}
	for _, m := range room.Members {
		cur[m] = a.histEnd(a.roomMemberSID(m, room.ID))
	}
	if err := a.stagePut(st, room, StageRec{Kind: "cursors", Line: line, Cursors: cur}); err != nil {
		a.stageBroken(room, err)
	}
}

// stageOp 记导演的操作。调用方持有锁。
func (a *App) stageOp(room rooms.Room, op string, detail any) {
	st, _ := a.stageFor(roomLogSID(room.ID))
	if st == nil {
		return
	}
	rec := StageRec{Kind: "op", Op: op}
	if detail != nil {
		rec.Detail, _ = json.Marshal(detail)
	}
	if err := a.stagePut(st, room, rec); err != nil {
		a.stageBroken(room, err)
	}
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func cutText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// sceneModel 在一次模型请求发出之前登记它（前缀第一次出现时记全文），返回结果回来后调用的函数。
// 这个会话所在的场景没有场景记录就返回 nil。调用方持有锁。
func (a *App) sceneModel(s *sess, system string, req llm.Request, tail []string, base, end int) func(res *llm.Result, err error) {
	st, room := a.stageFor(s.sid)
	if st == nil {
		return nil
	}
	toolsJSON, _ := json.MarshalIndent(req.Tools, "", "  ")
	body := system + "\n\n== tools ==\n" + string(toolsJSON)
	prefix := hashHex([]byte(body))
	if !st.prefixes[prefix] {
		if err := a.stagePut(st, room, StageRec{Kind: "prefix", Hash: prefix, Body: body}); err != nil {
			a.stageBroken(room, err)
		} else {
			st.prefixes[prefix] = true
		}
	}
	input, _ := json.Marshal(req.Messages)
	m, e, _ := a.modelOf(s.sid)
	rec := ModelRec{Endpoint: e.Name, Model: m.Name, Params: map[string]any{"thinking": req.Thinking, "max_tokens": req.MaxTokens},
		Prefix: prefix, Input: hashHex(input), Msgs: [2]int{base, end}, Tail: tail}
	member := roomSender(s.sid)
	runID := s.run.runID
	return func(res *llm.Result, err error) {
		out := StageRec{Kind: "model", Run: runID, Member: member}
		if err != nil {
			rec.Reason, rec.Err = "error", err.Error()
		} else {
			rec.Reason, rec.Out, rec.Calls = res.Reason, cutText(res.Content, stageOutMax), res.Calls
			out.Reasoning, out.Usage = cutText(res.Reasoning, stageThinkMax), &res.Usage
		}
		out.Model = &rec
		if err := a.stagePut(st, room, out); err != nil {
			a.stageBroken(room, err)
		}
	}
}

// sceneRunEnd 在成员的一轮结束时记一条 run（谁跑的、想了什么、说没说、花了多少）。调用方持有锁。
func (a *App) sceneRunEnd(s *sess, r *run, status string) {
	st, room := a.stageFor(s.sid)
	if st == nil {
		return
	}
	member := roomSender(s.sid)
	u := r.usage
	rec := StageRec{Kind: "run", Run: r.runID, Member: member, Status: status, Inner: r.thought, Usage: &u, Said: st.lastSaid[member]}
	delete(st.lastSaid, member)
	if err := a.stagePut(st, room, rec); err != nil {
		a.stageBroken(room, err)
	}
}

// ---- 心里话 ----

// 加在场景说明后面的固定说明，同一场内不变。
const innerGuide = "[心里话] 每次回复先用 <心里>一句心里话</心里> 写下你此刻真正的想法（不超过 200 个字，别人听不到），再接着写你说出口的话。不想说话，就在心里话后面只回 [不回复]。"

var innerRe = regexp.MustCompile(`(?s)^\s*<心里>(.*?)</心里>\s*(.*)$`)

// splitInner 把回复拆成心里话和说出口的话；没按格式写的整段算说出口。
func splitInner(text string) (inner, said string) {
	m := innerRe.FindStringSubmatch(text)
	if m == nil {
		return "", text
	}
	return cutText(strings.TrimSpace(m[1]), stageInnerMax), strings.TrimSpace(m[2])
}

// sceneSplit 按场景的规则把成员这一轮的回复拆开：返回说出口的部分，心里话记在这一轮上。
// 没开心里话的场景原样返回。调用方持有锁。
func (a *App) sceneSplit(s *sess, r *run, body string) string {
	sc, ok := a.sceneOf(s.sid)
	if !ok || !sc.Rules.Stage || !sc.Rules.Inner {
		return body
	}
	r.thought, body = splitInner(body)
	return body
}

// sceneIsolated 这个会话所在的场景把场内的事关在里面：不记经历、不进整理记忆、没有记忆说明。
func (a *App) sceneIsolated(sid string) bool {
	sc, ok := a.sceneOf(sid)
	return ok && sc.Rules.Stage
}

// ---- 接口：时间线 ----

// stageFile 是群的场景记录文件；不是一场戏返回错误。
func (a *App) stageFile(roomID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return "", errors.New("房间列表还没准备好")
	}
	r, ok := a.roomStore.Get(roomID)
	if !ok {
		return "", errors.New("没有这个群")
	}
	if !r.Stage {
		return "", errors.New("这个群不是一场戏")
	}
	return a.stagePath(roomID), nil
}

// readStage 按序读场景记录，f 返回 false 就停。只读完整的行，在锁外读（记录只追加）。
func readStage(path string, f func(StageRec) bool) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	rd := bufio.NewReaderSize(file, stageRecMax+8192)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil || !bytes.HasSuffix(line, []byte("\n")) {
			return nil // 到头了，或者最后一条还没写完
		}
		var env struct {
			Seq  uint64          `json:"seq"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return fmt.Errorf("记录损坏：%w", err)
		}
		var rec StageRec
		if err := json.Unmarshal(env.Data, &rec); err != nil {
			return fmt.Errorf("记录损坏：%w", err)
		}
		rec.Seq = env.Seq
		if !f(rec) {
			return nil
		}
	}
}

// StageTimeline 是序号大于 after 的记录，最多 n 条（默认 200，最多 1000）；prefix 只给哈希和长度。
// last 是文件里最新的序号。
func (a *App) StageTimeline(roomID string, after uint64, n int) (items []StageRec, last uint64, err error) {
	path, err := a.stageFile(roomID)
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 {
		n = 200
	}
	n = min(n, 1000)
	items = []StageRec{}
	err = readStage(path, func(rec StageRec) bool {
		last = rec.Seq
		if rec.Seq <= after {
			return true
		}
		if rec.Kind == "prefix" {
			rec.Len, rec.Body = len(rec.Body), ""
		}
		if len(items) < n {
			items = append(items, rec)
		}
		return true
	})
	return items, last, err
}

// StagePrefix 是某个前缀的全文。
func (a *App) StagePrefix(roomID, hash string) (string, error) {
	path, err := a.stageFile(roomID)
	if err != nil {
		return "", err
	}
	body, found := "", false
	err = readStage(path, func(rec StageRec) bool {
		if rec.Kind == "prefix" && rec.Hash == hash {
			body, found = rec.Body, true
			return false
		}
		return true
	})
	if err == nil && !found {
		err = errors.New("没有这个前缀")
	}
	return body, err
}
