package eventlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxRecord = maxTransaction + 4096

type record struct {
	Version int             `json:"version"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	Hash    string          `json:"sha256"`
}

func encode(seq uint64, p patch) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > maxTransaction {
		return nil, errors.New("可靠日志记录过大")
	}
	hash := sha256.Sum256(raw)
	line, err := json.Marshal(record{1, seq, raw, hex.EncodeToString(hash[:])})
	return append(line, '\n'), err
}
func (s *Store) replay() error {
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > s.limits.JournalBytes {
		return ErrCapacity
	}
	reader := bufio.NewReaderSize(s.file, maxRecord)
	var offset int64
	for {
		line, readErr := reader.ReadSlice('\n')
		if readErr != nil {
			if readErr == io.EOF {
				// Only an unterminated tail is discardable. A corrupt complete
				// record at any position freezes recovery and remains intact.
				if len(line) > 0 {
					if err = s.file.Truncate(offset); err != nil {
						return err
					}
					if err = s.file.Sync(); err != nil {
						return err
					}
				}
				break
			}
			return fmt.Errorf("偏移 %d 的记录超过上限或读取失败: %w", offset, readErr)
		}
		var entry record
		if err = json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("偏移 %d: %w", offset, err)
		}
		hash := sha256.Sum256(entry.Data)
		if entry.Version != 1 || entry.Seq != s.seq+1 || entry.Hash != hex.EncodeToString(hash[:]) {
			return fmt.Errorf("偏移 %d 的日志版本、顺序或校验损坏", offset)
		}
		var p patch
		if len(entry.Data) > maxTransaction || json.Unmarshal(entry.Data, &p) != nil || empty(p) {
			return fmt.Errorf("偏移 %d 的事务损坏", offset)
		}
		for _, c := range p.Cursors {
			if !c.valid() {
				return errors.New("事件来源游标损坏")
			}
			if old, ok := s.cursors[cursorKey(c.SourceID, c.Epoch)]; ok {
				if c.Through < old.Through {
					return errors.New("来源确认游标倒退")
				}
				for _, seq := range old.Seen {
					if !c.contains(seq) {
						return errors.New("来源确认记录丢失")
					}
				}
			}
		}
		for _, j := range p.Jobs {
			if err = jobValid(j.Job); err != nil {
				return err
			}
			if j.Reserved == terminal(j.Receipt.Status) || (j.Reserved && j.TerminalEventID != "") ||
				(!j.Reserved && j.TerminalEventID != EventID(j.SourceID+"/"+j.Receipt.JobID, j.Receipt.NodeEpoch, j.Receipt.LastEventSeq)) {
				return errors.New("任务终态预留或事件身份损坏")
			}
			if old, ok := s.jobs[jobKey(j.Job)]; ok {
				if identity(old.Job) != identity(j.Job) || j.Receipt.Revision < old.Receipt.Revision ||
					terminal(old.Receipt.Status) && !sameTerminal(old.Receipt, j.Receipt) {
					return errors.New("任务恢复记录覆盖了固定身份或终态")
				}
			}
		}
		for _, e := range p.Events {
			if err = e.valid(); err != nil {
				return err
			}
		}
		for _, d := range p.Deliveries {
			if err = d.valid(); err != nil {
				return err
			}
		}
		s.apply(p)
		if len(s.cursors) > s.limits.Sources || len(s.jobs) > s.limits.Jobs || len(s.events)+s.reserved > s.limits.Events ||
			len(s.deliveries) > s.limits.Deliveries || s.live+s.reserveBytes > s.limits.LiveBytes {
			return ErrCapacity
		}
		s.seq = entry.Seq
		offset += int64(len(line))
	}
	s.bytes = offset
	_, err = s.file.Seek(offset, io.SeekStart)
	return err
}
func (s *Store) checkState() error {
	for _, e := range s.events {
		if e.JobID == "" {
			c, ok := s.cursors[cursorKey(e.SourceID, e.SourceEpoch)]
			if !ok || !c.contains(e.Seq) {
				return errors.New("事件没有可靠来源确认游标")
			}
		}
		if e.JobID != "" {
			source := strings.TrimSuffix(e.SourceID, "/"+e.JobID)
			j, ok := s.jobs[cursorKey(source, e.JobID)]
			if !ok || j.TerminalEventID != e.ID || eventMatchesJob(e.Event, j.Job) != nil {
				return errors.New("任务和可靠终态记录未一起恢复")
			}
		}
		if !e.Matched && len(e.DeliveryIDs) > 0 {
			return errors.New("未匹配事件含有投递记录")
		}
		seen := map[string]bool{}
		for _, id := range e.DeliveryIDs {
			d, ok := s.deliveries[id]
			if !ok || d.EventID != e.ID || seen[id] {
				return errors.New("事件投递索引损坏")
			}
			seen[id] = true
		}
	}
	for _, d := range s.deliveries {
		e, ok := s.events[d.EventID]
		if !ok || !e.Matched {
			return errors.New("投递没有已匹配的事件来源")
		}
		found := false
		for _, id := range e.DeliveryIDs {
			if id == d.ID {
				found = true
			}
		}
		if !found {
			return errors.New("投递不在事件固定清单中")
		}
	}
	return nil
}
func (s *Store) append(p patch) error {
	line, err := encode(s.seq+1, p)
	if err != nil {
		return err
	}
	if s.bytes+int64(len(line)) > s.limits.JournalBytes || s.bytes > s.compactAt {
		if err = s.compact(); err != nil {
			return err
		}
		line, err = encode(s.seq+1, p)
		if err != nil {
			return err
		}
	}
	if s.bytes+int64(len(line)) > s.limits.JournalBytes {
		return ErrCapacity
	}
	if _, err = s.file.Write(line); err != nil {
		return err
	}
	if err = s.file.Sync(); err != nil {
		return err
	}
	s.mu.Lock()
	s.seq++
	s.bytes += int64(len(line))
	s.mu.Unlock()
	return nil
}
func (s *Store) compact() error {
	// Snapshot and journal share one publication point. A .next file never
	// becomes authoritative until its complete contents are synced/replaced.
	s.mu.RLock()
	cursors := make([]Cursor, 0, len(s.cursors))
	for _, c := range s.cursors {
		cursors = append(cursors, c)
	}
	jobs := make([]JobRecord, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	events := make([]EventRecord, 0, len(s.events))
	for _, e := range s.events {
		events = append(events, e)
	}
	deliveries := make([]Delivery, 0, len(s.deliveries))
	for _, d := range s.deliveries {
		deliveries = append(deliveries, d)
	}
	s.mu.RUnlock()
	next := s.path + ".next"
	file, err := os.OpenFile(next, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	var seq uint64
	var count int64
	write := func(p patch) error {
		line, err := encode(seq+1, p)
		if err != nil {
			return err
		}
		if count+int64(len(line)) > s.limits.JournalBytes {
			return ErrCapacity
		}
		n, err := file.Write(line)
		if err == nil {
			seq++
			count += int64(n)
		}
		return err
	}
	for _, c := range cursors {
		if err = write(patch{Cursors: []Cursor{c}}); err != nil {
			break
		}
	}
	if err == nil {
		for _, j := range jobs {
			if err = write(patch{Jobs: []JobRecord{j}}); err != nil {
				break
			}
		}
	}
	if err == nil {
		for _, e := range events {
			if err = write(patch{Events: []EventRecord{e}}); err != nil {
				break
			}
		}
	}
	if err == nil {
		for _, d := range deliveries {
			if err = write(patch{Deliveries: []Delivery{d}}); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Windows needs the old handle closed for atomic replacement. Any error
	// here freezes the store; restart replays whichever full file is published.
	if err = s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	if err = replaceJournal(next, s.path); err != nil {
		return err
	}
	if s.file, err = os.OpenFile(s.path, os.O_RDWR|os.O_APPEND, 0600); err != nil {
		return err
	}
	s.mu.Lock()
	s.seq = seq
	s.bytes = count
	s.compactAt = max(s.limits.CompactBytes, s.live*2)
	s.mu.Unlock()
	return nil
}

// Verify is a read-only diagnostic. Open performs tail recovery; Verify reports
// a torn tail without changing the file or accepting any new state.
func Verify(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, maxRecord)
	var seq uint64
	for {
		line, err := reader.ReadSlice('\n')
		if err == io.EOF && len(line) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("可靠日志不完整: %w", err)
		}
		var e record
		if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
			return errors.New("可靠日志 JSON 损坏")
		}
		hash := sha256.Sum256(e.Data)
		if e.Version != 1 || e.Seq != seq+1 || e.Hash != hex.EncodeToString(hash[:]) {
			return errors.New("可靠日志校验失败")
		}
		seq = e.Seq
	}
}
