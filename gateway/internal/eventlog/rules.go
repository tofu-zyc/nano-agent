package eventlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

const maxRules = 512

// RuleRecord keeps configuration and its once-consumption in the same journal
// as the fixed delivery set. Deleted identities remain tombstones.
type RuleRecord struct {
	ID          string          `json:"id"`
	Revision    uint64          `json:"revision"`
	AgentID     string          `json:"agent_id"`
	Creator     string          `json:"creator"`
	OperationID string          `json:"operation_id"`
	Enabled     bool            `json:"enabled"`
	Once        bool            `json:"once"`
	Silent      bool            `json:"silent"`
	Deleted     bool            `json:"deleted,omitempty"`
	ClaimedBy   string          `json:"claimed_by,omitempty"`
	Body        json.RawMessage `json:"body"`
}
type RuleChange struct {
	Record   RuleRecord `json:"record"`
	Expected uint64     `json:"expected_revision"`
	Rearm    bool       `json:"rearm,omitempty"`
}

func (r RuleRecord) valid() error {
	if r.ID == "" || len(r.ID) > 128 || r.Revision == 0 || r.AgentID == "" || len(r.AgentID) > 128 ||
		r.Creator == "" || len(r.Creator) > 256 || r.OperationID == "" || len(r.OperationID) > 128 ||
		len(r.ClaimedBy) > 128 || len(r.Body) > 4096 || !json.Valid(r.Body) || r.Deleted && r.Enabled {
		return errors.New("Hook 规则身份、修订或正文无效")
	}
	return nil
}
func sameRuleIdentity(a, b RuleRecord) bool {
	return a.ID == b.ID && a.AgentID == b.AgentID && a.Creator == b.Creator && a.OperationID == b.OperationID
}
func replayRule(old, next RuleRecord) error {
	if !sameRuleIdentity(old, next) || old.Deleted && !next.Deleted || next.Revision < old.Revision || next.Revision > old.Revision+1 {
		return errors.New("Hook 规则恢复覆盖固定身份、删除标记或修订")
	}
	if next.Revision == old.Revision {
		if !bytes.Equal(old.Body, next.Body) || old.Once != next.Once || old.Silent != next.Silent || old.Deleted != next.Deleted ||
			!old.Enabled && next.Enabled || old.ClaimedBy != "" && old.ClaimedBy != next.ClaimedBy {
			return errors.New("Hook 同修订配置被覆盖或消费记录倒退")
		}
	}
	return nil
}
func (s *Store) prepareRule(change RuleChange, p *patch) error {
	next := change.Record
	if err := next.valid(); err != nil {
		return err
	}
	old, exists := s.rules[next.ID]
	if !exists {
		if change.Expected != 0 || next.Revision != 1 || next.ClaimedBy != "" || next.Deleted {
			return errors.New("Hook 创建修订或初始状态无效")
		}
		for _, rule := range s.rules {
			if rule.OperationID == next.OperationID {
				return errors.New("Hook 创建操作已受理，不能换 ID 重建")
			}
		}
	} else {
		if old.Deleted || !sameRuleIdentity(old, next) || change.Expected != old.Revision || next.Revision != old.Revision+1 {
			return errors.New("Hook 已删除、权限身份改变或修订冲突，请重新读取")
		}
		next.ClaimedBy = old.ClaimedBy
		if change.Rearm {
			next.ClaimedBy = ""
		}
	}
	p.Rules = append(p.Rules, next)
	return nil
}
func (s *Store) claimRules(m Match, e EventRecord, p *patch) error {
	if !m.Checked || e.Matched {
		return nil
	}
	for _, d := range m.Deliveries {
		rule, ok := s.rules[d.HookID]
		if !ok || rule.Deleted || !rule.Enabled || rule.Revision != d.Revision || rule.AgentID != d.AgentID ||
			rule.Creator != d.Creator || !bytes.Equal(rule.Body, d.Rule) || rule.Silent && !d.Silent {
			return errors.New("Hook 匹配的规则修订、权限或固定正文已变化")
		}
		if rule.Once {
			if rule.ClaimedBy != "" && rule.ClaimedBy != e.ID {
				return errors.New("一次性 Hook 已被其他事件占用")
			}
			rule.ClaimedBy = e.ID
			p.Rules = append(p.Rules, rule)
		}
	}
	return nil
}
func (s *Store) Rule(id string) (RuleRecord, bool) {
	s.mu.RLock()
	r, ok := s.rules[id]
	s.mu.RUnlock()
	return copyValue(r), ok
}
func (s *Store) RuleOperation(op string) (RuleRecord, bool) {
	s.mu.RLock()
	var found RuleRecord
	ok := false
	for _, r := range s.rules {
		if r.OperationID == op {
			found = r
			ok = true
			break
		}
	}
	s.mu.RUnlock()
	return copyValue(found), ok
}
func (s *Store) Rules(agent string) []RuleRecord {
	s.mu.RLock()
	out := make([]RuleRecord, 0, len(s.rules))
	for _, r := range s.rules {
		if !r.Deleted && (agent == "" || r.AgentID == agent) {
			out = append(out, r)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return copyValue(out)
}

// Deliveries returns a bounded scheduling/inspection page; bodies are copied
// only for the requested page, never all pending scripts/conversations.
func (s *Store) Deliveries(after string, limit int) ([]Delivery, string) {
	limit = min(max(limit, 1), 128)
	s.mu.RLock()
	rows := make([]Delivery, 0, limit+1)
	for _, d := range s.deliveries {
		if d.ID <= after {
			continue
		}
		at := sort.Search(len(rows), func(i int) bool { return rows[i].ID >= d.ID })
		if at > limit {
			continue
		}
		rows = append(rows, Delivery{})
		copy(rows[at+1:], rows[at:])
		rows[at] = d
		if len(rows) > limit+1 {
			rows = rows[:limit+1]
		}
	}
	s.mu.RUnlock()
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].ID
	}
	return copyValue(rows), next
}

func (s *Store) RuleVersion() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.ruleVersion }
