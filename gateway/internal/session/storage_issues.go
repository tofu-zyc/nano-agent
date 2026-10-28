package session

import (
	"fmt"
	"sort"
)

type StorageIssue struct {
	Scope string `json:"scope"`
	Error string `json:"error"`
}

func (s *Store) RecordStorageIssue(scope string, err error) {
	if err == nil {
		return
	}
	if s.issues == nil {
		s.issues = map[string]string{}
	}
	if len(s.issues) < 100 || s.issues[scope] != "" {
		s.issues[scope] = err.Error()
	}
}
func (s *Store) StorageIssues() []StorageIssue {
	out := make([]StorageIssue, 0, len(s.issues))
	for scope, err := range s.issues {
		out = append(out, StorageIssue{scope, err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}
func (s *Store) Check(sid string) error {
	for _, suffix := range []string{".jsonl", ".settings.json", ".branch.json"} {
		if _, _, err := s.locate(sid, suffix); err != nil {
			s.RecordStorageIssue(sid, err)
			return fmt.Errorf("会话存储未完成恢复：%w", err)
		}
	}
	delete(s.issues, sid)
	return nil
}
