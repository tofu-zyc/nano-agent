package session

import (
	"fmt"
	"strings"
	"time"
)

// EffectiveHistory applies placeholders without touching the append-only journal.
func (s *Store) EffectiveHistory(sid string) []Msg {
	h := append([]Msg(nil), s.History(sid)...)
	st := s.Settings(sid)
	for i := range h {
		if text, ok := st.Omitted[st.Base+i]; ok {
			h[i].Content = text
		}
	}
	return h
}
func ProtectedStart(h []Msg, keep int) int {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			keep--
			if keep <= 0 {
				return i
			}
		}
	}
	return 0
}
func (s *Store) Elide(sid string, keep int) error {
	h := s.History(sid)
	st := s.Settings(sid)
	end := ProtectedStart(h, keep)
	omitted := map[int]string{}
	for k, v := range st.Omitted {
		omitted[k] = v
	}
	changed := false
	names := map[string]string{}
	for i, m := range h[:end] {
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name + " " + tc.Function.Arguments
		}
		if m.Role == "tool" && len(m.Content) > 2048 {
			idx := st.Base + i
			if _, ok := omitted[idx]; !ok {
				omitted[idx] = fmt.Sprintf("[已省略：%s 的输出，%dKB。需要时重新读取]", names[m.ToolCallID], len(m.Content)/1024)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	st.Omitted = omitted
	st.Meter = Meter{}
	return s.SaveSettings(sid, st)
}
func PlanCut(h []Msg, budget, keep int) int { return min(CutPoint(h, budget), ProtectedStart(h, keep)) }
func (s *Store) CommitCompact(sid string, cut int, summary string, now time.Time) error {
	h := s.History(sid)
	if cut <= 0 || cut > len(h) {
		return nil
	}
	st := s.Settings(sid)
	st.Base += cut
	if summary != "" {
		st.Summary = "[系统] 更早对话的摘要：\n" + summary
	} else if !strings.Contains(st.Summary, "本次省略了旧对话") {
		st.Summary += "\n[系统] 本次省略了旧对话，因为太长已经省略。"
	}
	if st.EnvNote != "" && !strings.Contains(st.Summary, st.EnvNote) {
		st.Summary += "\n" + st.EnvNote
	}
	st.Meter = Meter{}
	st.Prefix = nil
	st.CompactedAt = now.Unix()
	if err := s.SaveSettings(sid, st); err != nil {
		return err
	}
	s.hist[sid] = append([]Msg(nil), h[cut:]...)
	return nil
}
