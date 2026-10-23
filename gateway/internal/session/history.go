package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func archiveKey(sid string) string {
	if sid == "." || sid == ".." {
		return "%2E" + url.PathEscape(sid)
	}
	return url.PathEscape(sid)
}
func (s *Store) archiveDir(sid string) string {
	return filepath.Join(s.dir, "archive", archiveKey(sid))
}
func (s *Store) ArchiveReset(sid string, now time.Time) error {
	file := s.file(sid)
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
	entries, _ := os.ReadDir(s.archiveDir(sid))
	out := []ArchiveInfo{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.archiveDir(sid), e.Name()))
		if err != nil {
			continue
		}
		n := 0
		for _, c := range b {
			if c == '\n' {
				n++
			}
		}
		info, _ := e.Info()
		out = append(out, ArchiveInfo{e.Name(), n, info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
func (s *Store) DeleteArchive(sid, id string) error {
	if id == "" || filepath.Base(id) != id || filepath.Ext(id) != ".jsonl" {
		return fmt.Errorf("归档 ID 不合法")
	}
	return os.Remove(filepath.Join(s.archiveDir(sid), id))
}
func (s *Store) PruneArchives(now time.Time, days int) error {
	if days == 0 {
		return nil
	}
	root := filepath.Join(s.dir, "archive")
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".jsonl" && info.ModTime().Before(now.AddDate(0, 0, -days)) {
			return os.Remove(path)
		}
		return nil
	})
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
