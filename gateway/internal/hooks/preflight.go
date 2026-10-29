package hooks

import (
	"context"
	"errors"
	"fmt"
	"nano-agent/gateway/internal/eventlog"
	"sort"
	"time"
)

type CheckResult struct {
	Allow, RequireApproval bool
	Detail                 string
}
type preflight struct {
	ctx   context.Context
	event eventlog.Event
	done  func(CheckResult)
}

// Check joins the two existing command workers and their bounded queue. It
// never creates a model run, never executes inline, and never waits in App.mu.
func (e *Engine) Check(ctx context.Context, event eventlog.Event, done func(CheckResult)) bool {
	if ctx.Err() != nil || e.ctx.Err() != nil {
		return false
	}
	select {
	case e.commands <- &active{check: &preflight{ctx, event, done}}:
		return true
	default:
		return false
	}
}
func (e *Engine) runCheck(check *preflight) {
	result := CheckResult{Allow: true}
	defer func() { check.done(result) }()
	ctx, event := check.ctx, check.event
	deny := func(err error) { result.Allow = false; result.Detail = err.Error() }
	if err := ctx.Err(); err != nil {
		deny(err)
		return
	}
	if err := e.store.Commit(ctx, eventlog.Transaction{Event: &event}); err != nil {
		deny(err)
		return
	}
	type match struct {
		record   eventlog.RuleRecord
		spec     Spec
		delivery eventlog.Delivery
	}
	var matches []match
	for _, r := range e.store.Rules(event.AgentID) {
		if !r.Enabled || r.Deleted || r.ID == event.OriginHook || event.Depth >= 4 || r.Once && r.ClaimedBy != "" && r.ClaimedBy != event.ID {
			continue
		}
		spec, err := Parse(r)
		if err != nil || !containsBefore(spec.On) || !Matches(spec, event) || !e.executor.Visible(r, event, spec) {
			continue
		}
		matches = append(matches, match{record: r, spec: spec})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].spec.Priority != matches[j].spec.Priority {
			return matches[i].spec.Priority < matches[j].spec.Priority
		}
		return matches[i].record.ID < matches[j].record.ID
	})
	deliveries := []eventlog.Delivery{}
	now := e.clock().UnixMilli()
	for i := range matches {
		m := &matches[i]
		r := m.record
		m.delivery = eventlog.Delivery{ID: eventlog.DeliveryID(event.ID, r.ID, r.Revision), EventID: event.ID, HookID: r.ID, Revision: r.Revision,
			AgentID: r.AgentID, Creator: r.Creator, Silent: r.Silent || event.Silent, TargetSID: m.spec.Scope.SID, Rule: r.Body, Status: "pending", Registered: true, Preflight: true, UpdatedMS: now}
		deliveries = append(deliveries, m.delivery)
	}
	if err := e.store.Commit(ctx, eventlog.Transaction{Match: &eventlog.Match{EventID: event.ID, Checked: true, Deliveries: deliveries}}); err != nil {
		deny(err)
		return
	}
	for _, m := range matches {
		d := m.delivery
		if !result.Allow {
			d.Status = "cancelled"
			d.Error = "前面的 Hook 已拒绝"
			_ = e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d})
			continue
		}
		if old, ok := e.store.Delivery(d.ID); ok && old.Status != "pending" {
			// A duplicate check may inspect its own recorded decision, never rerun
			// a command whose external outcome has not been established.
			if old.Status == "succeeded" {
				switch old.Result {
				case "allow":
				case "approval":
					result.RequireApproval = true
				default:
					deny(errors.New("前置 Hook 已拒绝"))
				}
			} else {
				deny(errors.New("前置 Hook 尝试已存在，未重新执行"))
			}
			continue
		}
		err := ctx.Err()
		e.mu.Lock()
		busy := e.hooks[d.HookID]
		if !busy {
			e.hooks[d.HookID] = true
		}
		e.mu.Unlock()
		if busy {
			err = errors.New("该 Hook 已有一次执行，前置检查未占用额外并发")
		}
		if err == nil && e.store.AttemptsSince(d.HookID, e.clock().Add(-time.Minute).UnixMilli()) >= 16 {
			err = errors.New("前置 Hook 每分钟触发上限已到")
		}
		var action Result
		if err == nil {
			d.Status = "running"
			d.Attempt++
			d.UpdatedMS = e.clock().UnixMilli()
			err = e.store.Commit(ctx, eventlog.Transaction{Delivery: &d})
		}
		if err == nil {
			action.Decision = m.spec.Handler.Decision
			if action.Decision == "" {
				attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(min(2000, m.spec.Handler.TimeoutMS))*time.Millisecond)
				done := make(chan Result, 1)
				e.executor.Execute(attemptCtx, Execution{d, event, m.spec}, func(r Result) {
					select {
					case done <- r:
					default:
					}
				})
				select {
				case action = <-done:
				case <-attemptCtx.Done():
					action.Err = attemptCtx.Err()
				}
				cancel()
			}
			err = action.Err
		}
		if !busy {
			e.mu.Lock()
			delete(e.hooks, d.HookID)
			e.mu.Unlock()
		}
		if err == nil && action.Decision != "allow" && action.Decision != "deny" && action.Decision != "approval" {
			err = errors.New("前置命令需要 decision=allow/deny/approval")
		}
		d.UpdatedMS = e.clock().UnixMilli()
		if err != nil {
			d.Status = "failed"
			d.Error = fmt.Sprintf("%.400s", err.Error())
			if m.spec.OnError != "allow" {
				deny(err)
			}
		} else {
			d.Status = "succeeded"
			d.Result = action.Decision
			if action.Decision == "deny" {
				deny(errors.New("前置 Hook " + d.HookID + " 拒绝，未执行"))
			}
			if action.Decision == "approval" {
				result.RequireApproval = true
			}
		}
		if commitErr := e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d}); commitErr != nil {
			deny(commitErr)
		}
		if observer, ok := e.executor.(interface{ Changed(eventlog.Delivery) }); ok {
			observer.Changed(d)
		}
	}
}
