package hooks

import (
	"container/heap"
	"fmt"
	"nano-agent/gateway/internal/eventlog"
	"time"
)

type deadline struct {
	at     int64
	record eventlog.RuleRecord
	spec   Spec
}
type deadlines []deadline

func (d deadlines) Len() int { return len(d) }
func (d deadlines) Less(i, j int) bool {
	if d[i].at != d[j].at {
		return d[i].at < d[j].at
	}
	return d[i].record.ID < d[j].record.ID
}
func (d deadlines) Swap(i, j int) { d[i], d[j] = d[j], d[i] }
func (d *deadlines) Push(v any)   { *d = append(*d, v.(deadline)) }
func (d *deadlines) Pop() any {
	old := *d
	v := old[len(old)-1]
	old[len(old)-1] = deadline{}
	*d = old[:len(old)-1]
	return v
}
func (e *Engine) refreshRules() {
	version := e.store.RuleVersion()
	if version == e.ruleVersion {
		return
	}
	e.rules = e.store.Rules("")
	e.ruleVersion = version
	e.deadlines = nil
	for _, record := range e.rules {
		if !record.Enabled || record.Deleted || record.Once && record.ClaimedBy != "" {
			continue
		}
		spec, err := Parse(record)
		if err != nil || spec.DeadlineAt <= 0 {
			continue
		}
		if !Matches(spec, eventlog.Event{Name: "deadline.at"}) {
			continue
		}
		epoch := fmt.Sprintf("%s-r%d", record.ID, record.Revision)
		if e.store.Seen("deadline", epoch, 1) {
			continue
		}
		heap.Push(&e.deadlines, deadline{spec.DeadlineAt, record, spec})
	}
}
func (e *Engine) due(now time.Time) {
	for n := 0; n < 16 && e.deadlines.Len() > 0 && e.deadlines[0].at <= now.UnixMilli(); n++ {
		d := heap.Pop(&e.deadlines).(deadline)
		epoch := fmt.Sprintf("%s-r%d", d.record.ID, d.record.Revision)
		event := eventlog.Event{Name: "deadline.at", SourceID: "deadline", SourceEpoch: epoch, Seq: 1, AgentID: d.record.AgentID,
			OriginSID: d.spec.Scope.SID, Silent: d.record.Silent, OccurredAt: max(1, d.at/1000), ObservedAt: now.Unix()}
		event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
		if err := e.store.Commit(e.ctx, eventlog.Transaction{Event: &event}); err != nil {
			heap.Push(&e.deadlines, d)
			e.fail(err)
			return
		}
	}
	if now.UnixMilli() >= e.nextPrune {
		ids := e.store.RetentionCandidates(now, 100, 7, 16)
		if len(ids) > 0 {
			if err := e.store.Commit(e.ctx, eventlog.Transaction{Forget: ids}); err != nil {
				e.fail(err)
			}
		}
		e.nextPrune = now.Add(time.Minute).UnixMilli()
	}
}
