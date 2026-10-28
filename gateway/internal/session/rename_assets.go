package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type renameAsset struct {
	Kind      string     `json:"kind"`
	Root      string     `json:"root"`
	Extension string     `json:"extension"`
	File      renameFile `json:"file"`
}

func (s *Store) assetManifest(kind, sid string) string {
	return filepath.Join(s.dirV1(), "assets", kind, Key(sid)+".json")
}

func (s *Store) readAsset(kind, sid string) (auxiliaryRecord, bool, error) {
	var rec auxiliaryRecord
	if kind != "persona" && kind != "interjections" {
		return rec, false, errors.New("辅助文件类型无效")
	}
	b, err := os.ReadFile(s.assetManifest(kind, sid))
	if errors.Is(err, os.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	if json.Unmarshal(b, &rec) != nil || rec.Version != 1 || rec.Kind != kind || rec.SID != sid || rec.OperationID == "" || (!filepath.IsAbs(rec.Root) && !strings.HasPrefix(rec.Root, "@data/")) {
		return rec, true, errors.New("辅助文件清单身份损坏")
	}
	if !((kind == "persona" && rec.Extension == ".md") || (kind == "interjections" && rec.Extension == ".jsonl")) {
		return rec, true, errors.New("辅助文件清单类型无效")
	}
	return rec, true, nil
}

func (s *Store) planRenameAssets(from, to string) ([]renameAsset, error) {
	var out []renameAsset
	for _, kind := range []string{"persona", "interjections"} {
		rec, has, err := s.readAsset(kind, from)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		path, exists, err := s.AuxiliaryPath(kind, s.assetRoot(rec.Root), rec.Extension, from, false)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if _, has, err := s.readAsset(kind, to); err != nil {
			return nil, err
		} else if has {
			return nil, fmt.Errorf("目标已有%s发布记录，保留双方", kind)
		}
		if _, err := os.Stat(AuxiliaryFile(s.assetRoot(rec.Root), to, rec.Extension)); err == nil {
			return nil, fmt.Errorf("目标已有%s未认领文件，保留双方", kind)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		n, sum, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, renameAsset{Kind: kind, Root: rec.Root, Extension: rec.Extension, File: renameFile{Len: n, Sum: sum}})
	}
	return out, nil
}

func (s *Store) validateRenameAssets(op renameOp, staging string, source bool) error {
	seen := map[string]bool{}
	for _, asset := range op.Assets {
		if seen[asset.Kind] {
			return errors.New("改名清单包含重复辅助文件")
		}
		seen[asset.Kind] = true
		rec, has, err := s.readAsset(asset.Kind, op.From)
		if err != nil {
			return err
		}
		if !has || rec.Root != asset.Root || rec.Extension != asset.Extension || rec.Generation != op.FromGen || rec.Stage != "active" || asset.File.Len < 0 || !validSum(asset.File.Sum) {
			return errors.New("改名辅助文件的来源或授权不匹配")
		}
		path := filepath.Join(staging, "asset-"+asset.Kind)
		if source {
			path = AuxiliaryFile(s.assetRoot(asset.Root), op.From, asset.Extension)
		}
		if err := verifyFileVersion(path, asset.File, "改名辅助文件 "+asset.Kind); err != nil {
			return err
		}
	}
	if source {
		// A newly accepted journal or persona must not disappear from a frozen plan.
		for _, kind := range []string{"persona", "interjections"} {
			rec, has, err := s.readAsset(kind, op.From)
			if err != nil {
				return err
			}
			if !has || seen[kind] {
				continue
			}
			if _, exists, err := s.AuxiliaryPath(kind, s.assetRoot(rec.Root), rec.Extension, op.From, false); err != nil {
				return err
			} else if exists {
				return errors.New("改名期间新增辅助文件，保留双方")
			}
		}
	}
	return nil
}

func (s *Store) copyRenameAssets(op renameOp, staging string) error {
	if err := s.validateRenameAssets(op, staging, true); err != nil {
		return err
	}
	for _, asset := range op.Assets {
		got, err := copyFileVersion(AuxiliaryFile(s.assetRoot(asset.Root), op.From, asset.Extension), filepath.Join(staging, "asset-"+asset.Kind))
		if err != nil {
			return err
		}
		if !sameVersion(got, asset.File) {
			return errors.New("辅助文件复制期间来源变化")
		}
	}
	return nil
}

func (s *Store) publishRenameAssets(op renameOp, staging string, checkOnly bool) error {
	for _, asset := range op.Assets {
		dst := AuxiliaryFile(s.assetRoot(asset.Root), op.To, asset.Extension)
		rec, has, err := s.readAsset(asset.Kind, op.To)
		if err != nil {
			return err
		}
		identity := op.OperationID + "-" + asset.Kind
		if has && (rec.OperationID != identity || rec.Root != asset.Root || rec.Extension != asset.Extension || rec.Generation != op.ToGen || rec.Revision != op.ToRev) {
			return errors.New("改名目标辅助文件属于其他发布操作")
		}
		if !has {
			if checkOnly {
				return errors.New("改名提交后缺少辅助文件发布记录")
			}
			rec = auxiliaryRecord{Version: 1, SID: op.To, Kind: asset.Kind, Root: asset.Root, Extension: asset.Extension, OperationID: identity, Generation: op.ToGen, Revision: op.ToRev, Stage: "rename_pending", Source: filepath.Join(staging, "asset-"+asset.Kind), File: asset.File}
			b, _ := json.Marshal(rec)
			if err := writeFileAtomic(s.assetManifest(asset.Kind, op.To), b); err != nil {
				return err
			}
		}
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) && !checkOnly {
			got, err := copyFileVersion(filepath.Join(staging, "asset-"+asset.Kind), dst)
			if err != nil {
				return err
			}
			if !sameVersion(got, asset.File) {
				return errors.New("改名辅助文件发布校验失败")
			}
		} else if err != nil {
			return err
		}
		var ok bool
		if asset.Kind == "interjections" {
			ok, err = versionIsPrefix(dst, asset.File)
		} else {
			ok, err = versionMatches(dst, asset.File)
		}
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("改名目标辅助文件内容冲突，保留双方")
		}
		if rec.Stage != "active" && rec.Stage != "rename_pending" {
			return errors.New("改名目标辅助文件发布阶段无效")
		}
	}
	return nil
}

func (s *Store) finalizeRenameAssets(op renameOp) error {
	for _, asset := range op.Assets {
		rec, has, err := s.readAsset(asset.Kind, op.To)
		if err != nil {
			return err
		}
		if !has || rec.OperationID != op.OperationID+"-"+asset.Kind || rec.Generation != op.ToGen || rec.Revision != op.ToRev {
			return errors.New("改名辅助文件最终发布归属不匹配")
		}
		if rec.Stage == "active" {
			continue
		}
		owner, exists, err := s.deadMarkerOwner(op.From)
		if err != nil {
			return err
		}
		if !exists || owner != op.OperationID {
			return errors.New("改名辅助文件尚未越过提交点")
		}
		if err := s.publishRenameAssets(op, s.renameStagingDir(op.From, op.To), true); err != nil {
			return err
		}
		rec.Stage = "active"
		b, _ := json.Marshal(rec)
		if err := writeFileAtomic(s.assetManifest(asset.Kind, op.To), b); err != nil {
			return err
		}
	}
	return nil
}
