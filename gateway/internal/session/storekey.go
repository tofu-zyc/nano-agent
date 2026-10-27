package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
	"strings"
)

// ---- 会话在磁盘上的键（W1） ----
//
// 逻辑 sid 保持原值（"tg:123"、"@bot/main"、"中文"），只改**文件名键**。
// 旧实现用 url.PathEscape 当文件名：它在 Windows 上会留下 %3A 之类的字面名字，
// 而且对大小写、保留字符、末尾点/空格、设备名都没有保证。
//
// 新键是带版本前缀的规范 ASCII：
//
//	v1-<十六进制>（原始 sid 的 UTF-8 字节）
//	v1h-<sha256 前 32 位>（原始 sid 太长、超出文件名上限时）
//
// 十六进制不含大小写歧义、不含路径分隔符和保留字符，反解是唯一确定的；
// 太长的 ID 用稳定哈希，并在 sessions/keys/<hash>.json 里持久保存原始 sid，
// 绝不截断 ID。旧键（url.PathEscape）只用于读取兼容和迁移，不再用于新写入。

const (
	keyPrefix    = "v1-"
	keyLongPrefix = "v1h-"
	// keyMaxBase 是“仍然用可逆编码”的最长原始 sid（字节）。超过就走哈希 + 映射。
	// NTFS 文件名上限 255 个 UTF-16 码元，减去后缀（".settings.json" 等）留足余量。
	keyMaxBase = 96
)

// Key 是会话 ID 的规范文件系统键。
func Key(sid string) string {
	if len(sid) <= keyMaxBase {
		return keyPrefix + hex.EncodeToString([]byte(sid))
	}
	sum := sha256.Sum256([]byte(sid))
	return keyLongPrefix + hex.EncodeToString(sum[:16])
}

// ParseKey 把文件名键还原成原始 sid。第二个返回值表示是不是规范键。
func ParseKey(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, keyLongPrefix):
		return "", false // 需要查持久映射（LookupLongKey）
	case strings.HasPrefix(name, keyPrefix):
		raw, err := hex.DecodeString(strings.TrimPrefix(name, keyPrefix))
		if err != nil || len(raw) == 0 {
			return "", false
		}
		return string(raw), true
	}
	return "", false
}

// LegacyKey 是旧实现用的文件名（url.PathEscape）。只读兼容，不再用于新写入。
func LegacyKey(sid string) string {
	if sid == "." || sid == ".." {
		return "%2E" + url.PathEscape(sid[1:])
	}
	return url.PathEscape(sid)
}

// keysFor 返回一个 sid 所有可能出现的磁盘键：新键在前，旧键在后（兼容读取）。
func keysFor(sid string) []string {
	k := Key(sid)
	if k == LegacyKey(sid) {
		return []string{k}
	}
	return []string{k, LegacyKey(sid)}
}

// keyMapFile 是超长 sid 的哈希 → 原始 sid 映射。
func (s *Store) keyMapFile(key string) string {
	return filepath.Join(s.dir, "sessions", "keys", key+".json")
}

// rememberLongKey 记下超长 sid 的原始值（幂等）。
func (s *Store) rememberLongKey(sid string) error {
	if len(sid) <= keyMaxBase {
		return nil
	}
	key := Key(sid)
	path := s.keyMapFile(key)
	if b, err := os.ReadFile(path); err == nil {
		var old struct {
			SID string `json:"sid"`
		}
		if json.Unmarshal(b, &old) == nil && old.SID == sid {
			return nil
		}
	}
	b, _ := json.MarshalIndent(map[string]string{"sid": sid, "key": key}, "", "  ")
	return writeFileAtomic(path, b)
}

// LookupLongKey 用哈希键查回原始 sid。
func (s *Store) LookupLongKey(key string) (string, bool) {
	b, err := os.ReadFile(s.keyMapFile(key))
	if err != nil {
		return "", false
	}
	var v struct {
		SID string `json:"sid"`
	}
	if json.Unmarshal(b, &v) != nil || v.SID == "" || Key(v.SID) != key {
		return "", false
	}
	return v.SID, true
}

// resolveKey 把磁盘上的键还原成 sid：先按规范键解，再查超长映射，最后按旧键解。
func (s *Store) resolveKey(name string) (string, bool) {
	if sid, ok := ParseKey(name); ok {
		return sid, true
	}
	if strings.HasPrefix(name, keyLongPrefix) {
		return s.LookupLongKey(name)
	}
	sid, err := url.PathUnescape(name)
	if err != nil || sid == "" {
		return "", false
	}
	return sid, true
}

// ---- 迁移 ----

// MigrationRecord 是一次键迁移的结果（持久记录，中断后接着做同一项）。
type MigrationRecord struct {
	Version int      `json:"version"`
	At      int64    `json:"at"`
	Moved   []string `json:"moved"`   // 已经复制到新键的 sid
	Failed  []string `json:"failed,omitempty"`
	Kept    []string `json:"kept,omitempty"` // 无法确定归属、保留现场的项目
}

// MigrationKind 是一个要迁移的“流”：目录 + 后缀。
type migrationKind struct {
	dir    string
	suffix string // 例如 ".jsonl"
	check  func([]byte) error
}

// MigrateKeys 把旧的 url.PathEscape 键复制成新的规范键。
//
// 只复制、不删旧文件（保留旧流和备份）；目标已存在时先比对内容，相同就算完成，
// 不同就保留双方并记进 Kept。每一项都先校验内容再提交，失败不影响其他项。
// 整个过程幂等：中断后再跑会跳过已经迁移好的。
func (s *Store) MigrateKeys() (MigrationRecord, error) {
	rec := MigrationRecord{Version: 1, At: time.Now().Unix()}
	kinds := []migrationKind{
		{dir: filepath.Join(s.dir, "sessions"), suffix: ".jsonl", check: jsonlCheck},
		{dir: filepath.Join(s.dir, "sessions"), suffix: ".settings.json", check: jsonCheck},
		{dir: filepath.Join(s.dir, "sessions"), suffix: ".branch.json", check: jsonCheck},
		{dir: filepath.Join(s.dir, "interjections"), suffix: ".jsonl", check: jsonlCheck},
	}
	for _, k := range kinds {
		entries, err := os.ReadDir(k.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), k.suffix) {
				continue
			}
			old := e.Name()
			base := strings.TrimSuffix(old, k.suffix)
			sid, ok := s.resolveKey(base)
			if !ok {
				rec.Kept = append(rec.Kept, filepath.Join(k.dir, old))
				continue
			}
			newKey := Key(sid)
			if base == newKey {
				continue // 已经是新键
			}
			if len(sid) > keyMaxBase {
				if err := s.rememberLongKey(sid); err != nil {
					rec.Failed = append(rec.Failed, filepath.Join(k.dir, newKey+k.suffix))
					continue
				}
			}
			src := filepath.Join(k.dir, old)
			dst := filepath.Join(k.dir, newKey+k.suffix)
			data, err := os.ReadFile(src)
			if err != nil {
				rec.Failed = append(rec.Failed, src)
				continue
			}
			if err := k.check(data); err != nil {
				rec.Kept = append(rec.Kept, src) // 内容看不懂：保留现场，不动
				continue
			}
			if oldData, err := os.ReadFile(dst); err == nil {
				if string(oldData) == string(data) {
					continue // 已经迁移过
				}
				rec.Kept = append(rec.Kept, src) // 两边内容不同：都留着，报告冲突
				continue
			}
			if err := writeFileAtomic(dst, data); err != nil {
				rec.Failed = append(rec.Failed, src)
				continue
			}
			if got, err := os.ReadFile(dst); err != nil || string(got) != string(data) {
				rec.Failed = append(rec.Failed, src)
				continue
			}
			rec.Moved = append(rec.Moved, sid)
		}
	}
	// 归档目录：每个会话一个子目录，同样按旧键命名
	arch := filepath.Join(s.dir, "archive")
	if entries, err := os.ReadDir(arch); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sid, ok := s.resolveKey(e.Name())
			if !ok || e.Name() == Key(sid) {
				continue
			}
			dst := filepath.Join(arch, Key(sid))
			if _, err := os.Stat(dst); err == nil {
				continue // 已迁移（归档目录里的文件本来就可以并存）
			}
			if err := os.MkdirAll(dst, 0o700); err != nil {
				rec.Failed = append(rec.Failed, filepath.Join(arch, e.Name()))
				continue
			}
			files, err := os.ReadDir(filepath.Join(arch, e.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				data, err := os.ReadFile(filepath.Join(arch, e.Name(), f.Name()))
				if err != nil {
					rec.Failed = append(rec.Failed, f.Name())
					continue
				}
				if err := writeFileAtomic(filepath.Join(dst, f.Name()), data); err != nil {
					rec.Failed = append(rec.Failed, f.Name())
				}
			}
			rec.Moved = append(rec.Moved, sid)
		}
	}
	sort.Strings(rec.Moved)
	if b, err := json.MarshalIndent(rec, "", "  "); err == nil {
		_ = writeFileAtomic(filepath.Join(s.dir, "sessions", "keys", "migration.json"), b)
	}
	return rec, nil
}

// HasLegacyKeys 报告磁盘上还有没有旧键文件（启动时用来决定要不要迁移）。
func (s *Store) HasLegacyKeys() bool {
	for _, k := range []struct{ dir, suffix string }{
		{filepath.Join(s.dir, "sessions"), ".settings.json"},
		{filepath.Join(s.dir, "sessions"), ".jsonl"},
		{filepath.Join(s.dir, "interjections"), ".jsonl"},
	} {
		entries, err := os.ReadDir(k.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), k.suffix) {
				continue
			}
			base := strings.TrimSuffix(e.Name(), k.suffix)
			sid, ok := s.resolveKey(base)
			if !ok || base == Key(sid) {
				continue // 解不出来，或者本来就是新键
			}
			// 旧键文件：新键那份已经在了就说明迁移过了
			if _, err := os.Stat(filepath.Join(k.dir, Key(sid)+k.suffix)); err == nil {
				continue
			}
			return true
		}
	}
	return false
}

func jsonCheck(b []byte) error {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	var v any
	return json.Unmarshal(b, &v)
}

func jsonlCheck(b []byte) error {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return err
		}
	}
	return nil
}
