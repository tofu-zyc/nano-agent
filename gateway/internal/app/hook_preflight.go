package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/proto"
	"time"
)

// Caller holds App.mu; accepted tools still wait for their whole batch. This
// is a pre-execution gate, not a new model round or an interjection shortcut.
func (a *App) preToolCheck(s *sess, m *proto.Msg) {
	r := s.run
	reply := func(result hooks.CheckResult) {
		a.hub.Send(r.node, proto.Msg{T: proto.THookResult, SID: s.sid, RunID: r.runID, TurnID: m.TurnID, ID: m.ID, RequestID: m.RequestID,
			OK: proto.Bool(result.Allow), ForceApproval: result.RequireApproval, Detail: result.Detail})
	}
	call, ok := r.callArgs[m.ID]
	if !ok || call.Name != m.Name || m.RequestID != m.ID || m.RunID != r.runID || m.TurnID != r.turnID || a.hookEngine == nil {
		reply(hooks.CheckResult{Detail: "前置检查调用身份无效"})
		return
	}
	if r.execs[m.ID] != nil {
		reply(hooks.CheckResult{Detail: "这个调用已有前置检查或执行"})
		return
	}
	data := map[string]any{"tool": call.Name, "call_id": m.ID, "turn_id": m.TurnID, "elevated": m.Elevated}
	var arguments map[string]any
	if json.Unmarshal(call.Args, &arguments) == nil {
		if workspace, ok := arguments["workspace"].(string); ok {
			data["workspace_id"] = workspace
		}
		if len(call.Args) <= 1200 {
			data["args"] = json.RawMessage(call.Args)
		} else {
			data["args_truncated"] = true
			data["args_ref"] = m.ID
		}
	}
	raw, _ := json.Marshal(data)
	a.runtimeEventSeq++
	now := a.now().Unix()
	event := eventlog.Event{Name: "tool.before", SourceID: "gateway", SourceEpoch: a.dispatchBoot, Seq: a.runtimeEventSeq, OccurredAt: now, ObservedAt: now,
		AgentID: a.execAgentID(s.sid), OriginSID: s.sid, RunID: r.runID, Silent: a.branchSilent(s.sid), Data: raw}
	if r.hook != nil {
		event.OriginHook = r.hook.work.Delivery.HookID
		event.CausationID = r.hook.work.Event.ID
		event.Depth = r.hook.work.Event.Depth + 1
	}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	ctx, cancel := context.WithTimeout(a.ctx, 1900*time.Millisecond)
	if r.execs == nil {
		r.execs = map[string]context.CancelFunc{}
	}
	r.execs[m.ID] = cancel
	queued := a.hookEngine.Check(ctx, event, func(result hooks.CheckResult) {
		defer cancel()
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(r.execs, m.ID)
		if s.run != r || a.ctx.Err() != nil {
			return
		}
		reply(result)
	})
	if !queued {
		delete(r.execs, m.ID)
		cancel()
		reply(hooks.CheckResult{Detail: "前置 Hook 队列已满，未执行"})
	}
}

func (a *App) hasBeforeHooks(sid string) bool {
	if a.jobEvents == nil {
		return false
	}
	source := sid
	if inv := a.sessionHook(sid); inv != nil {
		source = inv.work.Spec.Scope.SID
	}
	for _, r := range a.jobEvents.Rules(a.execAgentID(sid)) {
		if !r.Enabled || r.Deleted || r.Once && r.ClaimedBy != "" {
			continue
		}
		spec, err := hooks.Parse(r)
		if err == nil && spec.Scope.SID == source {
			for _, name := range spec.On {
				if name == "tool.before" {
					return true
				}
			}
		}
	}
	return false
}
