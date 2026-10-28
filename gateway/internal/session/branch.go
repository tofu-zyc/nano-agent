package session

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"nano-agent/gateway/internal/llm"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"
)

// ---- 会话 Fork 的存储层（设计文档 §8 / F1） ----
//
// 分叉点保存成一份**不可变快照**（data/session-snapshots/<id>/），分支只追加自己的尾部
// （data/sessions/<branch>.jsonl）。分支的有效历史 = 快照里分叉点的有效窗口 + 自己的尾部：
//
//	data/session-snapshots/<id>/messages.jsonl   分叉点前的完整已提交历史（只读）
//	data/session-snapshots/<id>/context.json     当时的 Summary / Base / Omitted / 设置 / 附件引用
//	data/sessions/<branch>.branch.json           父子关系、快照引用、操作号、状态、投递策略
//	data/sessions/<branch>.jsonl                 分叉之后独立追加的历史（不写回父会话）
//	data/fork-ops/<hash>.json                    幂等操作记录：同一个 request_id 只建一次分支
//
// 源会话之后压缩、重置、删除都不会改这份快照；删除分支只删它自己的尾部与元信息，
// 快照靠引用计数回收（GCSnapshots）。

const (
	BranchPreparing       = "preparing"        // 已受理，等安全边界
	BranchPendingBoundary = "pending_boundary" // 同批工具里调用的 Fork：先返回，批次闭合后再做快照
	BranchReady           = "ready"
	BranchQueued          = "queued"
	BranchRunning         = "running"
	BranchPaused          = "paused"
	BranchNeedsAttention  = "needs_attention"
	BranchCompleted       = "completed"
	BranchArchived        = "archived"
	BranchCancelled       = "cancelled"

	BranchDeliverySilent = "silent" // 默认：只写分支记录，不向源聊天推送
	BranchDeliveryReport = "report" // 用户明确要求时才带回结果
)

// BranchMeta 是一个分支会话的来源、边界与状态。
type BranchMeta struct {
	OriginRunID     string `json:"origin_run_id,omitempty"`
	StartRequested  bool   `json:"start_requested,omitempty"`
	InheritedEnd    int    `json:"inherited_end,omitempty"`
	InheritedEndSet bool   `json:"inherited_end_set,omitempty"`
	SID             string `json:"sid"`
	ParentSID       string `json:"parent_sid"`
	RootSID         string `json:"root_sid,omitempty"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	OwnerAgent      string `json:"owner_agent,omitempty"`
	CreatedBy       string `json:"created_by,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	ReadyAt         int64  `json:"ready_at,omitempty"`
	Title           string `json:"title,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Status          string `json:"status"`
	Delivery        string `json:"delivery_policy"`
	// Checkpoint 是分叉点在源会话里的绝对位置（含已经被压缩掉的）
	Checkpoint int    `json:"fork_checkpoint"`
	Generation int64  `json:"generation,omitempty"`
	LastActive int64  `json:"last_active,omitempty"`
	Note       string `json:"note,omitempty"`
}

// SnapshotInfo 是一份不可变快照的说明（context.json）。
type SnapshotInfo struct {
	Stats      HistoryStats   `json:"stats"`
	SnapshotID string         `json:"snapshot_id"`
	SourceSID  string         `json:"source_sid"`
	Messages   int            `json:"messages"`  // 快照里的总条数（含已压缩前缀）
	Base       int            `json:"base"`      // 当时已经压缩掉多少条
	Effective  int            `json:"effective"` // 有效窗口条数 = Messages - Base
	Omitted    map[int]string `json:"omitted,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Generation int64          `json:"generation,omitempty"`
	CreatedAt  int64          `json:"created_at"`
	Bytes      int64          `json:"bytes"`
	// Settings 是分叉时适用的会话设置（模型、思考、agent、工作目录、权限模式、标题）
	Settings SnapshotSettings `json:"settings"`
}

type HistoryStats struct {
	Messages int       `json:"messages"`
	LastAt   int64     `json:"last_at"`
	Usage    llm.Usage `json:"usage"`
}

// SnapshotSettings 是快照里保存的"适用设置"：分支继承这些，但不继承运行态。
type SnapshotSettings struct {
	Persona       string   `json:"persona,omitempty"`
	WorkspacesSet bool     `json:"workspaces_set,omitempty"`
	Group         bool     `json:"group,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	Model         string   `json:"model,omitempty"`
	Thinking      string   `json:"thinking,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	Workspaces    []string `json:"workspaces,omitempty"`
	Title         string   `json:"title,omitempty"`
}

// ForkOp 是一次 Fork 操作的持久记录，用来保证重试和重启幂等。
type ForkOp struct {
	Version          int    `json:"version,omitempty"`
	TargetGeneration int64  `json:"target_generation,omitempty"`
	TargetRevision   int64  `json:"target_revision,omitempty"`
	Stage            string `json:"stage,omitempty"`
	SnapshotID       string `json:"snapshot_id,omitempty"`
	OperationID      string `json:"operation_id"`
	RequestID        string `json:"request_id"`
	ParentSID        string `json:"parent_sid"`
	BranchSID        string `json:"branch_sid"`
	CreatedAt        int64  `json:"created_at"`
}

func (s *Store) snapshotDir(id string) string {
	return filepath.Join(s.dir, "session-snapshots", id)
}

func (s *Store) branchFile(sid string) string {
	return s.newPath(sid, ".branch.json")
}

// legacyBranchFile 是旧布局里的分支元数据（只读兼容）。
func (s *Store) legacyBranchFile(sid string) string {
	return filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".branch.json")
}

func (s *Store) opFile(requestID string) string {
	sum := sha1.Sum([]byte(requestID))
	return filepath.Join(s.dir, "fork-ops", hex.EncodeToString(sum[:])[:16]+".json")
}

// ForkOperationID 是一次操作对外暴露的稳定 ID（同一个 request_id 永远一样）。
func ForkOperationID(requestID string) string {
	sum := sha1.Sum([]byte(requestID))
	return "op-" + hex.EncodeToString(sum[:])[:16]
}

// ---- 操作记录 ----

// BeginOp 持久记下一个 Fork 操作（先于任何副作用）。同一个 request_id 返回同一个操作。
func (s *Store) BeginOp(op ForkOp) (ForkOp, error) {
	if op.RequestID == "" {
		return ForkOp{}, errors.New("Fork 操作缺少 request_id")
	}
	if old, ok := s.OpByRequest(op.RequestID); ok {
		return old, nil
	}
	if op.OperationID == "" {
		op.OperationID = ForkOperationID(op.RequestID)
	}
	if op.CreatedAt == 0 {
		op.CreatedAt = time.Now().Unix()
	}
	b, _ := json.MarshalIndent(op, "", "  ")
	if err := writeFileAtomic(s.opFile(op.RequestID), b); err != nil {
		return ForkOp{}, err
	}
	return op, nil
}

// UpdateOp 更新操作记录（换分支号、改状态）。
func (s *Store) UpdateOp(op ForkOp) error {
	b, _ := json.MarshalIndent(op, "", "  ")
	return writeFileAtomic(s.opFile(op.RequestID), b)
}

// OpByRequest 查一个 request_id 是不是已经建过操作。
func (s *Store) OpByRequest(requestID string) (ForkOp, bool) {
	if requestID == "" {
		return ForkOp{}, false
	}
	b, err := os.ReadFile(s.opFile(requestID))
	if err != nil {
		return ForkOp{}, false
	}
	var op ForkOp
	if json.Unmarshal(b, &op) != nil {
		return ForkOp{}, false
	}
	return op, true
}

// Ops 列出所有操作记录（诊断和恢复用）。
func (s *Store) Ops() []ForkOp {
	entries, err := os.ReadDir(filepath.Join(s.dir, "fork-ops"))
	if err != nil {
		return nil
	}
	var out []ForkOp
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, "fork-ops", e.Name()))
		if err != nil {
			continue
		}
		var op ForkOp
		if json.Unmarshal(b, &op) == nil && op.OperationID != "" {
			out = append(out, op)
		}
	}
	return out
}

// ---- 快照 ----

// WriteSnapshot 把源会话到当前边界的完整已提交历史写成不可变快照。
//
// 流式复制：按行读、按行写，不在内存里攒整份历史；同一份内容复用同一个 snapshot_id，
// 多个分支共用一份快照。contentHash 由调用方给出（源文件与设置一起算），
// 内容相同就直接复用，不重复写盘。
func (s *Store) WriteSnapshot(srcSID, snapshotID string, src Settings) (SnapshotInfo, error) {
	lease, err := s.FreezeSnapshot(srcSID, src)
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer lease.Close()
	id, err := lease.Fingerprint()
	if err != nil {
		return SnapshotInfo{}, err
	}
	if id != snapshotID {
		return SnapshotInfo{}, errors.New("快照版本在冻结边界之后变化")
	}
	return lease.Write()
}
func (s *Store) SnapshotIDFor(srcSID string, st Settings) string {
	lease, err := s.FreezeSnapshot(srcSID, st)
	if err != nil {
		return ""
	}
	defer lease.Close()
	id, err := lease.Fingerprint()
	if err != nil {
		return ""
	}
	return id
}

// Snapshot 读一份快照的说明。
func (s *Store) Snapshot(id string) (SnapshotInfo, bool) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return SnapshotInfo{}, false
	}
	b, err := os.ReadFile(filepath.Join(s.snapshotDir(id), "context.json"))
	if err != nil {
		return SnapshotInfo{}, false
	}
	var info SnapshotInfo
	if json.Unmarshal(b, &info) != nil {
		return SnapshotInfo{}, false
	}
	return info, true
}

// SnapshotMessages 读快照里分叉点的有效窗口（Base 之后的部分）。
// 结果进一个小的只读缓存，超出预算就整体清掉——空闲分支不会各留一份。
func (s *Store) SnapshotMessages(id string) []Msg {
	if id == "" {
		return nil
	}
	info, ok := s.Snapshot(id)
	if !ok {
		return nil
	}
	s.snapCacheMu.Lock()
	if h, ok := s.snapCache[id]; ok {
		s.snapCacheMu.Unlock()
		return h
	}
	s.snapCacheMu.Unlock()

	all := readMsgs(filepath.Join(s.snapshotDir(id), "messages.jsonl"), info.Base)
	cost := int64(cap(all)) * int64(unsafe.Sizeof(Msg{}))
	for _, m := range all {
		cost += int64(len(m.Content) + len(m.Reasoning) + len(m.Role) + len(m.ToolCallID) + len(m.Agent) + len(m.Kind) + len(m.ParentRun) + len(m.SourceID) + len(m.Sender) + len(m.ID) + len(m.AgendaID) + len(m.Reason))
		for _, id := range m.SourceIDs {
			cost += int64(unsafe.Sizeof(id)) + int64(len(id))
		}
		for _, tc := range m.ToolCalls {
			cost += int64(unsafe.Sizeof(tc)) + int64(len(tc.ID)+len(tc.Type)+len(tc.Function.Name)+len(tc.Function.Arguments))
		}
		for _, at := range m.Images {
			cost += int64(unsafe.Sizeof(at)) + int64(len(at.ID)+len(at.Name)+len(at.MIME)+len(at.Source))
		}
		for _, at := range m.Sent {
			cost += int64(unsafe.Sizeof(at)) + int64(len(at.ID)+len(at.Name)+len(at.MIME)+len(at.Source))
		}
		if m.Usage != nil {
			cost += int64(unsafe.Sizeof(*m.Usage))
		}
	}
	s.snapCacheMu.Lock()
	if s.snapCache == nil {
		s.snapCache = map[string][]Msg{}
	}
	if s.snapBytes == nil {
		s.snapBytes = map[string]int64{}
	}
	s.snapCache[id], s.snapBytes[id] = all, cost
	// 预算：超出就清掉（简单有界，不追求命中率）
	var total int64
	for _, v := range s.snapBytes {
		total += v
	}
	if total > s.snapBudget {
		s.snapCache, s.snapBytes = map[string][]Msg{}, map[string]int64{}
	}
	s.snapCacheMu.Unlock()
	return all
}

// SetSnapshotBudget 设置快照只读缓存的上限（字节）。0 表示用默认值。
func (s *Store) SetSnapshotBudget(n int64) {
	if n <= 0 {
		n = 8 << 20
	}
	s.snapCacheMu.Lock()
	s.snapBudget = n
	s.snapCacheMu.Unlock()
}

// snapshotBudget 读出预算（测试与诊断用）。
func (s *Store) snapshotBudget() int64 {
	s.snapCacheMu.Lock()
	defer s.snapCacheMu.Unlock()
	if s.snapBudget <= 0 {
		return 8 << 20
	}
	return s.snapBudget
}

// UnloadHistory 丢掉某个会话在内存里的历史（空闲分支用，下次访问再从磁盘加载）。
func (s *Store) UnloadHistory(sid string) {
	delete(s.hist, sid)
	delete(s.settings, sid)
}

// CacheUsage is a lightweight diagnostic; call under the Store owner's lock.
func (s *Store) CacheUsage() (histories, snapshots int, bytes int64) {
	histories = len(s.hist)
	s.snapCacheMu.Lock()
	defer s.snapCacheMu.Unlock()
	snapshots = len(s.snapCache)
	for _, n := range s.snapBytes {
		bytes += n
	}
	return
}

// ---- 分支 ----

// BeginBranch 先记下分支元信息（不写历史）：受理之后进程挂了也留得下痕迹。
func (s *Store) BeginBranch(meta BranchMeta) error {
	if meta.SID == "" || meta.ParentSID == "" {
		return errors.New("分支缺少会话 ID")
	}
	if _, ok := s.Branch(meta.SID); ok {
		return errors.New("这个分支已经存在")
	}
	if meta.Status == "" {
		meta.Status = BranchPreparing
	}
	if meta.Delivery == "" {
		meta.Delivery = BranchDeliverySilent
	}
	if meta.CreatedAt == 0 {
		meta.CreatedAt = time.Now().Unix()
	}
	if meta.RootSID == "" {
		if parent, ok := s.Branch(meta.ParentSID); ok {
			meta.RootSID = parent.RootSID
		} else {
			meta.RootSID = meta.ParentSID
		}
	}
	if err := s.saveBranch(meta); err != nil {
		return err
	}
	return nil
}

// ReadyBranch 到安全边界后把分支连到快照上并发布 ready。历史是"快照 + 自己的空尾部"，
// 不从源会话复制一份进来。
func (s *Store) ReadyBranch(sid string, snap SnapshotInfo, status string) error {
	meta, ok := s.Branch(sid)
	if !ok {
		return errors.New("没有这个分支")
	}
	meta.SnapshotID = snap.SnapshotID
	meta.Checkpoint = snap.Base + snap.Effective
	meta.ReadyAt = time.Now().Unix()
	if status == "" {
		status = BranchReady
	}
	meta.Status = status
	// Settings must be durable before ready is visible.
	st := Settings{Agent: meta.OwnerAgent, Model: snap.Settings.Model, Mode: snap.Settings.Mode}
	if st.Agent == "" {
		st.Agent = snap.Settings.Agent
	}
	st.Thinking = snap.Settings.Thinking
	if snap.Settings.Mode != "" {
		st.Mode = snap.Settings.Mode
	}
	st.Workspaces = append([]string(nil), snap.Settings.Workspaces...)
	st.WorkspacesSet = snap.Settings.WorkspacesSet
	st.Title = meta.Title
	st.Prefix = nil
	st.Meter = Meter{}
	st.EnvNote, st.MemNote, st.LifeNote, st.Handoff = "", "", "", ""
	st.Recalled = nil
	st.Omitted = snap.Omitted
	st.Summary = snap.Summary
	st.Base = snap.Base // 快照里的有效窗口从 Base 之后开始
	st.Group, st.Web = snap.Settings.Group, false
	if err := s.SaveSettings(sid, st); err != nil {
		return err
	}
	meta.InheritedEnd, meta.InheritedEndSet = snap.Messages, true
	if err := s.saveBranch(meta); err != nil {
		return err
	}
	delete(s.hist, sid)
	return nil
}

func (s *Store) saveBranch(meta BranchMeta) error {
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := writeFileAtomic(s.branchFile(meta.SID), b); err != nil {
		return err
	}
	s.branches[meta.SID] = meta
	return nil
}

// Branch 查一个会话是不是分支。
func (s *Store) Branch(sid string) (BranchMeta, bool) {
	if sid == "" || s.isDead(sid) {
		return BranchMeta{}, false // 删除墓碑：旧元数据不再自动授权
	}
	if s.branches == nil {
		s.loadBranches()
	}
	m, ok := s.branches[sid]
	if !ok || s.isDead(m.SID) {
		return BranchMeta{}, false
	}
	return m, true
}

// Branches 是所有分支，按创建时间排序。
func (s *Store) Branches() []BranchMeta {
	if s.branches == nil {
		s.loadBranches()
	}
	out := make([]BranchMeta, 0, len(s.branches))
	for _, m := range s.branches {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].SID < out[j].SID
	})
	return out
}

// Children 是某个会话直接分出来的分支。
func (s *Store) Children(parent string) []BranchMeta {
	var out []BranchMeta
	for _, m := range s.Branches() {
		if m.ParentSID == parent {
			out = append(out, m)
		}
	}
	return out
}

// SetBranch 改分支元信息。
func (s *Store) SetBranch(sid string, edit func(*BranchMeta)) error {
	m, ok := s.Branch(sid)
	if !ok {
		return errors.New("没有这个分支")
	}
	edit(&m)
	return s.saveBranch(m)
}

// DeleteBranch 删除分支自己的尾部、设置和元信息；快照留给其他引用。
func (s *Store) DeleteBranch(sid string) error {
	if _, ok := s.Branch(sid); !ok {
		return errors.New("没有这个分支")
	}
	delete(s.branches, sid)
	delete(s.hist, sid)
	delete(s.settings, sid)
	_ = os.Remove(s.branchFile(sid))
	_ = os.Remove(s.file(sid))
	_ = os.Remove(s.settingsFile(sid))
	return nil
}

// GCSnapshots 回收没有分支引用的快照。返回删掉的快照 ID。
func (s *Store) GCSnapshots() []string {
	used := map[string]bool{}
	for _, m := range s.Branches() {
		if m.SnapshotID != "" {
			used[m.SnapshotID] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "session-snapshots"))
	if err != nil {
		return nil
	}
	var gone []string
	for _, e := range entries {
		if !e.IsDir() || used[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.snapshotDir(e.Name()))); err == nil {
			gone = append(gone, e.Name())
		}
	}
	if len(gone) > 0 {
		s.snapCacheMu.Lock()
		for _, id := range gone {
			delete(s.snapCache, id)
			delete(s.snapBytes, id)
		}
		s.snapCacheMu.Unlock()
	}
	return gone
}

// SnapshotRefs 是快照被多少分支引用（诊断和回收对账用）。
func (s *Store) SnapshotRefs(id string) int {
	n := 0
	for _, m := range s.Branches() {
		if m.SnapshotID == id {
			n++
		}
	}
	return n
}

func (s *Store) loadBranches() {
	s.branches = map[string]BranchMeta{}
	sides := []struct {
		dir     string
		resolve func(string) (string, bool)
	}{
		{s.dirV1Sessions(), s.resolveNewKey},
		{s.dirLegacySessions(), resolveLegacyName},
	}
	for _, side := range sides {
		for _, e := range mustReadDir(side.dir) {
			name := e.Name()
			if !strings.HasSuffix(name, ".branch.json") {
				continue
			}
			key := strings.TrimSuffix(name, ".branch.json")
			sid, ok := side.resolve(key)
			if !ok {
				continue
			}
			b, err := os.ReadFile(filepath.Join(side.dir, name))
			if err != nil {
				continue
			}
			var m BranchMeta
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			if m.SID == "" {
				m.SID = sid
			}
			if s.isDead(m.SID) || (side.dir == s.dirLegacySessions() && s.legacyDead(m.SID)) {
				continue // 删除墓碑 / 已开新一代：旧根备份永久失去自动授权
			}
			if _, dup := s.branches[m.SID]; dup {
				continue // 新布局优先
			}
			s.branches[m.SID] = m
		}
	}
}

// loadBranch 是分支的有效历史：快照里分叉点的有效窗口 + 自己的尾部。
// skip 是已经压缩掉的条数（Settings.Base），跨这两段一起数。
func (s *Store) loadBranch(br BranchMeta, skip int) []Msg {
	info, ok := s.Snapshot(br.SnapshotID)
	if !ok {
		return nil
	}
	snap := s.SnapshotMessages(br.SnapshotID)
	end := info.Messages
	if br.InheritedEndSet {
		end = min(end, br.InheritedEnd)
	}
	keep := max(0, end-info.Base)
	if keep < len(snap) {
		snap = snap[:keep]
	}
	offset := max(0, skip-info.Base)
	out := make([]Msg, 0, len(snap)+16)
	if offset < len(snap) {
		out = append(out, snap[offset:]...)
	}
	return append(out, s.loadFrom(br.SID, max(0, skip-end))...)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
