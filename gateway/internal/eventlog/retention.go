package eventlog

import (
	"sort"
	"time"
)

// RetentionCandidates selects only fully settled facts. Source cursors and
// terminal Job identities remain the deduplication authority after pruning.
func (s *Store) RetentionCandidates(now time.Time, perAgent, days, limit int) []string {
	type header struct {
		id, agent string
		at        int64
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var rows []header
	for _, e := range s.events {
		if !e.Matched {
			continue
		}
		settled := true
		for _, id := range e.DeliveryIDs {
			if d, ok := s.deliveries[id]; !ok || !deliveryDone(d.Status) {
				settled = false
				break
			}
		}
		if settled {
			rows = append(rows, header{e.ID, e.AgentID, e.ObservedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].at != rows[j].at {
			return rows[i].at > rows[j].at
		}
		return rows[i].id > rows[j].id
	})
	count := map[string]int{}
	out := []string{}
	for _, e := range rows {
		count[e.agent]++
		if count[e.agent] > perAgent || e.at < now.AddDate(0, 0, -days).Unix() {
			out = append(out, e.id)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
func (s *Store) LastAttempt(hook, key string) (int64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	latest, count := int64(0), 0
	for _, d := range s.deliveries {
		if d.HookID != hook || d.Attempt == 0 {
			continue
		}
		if d.ObjectKey == key && d.UpdatedMS > latest {
			latest = d.UpdatedMS
		}
	}
	// Counts are based on persisted attempts, so restart does not reset a
	// trigger's rate allowance. The engine supplies the minute boundary below.
	return latest, count
}
func (s *Store) AttemptsSince(hook string, since int64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, d := range s.deliveries {
		if d.HookID == hook && d.Attempt > 0 && d.UpdatedMS >= since {
			count += d.Attempt
		}
	}
	return count
}
func (s *Store) Seen(source, epoch string, seq uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursors[cursorKey(source, epoch)].contains(seq)
}
