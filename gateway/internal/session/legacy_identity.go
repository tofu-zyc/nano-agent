package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NTFS legacy names are case-insensitive, logical IDs are not. Persist the
// bundle's original owner before copying; another logical ID may create a
// clean canonical view, but cannot claim the same old body through an alias.
func (s *Store) legacyBundle(root, sid string) string {
	return s.assetRootRef(root) + "/" + LegacyKey(sid)
}
func foldLegacy(bundle string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(bundle)
	}
	return bundle
}
func (s *Store) legacyOwnerFile(root, sid string) string {
	h := sha256.Sum256([]byte(foldLegacy(s.legacyBundle(root, sid))))
	return filepath.Join(s.dirV1(), "legacy-ownership", hex.EncodeToString(h[:])+".json")
}

type legacyIdentity struct {
	Version int    `json:"version"`
	SID     string `json:"sid"`
	Bundle  string `json:"bundle"`
}

func (s *Store) legacyOwner(root, sid string) (string, error) {
	paths := []string{s.legacyOwnerFile(root, sid)}
	if runtime.GOOS == "windows" {
		entries, err := os.ReadDir(filepath.Join(s.dirV1(), "legacy-ownership"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if len(entries) > 4096 {
			return "", errors.New("旧来源身份记录超过盘点上限")
		}
		paths = nil
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				paths = append(paths, filepath.Join(s.dirV1(), "legacy-ownership", entry.Name()))
			}
		}
	}
	owner := ""
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var rec legacyIdentity
		if json.Unmarshal(b, &rec) != nil || rec.Version != 1 || rec.SID == "" || rec.Bundle == "" {
			return "", errors.New("旧来源身份记录损坏")
		}
		if foldLegacy(rec.Bundle) != foldLegacy(s.legacyBundle(root, sid)) {
			continue
		}
		if owner != "" && owner != rec.SID {
			return "", fmt.Errorf("旧来源存在大小写冲突，保留双方：%s / %s", owner, rec.SID)
		}
		owner = rec.SID
	}
	return owner, nil
}
func (s *Store) preferredLegacySID(root, sid string) (string, error) {
	if owner, err := s.legacyOwner(root, sid); err != nil || owner != "" {
		return owner, err
	}
	if runtime.GOOS != "windows" {
		return sid, nil
	}
	// Existing endpoint bindings are stronger evidence than the spelling of a
	// case-folded base filename used by several alternate streams.
	b, err := os.ReadFile(filepath.Join(s.dir, "endpoints.json"))
	if errors.Is(err, os.ErrNotExist) {
		return sid, nil
	}
	if err != nil {
		return "", err
	}
	var bindings map[string]struct {
		Session string `json:"session"`
	}
	if json.Unmarshal(b, &bindings) != nil {
		return "", errors.New("旧会话绑定损坏，不能猜测数据流归属")
	}
	found := ""
	for _, binding := range bindings {
		candidate := binding.Session
		if candidate != "" && foldLegacy(s.legacyBundle(root, candidate)) == foldLegacy(s.legacyBundle(root, sid)) {
			if found != "" && found != candidate {
				return "", fmt.Errorf("旧文件对应多个大小写不同的会话，保留双方等待确认：%s / %s", found, candidate)
			}
			found = candidate
		}
	}
	if found != "" {
		return found, nil
	}
	return sid, nil
}
func (s *Store) claimLegacySID(root, sid string) error {
	owner, err := s.preferredLegacySID(root, sid)
	if err != nil {
		return err
	}
	if owner != "" && owner != sid {
		return fmt.Errorf("%w：旧来源属于 %s", errLegacyUnauthorized, owner)
	}
	if existing, err := s.legacyOwner(root, sid); err != nil || existing != "" {
		return err
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "sid": sid, "bundle": s.legacyBundle(root, sid)})
	return writeFileAtomic(s.legacyOwnerFile(root, sid), b)
}
