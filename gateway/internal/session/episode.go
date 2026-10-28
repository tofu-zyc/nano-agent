package session

import (
	"bufio"
	"encoding/json"
	"nano-agent/gateway/internal/llm"
	"os"
)

// RawStats counts the persistent history without installing a history cache.
// Episode lists must not appear empty just because their model window moved.
func (s *Store) RawStats(sid string) (count int, last int64, usage llm.Usage) {
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
