package session

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---- 会话在磁盘上的位置（W1） ----
//
// 逻辑 sid 保持原值，只改**文件系统位置**。新布局放在独立的命名空间目录里：
//
//	<data>/v1/sessions/<key>.jsonl | .settings.json | .branch.json
//	<data>/v1/archive/<key>/...          （/reset 归档）
//	<data>/v1/active/<key>               （这个会话已经在新布局里激活）
//	<data>/v1/sessions/keys/<key>.json   （超长 sid 的映射）
//
// 旧布局（url.PathEscape 直接当文件名）留在原处，只作为迁移来源和恢复备份：
//
//	<data>/sessions/<旧名>.jsonl | .settings.json ...
//	<data>/archive/<旧名>/...
//
// 为什么用目录隔离而不是给文件名加前缀：旧的逻辑 sid 完全可能就是字面量 "v1-61"，
// 它在新编码下正好等于 sid "a" 的键——只看文件名无法区分版本。放在不同目录里，
// 目录本身就说明了格式归属，不存在歧义。

const (
	keyPrefix     = ""
	keyLongPrefix = "h-"
	keyMaxBase    = 96 // 超过这个长度的 sid 用哈希键 + 持久映射
)

// errLegacyUnauthorized 表示旧备份已经失去自动授权（开过新一代或世代状态损坏）。
// 调用方不能像普通升级失败那样退回旧路径继续读。
var errLegacyUnauthorized = errors.New("旧备份没有自动授权")

// Key 是会话 ID 的规范文件名键：UTF-8 字节的十六进制。
func Key(sid string) string {
	if len(sid) <= keyMaxBase {
		return hex.EncodeToString([]byte(sid))
	}
	sum := sha256.Sum256([]byte(sid))
	return keyLongPrefix + hex.EncodeToString(sum[:16])
}

// ParseKey 把规范键还原成 sid。只接受 Key(sid) 的原样输出，不做前缀猜测。
func ParseKey(key string) (string, bool) {
	if key == "" || strings.HasPrefix(key, keyLongPrefix) {
		return "", false // 长键要查持久映射
	}
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) == 0 {
		return "", false
	}
	sid := string(raw)
	if Key(sid) != key {
		return "", false // 不是规范形式（大小写、长度不对等）
	}
	return sid, true
}

// LegacyKey 是旧布局用的文件名，各不相同的数据类型规则略有差异。
func LegacyKey(sid string) string {
	if sid == "." || sid == ".." {
		return "%2E" + url.PathEscape(sid[1:])
	}
	return url.PathEscape(sid)
}

// legacyArchiveKey 是旧归档目录用的名字（和旧文件名的特殊规则不同）。
func legacyArchiveKey(sid string) string {
	if sid == "." || sid == ".." {
		return "%2E" + url.PathEscape(sid)
	}
	return url.PathEscape(sid)
}

func (s *Store) dirV1() string             { return filepath.Join(s.dir, "v1") }
func (s *Store) dirV1Sessions() string     { return filepath.Join(s.dirV1(), "sessions") }
func (s *Store) dirV1Archive() string      { return filepath.Join(s.dirV1(), "archive") }
func (s *Store) dirLegacySessions() string { return filepath.Join(s.dir, "sessions") }
func (s *Store) dirLegacyArchive() string  { return filepath.Join(s.dir, "archive") }

// newPath 是会话某一类文件在新布局里的位置。
func (s *Store) newPath(sid, suffix string) string {
	return filepath.Join(s.dirV1Sessions(), Key(sid)+suffix)
}

// isDead 报告这个会话是不是被明确删除过（墓碑）。已删除的会话不参与读取、列举和分支视图。
func (s *Store) isDead(sid string) bool {
	if sid == "" {
		return false
	}
	_, err := os.Stat(s.deletedMarker(sid))
	return err == nil
}

// deadLegacyMarker 是会话的世代号文件：每开一次新一代就加一。旧根备份的授权只在
// 第 0 代（从没删除重建过）有效；改名等操作也用它绑定“自己处理的是哪一代”。
func (s *Store) genFile(sid string) string {
	return filepath.Join(s.dirV1(), "gen", Key(sid))
}

// 世代 / 上下文版本都是有限单调计数。超过上界视为损坏，不能继续加、更不能回绕。
const maxCounter = int64(1) << 62

// parseCounter 只接受规范的十进制非负整数。空内容、带单位、负值、超出上界都算损坏。
func parseCounter(txt string) (int64, bool) {
	txt = strings.TrimSpace(txt)
	if txt == "" || strings.HasPrefix(txt, "+") || strings.HasPrefix(txt, "-") {
		return 0, false
	}
	if len(txt) > 1 && txt[0] == '0' {
		return 0, false // 只接受规范十进制，前导零不是产品写出的格式
	}
	n, err := strconv.ParseInt(txt, 10, 64)
	if err != nil || n < 0 || n > maxCounter {
		return 0, false
	}
	return n, true
}

// readGen 读当前世代号。只有文件真正不存在才是第 0 代；空文件、未知格式、
// 负值 / 越界都返回错误，绝不给出一个可能恰好匹配旧操作的默认数字。
// 上一版写进文件的是 Key(sid) 本身，只有它精确等于当前 sid 的旧键时才兼容为第 1 代。
func (s *Store) readGen(sid string) (int64, error) {
	b, err := os.ReadFile(s.genFile(sid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // 从没开过新一代
		}
		return 0, fmt.Errorf("读会话世代失败（%s）：%w", sid, err)
	}
	txt := strings.TrimSpace(string(b))
	if txt == Key(sid) {
		if _, ambiguous := parseCounter(txt); ambiguous {
			// 旧版标记和普通计数恰好长得一样：不猜，保留现场等待迁移。
			return 0, fmt.Errorf("会话世代文件格式有歧义（%s）：%q", sid, txt)
		}
		return 1, nil // 上一版的旧键标记
	}
	if n, ok := parseCounter(txt); ok {
		return n, nil
	}
	return 0, fmt.Errorf("会话世代文件内容无效（%s）：%q", sid, txt)
}

// bumpGen 把世代号加一并持久化，返回新值。先成功读取并验证原值，再写入。
func (s *Store) bumpGen(sid string) (int64, error) {
	cur, err := s.readGen(sid)
	if err != nil {
		return 0, err
	}
	if cur >= maxCounter {
		return 0, fmt.Errorf("会话世代已经达到上界（%s）：%d", sid, cur)
	}
	next := cur + 1
	if err := os.MkdirAll(filepath.Dir(s.genFile(sid)), 0o700); err != nil {
		return 0, err
	}
	if err := writeFileAtomic(s.genFile(sid), []byte(strconv.FormatInt(next, 10))); err != nil {
		return 0, err
	}
	return next, nil
}

// revFile 是上下文版本：Delete / Reset 这类“后续失效决定”会把它推进。
// 改名操作同时记录源 / 目标的世代与版本，恢复时必须逐项核对，旧授权不能覆盖后来的删除或重置。
func (s *Store) revFile(sid string) string {
	return filepath.Join(s.dirV1(), "rev", Key(sid))
}

func (s *Store) readRev(sid string) (int64, error) {
	b, err := os.ReadFile(s.revFile(sid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("读会话上下文版本失败（%s）：%w", sid, err)
	}
	txt := strings.TrimSpace(string(b))
	n, ok := parseCounter(txt)
	if !ok {
		return 0, fmt.Errorf("会话上下文版本文件内容无效（%s）：%q", sid, txt)
	}
	return n, nil
}

func (s *Store) bumpRev(sid string) (int64, error) {
	cur, err := s.readRev(sid)
	if err != nil {
		return 0, err
	}
	if cur >= maxCounter {
		return 0, fmt.Errorf("会话上下文版本已经达到上界（%s）：%d", sid, cur)
	}
	next := cur + 1
	if err := writeFileAtomic(s.revFile(sid), []byte(strconv.FormatInt(next, 10))); err != nil {
		return 0, err
	}
	return next, nil
}

// legacyDead 报告这个 sid 的旧根备份是不是已经永久失效（开过新一代）。
func (s *Store) legacyDead(sid string) bool {
	n, err := s.readGen(sid)
	if err != nil {
		return true // 读不出来就按“已经失效”处理，绝不把旧备份当活动数据
	}
	return n > 0
}

// activeMarker 是“这个会话已经在新布局里激活”的标记。
// deletedMarker 是“这个会话被明确删除过”的墓碑：旧备份保留，但不再自动复活。
func (s *Store) deletedMarker(sid string) string {
	return filepath.Join(s.dirV1(), "deleted", Key(sid))
}

func (s *Store) activeMarker(sid string) string {
	return filepath.Join(s.dirV1(), "active", Key(sid))
}

// keyMapFile 是超长 sid 的哈希 → 原始 sid 映射。
func (s *Store) keyMapFile(key string) string {
	return filepath.Join(s.dirV1Sessions(), "keys", key+".json")
}

// ---- 长键映射 ----

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
		if json.Unmarshal(b, &old) == nil {
			if old.SID == sid {
				return nil
			}
			return fmt.Errorf("长 ID 映射冲突：%s 已经属于另一个会话", key)
		}
		return fmt.Errorf("长 ID 映射损坏：%s", path)
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

// resolveNewKey 把新布局里的文件名键还原成 sid。
func (s *Store) resolveNewKey(name string) (string, bool) {
	if sid, ok := ParseKey(name); ok {
		return sid, true
	}
	if strings.HasPrefix(name, keyLongPrefix) {
		return s.LookupLongKey(name)
	}
	return "", false
}

// resolveLegacyName 把旧布局里的文件名还原成 sid（旧规则，允许任意名称）。
func resolveLegacyName(name string) (string, bool) {
	sid, err := url.PathUnescape(name)
	if err != nil || sid == "" {
		return "", false
	}
	return sid, true
}

// ---- 权威定位 ----

// locate 返回会话某一类文件的权威位置和是否存在。
//
//  1. 新布局里有这个文件 → 用它；
//  2. 会话已经激活（有标记）→ 用新位置，即使文件不存在（重置/删除后不回退旧备份）；
//  3. 旧布局里有 → 先把整个会话提升到新布局（校验后提交），再用新位置；
//  4. 都没有 → 新位置（新会话）。
//
// 写入路径必须用这个接口，不能各自猜文件在不在。
func (s *Store) locate(sid, suffix string) (string, bool, error) {
	np := s.newPath(sid, suffix)
	// 已激活：新位置就是权威位置（重置后文件不存在也不回退旧备份）
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		if _, err := os.Stat(np); err == nil {
			return np, true, nil
		}
		return np, false, nil
	}
	// 删除墓碑：旧备份只作恢复来源，扫描和重放都不能自动复活
	if _, err := os.Stat(s.deletedMarker(sid)); err == nil {
		return np, false, nil
	}
	if s.hasLegacy(sid) {
		// 旧会话：必须完整提升并提交激活标记，未完成之前一律失败，不能拿半成品当数据
		if err := s.promote(sid); err != nil {
			return np, false, fmt.Errorf("把旧会话数据升级到新布局失败：%w", err)
		}
		if _, err := os.Stat(np); err == nil {
			return np, true, nil
		}
		return np, false, nil
	}
	// 没有旧文件：新会话自己就是权威的
	if _, err := os.Stat(np); err == nil {
		return np, true, nil
	}
	return np, false, nil
}

// hasLegacy 报告旧布局里有没有这个会话的文件（历史 / 设置 / 分支 / 归档）。
func (s *Store) hasLegacy(sid string) bool {
	ln := LegacyKey(sid)
	for _, p := range []string{
		filepath.Join(s.dirLegacySessions(), ln+".jsonl"),
		filepath.Join(s.dirLegacySessions(), ln+".settings.json"),
		filepath.Join(s.dirLegacySessions(), ln+".branch.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if fi, err := os.Stat(filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid))); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// promote 把一个会话的旧文件升级到新布局：逐项校验、复制、回读比对，最后写激活标记。
// 只复制不删旧文件；任何一项失败都不写标记，下次进来还会重试。
func (s *Store) promote(sid string) error {
	// 已经提升过：旧文件只是备份，不再做首次导入（否则重置/删除会被重放复活）
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		return nil
	}
	if _, err := os.Stat(s.deletedMarker(sid)); err == nil {
		return nil
	}
	// 旧备份的自动升级也必须消费世代错误：身份读不出来或已经开过新一代时，
	// 不能把旧文件重新提升成权威数据。
	gen, err := s.readGen(sid)
	if err != nil {
		return fmt.Errorf("%w：旧会话的世代状态不可读：%v", errLegacyUnauthorized, err)
	}
	if gen > 0 {
		return fmt.Errorf("%w：旧会话已经开过第 %d 代，旧备份不能自动升级", errLegacyUnauthorized, gen)
	}
	copyOne := func(src, dst string, check func(io.Reader) error) error {
		sf, err := os.Open(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		checkErr := check(sf)
		sf.Close()
		if checkErr != nil {
			return fmt.Errorf("%s 内容无法校验：%w", src, checkErr)
		}
		if _, err := os.Stat(dst); err == nil {
			same, err := sameFileContent(src, dst)
			if err != nil {
				return err
			}
			if same {
				return nil // 已经复制过
			}
			return fmt.Errorf("%s 与 %s 内容不同，保留双方等待人工核对", src, dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("读目标 %s 失败：%w", dst, err)
		}
		if _, err := copyFileVersion(src, dst); err != nil {
			return err
		}
		same, err := sameFileContent(src, dst)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("写 %s 后内容与源不一致", dst)
		}
		return nil
	}
	ln := LegacyKey(sid)
	for _, item := range []struct {
		suffix string
		check  func(io.Reader) error
	}{
		{".jsonl", jsonlCheckReader},
		{".settings.json", jsonCheckReader},
		{".branch.json", jsonCheckReader},
	} {
		if err := copyOne(filepath.Join(s.dirLegacySessions(), ln+item.suffix),
			s.newPath(sid, item.suffix), item.check); err != nil {
			return err
		}
	}
	oldArch := filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid))
	if entries, err := os.ReadDir(oldArch); err == nil {
		newArch := filepath.Join(s.dirV1Archive(), Key(sid))
		if err := os.MkdirAll(newArch, 0o700); err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := copyOne(filepath.Join(oldArch, e.Name()), filepath.Join(newArch, e.Name()), jsonlCheckReader); err != nil {
				return err
			}
		}
	}
	if err := s.rememberLongKey(sid); err != nil {
		return err
	}
	// 最后写激活标记：所有东西都校验并提交到位，这个会话才算搬到新布局
	if err := os.MkdirAll(filepath.Dir(s.activeMarker(sid)), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(s.activeMarker(sid), []byte(Key(sid))); err != nil {
		return err
	}
	s.legacyActive(sid)
	return nil
}

// legacyActive 标记内存里这个会话已经升级过（避免每次访问都 stat 旧目录）。
func (s *Store) legacyActive(sid string) {
	if s.promoted == nil {
		s.promoted = map[string]bool{}
	}
	s.promoted[sid] = true
}

// ---- 迁移 ----

// MigrationItem 是单个文件的迁移阶段记录。
type MigrationItem struct {
	SID    string `json:"sid"`
	Kind   string `json:"kind"` // history | settings | branch | archive
	Source string `json:"source"`
	Target string `json:"target"`
	Stage  string `json:"stage"` // copied | verified | conflict | failed
	Bytes  int64  `json:"bytes"`
}

// MigrationRecord 是一次迁移的真实结果。
type MigrationRecord struct {
	Version int             `json:"version"`
	At      int64           `json:"at"`
	Items   []MigrationItem `json:"items"`
}

// Migrate 把旧布局升级到新布局。只复制不删旧文件；逐项记录阶段；
// 清单提交失败会作为错误返回（调用方不能把部分完成当成成功）。
func (s *Store) Migrate() (MigrationRecord, error) {
	rec := MigrationRecord{Version: 1, At: time.Now().Unix()}
	sids := map[string]bool{}
	for _, k := range []struct{ dir, suffix string }{
		{s.dirLegacySessions(), ".jsonl"},
		{s.dirLegacySessions(), ".settings.json"},
		{s.dirLegacySessions(), ".branch.json"},
	} {
		for _, e := range mustReadDir(k.dir) {
			if e.IsDir() || !strings.HasSuffix(e.Name(), k.suffix) {
				continue
			}
			if sid, ok := resolveLegacyName(strings.TrimSuffix(e.Name(), k.suffix)); ok {
				sids[sid] = true
			}
		}
	}
	for _, e := range mustReadDir(s.dirLegacyArchive()) {
		if !e.IsDir() {
			continue
		}
		if sid, ok := resolveLegacyName(e.Name()); ok {
			sids[sid] = true
		}
	}
	var firstErr error
	for sid := range sids {
		// active 是备份、deleted 是墓碑：都不是“待迁移”
		if _, err := os.Stat(s.activeMarker(sid)); err == nil {
			continue
		}
		if _, err := os.Stat(s.deletedMarker(sid)); err == nil {
			continue
		}
		if err := s.promote(sid); err != nil {
			rec.Items = append(rec.Items, MigrationItem{SID: sid, Stage: "conflict"})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ln := LegacyKey(sid)
		for _, item := range []struct{ suffix, kind string }{
			{".jsonl", "history"}, {".settings.json", "settings"}, {".branch.json", "branch"},
		} {
			src := filepath.Join(s.dirLegacySessions(), ln+item.suffix)
			st, err := os.Stat(src)
			if err != nil {
				continue
			}
			rec.Items = append(rec.Items, MigrationItem{SID: sid, Kind: item.kind, Source: src,
				Target: s.newPath(sid, item.suffix), Stage: "verified", Bytes: st.Size()})
		}
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := writeFileAtomic(filepath.Join(s.dirV1Sessions(), "keys", "migration.json"), b); err != nil {
		return rec, fmt.Errorf("写迁移清单失败：%w", err)
	}
	return rec, firstErr
}

// HasLegacyKeys 报告旧布局里还有没有**没升级过**的会话（已经写激活标记的不算）。
func (s *Store) HasLegacyKeys() bool {
	live := func(sid string) bool {
		if _, err := os.Stat(s.activeMarker(sid)); err == nil {
			return false // 已提升：旧文件是备份
		}
		if _, err := os.Stat(s.deletedMarker(sid)); err == nil {
			return false // 已删除：不能复活
		}
		return true
	}
	for _, e := range mustReadDir(s.dirLegacySessions()) {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
			if strings.HasSuffix(name, suffix) {
				if sid, ok := resolveLegacyName(strings.TrimSuffix(name, suffix)); ok && live(sid) {
					return true
				}
			}
		}
	}
	for _, e := range mustReadDir(s.dirLegacyArchive()) {
		if !e.IsDir() {
			continue
		}
		if sid, ok := resolveLegacyName(e.Name()); ok && live(sid) {
			return true
		}
	}
	return false
}

func jsonCheck(b []byte) error {
	return jsonCheckReader(strings.NewReader(string(b)))
}

func jsonCheckReader(r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	var v any
	err := dec.Decode(&v)
	if errors.Is(err, io.EOF) {
		return nil // 空文件允许
	}
	if err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON 后面还有多余内容")
		}
		return err
	}
	return nil
}

func jsonlCheck(b []byte) error {
	return jsonlCheckReader(strings.NewReader(string(b)))
}

func jsonlCheckReader(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return err
		}
	}
	return sc.Err()
}

// mustReadDir 读目录，失败返回空（不存在和读失败在这里等价）。
func mustReadDir(dir string) []os.DirEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	return entries
}
