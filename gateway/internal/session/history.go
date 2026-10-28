package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func archiveKey(sid string) string { return Key(sid) }
func (s *Store) archiveDir(sid string) string {
	return filepath.Join(s.dirV1Archive(), archiveKey(sid))
}

// legacyArchiveDir 是旧布局的归档目录（迁移前读取用）。
func (s *Store) legacyArchiveDir(sid string) string {
	return filepath.Join(s.dirLegacyArchive(), legacyArchiveKey(sid))
}

// ArchiveDirPath 是某个会话在新布局里的归档目录（测试和诊断用）。
func (s *Store) ArchiveDirPath(sid string) string { return s.archiveDir(sid) }

// 所有归档操作使用同一个已发布视图。不能因为目标目录已创建就
// 暴露部分迁移，也不能在删除归档后回退到保留的旧备份。
func (s *Store) archiveReadDir(sid string) (string, error) {
	if s.isDead(sid) {
		return "", os.ErrNotExist
	}
	if _, _, err := s.locate(sid, ".jsonl"); err != nil {
		return "", err
	}
	return s.archiveDir(sid), nil
}
func (s *Store) ArchiveReset(sid string, now time.Time) error {
	file, _, err := s.locate(sid, ".jsonl")
	if err != nil {
		return err
	}
	dir := s.archiveDir(sid)
	if _, err := os.Stat(file); err == nil {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		dst := filepath.Join(dir, now.Format("20060102-150405.000000000")+".jsonl")
		if err = os.Rename(file, dst); err != nil {
			return err
		}
		_ = os.Chtimes(dst, now, now)
		if err = s.Reset(sid); err != nil {
			_ = os.Rename(dst, file)
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.Reset(sid)
}

type ArchiveInfo struct {
	ID       string `json:"id"`
	Messages int    `json:"messages"`
	At       int64  `json:"at"`
}

func (s *Store) Archives(sid string) []ArchiveInfo {
	out := []ArchiveInfo{}
	dir, err := s.archiveReadDir(sid)
	if err != nil {
		return out
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		counter := &lineCounter{}
		_, copyErr := io.Copy(counter, f)
		_ = f.Close()
		info, err := e.Info()
		if err != nil || copyErr != nil {
			continue
		}
		out = append(out, ArchiveInfo{e.Name(), counter.n, info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

type lineCounter struct{ n int }

func (c *lineCounter) Write(b []byte) (int, error) {
	c.n += bytes.Count(b, []byte{'\n'})
	return len(b), nil
}

// ArchiveCount 是所有会话归档的份数
func (s *Store) ArchiveCount() int {
	n := 0
	for _, sid := range s.List() {
		n += len(s.Archives(sid))
	}
	return n
}
func (s *Store) DeleteArchive(sid, id string) error {
	if id == "" || filepath.Base(id) != id || filepath.Ext(id) != ".jsonl" {
		return fmt.Errorf("归档 ID 不合法")
	}
	dir, err := s.archiveReadDir(sid)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, id))
}
func (s *Store) PruneArchives(now time.Time, days int) error {
	if days == 0 {
		return nil
	}
	for _, sid := range s.List() {
		if _, err := s.archiveReadDir(sid); err != nil {
			return err
		}
		for _, item := range s.Archives(sid) {
			if time.Unix(item.At, 0).Before(now.AddDate(0, 0, -days)) {
				if err := s.DeleteArchive(sid, item.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (s *Store) DeleteAllHistory(sid string) error {
	if err := s.Reset(sid); err != nil {
		return err
	}
	return os.RemoveAll(s.archiveDir(sid))
}

// DeleteTurns 删掉第 from 到 to 轮（按历史文件里的用户消息数，从 0 起，含已整理掉的部分），重写历史文件，并重算 Base、占位表和计量
func (s *Store) DeleteTurns(sid string, from, to int) error {
	h := s.Archive(sid)
	var starts []int
	for i, m := range h {
		if m.Role == "user" {
			starts = append(starts, i)
		}
	}
	if from < 0 || to < from || to >= len(starts) {
		return fmt.Errorf("轮次范围不存在")
	}
	begin, end := starts[from], len(h)
	if to+1 < len(starts) {
		end = starts[to+1]
	}
	st := s.Settings(sid)
	old := st.Base
	removed := end - begin
	st.Base -= max(0, min(end, old)-min(begin, old))
	st.Meter = Meter{}
	st.EnvNote = ""
	omitted := map[int]string{}
	for i, v := range st.Omitted {
		if i < begin {
			omitted[i] = v
		} else if i >= end {
			omitted[i-removed] = v
		}
	}
	st.Omitted = omitted
	out := append(append([]Msg(nil), h[:begin]...), h[end:]...)
	var b []byte
	for _, m := range out {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		b = append(b, line...)
		b = append(b, '\n')
	}
	original, err := os.ReadFile(s.file(sid))
	if err != nil {
		return err
	}
	tmp := s.file(sid) + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, s.file(sid)); err != nil {
		return err
	}
	if err = s.SaveSettings(sid, st); err != nil {
		_ = os.WriteFile(s.file(sid), original, 0600)
		return err
	}
	s.hist[sid] = append([]Msg(nil), out[st.Base:]...)
	return nil
}
