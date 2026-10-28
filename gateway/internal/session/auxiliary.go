package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) MigrateAuxiliary(kind, root, extension string) error {
	names, err := LegacyFiles(root)
	if err != nil {
		return err
	}
	var first error
	for _, name := range names {
		if !strings.HasSuffix(name, extension) || (kind == "persona" && name == "default.md") {
			continue
		}
		sid, err := url.PathUnescape(strings.TrimSuffix(name, extension))
		if err != nil || sid == "" {
			continue
		}
		owner, ownerErr := s.preferredLegacySID(root, sid)
		if ownerErr != nil {
			if first == nil {
				first = ownerErr
			}
			continue
		}
		sid = owner
		if _, _, err := s.AuxiliaryPath(kind, root, extension, sid, false); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Auxiliary files share the session identity/lifecycle authority. A canonical
// filename alone never publishes an interrupted copy; source backups are never
// consulted again after this record has selected a view.
type auxiliaryRecord struct {
	Version     int        `json:"version"`
	SID         string     `json:"sid"`
	Kind        string     `json:"kind"`
	Root        string     `json:"root"`
	Extension   string     `json:"extension"`
	OperationID string     `json:"operation_id"`
	Generation  int64      `json:"generation"`
	Revision    int64      `json:"revision"`
	Stage       string     `json:"stage"`
	Source      string     `json:"source,omitempty"`
	File        renameFile `json:"file"`
}

func AuxiliaryFile(root, sid, extension string) string {
	return filepath.Join(root, "v1", Key(sid)+extension)
}
func (s *Store) SIDForKey(key string) (string, bool) { return s.resolveNewKey(key) }

func (s *Store) AuxiliaryPath(kind, root, extension, sid string, write bool) (string, bool, error) {
	if !((kind == "interjections" && extension == ".jsonl") || (kind == "persona" && extension == ".md")) {
		return "", false, errors.New("未知辅助文件类型")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	target := AuxiliaryFile(root, sid, extension)
	rootRef := s.assetRootRef(root)
	if s.isDead(sid) {
		if write {
			return target, false, errors.New("已删除会话的辅助文件不能由旧入口重新激活")
		}
		return target, false, nil
	}
	if _, _, err := s.locate(sid, ".settings.json"); err != nil {
		return target, false, err
	}
	gen, rev, err := s.LifecycleVersion(sid)
	if err != nil {
		return target, false, err
	}
	manifest := filepath.Join(s.dirV1(), "assets", kind, Key(sid)+".json")
	var rec auxiliaryRecord
	b, readErr := os.ReadFile(manifest)
	has := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return target, false, readErr
	}
	if has {
		if json.Unmarshal(b, &rec) != nil || rec.Version != 1 || rec.SID != sid || rec.Kind != kind || rec.Root != rootRef || rec.Extension != extension || rec.OperationID == "" {
			return target, false, errors.New("辅助文件操作记录损坏、版本或归属不匹配")
		}
		switch rec.Stage {
		case "copying", "active", "superseded", "rename_pending":
		default:
			return target, false, errors.New("辅助文件操作阶段未知")
		}
	}
	save := func() error {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return writeFileAtomic(manifest, b)
	}
	if has && (rec.Generation != gen || ((rec.Stage == "copying" || rec.Stage == "rename_pending") && rec.Revision != rev)) {
		rec.Stage = "superseded"
		if err := save(); err != nil {
			return target, false, err
		}
	}
	if has && rec.Stage == "rename_pending" {
		return target, false, errors.New("辅助文件的改名尚未提交，等待恢复")
	}
	if has && rec.Stage == "active" {
		st, err := os.Stat(target)
		if errors.Is(err, os.ErrNotExist) {
			return target, false, nil
		}
		if err != nil {
			return target, false, err
		}
		if !st.Mode().IsRegular() {
			return target, false, errors.New("辅助文件不是普通文件")
		}
		return target, true, nil
	}
	if has && rec.Stage == "superseded" {
		if !write {
			return target, false, nil
		}
		// An explicit new write may create an empty current view, never import
		// the revoked source again under another operation identity.
		backup := filepath.Join(s.dirV1(), "orphan", "assets", kind, rec.OperationID)
		if _, err := os.Stat(target); err == nil {
			if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
				return target, false, err
			}
			if _, err := os.Stat(backup); err == nil {
				return target, false, errors.New("辅助文件隔离备份已存在，保留双方等待核对")
			} else if !errors.Is(err, os.ErrNotExist) {
				return target, false, err
			}
			if err := os.Rename(target, backup); err != nil {
				return target, false, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return target, false, err
		}
		id, err := newOperationID()
		if err != nil {
			return target, false, err
		}
		rec = auxiliaryRecord{Version: 1, SID: sid, Kind: kind, Root: rootRef, Extension: extension, OperationID: id, Generation: gen, Revision: rev, Stage: "active"}
		if err := save(); err != nil {
			return target, false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return target, false, err
		}
		return target, false, nil
	}
	if !has {
		if err := s.rememberLongKey(sid); err != nil {
			return target, false, err
		}
		if _, err := os.Stat(target); err == nil {
			return target, false, errors.New("发现没有发布记录的辅助文件，保留现场，拒绝普通写入激活")
		} else if !errors.Is(err, os.ErrNotExist) {
			return target, false, err
		}
		id, err := newOperationID()
		if err != nil {
			return target, false, err
		}
		rec = auxiliaryRecord{Version: 1, SID: sid, Kind: kind, Root: rootRef, Extension: extension, OperationID: id, Generation: gen, Revision: rev, Stage: "active"}
		legacy := filepath.Join(root, url.PathEscape(sid)+extension)
		_, statErr := os.Stat(legacy)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return target, false, statErr
		}
		if errors.Is(statErr, os.ErrNotExist) && !write {
			return target, false, nil
		}
		eligibilityErr := s.checkPromotionEligibility(sid)
		if eligibilityErr != nil && !errors.Is(eligibilityErr, errLegacyUnauthorized) {
			return target, false, eligibilityErr
		}
		eligible := gen == 0 && eligibilityErr == nil && !(kind == "persona" && sid == "default")
		if statErr == nil && eligible {
			if err := s.claimLegacySID(root, sid); err != nil {
				return target, false, err
			}
			check := jsonlCheckReader
			if kind == "persona" {
				check = nil
			}
			var meta renameFile
			if check != nil {
				meta, err = checkedFileVersion(legacy, check)
			} else {
				meta.Len, meta.Sum, err = hashFile(legacy)
			}
			if err != nil {
				return target, false, err
			}
			rec.Source, rec.File, rec.Stage = "legacy:"+filepath.Base(legacy), meta, "copying"
		}
		if err := save(); err != nil {
			return target, false, err
		}
	}
	if rec.Stage == "copying" {
		source := rec.Source
		if strings.HasPrefix(source, "legacy:") {
			name := strings.TrimPrefix(source, "legacy:")
			if filepath.Base(name) != name || name == "." || name == ".." {
				return target, false, errors.New("辅助文件来源不合法")
			}
			source = filepath.Join(root, name)
		}
		if ok, err := versionMatches(source, rec.File); err != nil || !ok {
			return target, false, fmt.Errorf("辅助文件来源变化，保留迁移现场：%v", err)
		}
		// ADS sources must also have a visible, immutable backup before the
		// writable canonical view can be published. Ordinary legacy files use
		// the same bounded copy/verification path.
		backup, err := s.retainLegacyBackup(kind, rec.OperationID, "content"+extension, source, rec.File)
		if err != nil {
			return target, false, err
		}

		if _, err := os.Stat(target); err == nil {
			if ok, err := versionMatches(target, rec.File); err != nil || !ok {
				return target, false, errors.New("辅助文件迁移目标冲突，保留双方")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			copied, err := copyFileVersion(backup, target)
			if err != nil {
				return target, false, err
			}
			if !sameVersion(copied, rec.File) {
				return target, false, errors.New("辅助文件复制期间来源变化")
			}
		} else {
			return target, false, err
		}
		if ok, err := versionMatches(target, rec.File); err != nil || !ok {
			return target, false, errors.New("辅助文件副本校验失败")
		}
		rec.Stage = "active"
		if err := save(); err != nil {
			return target, false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return target, false, err
	}
	_, err = os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return target, false, nil
	}
	return target, err == nil, err
}

// Relative roots keep the same authority when a complete installation is moved
// between Linux and Windows. Cross-volume configurations retain an absolute
// reference and fail visibly if the configured root no longer matches.
func (s *Store) assetRootRef(root string) string {
	data, _ := filepath.Abs(s.dir)
	root, _ = filepath.Abs(root)
	rel, err := filepath.Rel(data, root)
	if err != nil {
		return root
	}
	return "@data/" + filepath.ToSlash(rel)
}
func (s *Store) assetRoot(ref string) string {
	if strings.HasPrefix(ref, "@data/") {
		return filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(ref, "@data/")))
	}
	return ref
}
