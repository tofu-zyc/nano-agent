// Package session 保存每个会话的对话历史：内存一份，磁盘上每个会话一个 JSONL 文件（只追加）。
//
// 发给模型的历史只追加不改动，这样每次请求都是上一次请求的延续，模型那边的前缀缓存才能命中。
// 历史太长时由 Compact 一次性丢掉开头的一大段（记下丢到了哪里），而不是每次都丢一点。
//
// Store 不加锁，由调用方（app）串行访问。
package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
)

// Msg 是历史里的一条消息。Reasoning 和 Usage 只保存备查，默认不回传给模型。
type Msg struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	Images     []media.Attachment `json:"images,omitempty"` // 用户消息附带的图片、工具（Read）读到的图片，文件在 data/uploads
	Sent       []media.Attachment `json:"sent,omitempty"`   // SendFile 发到聊天里的文件：只给人看，不再发给模型
	ToolCalls  []llm.ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	Reasoning  string             `json:"reasoning,omitempty"`
	Usage      *llm.Usage         `json:"usage,omitempty"`
	TS         int64              `json:"ts,omitempty"`
	// Agent 是产生这条消息的 agent（assistant、tool）。会话中途换过 agent 时，旧消息的来源靠它和后缀
	// （没有字段的老消息用 Settings.AgentSegments 推断）区分；用户消息没有这个字段。
	Agent string `json:"agent,omitempty"`

	// 下面几项标出来源，用户插话用得上（老消息没有这些字段，读出来是零值）：
	//   Kind      "user_interjection" 表示这是任务进行中收到的用户补充，不是新起的一轮
	//   ParentRun 这条插话属于哪一次 run（历史分组、压缩、撤回按它识别，不把一次任务拆成几次）
	//   SourceID  平台上的原始消息 ID；Sender 平台确认的发送者
	Kind      string `json:"kind,omitempty"`
	ParentRun string `json:"parent_run_id,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	Sender    string `json:"sender,omitempty"`
	// ID 是这条消息的稳定标识（插话批量注入时用它和历史对账，位置/Base 只作辅助）。
	ID string `json:"id,omitempty"`
	// SourceIDs 是这条消息由哪些插话合并而来（按接收顺序）。
	SourceIDs []string `json:"source_ids,omitempty"`
}

type Store struct {
	dir      string
	hist     map[string][]Msg // 从 Settings.Base 开始的历史
	settings map[string]Settings
	branches map[string]BranchMeta // 分支元信息（按需从磁盘加载）
	promoted map[string]bool       // 已经升级到新布局的会话（W1）

	snapCacheMu sync.Mutex
	snapCache   map[string][]Msg // 快照只读缓存（有上限，空闲分支不各留一份）
	snapBytes   map[string]int64
	snapBudget  int64
}

func New(dir string) (*Store, error) {
	for _, sub := range []string{
		filepath.Join(dir, "sessions"),
		filepath.Join(dir, "v1", "sessions"),
		filepath.Join(dir, "v1", "sessions", "keys"),
		filepath.Join(dir, "v1", "active"),
		filepath.Join(dir, "v1", "deleted"),
		filepath.Join(dir, "v1", "gen"),
		filepath.Join(dir, "v1", "renames"),
		filepath.Join(dir, "v1", "staging"),
		filepath.Join(dir, "v1", "orphan"),
		filepath.Join(dir, "v1", "archive"),
	} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, hist: map[string][]Msg{}, settings: map[string]Settings{},
		promoted: map[string]bool{}, snapBudget: 8 << 20}, nil
}

// targetHasOurContent 核对目标上确实已经有我们发布的内容（提交窗口恢复时用）。
// 历史允许以我们的内容开头再追加（目标已经接受过新消息），其它文件要求完全一致。
func (s *Store) targetHasOurContent(to string, op renameOp, suffixes []string) error {
	for _, suffix := range suffixes {
		meta, tracked := op.Files[suffix]
		if !tracked {
			continue
		}
		data, err := os.ReadFile(s.newPath(to, suffix))
		if err != nil {
			return fmt.Errorf("提交点已过但目标缺少内容（%s），保留现场等待人工核对", suffix)
		}
		if int64(len(data)) == meta.Len && sumOf(data) == meta.Sum {
			continue
		}
		if suffix == ".jsonl" && int64(len(data)) > meta.Len && sumOf(data[:meta.Len]) == meta.Sum {
			continue // 发布过之后又追加了新消息
		}
		return fmt.Errorf("提交点已过但目标内容与记录不一致（%s），保留双方等待人工核对", suffix)
	}
	return nil
}

// renameStagingDir 是这笔改名私有的暂存目录：没提交之前普通定位看不到它。
func (s *Store) renameStagingDir(from, to string) string {
	return filepath.Join(s.dirV1(), "staging", Key(from)+"-"+Key(to))
}

// ResumeRenames 在启动时把没做完的改名按阶段继续（启动入口和测试共用）。
func (s *Store) ResumeRenames() error {
	for _, e := range mustReadDir(filepath.Join(s.dirV1(), "renames")) {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dirV1(), "renames", e.Name()))
		if err != nil {
			continue
		}
		var op renameOp
		if json.Unmarshal(b, &op) != nil || op.From == "" || op.To == "" {
			return fmt.Errorf("改名操作记录损坏：%s", e.Name())
		}
		if err := s.Rename(op.From, op.To); err != nil {
			return err
		}
	}
	return nil
}

// renameOp 是一次改名的持久操作记录：绑定源的目标身份、世代与每个文件的内容版本。
type renameOp struct {
	From    string                `json:"from"`
	To      string                `json:"to"`
	Stage   string                `json:"stage"` // started | copied | committed
	FromGen int64                 `json:"from_gen"`
	Files   map[string]renameFile `json:"files,omitempty"`
}

// renameFile 是一个文件在复制时的内容版本（长度 + sha1）。
type renameFile struct {
	Len int64  `json:"len"`
	Sum string `json:"sum"`
}

func sumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Store) renameOpFile(from, to string) string {
	return filepath.Join(s.dirV1(), "renames", Key(from)+"-"+Key(to)+".json")
}

// Dir 是数据目录。
func (s *Store) Dir() string { return s.dir }

func (s *Store) file(sid string) string {
	return s.newPath(sid, ".jsonl")
}

// ensureActive 给新会话写激活标记：从创建那一刻起它就是新布局里的权威数据。
func (s *Store) ensureActive(sid string) error {
	if err := os.MkdirAll(filepath.Dir(s.activeMarker(sid)), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		if err := writeFileAtomic(s.activeMarker(sid), []byte(Key(sid))); err != nil {
			return err
		}
	}
	// 新代的激活提交点：墓碑必须真的删掉，删不掉就不能回执成功
	if err := os.Remove(s.deletedMarker(sid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("清除删除标记失败：%w", err)
	}
	return nil
}

// startNewGeneration 显式创建同 sid 的新一代：先把旧代残留整体挪进 orphan 目录隔离，
// 再清墓碑、写激活标记。任何一步失败都返回错误，绝不先追加到残留文件上。
func (s *Store) startNewGeneration(sid string) error {
	if !s.isDead(sid) {
		return nil
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	dst := filepath.Join(s.dirV1(), "orphan", Key(sid)+"-"+stamp)
	moved := false
	for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
		src := s.newPath(sid, suffix)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, filepath.Join(dst, strings.TrimPrefix(suffix, "."))); err != nil {
			return fmt.Errorf("隔离旧代文件失败：%w", err)
		}
		moved = true
	}
	_ = moved
	// 世代加一：旧根备份从此永久失效，正在进行的操作也靠它判断“这还是不是自己那一代”
	if _, err := s.bumpGen(sid); err != nil {
		return err
	}
	return s.ensureActive(sid)
}

// legacyPath 是旧布局里某一类文件的位置（只读兼容与迁移来源）。
func (s *Store) legacyPath(sid, suffix string) string {
	return filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+suffix)
}

// readFileFor 读会话某一类文件：先走权威定位；升级失败时退回旧位置继续读，绝不在这里新建。
func (s *Store) readFileFor(sid, suffix string) (string, bool) {
	p, ok, err := s.locate(sid, suffix)
	if err != nil {
		if lp := s.legacyPath(sid, suffix); lp != "" {
			if _, statErr := os.Stat(lp); statErr == nil {
				return lp, true
			}
		}
		return p, false
	}
	return p, ok
}

// History 返回会话历史（第一次访问时从磁盘加载）。调用方不要修改返回的切片。
func (s *Store) History(sid string) []Msg {
	if h, ok := s.hist[sid]; ok {
		return h
	}
	h := s.load(sid)
	s.hist[sid] = h
	return h
}

func (s *Store) load(sid string) []Msg {
	if s.isDead(sid) {
		return nil // 删除墓碑：连分支继承的快照视图也不再授权
	}
	base := s.Settings(sid).Base
	if br, ok := s.Branch(sid); ok {
		return s.loadBranch(br, base)
	}
	return s.loadFrom(sid, base)
}

// Archive 返回完整原始记录，仅用于查看历史；模型继续使用压缩后的 History。
func (s *Store) Archive(sid string) []Msg { return s.loadFrom(sid, 0) }

func (s *Store) loadFrom(sid string, skip int) []Msg {
	p, ok := s.readFileFor(sid, ".jsonl")
	if !ok {
		return nil
	}
	return readMsgs(p, skip)
}

// ReadArchive 读一份归档（/reset 时挪走的历史）的全部消息。整理记忆时当日记读。
func (s *Store) ReadArchive(sid, id string) []Msg {
	if filepath.Base(id) != id || filepath.Ext(id) != ".jsonl" {
		return nil
	}
	return readMsgs(filepath.Join(s.archiveDir(sid), id), 0)
}

func readMsgs(path string, skip int) []Msg {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var h []Msg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Role == "" {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		h = append(h, m)
	}
	return h
}

// Append 追加一条消息：先落盘并 fsync，成功之后才进内存。
//
// 顺序很重要：以前是先改内存再写盘，写失败时内存里看起来已经成功了，
// 重试就会把同一条消息写两遍，恢复时也没法和磁盘对账。
func (s *Store) Append(sid string, m Msg) error {
	if m.TS == 0 {
		m.TS = time.Now().Unix()
	}
	// 已删除的会话：同 sid 写入属于显式重建新代，先把旧代残留隔离开
	if err := s.startNewGeneration(sid); err != nil {
		return err
	}
	path, _, err := s.locate(sid, ".jsonl")
	if err != nil {
		return err
	}
	if err := s.rememberLongKey(sid); err != nil {
		return err
	}
	prev := s.History(sid)
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("保存会话历史：%w", err)
	}
	if _, err = f.Write(append(line, '\n')); err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return fmt.Errorf("保存会话历史：%w", err)
	}
	if cerr != nil {
		return fmt.Errorf("保存会话历史：%w", cerr)
	}
	// 新会话：从这里开始它就是新布局的权威数据（旧会话在 locate 里已经提升并写过标记）
	if err := s.ensureActive(sid); err != nil {
		return fmt.Errorf("登记会话激活状态失败：%w", err)
	}
	s.hist[sid] = append(append([]Msg(nil), prev...), m)
	return nil
}

// FindMessage 在会话历史里按稳定 ID 找消息。limit<=0 表示查全部：
// "历史有多长"不能改变"这条消息是否已经提交"这个事实。
func (s *Store) FindMessage(sid, id string, limit int) (Msg, bool) {
	if id == "" {
		return Msg{}, false
	}
	h := s.History(sid)
	start := 0
	if limit > 0 {
		start = len(h) - limit
		if start < 0 {
			start = 0
		}
	}
	for i := len(h) - 1; i >= start; i-- {
		if h[i].ID == id {
			return h[i], true
		}
	}
	// 压缩过的会话：已提交的前缀还在正式文件里，但不在 History 里。
	// "历史有多长/压缩过没有"不能改变"这条消息是否已经提交"的事实。
	return s.findInArchive(sid, func(m Msg) bool { return m.ID == id })
}

// FindMessageBySource 找包含某条插话 ID 的历史消息（补记 injected 时用）。
// limit<=0 表示查全部。
func (s *Store) FindMessageBySource(sid, sourceID string, limit int) (Msg, bool) {
	if sourceID == "" {
		return Msg{}, false
	}
	h := s.History(sid)
	start := 0
	if limit > 0 {
		start = len(h) - limit
		if start < 0 {
			start = 0
		}
	}
	for i := len(h) - 1; i >= start; i-- {
		for _, id := range h[i].SourceIDs {
			if id == sourceID {
				return h[i], true
			}
		}
	}
	return s.findInArchive(sid, func(m Msg) bool {
		for _, id := range m.SourceIDs {
			if id == sourceID {
				return true
			}
		}
		return false
	})
}

// findInArchive 在正式文件里流式查（含已经被压缩掉的 Base 之前的部分），
// 只保留最后一条匹配：不把整份历史读进内存。只有 Base>0 时才需要，平时走内存里的 History。
func (s *Store) findInArchive(sid string, match func(Msg) bool) (Msg, bool) {
	if s.Settings(sid).Base <= 0 {
		return Msg{}, false
	}
	path, okFile := s.readFileFor(sid, ".jsonl")
	if !okFile {
		return Msg{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return Msg{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	var found Msg
	ok := false
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Role == "" {
			continue
		}
		if match(m) {
			found, ok = m, true // 继续扫，取最后一条
		}
	}
	return found, ok
}

// Compact 在历史估算超过 high 时，一次性丢掉开头的一段，让剩下的不超过 low。
// 只在用户消息处切，当前这一轮永远保留。返回丢掉的消息数。
//
// summary 代替被丢掉的部分，放在剩下历史的最前面，直到下一次 Compact。
// 生成摘要失败时传空字符串，保留已有摘要并注明本次省略了旧对话。
func (s *Store) Compact(sid string, high, low int, summary string) (int, error) {
	h := s.History(sid)
	if Estimate(h) <= high {
		return 0, nil
	}
	cut := CutPoint(h, low)
	if cut == 0 {
		return 0, nil
	}
	st := s.Settings(sid)
	st.Base += cut
	const omitted = "[系统] 更早的对话因为太长已经省略。"
	if summary != "" {
		st.Summary = "[系统] 更早对话的摘要：\n" + summary
	} else if !strings.Contains(st.Summary, omitted) {
		st.Summary = strings.TrimSpace(st.Summary + "\n" + omitted)
	}
	if st.EnvNote != "" && !strings.Contains(st.Summary, st.EnvNote) {
		st.Summary += "\n" + st.EnvNote
	}
	st.MemNote, st.LifeNote, st.Recalled = "", "", nil
	if err := s.SaveSettings(sid, st); err != nil {
		return 0, err
	}
	s.hist[sid] = append([]Msg(nil), h[cut:]...)
	return cut, nil
}

// ErrNothingToUndo：没有可以撤回的一轮（历史是空的，或者都已经被 Compact 整理掉了）
var ErrNothingToUndo = errors.New("没有可以撤回的对话")

// Undo 撤回最近一轮：删掉最后一条用户消息和它后面的所有消息，磁盘上的文件也截掉。
// 只动历史的末尾，前面的部分不变，前缀缓存照样命中。被 Compact 整理掉的部分不能撤回。
// 返回删掉的消息。
func (s *Store) Undo(sid string) ([]Msg, error) {
	h := s.History(sid)
	i := len(h) - 1
	for i >= 0 && h[i].Role != "user" {
		i--
	}
	if i < 0 {
		return nil, ErrNothingToUndo
	}
	off, err := s.offsetOf(sid, s.Settings(sid).Base+i)
	if err != nil {
		return nil, err
	}
	if err := os.Truncate(s.file(sid), off); err != nil {
		return nil, fmt.Errorf("撤回：%w", err)
	}
	removed := append([]Msg(nil), h[i:]...)
	s.hist[sid] = append([]Msg(nil), h[:i]...)
	return removed, nil
}

// offsetOf 是历史文件里第 n 条消息（从 0 数，和 loadFrom 一样跳过坏行）开头的字节位置。
func (s *Store) offsetOf(sid string, n int) (int64, error) {
	path, okFile := s.readFileFor(sid, ".jsonl")
	if !okFile {
		return 0, fmt.Errorf("撤回：历史文件不存在")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var off int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m Msg
			if json.Unmarshal(line, &m) == nil && m.Role != "" {
				if n == 0 {
					return off, nil
				}
				n--
			}
			off += int64(len(line))
		}
		if err != nil {
			return 0, fmt.Errorf("撤回：历史文件里找不到这条消息")
		}
	}
}

// Reset 清空会话历史（磁盘文件一并删除）。工作目录等设置保留，但环境说明要在新的历史里重新给一次。
func (s *Store) Reset(sid string) error {
	// 分支的快照不能删（也许还有别的分支在用）：把 Base 推到超过快照长度，
	// 有效窗口就空了，下次访问看到的是干净上下文。
	base := 0
	if br, ok := s.Branch(sid); ok {
		base = len(s.SnapshotMessages(br.SnapshotID))
	}
	if st := s.Settings(sid); st.EnvNote != "" || st.Base != base || st.Summary != "" || st.Meter.Usage.PromptTokens != 0 || st.Prefix != nil || len(st.Omitted) > 0 || st.MemNote != "" || st.LifeNote != "" || len(st.Recalled) > 0 || st.Handoff != "" || len(st.AgentSegments) > 0 {
		st.EnvNote, st.Base, st.Summary = "", base, ""
		st.MemNote, st.LifeNote, st.Recalled = "", "", nil
		st.Meter = Meter{}
		st.Prefix = nil
		st.Omitted = nil
		st.CompactedAt = 0
		// 历史清空了：切换边界和交接说明都指向不存在的消息；选的 agent 本身保留
		st.Handoff, st.AgentSegments = "", nil
		if err := s.SaveSettings(sid, st); err != nil {
			return err
		}
	}
	s.hist[sid] = nil
	path, ok, err := s.locate(sid, ".jsonl")
	if err != nil {
		return err
	}
	if ok {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// 激活标记保留：重置之后这个会话仍然只认新布局，不会再从旧备份读回来
	return nil
}

// Exists 判断磁盘上有没有这个会话（有历史或者有设置）。
func (s *Store) Exists(sid string) bool {
	if s.isDead(sid) {
		return false
	}
	for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
		if _, ok, err := s.locate(sid, suffix); err == nil && ok {
			return true
		}
	}
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		return true // 刚重置过：没有文件但会话仍然存在
	}
	return false
}

// Rename 把会话的历史和设置换一个 ID（升级旧数据用）。新 ID 不能已经存在。
func (s *Store) Rename(from, to string) error {
	if from == to {
		return nil
	}
	opPath := s.renameOpFile(from, to)
	var op renameOp
	stage := ""
	if b, err := os.ReadFile(opPath); err == nil {
		if err := json.Unmarshal(b, &op); err != nil || op.From != from || op.To != to {
			return fmt.Errorf("改名操作记录损坏或不匹配：%s", opPath)
		}
		stage = op.Stage
		switch stage {
		case "started", "copied", "committed":
		default:
			return fmt.Errorf("改名操作记录的阶段不认识：%q（保留现场，不执行清理）", stage)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	writeOp := func() error {
		b, _ := json.MarshalIndent(op, "", "  ")
		return writeFileAtomic(opPath, b)
	}
	staging := s.renameStagingDir(from, to)
	suffixes := []string{".jsonl", ".settings.json", ".branch.json"}
	if stage == "" {
		if s.isDead(from) {
			return fmt.Errorf("会话 %s 已经被删除", from)
		}
		if s.Exists(to) {
			return fmt.Errorf("会话 %s 已经存在", to)
		}
		if err := s.rememberLongKey(to); err != nil {
			return err
		}
		gen, err := s.readGen(from)
		if err != nil {
			return err
		}
		op = renameOp{From: from, To: to, Stage: "started", FromGen: gen, Files: map[string]renameFile{}}
		if err := writeOp(); err != nil {
			return err
		}
		stage = "started"
	}
	if stage == "started" {
		if err := os.MkdirAll(staging, 0o700); err != nil {
			return err
		}
		for _, suffix := range suffixes {
			src, ok, err := s.locate(from, suffix)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(staging, strings.TrimPrefix(suffix, ".")), data, 0o600); err != nil {
				return err
			}
			op.Files[suffix] = renameFile{Len: int64(len(data)), Sum: sumOf(data)}
		}
		op.Stage = "copied"
		if err := writeOp(); err != nil {
			return err
		}
		stage = "copied"
	}
	if stage == "copied" {
		// 提交窗口：源墓碑已经写下去、committed 记录没写成就中断了。
		// 这说明提交点已经过了，只需核对目标确实有我们的内容，然后补记录收尾。
		if s.isDead(from) {
			if err := s.targetHasOurContent(to, op, suffixes); err != nil {
				return err
			}
			op.Stage = "committed"
			if err := writeOp(); err != nil {
				return err
			}
			stage = "committed"
		}
	}
	if stage == "copied" {
		// 源在这一笔期间有没有变：每次都要把**完整文件集合**核对一遍。
		// 复制之后新出现的文件（例如原本没有历史、后来写了一条）也要算进来，
		// 否则发布后清理会把这条已受理的消息删掉。
		for _, suffix := range suffixes {
			meta, tracked := op.Files[suffix]
			src, ok, err := s.locate(from, suffix)
			if err != nil {
				return err
			}
			if !tracked && !ok {
				continue
			}
			if !tracked && ok {
				return fmt.Errorf("源在这一笔改名期间新出现了文件（%s），保留双方等待人工核对", suffix)
			}
			if tracked && !ok {
				return fmt.Errorf("改名的源文件不见了（%s），保留现场等待人工核对", suffix)
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			if int64(len(data)) != meta.Len || sumOf(data) != meta.Sum {
				return fmt.Errorf("源在这一笔改名期间又被写了新内容（%s），保留双方等待人工核对", suffix)
			}
		}
		// 暂存副本必须齐全：记录里有、暂存里没了，就是丢了内容，不能继续发布再删源
		for _, suffix := range suffixes {
			meta, tracked := op.Files[suffix]
			if !tracked {
				continue
			}
			staged := filepath.Join(staging, strings.TrimPrefix(suffix, "."))
			data, err := os.ReadFile(staged)
			if err != nil {
				return fmt.Errorf("改名的暂存副本不见了（%s），保留源和现场", suffix)
			}
			if int64(len(data)) != meta.Len || sumOf(data) != meta.Sum {
				return fmt.Errorf("改名的暂存副本与记录不一致（%s），保留源和现场", suffix)
			}
		}
		if err := s.ensureActive(to); err != nil {
			return err
		}
		for _, suffix := range suffixes {
			staged := filepath.Join(staging, strings.TrimPrefix(suffix, "."))
			data, err := os.ReadFile(staged)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			dst := s.newPath(to, suffix)
			if old, err := os.ReadFile(dst); err == nil {
				if sumOf(old) == sumOf(data) {
					continue // 已经发布过
				}
				// 历史是追加写的：目标以我们的内容开头，说明发布已经发生、后面又追加了新消息，
				// 这些新消息必须保留，不能覆盖也不能因此丢掉源。只有前缀成立才这么判。
				if suffix == ".jsonl" && len(old) > len(data) && bytes.Equal(old[:len(data)], data) {
					continue
				}
				return fmt.Errorf("目标已经有不同的内容（%s），保留双方等待人工核对", suffix)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o600); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(s.deletedMarker(from)), 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(s.deletedMarker(from), []byte(Key(from))); err != nil {
			return err
		}
		_ = os.Remove(s.activeMarker(from))
		op.Stage = "committed"
		if err := writeOp(); err != nil {
			return err
		}
		stage = "committed"
	}
	// committed：只做清理。源如果已经开了新一代，这些路径属于新代，绝不能删。
	gen, gerr := s.readGen(from)
	if gerr != nil {
		return fmt.Errorf("改名已提交，但读会话世代失败，暂不清理源文件（可以重试）：%w", gerr)
	}
	if gen == op.FromGen {
		for _, suffix := range suffixes {
			if err := os.Remove(s.newPath(from, suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("改名已提交，清理源文件失败（可以重试）：%w", err)
			}
		}
	}
	_ = os.RemoveAll(staging)
	if err := os.Remove(opPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("改名已提交，清理操作记录失败（可以重试）：%w", err)
	}
	delete(s.hist, from)
	delete(s.settings, from)
	delete(s.hist, to)
	delete(s.settings, to)
	return nil
}

// Delete 删除会话的历史和设置。
func (s *Store) Delete(sid string) error {
	// 墓碑先落盘：即使后面清理失败，这个会话也不会从旧备份复活
	if err := os.MkdirAll(filepath.Dir(s.deletedMarker(sid)), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(s.deletedMarker(sid), []byte(Key(sid))); err != nil {
		return err
	}
	_ = os.Remove(s.activeMarker(sid))
	// 物理清理用的是“这次删除拥有的资源”：不能走 locate，墓碑会让它返回空集
	var firstErr error
	for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
		if err := os.Remove(s.newPath(sid, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if err := os.RemoveAll(filepath.Join(s.dirV1Archive(), Key(sid))); err != nil && firstErr == nil {
		firstErr = err
	}
	delete(s.hist, sid)
	delete(s.settings, sid)
	delete(s.branches, sid)
	if firstErr != nil {
		return fmt.Errorf("删除会话时清理文件失败（墓碑已生效，可以重试）：%w", firstErr)
	}
	return nil
}
