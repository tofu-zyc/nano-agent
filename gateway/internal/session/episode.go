package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"nano-agent/gateway/internal/llm"
	"os"
	"path/filepath"
)

// RawStats counts the persistent history without installing a history cache.
// Episode lists must not appear empty just because their model window moved.
func (s *Store) RawStats(sid string) (count int, last int64, usage llm.Usage) {
	if br, ok := s.Branch(sid); ok && br.SnapshotID != "" {
		if info, exists := s.Snapshot(br.SnapshotID); exists {
			stats := info.Stats
			if stats.Messages != info.Messages || (br.InheritedEndSet && br.InheritedEnd != info.Messages) {
				if f, err := os.Open(filepath.Join(s.snapshotDir(br.SnapshotID), "messages.jsonl")); err == nil {
					end := info.Messages
					if br.InheritedEndSet {
						end = br.InheritedEnd
					}
					stats, _ = scanHistoryStats(&prefixLines{reader: f, left: end})
					_ = f.Close()
				}
			}
			count, last, usage = stats.Messages, stats.LastAt, stats.Usage
		}
	}
	p, ok := s.readFileFor(sid, ".jsonl")
	if !ok {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		count++
		if m.TS > last {
			last = m.TS
		}
		if m.Usage != nil {
			usage.PromptTokens += m.Usage.PromptTokens
			usage.CachedTokens += m.Usage.CachedTokens
			usage.CompletionTokens += m.Usage.CompletionTokens
			usage.ReasoningTokens += m.Usage.ReasoningTokens
		}
	}
	return
}

func scanHistoryStats(r io.Reader) (HistoryStats, error) {
	var stats HistoryStats
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return stats, fmt.Errorf("历史记录损坏：%w", err)
		}
		if m.Role == "" {
			return stats, fmt.Errorf("历史记录缺少角色")
		}
		stats.Messages++
		stats.LastAt = max(stats.LastAt, m.TS)
		if m.Usage != nil {
			stats.Usage.PromptTokens += m.Usage.PromptTokens
			stats.Usage.CachedTokens += m.Usage.CachedTokens
			stats.Usage.CompletionTokens += m.Usage.CompletionTokens
			stats.Usage.ReasoningTokens += m.Usage.ReasoningTokens
		}
	}
	return stats, sc.Err()
}

// BeginEpisode starts a fresh model window while preserving the complete append
// only history for the viewer and Diary. The stable system/tool prefix survives.
func (s *Store) BeginEpisode(sid string) error {
	h := s.History(sid)
	st := s.Settings(sid)
	st.Base += len(h)
	st.Summary = ""
	st.Omitted = nil
	st.Meter = Meter{}
	st.EnvNote = ""
	st.MemNote = ""
	st.LifeNote = ""
	st.Recalled = nil
	st.CompactedAt = 0
	if err := s.SaveSettings(sid, st); err != nil {
		return err
	}
	s.hist[sid] = nil
	return nil
}
