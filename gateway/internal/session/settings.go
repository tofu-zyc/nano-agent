package session

import (
	"encoding/json"
	"nano-agent/gateway/internal/llm"
	"os"
	"path/filepath"
	"strings"
)

// Settings 是单个会话覆盖默认值的设置，存在 data/sessions/<会话ID>.settings.json。
type Prefix struct {
	System string     `json:"system"`
	Tools  []llm.Tool `json:"tools"`
	Ver    int        `json:"ver,omitempty"`  // 网关给的工具变了（升级加了新工具）时加一，旧快照作废一次
	Perm   string     `json:"perm,omitempty"` // 权限信号（管理员/访客、有没有工具、哪台内核、内核在不在）：变了就换工具快照
}

// AgentSegment 是会话交给某个 agent 的一段：Position 是这一段开始的消息位置（绝对位置，含已经被整理掉的）。
type AgentSegment struct {
	Agent    string `json:"agent"`
	At       int64  `json:"at,omitempty"`
	Position int    `json:"position"`
}

type Settings struct {
	SnapshotPersona string         `json:"snapshot_persona,omitempty"`
	Prefix          *Prefix        `json:"prefix,omitempty"`
	Omitted         map[int]string `json:"omitted,omitempty"`
	// Workspaces 是挂载的工作目录（工作目录列表里的 ID，app/workspaces.go），可以有好几个、在不同的机器上。
	// WorkspacesSet 为 false 时用新会话的默认工作目录，第一次说话时定下来；为 true 而列表为空表示只聊天。
	Workspaces    []string `json:"workspaces,omitempty"`
	WorkspacesSet bool     `json:"workspaces_set,omitempty"`
	// 旧版本：只能挂一个工作目录（启动时迁移到 Workspaces）
	Workspace    string `json:"workspace,omitempty"`
	WorkspaceSet bool   `json:"workspace_set,omitempty"`
	Mode         string `json:"mode,omitempty"` // 空表示用配置里的默认模式

	// 旧版本的设置（机器和目录分开设），启动时迁移到工作目录列表后清掉
	Node    string   `json:"node,omitempty"`
	DirsSet bool     `json:"dirs_set,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
	RO      []string `json:"ro,omitempty"`

	Thinking string `json:"thinking,omitempty"` // 思考强度，空表示用模型的默认值
	Model    string `json:"model,omitempty"`    // 用哪个模型（models 包里的 ID），空表示用默认模型
	Title    string `json:"title,omitempty"`    // 会话的名字，网页里显示

	// Agent 是这个会话当前用哪个 agent 的人格、模型、工具和限制（网页里切换）。
	// 空表示按会话 ID 的前缀推断（@<agent>/… 属于那个 agent，其他属于默认 agent）——旧数据不用迁移。
	// 注意：它只决定“谁来执行”，不改变会话 ID 的路由命名空间（机器人窗口接到哪个会话仍按 ID 前缀）。
	Agent string `json:"agent,omitempty"`
	// AgentSegments 记下切换历史：从哪条消息（绝对位置，含被整理的）开始是哪个 agent 在说话。
	// 消息自己的 Agent 字段优先；没有字段的老消息靠它判断来源（dream 不能把旧 agent 的自述当成新 agent 的亲历）。
	AgentSegments []AgentSegment `json:"agent_segments,omitempty"`
	// Handoff 是切换后还没告诉新 agent 的交接说明，下一条用户消息前放一次就清掉（像 EnvNote）。
	Handoff string `json:"handoff,omitempty"`
	Group   bool   `json:"group,omitempty"` // 这是某个群聊里的会话：不挂工作目录、只发正文、用群聊的工具
	Web     bool   `json:"web,omitempty"`   // 网页接着这个会话：在网页里新建的，或者在网页里说过话

	// EnvNote 是上一次告诉模型的环境说明。环境变了才在下一条用户消息前追加新的说明，
	// 不去改系统提示词，前缀缓存就不会失效。
	EnvNote string `json:"env_note,omitempty"`
	// SessionGeneration 是会话的“世代”：/reset、删除之后加一。插话日志用它作废旧记录，
	// 重启后不能把重置前的补充放回来。
	SessionGeneration int64 `json:"session_generation,omitempty"`

	// MemNote 是上一次放进对话的记忆说明的哈希（app/memory.go）：记忆变了才在下一条用户消息前再放一次。
	// Recalled 是这段上下文里已经“想起来”过的记忆，同一条不重复放。压缩、清空上下文后都清掉。
	MemNote  string   `json:"mem_note,omitempty"`
	LifeNote string   `json:"life_note,omitempty"` // 拟人型：上一次放的近况说明的哈希
	Recalled []string `json:"recalled,omitempty"`

	// Members 是群聊里说过话的人：名字 → 身份（整理记忆时用来把群友的画像和身份对上）。
	Members map[string]string `json:"members,omitempty"`

	// Base 是历史文件里已经被 Compact 丢掉的消息数；Summary 代替被丢掉的部分，放在历史最前面。
	Meter       Meter  `json:"meter,omitempty"`
	CompactedAt int64  `json:"compacted_at,omitempty"`
	Base        int    `json:"base,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

func (s *Store) settingsFile(sid string) string {
	return s.newPath(sid, ".settings.json")
}

// settingsReadFile 读设置时兼容旧键。
func (s *Store) settingsReadFile(sid string) (string, bool) {
	return s.readFileFor(sid, ".settings.json")
}

// Settings 返回会话设置（第一次访问时从磁盘加载）。
func (s *Store) Settings(sid string) Settings {
	if st, ok := s.settings[sid]; ok {
		return st
	}
	var st Settings
	if p, ok := s.readFileFor(sid, ".settings.json"); ok {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &st)
		}
	}
	s.settings[sid] = st
	return st
}

// SaveSettings 保存会话设置。
// SaveSettings 保存会话设置。已删除的会话同样要先开新一代，不能靠保存设置清掉墓碑。
func (s *Store) SaveSettings(sid string, st Settings) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := s.startNewGeneration(sid); err != nil {
		return err
	}
	path, _, err := s.locate(sid, ".settings.json")
	if err != nil {
		return err
	}
	if err := s.rememberLongKey(sid); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := s.ensureActive(sid); err != nil {
		return err
	}
	s.settings[sid] = st
	return nil
}

// List 返回磁盘上所有会话的 ID（有历史或者有设置的）。
func (s *Store) List() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(sid string) {
		if sid == "" || seen[sid] {
			return
		}
		// 墓碑：只作为恢复来源，不是活动会话
		if _, err := os.Stat(s.deletedMarker(sid)); err == nil {
			return
		}
		seen[sid] = true
		out = append(out, sid)
	}
	for _, side := range []struct {
		dir     string
		resolve func(string) (string, bool)
	}{
		{s.dirV1Sessions(), s.resolveNewKey},
		{s.dirLegacySessions(), resolveLegacyName},
	} {
		names := []string{}
		if side.dir == s.dirLegacySessions() {
			names, _ = LegacyFiles(side.dir)
		} else {
			for _, e := range mustReadDir(side.dir) {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
		}
		for _, name := range names {
			for _, suffix := range []string{".settings.json", ".branch.json", ".jsonl"} {
				if strings.HasSuffix(name, suffix) {
					if sid, ok := side.resolve(strings.TrimSuffix(name, suffix)); ok {
						if side.dir == s.dirLegacySessions() {
							owner, err := s.preferredLegacySID(side.dir, sid)
							if err != nil {
								s.RecordStorageIssue(sid, err)
								continue
							}
							sid = owner
						}
						add(sid)
					}
					break
				}
			}
		}
	}
	for _, e := range mustReadDir(s.dirLegacyArchive()) {
		if !e.IsDir() {
			continue
		}
		if sid, ok := resolveLegacyArchiveName(e.Name()); ok {
			add(sid)
		}
	}
	for _, e := range mustReadDir(s.dirV1Archive()) {
		if !e.IsDir() {
			continue
		}
		if sid, ok := s.resolveNewKey(e.Name()); ok {
			add(sid)
		}
	}
	for _, kind := range []string{"persona", "interjections"} {
		for _, e := range mustReadDir(filepath.Join(s.dirV1(), "assets", kind)) {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			sid, ok := s.resolveNewKey(strings.TrimSuffix(e.Name(), ".json"))
			if !ok || s.isDead(sid) {
				continue
			}
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
				add(sid)
			}
		}
	}
	return out
}
