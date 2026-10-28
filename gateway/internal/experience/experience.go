// Package experience stores a bounded, derived index of actual agent activity.
// Chat history remains authoritative; no cross-file transaction is implied.
package experience

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/session"
)

const MaxEntries = 500
const maxLine = 64 << 10

type Entry struct {
	ID         string   `json:"id"`
	At         int64    `json:"at"`
	Kind       string   `json:"kind"`
	SID        string   `json:"sid,omitempty"`
	Where      string   `json:"where,omitempty"`
	Visible    string   `json:"visible"`
	RunID      string   `json:"run_id,omitempty"`
	Episode    string   `json:"episode,omitempty"`
	AgendaID   string   `json:"agenda_id,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Asked      string   `json:"asked,omitempty"`
	Did        []string `json:"did,omitempty"`
	Said       string   `json:"said,omitempty"`
	Status     string   `json:"status"`
	Open       []string `json:"open,omitempty"`
	Generation int64    `json:"generation,omitempty"`
	Sources    []string `json:"sources,omitempty"`
}

type record struct {
	Version int    `json:"version"`
	Seq     uint64 `json:"seq"`
	Entry   *Entry `json:"entry,omitempty"`
}

type Store struct {
	path    string
	seq     uint64
	entries []Entry
	records int
	broken  error
	lastErr error
}

func (s *Store) Error() error {
	if s.broken != nil {
		return s.broken
	}
	return s.lastErr
}

func Open(dir, agent string) (*Store, error) {
	p := filepath.Join(dir, "experience", session.Key(agent)+".jsonl")
	s := &Store{path: p}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		s.broken = err
		return s, err
	}
	r := bufio.NewReaderSize(f, maxLine)
	var offset int64
	partial := false
	for {
		line, readErr := r.ReadSlice('\n')
		if readErr == io.EOF {
			partial = len(line) > 0
			break
		}
		if readErr != nil {
			err = fmt.Errorf("experience read at %d: %w", offset, readErr)
			break
		}
		var rec record
		if json.Unmarshal(line, &rec) != nil || rec.Version != 1 || rec.Seq < s.seq {
			err = fmt.Errorf("experience corruption at %d", offset)
			break
		}
		s.seq = rec.Seq
		s.records++
		offset += int64(len(line))
		if rec.Entry != nil {
			s.fold(*rec.Entry)
		}
	}
	_ = f.Close()
	if err == nil && partial {
		var repair *os.File
		repair, err = os.OpenFile(p, os.O_WRONLY, 0600)
		if err == nil {
			err = repair.Truncate(offset)
			if err == nil {
				err = repair.Sync()
			}
			_ = repair.Close()
		}
	}
	s.broken = err
	return s, err
}

func (s *Store) fold(e Entry) {
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].ID == e.ID {
			s.entries[i] = e
			return
		}
	}
	s.entries = append(s.entries, e)
	if len(s.entries) > MaxEntries {
		s.entries = append([]Entry(nil), s.entries[len(s.entries)-MaxEntries:]...)
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (s *Store) Append(e Entry, now time.Time) (resultErr error) {
	defer func() { s.lastErr = resultErr }()
	if s.broken != nil {
		return s.broken
	}
	for _, old := range s.entries {
		for _, source := range e.Sources {
			for _, seen := range old.Sources {
				if source != "" && source == seen {
					return nil
				}
			}
		}
	}
	e.At = now.Unix()
	e.Asked = clip(e.Asked, 400)
	e.Said = clip(e.Said, 500)
	e.Reason = clip(e.Reason, 200)
	e.Where = clip(e.Where, 100)
	if len(e.Did) > 16 {
		e.Did = e.Did[:16]
	}
	for i := range e.Did {
		e.Did[i] = clip(e.Did[i], 200)
	}
	if len(e.Open) > 16 {
		e.Open = e.Open[:16]
	}
	if len(e.Sources) > 16 {
		e.Sources = e.Sources[:16]
	}
	seq := s.seq + 1
	e.ID = fmt.Sprintf("x-%d", seq)
	if len(s.entries) > 0 && e.Kind == "chat" {
		old := s.entries[len(s.entries)-1]
		if old.Kind == "chat" && old.SID == e.SID && old.Generation == e.Generation && old.Visible == e.Visible && e.At-old.At >= 0 && e.At-old.At < 600 {
			e.ID = old.ID
			e.Asked = clip(old.Asked+"；"+e.Asked, 400)
			e.Did = append(append([]string(nil), old.Did...), e.Did...)
			if len(e.Did) > 16 {
				e.Did = e.Did[len(e.Did)-16:]
			}
			e.Sources = append(append([]string(nil), old.Sources...), e.Sources...)
			if len(e.Sources) > 16 {
				e.Sources = e.Sources[len(e.Sources)-16:]
			}
		}
	}
	b, err := json.Marshal(record{Version: 1, Seq: seq, Entry: &e})
	if err != nil {
		return err
	}
	if len(b) >= maxLine {
		return errors.New("experience entry too large")
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		if rollback := f.Truncate(st.Size()); rollback != nil {
			s.broken = rollback
		} else if syncErr := f.Sync(); syncErr != nil {
			s.broken = syncErr
		}
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		s.broken = closeErr
		return closeErr
	}
	s.seq = seq
	s.records++
	s.fold(e)
	if s.records > MaxEntries*2 || (len(s.entries) > 0 && s.entries[0].At < now.AddDate(0, 0, -30).Unix()) {
		return s.checkpoint(now)
	}
	return nil
}

func (s *Store) checkpoint(now time.Time) error {
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.At >= now.AddDate(0, 0, -30).Unix() {
			entries = append(entries, e)
		}
	}
	f, err := os.OpenFile(s.path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	err = enc.Encode(record{Version: 1, Seq: s.seq})
	for i := range entries {
		if err == nil {
			err = enc.Encode(record{Version: 1, Seq: s.seq, Entry: &entries[i]})
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(s.path+".tmp", s.path)
	}
	if err != nil {
		return err
	}
	s.entries = entries
	s.records = len(entries) + 1
	return nil
}

func (s *Store) Recent(v knowledge.Viewer, sid string, limit int, now time.Time) []Entry {
	limit = min(max(limit, 1), MaxEntries)
	out := make([]Entry, 0, limit)
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.entries[i]
		if e.At < now.AddDate(0, 0, -30).Unix() {
			continue
		}
		if sid != "" && e.SID == sid && (e.Kind == "chat" || e.Kind == "lifecycle") {
			continue
		}
		if !(knowledge.Entry{Visible: e.Visible}).VisibleTo(v) {
			continue
		}
		e.Did = append([]string(nil), e.Did...)
		e.Open = append([]string(nil), e.Open...)
		e.Sources = append([]string(nil), e.Sources...)
		out = append(out, e)
	}
	return out
}

func Render(entries []Entry, loc *time.Location, maxRunes int) string {
	var b strings.Builder
	for _, e := range entries {
		status := "已结束"
		if e.Status != "ok" {
			status = "已结束（" + e.Status + "）"
		}
		line := fmt.Sprintf("- %s %s [%s]：", time.Unix(e.At, 0).In(loc).Format("01-02 15:04"), e.Where, e.Kind)
		if e.Asked != "" {
			line += "当时对方说「" + e.Asked + "」→ "
		}
		if e.Reason != "" {
			line += "起因：" + e.Reason + "；"
		}
		if len(e.Did) > 0 {
			line += "实际操作：" + strings.Join(e.Did, "；") + "；"
		}
		if e.Said != "" {
			line += "当时回复「" + e.Said + "」；"
		}
		if e.Episode != "" {
			line += "片段 " + e.Episode + "；"
		}
		if e.AgendaID != "" {
			line += "日程 " + e.AgendaID + "；"
		}
		if len(e.Open) > 0 {
			line += "已显式交接日程 " + strings.Join(e.Open, ",") + "（按日程触发，不重复执行原聊天）；"
		}
		line += status + "。\n"
		if len([]rune(b.String()+line)) > maxRunes {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
