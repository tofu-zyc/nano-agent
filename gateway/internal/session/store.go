// Package session 保存每个会话的对话历史：内存一份，磁盘上每个会话一个 JSONL 文件（只追加）。
//
// 发给模型的历史只追加不改动，这样每次请求都是上一次请求的延续，模型那边的前缀缓存才能命中。
// 历史太长时由 Compact 一次性丢掉开头的一大段（记下丢到了哪里），而不是每次都丢一点。
//
// Store 不加锁，由调用方（app）串行访问。
package session

import (
	"bufio"
	"crypto/rand"
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
	"nano-agent/gateway/internal/proto"
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
	SourceIDs []string       `json:"source_ids,omitempty"`
	Sandbox   *proto.Sandbox `json:"sandbox,omitempty"`
	AgendaID  string         `json:"agenda_id,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

type Store struct {
	dir      string
	hist     map[string][]Msg // 从 Settings.Base 开始的历史
	settings map[string]Settings
	branches map[string]BranchMeta // 分支元信息（按需从磁盘加载）
	issues   map[string]string
	promoted map[string]bool // 已经升级到新布局的会话（W1）

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
		filepath.Join(dir, "v1", "rev"),
		filepath.Join(dir, "v1", "renames"),
		filepath.Join(dir, "v1", "renames", "settled"),
		filepath.Join(dir, "v1", "promotions"),
		filepath.Join(dir, "v1", "promotions", "settled"),
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
		path := s.newPath(to, suffix)
		if suffix == ".jsonl" {
			ok, err := versionIsPrefix(path, meta)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("提交点已过但目标缺少内容（%s），保留现场等待人工核对", suffix)
				}
				return err
			}
			if ok {
				continue
			}
		} else {
			ok, err := versionMatches(path, meta)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("提交点已过但目标缺少内容（%s），保留现场等待人工核对", suffix)
				}
				return err
			}
			if ok {
				continue
			}
		}
		return fmt.Errorf("提交点已过但目标内容与记录不一致（%s），保留双方等待人工核对", suffix)
	}
	return s.publishRenameAssets(op, s.renameStagingDir(op.From, op.To), true)
}

// renameStagingDir 是这笔改名私有的暂存目录：没提交之前普通定位看不到它。
func (s *Store) renameStagingDir(from, to string) string {
	return filepath.Join(s.dirV1(), "staging", Key(from)+"-"+Key(to))
}

// ResumeRenames 在启动时把没做完的改名按阶段继续（启动入口和测试共用）。
func (s *Store) ResumeRenames() error {
	entries, err := os.ReadDir(filepath.Join(s.dirV1(), "renames"))
	if err != nil {
		return fmt.Errorf("读未完成改名目录失败：%w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dirV1(), "renames", e.Name()))
		if err != nil {
			return fmt.Errorf("读改名操作记录失败：%s：%w", e.Name(), err)
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

// renameOp 是一次改名的持久操作记录：绑定 operation_id、源 / 目标身份、
// 双方世代与上下文版本，以及每个文件的内容版本。
//
// 版本 2 起，恢复必须先核对这份记录仍然有效（尤其是目标在操作之后没有被
// 删除 / 重置、源没有换代），再决定提交、清理或结算为失效。缺 operation_id
// 的记录是旧版本留下的，只能按旧协议处理，不能据此取得新的删除权限。
type renameOp struct {
	Version     int                   `json:"version,omitempty"`
	OperationID string                `json:"operation_id,omitempty"`
	From        string                `json:"from"`
	To          string                `json:"to"`
	Stage       string                `json:"stage"` // started | copied | committing | committed（另有终态 settled 记录）
	Reason      string                `json:"reason,omitempty"`
	SettledAt   int64                 `json:"settled_at,omitempty"`
	FromGen     int64                 `json:"from_gen"`
	FromRev     int64                 `json:"from_rev,omitempty"`
	ToGen       int64                 `json:"to_gen,omitempty"`
	ToRev       int64                 `json:"to_rev,omitempty"`
	Files       map[string]renameFile `json:"files,omitempty"`
	Assets      []renameAsset         `json:"assets,omitempty"`
}

// renameFile 是一个文件在复制时的内容版本（长度 + sha256）。
type renameFile struct {
	Len int64  `json:"len"`
	Sum string `json:"sum"`
}

// renameTombstone 是改名提交时写的来源墓碑。带 operation_id 才能区分
// “本操作按协议让源失效”和“用户后来主动删除源”；旧版墓碑是裸键，没有归属。
type renameTombstone struct {
	OperationID string `json:"operation_id"`
	From        string `json:"from,omitempty"`
	FromGen     int64  `json:"from_gen,omitempty"`
	At          int64  `json:"at,omitempty"`
}

func sumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Store) renameOpFile(from, to string) string {
	return filepath.Join(s.dirV1(), "renames", Key(from)+"-"+Key(to)+".json")
}

func newOperationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成操作 ID 失败：%w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Store) writeRenameTombstone(op renameOp) error {
	b, err := json.MarshalIndent(renameTombstone{OperationID: op.OperationID, From: op.From,
		FromGen: op.FromGen, At: time.Now().Unix()}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.deletedMarker(op.From), b)
}

// deadMarkerOwner 读墓碑归属。exists=false 表示没有墓碑；owner="" 表示
// 旧版墓碑或用户删除，不属于当前改名操作。
func (s *Store) deadMarkerOwner(sid string) (owner string, exists bool, err error) {
	b, err := os.ReadFile(s.deletedMarker(sid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", true, err
	}
	var t renameTombstone
	if json.Unmarshal(b, &t) == nil && t.OperationID != "" {
		return t.OperationID, true, nil
	}
	return "", true, nil
}

// renameTargetAuthorized 核对目标仍然是操作开始时的那一代 / 那一版上下文。
// 目标被删除、重置或换代后，旧操作必须失效，不能把旧上下文写回去。
func (s *Store) renameTargetAuthorized(op renameOp) (bool, error) {
	if s.isDead(op.To) {
		return false, nil
	}
	rev, err := s.readRev(op.To)
	if err != nil {
		return false, err
	}
	if rev != op.ToRev {
		return false, nil
	}
	if op.OperationID == "" {
		return true, nil // 旧记录没有绑定目标世代；至少删除 / 重置 / 版本错误仍然会使其失效
	}
	gen, err := s.readGen(op.To)
	if err != nil {
		return false, err
	}
	return gen == op.ToGen, nil
}

// renameSourceState 返回源在恢复时的归属状态：alive = 仍是操作开始时的同一代；
// ours = 墓碑属于本操作（提交点已过）；other = 已被用户删除 / 重置 / 换代，
// 旧操作不得再发布或清理。
func (s *Store) renameSourceState(op renameOp) (string, error) {
	if s.isDead(op.From) {
		owner, _, err := s.deadMarkerOwner(op.From)
		if err != nil {
			return "", err
		}
		if op.OperationID != "" && owner != op.OperationID {
			return "other", nil
		}
		return "ours", nil
	}
	gen, err := s.readGen(op.From)
	if err != nil {
		return "", err
	}
	if gen != op.FromGen {
		return "other", nil
	}
	rev, err := s.readRev(op.From)
	if err != nil {
		return "", err
	}
	if rev != op.FromRev {
		return "other", nil
	}
	return "alive", nil
}

// settleRenameOp 把一笔不再能继续（或已经完成）的操作写成终态记录，保留现场供排查，
// 但不静默发布 / 清理另一代。活动记录和暂存目录随后移除。
func (s *Store) settleRenameOp(op renameOp, opPath, stage, reason string) error {
	op.Stage, op.Reason, op.SettledAt = stage, reason, time.Now().Unix()
	b, _ := json.MarshalIndent(op, "", "  ")
	id := op.OperationID
	if id == "" {
		id = Key(op.From) + "-" + Key(op.To)
	}
	if err := writeFileAtomic(filepath.Join(s.dirV1(), "renames", "settled", id+".json"), b); err != nil {
		return fmt.Errorf("记录改名终态失败（保留活动记录）：%w", err)
	}
	_ = os.RemoveAll(s.renameStagingDir(op.From, op.To))
	if err := os.Remove(opPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("改名已结算，移除活动记录失败（可以重试）：%w", err)
	}
	return nil
}

// validateRenameSource 核对源仍是复制时的同一组文件、同一份内容。
// 复制之后新出现的文件也必须算进来，否则发布后清理会删掉已受理的新消息。
func (s *Store) validateRenameSource(op renameOp, suffixes []string) error {
	for _, suffix := range suffixes {
		meta, tracked := op.Files[suffix]
		src, ok, err := s.locate(op.From, suffix)
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
		if err := verifyFileVersion(src, meta, fmt.Sprintf("源在这一笔改名期间的内容（%s）", suffix)); err != nil {
			return err
		}
	}
	return s.validateRenameAssets(op, s.renameStagingDir(op.From, op.To), true)
}

// validateRenameStaging 核对暂存副本齐全且与记录一致。记录里有、暂存里没了，
// 就是丢了内容，不能继续发布再删源。
func (s *Store) validateRenameStaging(op renameOp, staging string) error {
	for suffix, meta := range op.Files {
		staged := filepath.Join(staging, strings.TrimPrefix(suffix, "."))
		if err := verifyFileVersion(staged, meta, fmt.Sprintf("改名的暂存副本（%s）", suffix)); err != nil {
			return err
		}
	}
	return s.validateRenameAssets(op, staging, false)
}

// publishRenameTarget 把暂存副本发布到目标。目标已激活过（重试）时，只接受
// 已经属于本操作的内容：完全相同，或历史以本操作发布的内容开头（后面有新消息）。
func (s *Store) publishRenameTarget(op renameOp, staging string, suffixes []string) error {
	if err := s.ensureActive(op.To); err != nil {
		return err
	}
	for _, suffix := range suffixes {
		meta, tracked := op.Files[suffix]
		if !tracked {
			continue
		}
		staged := filepath.Join(staging, strings.TrimPrefix(suffix, "."))
		dst := s.newPath(op.To, suffix)
		if _, statErr := os.Stat(dst); statErr == nil {
			var ok bool
			var err error
			if suffix == ".jsonl" {
				ok, err = versionIsPrefix(dst, meta)
			} else {
				ok, err = versionMatches(dst, meta)
			}
			if err != nil {
				return err
			}
			if ok {
				continue // 已经发布过
			}
			return fmt.Errorf("目标已经有不同的内容（%s），保留双方等待人工核对", suffix)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		got, err := copyFileVersion(staged, dst)
		if err != nil {
			return fmt.Errorf("发布改名目标失败（%s）：%w", suffix, err)
		}
		if got.Len != meta.Len || got.Sum != meta.Sum {
			return fmt.Errorf("发布后的目标内容与记录不一致（%s），保留双方等待人工核对", suffix)
		}
	}
	return s.publishRenameAssets(op, staging, false)
}

// Dir 是数据目录。
func (s *Store) Dir() string { return s.dir }

// LifecycleVersion is the common authorization boundary for deferred publication.
func (s *Store) LifecycleVersion(sid string) (generation, revision int64, err error) {
	generation, err = s.readGen(sid)
	if err != nil {
		return
	}
	revision, err = s.readRev(sid)
	return
}

func (s *Store) file(sid string) string {
	return s.newPath(sid, ".jsonl")
}

// ensureActive 给新会话写激活标记：从创建那一刻起它就是新布局里的权威数据。
//
// 注意：它会清掉删除墓碑。只有明确的用户写入（startNewGeneration）或已经核对过
// 目标世代 / 版本 / 归属的改名发布才能调用；恢复旧操作不能直接拿它取消后续删除。
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
	_, revokedErr := os.Stat(s.promotionRevokedPath(sid))
	_, activeErr := os.Stat(s.activeMarker(sid))
	if !s.isDead(sid) && !(revokedErr == nil && errors.Is(activeErr, os.ErrNotExist)) {
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

// readFileFor never falls back across a failed publication decision.
func (s *Store) readFileFor(sid, suffix string) (string, bool) {
	p, ok, err := s.locate(sid, suffix)
	if err != nil {
		s.RecordStorageIssue(sid, err)
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
func (s *Store) Archive(sid string) []Msg {
	if _, branch := s.Branch(sid); !branch {
		return s.loadFrom(sid, 0)
	}
	lease, err := s.FreezeSnapshot(sid, s.Settings(sid))
	if err != nil {
		return nil
	}
	defer lease.Close()
	sc := bufio.NewScanner(lease.reader())
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	var out []Msg
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Role != "" {
			out = append(out, m)
		}
	}
	return out
}

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
	dir, err := s.archiveReadDir(sid)
	if err != nil {
		return nil
	}
	return readMsgs(filepath.Join(dir, id), 0)
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
	absolute := s.Settings(sid).Base + i
	physical := absolute
	if br, ok := s.Branch(sid); ok {
		info, exists := s.Snapshot(br.SnapshotID)
		if !exists {
			return nil, errors.New("分支快照缺失")
		}
		end := info.Messages
		if br.InheritedEndSet {
			end = br.InheritedEnd
		}
		if absolute < end {
			if err := writeFileAtomic(s.file(sid), nil); err != nil {
				return nil, err
			}
			br.InheritedEnd, br.InheritedEndSet = absolute, true
			if err := s.saveBranch(br); err != nil {
				return nil, err
			}
			removed := append([]Msg(nil), h[i:]...)
			s.hist[sid] = append([]Msg(nil), h[:i]...)
			return removed, nil
		}
		physical -= end
	}
	off, err := s.offsetOf(sid, physical)
	if err != nil {
		return nil, err
	}
	if err := replaceHistoryPrefix(s.file(sid), off); err != nil {
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
	// 先固定一个完整已提交视图，再推进失效 revision；不能让 Reset 自己
	// 将一个尚有导入资格的旧会话留在“revision 已变但未提升”的窗口。
	if _, _, err := s.locate(sid, ".jsonl"); err != nil {
		return err
	}
	// 先推进上下文版本：即使后面清理失败，也绝不能让更早的改名恢复把旧上下文写回来。
	if _, err := s.bumpRev(sid); err != nil {
		return fmt.Errorf("重置前登记上下文版本失败：%w", err)
	}
	// 分支的快照不能删（也许还有别的分支在用）：把 Base 推到超过快照长度，
	// 有效窗口就空了，下次访问看到的是干净上下文。
	base := 0
	if br, ok := s.Branch(sid); ok {
		if info, exists := s.Snapshot(br.SnapshotID); exists {
			base = info.Messages
			if br.InheritedEndSet {
				base = br.InheritedEnd
			}
		}
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
	for _, kind := range []string{"persona", "interjections"} {
		rec, has, err := s.readAsset(kind, sid)
		if err != nil {
			s.RecordStorageIssue(sid, err)
			continue
		}
		if !has {
			continue
		}
		if _, exists, err := s.AuxiliaryPath(kind, s.assetRoot(rec.Root), rec.Extension, sid, false); err != nil {
			s.RecordStorageIssue(sid, err)
		} else if exists {
			return true
		}
	}
	return false
}

// Rename 把会话的历史、设置和已发布的辅助文件换一个 ID（升级旧数据用）。新 ID 不能已经存在。
//
// 恢复协议（版本 2）：operation_id 绑定双方世代 / 上下文版本；提交前先落盘
// committing 意图，来源墓碑带 operation_id。恢复时先核对这份授权仍然有效，
// 再消费复制内容；目标被删除 / 重置或源已换代时只结算旧操作，不发布、不清理。
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
		case "started", "copied", "committing", "committed":
		case "cancelled", "superseded", "conflict":
			return s.settleRenameOp(op, opPath, stage, op.Reason)
		default:
			return fmt.Errorf("改名操作记录的阶段不认识：%q（保留现场，不执行清理）", stage)
		}
		if op.Version >= 2 && op.OperationID == "" {
			return fmt.Errorf("改名操作记录缺少 operation_id：%s（保留现场，不执行清理）", opPath)
		}
		if op.Files == nil {
			op.Files = map[string]renameFile{}
		}
		for suffix := range op.Files {
			switch suffix {
			case ".jsonl", ".settings.json", ".branch.json":
			default:
				return fmt.Errorf("改名操作记录包含未知文件类型 %q（保留现场，不执行清理）", suffix)
			}
		}
		if op.FromGen < 0 || op.FromGen > maxCounter || op.FromRev < 0 || op.FromRev > maxCounter ||
			op.ToGen < 0 || op.ToGen > maxCounter || op.ToRev < 0 || op.ToRev > maxCounter {
			return fmt.Errorf("改名操作记录的世代 / 版本越界（保留现场，不执行清理）")
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
		if s.isDead(to) {
			return fmt.Errorf("目标会话 %s 已经被删除", to)
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
		rev, err := s.readRev(from)
		if err != nil {
			return err
		}
		toGen, err := s.readGen(to)
		if err != nil {
			return err
		}
		toRev, err := s.readRev(to)
		if err != nil {
			return err
		}
		id, err := newOperationID()
		if err != nil {
			return err
		}
		assets, err := s.planRenameAssets(from, to)
		if err != nil {
			return err
		}
		op = renameOp{Version: 3, OperationID: id, From: from, To: to, Stage: "started",
			FromGen: gen, FromRev: rev, ToGen: toGen, ToRev: toRev, Files: map[string]renameFile{}, Assets: assets}
		if err := writeOp(); err != nil {
			return err
		}
		stage = "started"
	}
	if stage == "started" {
		if op.OperationID != "" {
			state, err := s.renameSourceState(op)
			if err != nil {
				return err
			}
			if state != "alive" {
				return s.settleRenameOp(op, opPath, "superseded", "源在复制前已删除、重置或换代")
			}
			ok, err := s.renameTargetAuthorized(op)
			if err != nil {
				return err
			}
			if !ok {
				return s.settleRenameOp(op, opPath, "superseded", "目标在复制前已删除、重置或换代")
			}
		}
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
			if _, err := os.Stat(src); err != nil {
				return err
			}
			meta, err := copyFileVersion(src, filepath.Join(staging, strings.TrimPrefix(suffix, ".")))
			if err != nil {
				return fmt.Errorf("复制改名源文件失败（%s）：%w", suffix, err)
			}
			op.Files[suffix] = meta
		}
		if err := s.copyRenameAssets(op, staging); err != nil {
			return err
		}
		op.Stage = "copied"
		if err := writeOp(); err != nil {
			return err
		}
		stage = "copied"
	}
	// 任何继续动作之前先核对目标授权：后面的 ensureActive 会清墓碑、发布会写正文，
	// 被后续删除 / 重置使旧操作失效时绝不能走到那里。
	if op.OperationID != "" {
		ok, err := s.renameTargetAuthorized(op)
		if err != nil {
			return err
		}
		if !ok {
			return s.settleRenameOp(op, opPath, "superseded", "目标在改名期间被删除、重置或换代")
		}
	}
	if stage == "copied" {
		state, err := s.renameSourceState(op)
		if err != nil {
			return err
		}
		if state == "other" {
			return s.settleRenameOp(op, opPath, "superseded", "源在改名期间被删除、重置或换代")
		}
		if state == "ours" {
			// 提交窗口：来源墓碑已经是本操作的，committed 记录没写成就中断了。
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
		// 源仍是同一代：完整文件集合、内容和暂存副本都核对一遍，再写提交意图。
		if err := s.validateRenameSource(op, suffixes); err != nil {
			return err
		}
		if err := s.validateRenameStaging(op, staging); err != nil {
			return err
		}
		op.Stage = "committing"
		if err := writeOp(); err != nil {
			return err
		}
		stage = "committing"
	}
	if stage == "committing" {
		state, err := s.renameSourceState(op)
		if err != nil {
			return err
		}
		switch state {
		case "other":
			return s.settleRenameOp(op, opPath, "superseded", "源的后续失效不属于本操作")
		case "ours":
			// 提交点已过：只核对目标确实承接了我们的内容，然后补记录收尾。
			if err := s.targetHasOurContent(to, op, suffixes); err != nil {
				return err
			}
			op.Stage = "committed"
			if err := writeOp(); err != nil {
				return err
			}
			stage = "committed"
		case "alive":
			if err := s.validateRenameSource(op, suffixes); err != nil {
				return err
			}
			if err := s.validateRenameStaging(op, staging); err != nil {
				return err
			}
			if err := s.publishRenameTarget(op, staging, suffixes); err != nil {
				return err
			}
			if err := s.writeRenameTombstone(op); err != nil {
				return err
			}
			_ = os.Remove(s.activeMarker(from))
			op.Stage = "committed"
			if err := writeOp(); err != nil {
				return err
			}
			stage = "committed"
		}
	}
	if stage != "committed" {
		return fmt.Errorf("改名操作停在未知状态 %q（保留现场）", stage)
	}
	if err := s.finalizeRenameAssets(op); err != nil {
		return err
	}
	// committed：只做清理。源如果已经开了新一代，这些路径属于新代，绝不能删。
	gen, gerr := s.readGen(from)
	if gerr != nil {
		return fmt.Errorf("改名已提交，但读会话世代失败，暂不清理源文件（可以重试）：%w", gerr)
	}
	rev, rerr := s.readRev(from)
	if rerr != nil {
		return fmt.Errorf("改名已提交，但读会话上下文版本失败，暂不清理源文件（可以重试）：%w", rerr)
	}
	owned := gen == op.FromGen && rev == op.FromRev
	if owned {
		owner, exists, err := s.deadMarkerOwner(from)
		if err != nil {
			return err
		}
		if !exists {
			return s.settleRenameOp(op, opPath, "conflict", "改名已提交但来源墓碑不存在，保留双方")
		}
		if op.OperationID != "" && owner != op.OperationID {
			return s.settleRenameOp(op, opPath, "superseded", "来源墓碑属于后续操作，保留现有状态")
		}
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
	// 推进上下文版本：删除之后的恢复必须能看出旧改名已经失效。
	// 墓碑已经生效，即使这里失败也绝不能反过来复活旧内容。
	var revErr error
	if _, err := s.bumpRev(sid); err != nil {
		revErr = fmt.Errorf("登记删除后的上下文版本失败：%w", err)
	}
	_ = os.Remove(s.activeMarker(sid))
	// 物理清理用的是“这次删除拥有的资源”：不能走 locate，墓碑会让它返回空集
	var firstErr error
	for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
		if err := os.Remove(s.newPath(sid, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = revErr
	}
	if err := os.RemoveAll(filepath.Join(s.dirV1Archive(), Key(sid))); err != nil && firstErr == nil {
		firstErr = err
	}
	delete(s.hist, sid)
	delete(s.settings, sid)
	delete(s.branches, sid)
	if firstErr != nil {
		return fmt.Errorf("删除会话时登记删除状态或清理文件失败（墓碑已生效，可以重试）：%w", firstErr)
	}
	return nil
}
