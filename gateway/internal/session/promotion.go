package session

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 旧布局提升（promote）也需要可恢复清单：每次只处理一项，处理前先把计划落盘，
// 复制 / 校验后更新该项阶段；启动或 locate 再次触发提升时读取这份记录，按源 / 目标
// 现状核对续办，而不是只看 active 标记或目标文件是否存在。
//
// 提升只复制不删旧文件，active 标记是最终提交点。清单绑定了 operation_id、源 / 目标
// 世代与 revision、完整文件集合（含归档）和每个文件的内容版本。
const promoteVersion = 1

type promoteItem struct {
	Kind  string `json:"kind"` // history | settings | branch | archive
	Name  string `json:"name,omitempty"`
	Len   int64  `json:"len"`
	Sum   string `json:"sum"`
	Stage string `json:"stage"` // planned | copying | verified | conflict
	Note  string `json:"note,omitempty"`
}

type promoteOp struct {
	Version     int           `json:"version"`
	OperationID string        `json:"operation_id"`
	SID         string        `json:"sid"`
	FromGen     int64         `json:"from_gen"`
	ToGen       int64         `json:"to_gen"`
	FromRev     int64         `json:"from_rev"`
	ToRev       int64         `json:"to_rev"`
	Stage       string        `json:"stage"` // started | copying | verified | committed | conflict | superseded
	Reason      string        `json:"reason,omitempty"`
	UpdatedAt   int64         `json:"updated_at,omitempty"`
	Items       []promoteItem `json:"items"`
}

func (s *Store) dirV1Promotions() string {
	return filepath.Join(s.dirV1(), "promotions")
}

func (s *Store) promoteOpPath(sid string) string {
	return filepath.Join(s.dirV1Promotions(), Key(sid)+".json")
}

func (s *Store) promoteSettledPath(op promoteOp) string {
	id := op.OperationID
	if id == "" {
		id = Key(op.SID)
	}
	return filepath.Join(s.dirV1Promotions(), "settled", id+".json")
}

func (s *Store) writePromoteOp(op promoteOp) error {
	op.UpdatedAt = time.Now().Unix()
	b, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.promoteOpPath(op.SID), b)
}

func promoteItemKey(kind, name string) string {
	if kind == "archive" {
		return kind + ":" + name
	}
	return kind
}

func (s *Store) promoteItemPaths(sid string, item promoteItem) (src, dst string, err error) {
	switch item.Kind {
	case "history":
		return filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".jsonl"), s.newPath(sid, ".jsonl"), nil
	case "settings":
		return filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".settings.json"), s.newPath(sid, ".settings.json"), nil
	case "branch":
		return filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".branch.json"), s.newPath(sid, ".branch.json"), nil
	case "archive":
		if item.Name == "" || item.Name == "." || item.Name == ".." || filepath.Base(item.Name) != item.Name {
			return "", "", fmt.Errorf("旧归档文件名不合法：%q", item.Name)
		}
		return filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid), item.Name),
			filepath.Join(s.dirV1Archive(), Key(sid), item.Name), nil
	default:
		return "", "", fmt.Errorf("提升清单包含未知文件类型：%q", item.Kind)
	}
}

func validatePromoteItem(item promoteItem) error {
	switch item.Kind {
	case "history", "settings", "branch", "archive":
	default:
		return fmt.Errorf("提升清单包含未知文件类型：%q", item.Kind)
	}
	if item.Kind == "archive" {
		if item.Name == "" || item.Name == "." || item.Name == ".." || filepath.Base(item.Name) != item.Name {
			return fmt.Errorf("旧归档文件名不合法：%q", item.Name)
		}
	}
	return nil
}

func validSum(sum string) bool {
	if len(sum) != 64 {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil
}

func validatePromoteOp(op promoteOp, sid string) error {
	if op.Version != promoteVersion || op.OperationID == "" || op.SID != sid {
		return fmt.Errorf("提升操作记录身份或版本不匹配：%s", sid)
	}
	switch op.Stage {
	case "started", "copying", "verified", "committed", "conflict", "superseded":
	default:
		return fmt.Errorf("提升操作记录的阶段不认识：%q", op.Stage)
	}
	if op.FromGen < 0 || op.FromGen > maxCounter || op.ToGen < 0 || op.ToGen > maxCounter ||
		op.FromRev < 0 || op.FromRev > maxCounter || op.ToRev < 0 || op.ToRev > maxCounter {
		return fmt.Errorf("提升操作记录的世代 / 版本越界")
	}
	seen := map[string]bool{}
	for _, item := range op.Items {
		key := promoteItemKey(item.Kind, item.Name)
		if seen[key] {
			return fmt.Errorf("提升操作记录重复包含 %s", key)
		}
		seen[key] = true
		if item.Len < 0 || !validSum(item.Sum) {
			return fmt.Errorf("提升操作记录的文件版本无效：%s", key)
		}
		switch item.Stage {
		case "planned", "copying", "verified", "conflict":
		default:
			return fmt.Errorf("提升操作记录的文件阶段不认识：%q", item.Stage)
		}
		if err := validatePromoteItem(item); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readPromoteOp(sid string) (promoteOp, bool, error) {
	b, err := os.ReadFile(s.promoteOpPath(sid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return promoteOp{}, false, nil
		}
		return promoteOp{}, true, fmt.Errorf("读提升操作记录失败（%s）：%w", sid, err)
	}
	var op promoteOp
	if err := json.Unmarshal(b, &op); err != nil {
		return op, true, fmt.Errorf("提升操作记录损坏（%s）：%w", sid, err)
	}
	if err := validatePromoteOp(op, sid); err != nil {
		return op, true, err
	}
	return op, true, nil
}

func checkedFileVersion(path string, check func(io.Reader) error) (renameFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return renameFile{}, err
	}
	defer f.Close()
	if err := check(f); err != nil {
		return renameFile{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return renameFile{}, err
	}
	n, sum, err := hashReader(f)
	if err != nil {
		return renameFile{}, err
	}
	return renameFile{Len: n, Sum: sum}, nil
}

func (s *Store) planPromotion(sid string) ([]promoteItem, error) {
	var items []promoteItem
	ln := LegacyKey(sid)
	for _, def := range []struct {
		kind   string
		suffix string
		check  func(io.Reader) error
	}{
		{"history", ".jsonl", jsonlCheckReader},
		{"settings", ".settings.json", jsonCheckReader},
		{"branch", ".branch.json", jsonCheckReader},
	} {
		src := filepath.Join(s.dirLegacySessions(), ln+def.suffix)
		st, err := os.Stat(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("旧会话文件是目录，不能提升：%s", src)
		}
		meta, err := checkedFileVersion(src, def.check)
		if err != nil {
			return nil, fmt.Errorf("旧会话文件无法校验（%s）：%w", src, err)
		}
		items = append(items, promoteItem{Kind: def.kind, Len: meta.Len, Sum: meta.Sum, Stage: "planned"})
	}
	oldArch := filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid))
	entries, err := os.ReadDir(oldArch)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "." || name == ".." || filepath.Base(name) != name {
			return nil, fmt.Errorf("旧归档文件名不合法：%q", name)
		}
		src := filepath.Join(oldArch, name)
		meta, err := checkedFileVersion(src, jsonlCheckReader)
		if err != nil {
			return nil, fmt.Errorf("旧归档文件无法校验（%s）：%w", src, err)
		}
		items = append(items, promoteItem{Kind: "archive", Name: name, Len: meta.Len, Sum: meta.Sum, Stage: "planned"})
	}
	return items, nil
}

func (s *Store) currentPromoteSources(sid string) (map[string]bool, error) {
	keys := map[string]bool{}
	ln := LegacyKey(sid)
	for _, def := range []struct{ kind, suffix string }{
		{"history", ".jsonl"}, {"settings", ".settings.json"}, {"branch", ".branch.json"},
	} {
		st, err := os.Stat(filepath.Join(s.dirLegacySessions(), ln+def.suffix))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("旧会话文件是目录，不能提升：%s", def.suffix)
		}
		keys[def.kind] = true
	}
	oldArch := filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid))
	entries, err := os.ReadDir(oldArch)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			keys[promoteItemKey("archive", e.Name())] = true
		}
	}
	return keys, nil
}

func promoteItemSet(items []promoteItem) map[string]bool {
	keys := map[string]bool{}
	for _, item := range items {
		keys[promoteItemKey(item.Kind, item.Name)] = true
	}
	return keys
}

func samePromoteSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sameVersion(a, b renameFile) bool {
	return a.Len == b.Len && a.Sum == b.Sum
}

func (s *Store) markPromoteConflict(op *promoteOp, idx int, note string) error {
	if idx >= 0 && idx < len(op.Items) {
		op.Items[idx].Stage = "conflict"
		op.Items[idx].Note = note
	}
	op.Stage = "conflict"
	op.Reason = note
	if err := s.writePromoteOp(*op); err != nil {
		return fmt.Errorf("%s；登记提升冲突失败：%w", note, err)
	}
	return fmt.Errorf("%s，保留旧文件和目标现场等待人工核对", note)
}

func (s *Store) settlePromoteOp(op promoteOp, stage, reason string) error {
	op.Stage, op.Reason, op.UpdatedAt = stage, reason, time.Now().Unix()
	b, _ := json.MarshalIndent(op, "", "  ")
	if err := writeFileAtomic(s.promoteSettledPath(op), b); err != nil {
		return fmt.Errorf("记录提升终态失败（保留活动记录）：%w", err)
	}
	if err := os.Remove(s.promoteOpPath(op.SID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("提升已结算，移除活动记录失败（可以重试）：%w", err)
	}
	return nil
}

func (s *Store) finishPromoteCommitted(op promoteOp) error {
	op.Stage = "committed"
	op.Reason = ""
	op.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(op, "", "  ")
	if err := writeFileAtomic(s.promoteSettledPath(op), b); err != nil {
		return fmt.Errorf("记录提升完成状态失败（保留活动记录）：%w", err)
	}
	if err := os.Remove(s.promoteOpPath(op.SID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("提升已提交，移除活动记录失败（可以重试）：%w", err)
	}
	return nil
}

// promote 是 locate / Migrate 使用的入口。真正的逐项状态保存在 promoteOp 中，
// 中断后再次调用会接着核对，而不是从头覆盖。
func (s *Store) promote(sid string) error {
	_, err := s.promoteSession(sid)
	return err
}

// promoteSession 返回本次尝试结束时的操作记录，供 Migrate 汇总逐项结果。
func (s *Store) promoteSession(sid string) (promoteOp, error) {
	op, has, err := s.readPromoteOp(sid)
	if err != nil {
		return op, err
	}
	gen, err := s.readGen(sid)
	if err != nil {
		return op, fmt.Errorf("%w：旧会话的世代状态不可读：%v", errLegacyUnauthorized, err)
	}
	rev, err := s.readRev(sid)
	if err != nil {
		return op, fmt.Errorf("%w：旧会话的上下文版本不可读：%v", errLegacyUnauthorized, err)
	}
	_, activeErr := os.Stat(s.activeMarker(sid))
	active := activeErr == nil
	_, deletedErr := os.Stat(s.deletedMarker(sid))
	deleted := deletedErr == nil

	if has {
		if op.Stage == "superseded" {
			// 终态记录如果因移除失败留在活动目录，绝不能重新处理。
			if err := s.settlePromoteOp(op, "superseded", op.Reason); err != nil {
				return op, err
			}
			if active {
				return op, nil
			}
			return op, fmt.Errorf("%w：提升操作已经结算为失效", errLegacyUnauthorized)
		}
		// 任何后续删除 / 重置 / 换代都先让旧提升失效，再谈续办。
		if deleted {
			_ = s.settlePromoteOp(op, "superseded", "会话已被删除")
			return op, nil
		}
		if op.FromGen != gen || op.ToGen != gen || op.FromRev != rev || op.ToRev != rev {
			_ = s.settlePromoteOp(op, "superseded", "会话在提升期间删除、重置或换代")
			if active {
				return op, nil
			}
			return op, fmt.Errorf("%w：提升操作已因后续失效被取消", errLegacyUnauthorized)
		}
		if active {
			// active 是提交点。记录停在 verified 说明提交后没来得及写终态，补上即可。
			_ = s.finishPromoteCommitted(op)
			op.Stage = "committed"
			return op, nil
		}
		cur, err := s.currentPromoteSources(sid)
		if err != nil {
			return op, err
		}
		if !samePromoteSet(cur, promoteItemSet(op.Items)) {
			cerr := s.markPromoteConflict(&op, -1, "旧文件集合在提升期间发生变化")
			return op, cerr
		}
	} else {
		if active || deleted {
			return op, nil
		}
		if gen > 0 {
			return op, fmt.Errorf("%w：旧会话已经开过第 %d 代，旧备份不能自动升级", errLegacyUnauthorized, gen)
		}
		items, err := s.planPromotion(sid)
		if err != nil {
			return op, err
		}
		id, err := newOperationID()
		if err != nil {
			return op, err
		}
		op = promoteOp{Version: promoteVersion, OperationID: id, SID: sid,
			FromGen: gen, ToGen: gen, FromRev: rev, ToRev: rev, Stage: "started", Items: items}
		if err := s.writePromoteOp(op); err != nil {
			return op, fmt.Errorf("登记提升清单失败：%w", err)
		}
	}

	for i := range op.Items {
		item := &op.Items[i]
		item.Stage = "copying"
		item.Note = ""
		op.Stage = "copying"
		if err := s.writePromoteOp(op); err != nil {
			return op, fmt.Errorf("登记提升项 %s 失败：%w", promoteItemKey(item.Kind, item.Name), err)
		}
		src, dst, err := s.promoteItemPaths(sid, *item)
		if err != nil {
			cerr := s.markPromoteConflict(&op, i, err.Error())
			return op, cerr
		}
		planned := renameFile{Len: item.Len, Sum: item.Sum}
		st, err := os.Stat(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				cerr := s.markPromoteConflict(&op, i, "源文件在提升期间消失")
				return op, cerr
			}
			return op, err
		}
		if st.IsDir() {
			cerr := s.markPromoteConflict(&op, i, "源文件在提升期间变成目录")
			return op, cerr
		}
		ok, err := versionMatches(src, planned)
		if err != nil {
			return op, err
		}
		if !ok {
			cerr := s.markPromoteConflict(&op, i, "源文件在提升期间被改写")
			return op, cerr
		}
		if _, err := os.Stat(dst); err == nil {
			ok, err := versionMatches(dst, planned)
			if err != nil {
				return op, err
			}
			if !ok {
				cerr := s.markPromoteConflict(&op, i, "目标已经有不同的内容")
				return op, cerr
			}
			// 复制完成后状态没记下、或上一次重试已经发布过：按内容复用。
			item.Stage = "verified"
			if err := s.writePromoteOp(op); err != nil {
				return op, fmt.Errorf("登记提升项 %s 已校验失败：%w", promoteItemKey(item.Kind, item.Name), err)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return op, err
		}
		copied, err := copyFileVersion(src, dst)
		if err != nil {
			return op, err
		}
		if !sameVersion(copied, planned) {
			cerr := s.markPromoteConflict(&op, i, "源文件在复制期间发生变化")
			return op, cerr
		}
		ok, err = versionMatches(dst, planned)
		if err != nil {
			return op, err
		}
		if !ok {
			cerr := s.markPromoteConflict(&op, i, "目标副本与提升清单不一致")
			return op, cerr
		}
		item.Stage = "verified"
		if err := s.writePromoteOp(op); err != nil {
			return op, fmt.Errorf("登记提升项 %s 已校验失败：%w", promoteItemKey(item.Kind, item.Name), err)
		}
	}
	// 最后再核对一次完整集合：处理期间新出现的源文件不能被忽略。
	cur, err := s.currentPromoteSources(sid)
	if err != nil {
		return op, err
	}
	if !samePromoteSet(cur, promoteItemSet(op.Items)) {
		cerr := s.markPromoteConflict(&op, -1, "旧文件集合在提升期间发生变化")
		return op, cerr
	}
	op.Stage = "verified"
	op.Reason = ""
	if err := s.writePromoteOp(op); err != nil {
		return op, fmt.Errorf("提交提升清单失败：%w", err)
	}
	if err := s.rememberLongKey(sid); err != nil {
		return op, err
	}
	if err := os.MkdirAll(filepath.Dir(s.activeMarker(sid)), 0o700); err != nil {
		return op, err
	}
	if err := writeFileAtomic(s.activeMarker(sid), []byte(Key(sid))); err != nil {
		return op, err
	}
	s.legacyActive(sid)
	op.Stage = "committed"
	// active 已经落盘，提交点已过；终态记录失败不能反过来让调用方以为没提升。
	_ = s.finishPromoteCommitted(op)
	return op, nil
}

// ResumePromotions 在启动时继续/结算中断的旧布局提升。没有 active 标记的记录会
// 按清单逐项核对；已提交或已失效的记录只做终态收尾。
func (s *Store) ResumePromotions() error {
	entries, err := os.ReadDir(s.dirV1Promotions())
	if err != nil {
		return fmt.Errorf("读未完成提升目录失败：%w", err)
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dirV1Promotions(), e.Name()))
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("读提升操作记录失败：%s：%w", e.Name(), err)
			}
			continue
		}
		var op promoteOp
		if json.Unmarshal(b, &op) != nil || op.SID == "" || Key(op.SID)+".json" != e.Name() {
			if firstErr == nil {
				firstErr = fmt.Errorf("提升操作记录损坏或身份不匹配：%s", e.Name())
			}
			continue
		}
		if err := s.promote(op.SID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
