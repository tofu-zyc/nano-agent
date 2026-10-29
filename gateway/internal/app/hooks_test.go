package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createAppHook(t *testing.T, e *env, handler hooks.Handler, once bool) HookView {
	t.Helper()
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	spec := hooks.Spec{Name: "测试", On: []string{"device.online"}, Handler: handler}
	out, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "one", Spec: &spec, Once: &once})
	if err != nil || out.Rule == nil {
		t.Fatal(out, err)
	}
	return *out.Rule
}
func appHookEvent(t *testing.T, e *env, seq uint64, origin string) eventlog.Event {
	t.Helper()
	event := eventlog.Event{Name: "device.online", AgentID: "default", OriginSID: origin, SourceID: "fixture", SourceEpoch: "boot", Seq: seq, OccurredAt: 100, ObservedAt: 100}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Event: &event}); err != nil {
		t.Fatal(err)
	}
	e.a.hookEngine.Wake()
	return event
}
func hookDelivery(t *testing.T, e *env, rule HookView, event eventlog.Event, status string) eventlog.Delivery {
	t.Helper()
	id := eventlog.DeliveryID(event.ID, rule.ID, rule.Revision)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		d, ok := e.a.jobEvents.Delivery(id)
		if ok && d.Status == status {
			return d
		}
		time.Sleep(time.Millisecond)
	}
	d, _ := e.a.jobEvents.Delivery(id)
	t.Fatalf("Hook 未到 %s: %+v / engine %v", status, d, e.a.hookEngine.Err())
	return d
}
func TestHookAppNotifyUsesReliableEventAndFrozenEndpoint(t *testing.T) {
	e := setup(t)
	r := createAppHook(t, e, hooks.Handler{Type: "notify", Text: "设备 {{event_name}}"}, true)
	event := appHookEvent(t, e, 1, mainSID)
	e.ch.wait(t, "设备 device.online")
	hookDelivery(t, e, r, event, "succeeded")
	if got := e.a.Schedules(); len(got.Tasks) != 0 {
		t.Fatal("Hook created Schedule", got)
	}
	e.a.mu.Lock()
	history := e.a.store.History(mainSID)
	e.a.mu.Unlock()
	if len(history) != 0 {
		t.Fatal("fixed notification polluted chat model history", history)
	}
	if r, _ := e.a.jobEvents.Rule(r.ID); r.Enabled {
		t.Fatal("once not consumed")
	}
	// A later endpoint reassignment revokes this delivery target; it cannot use
	// mainOf/creator fallback to send the event to the newly attached chat.
	e2 := setup(t)
	r2 := createAppHook(t, e2, hooks.Handler{Type: "notify", Text: "不能发送"}, false)
	e2.a.mu.Lock()
	e2.a.eps["fake:c1"].Session = "other"
	e2.a.mu.Unlock()
	event2 := appHookEvent(t, e2, 1, mainSID)
	hookDelivery(t, e2, r2, event2, "retry")
	select {
	case text := <-e2.ch.sent:
		t.Fatal("retargeted notification", text)
	default:
	}
}
func TestHookAppTemporaryCProtocolKeepsOriginalHistoryAndUnloads(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "source secret, not copied"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()
	r := createAppHook(t, e, hooks.Handler{Type: "agent", Conversation: "temporary", Prompt: "分析这次事件", TokenBudget: 1000}, true)
	event := appHookEvent(t, e, 1, mainSID)
	in := e.k.expect(proto.TInput)
	if !isHookSID(in.SID) || in.From != "fake:u1" || in.OwnerAgent != "default" || in.Mode != "ask" {
		t.Fatal(in)
	}
	if !fixedAgentSID(in.SID) {
		t.Fatal("switchable Hook identity")
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	request := e.llm.lastReq()
	raw, _ := json.Marshal(request)
	if strings.Contains(string(raw), "source secret") || !strings.Contains(string(raw), "分析这次事件") {
		t.Fatal(string(raw))
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")
	hookDelivery(t, e, r, event, "succeeded")
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if e.a.sessions[in.SID] != nil {
		t.Fatal("temporary App session cache retained")
	}
	if h := e.a.store.History(mainSID); len(h) != 1 || h[0].Content != "source secret, not copied" {
		t.Fatal(h)
	}
	histories, _, _ := e.a.store.CacheUsage()
	if histories > 1 {
		t.Fatal("temporary Store cache retained", histories)
	}
}
func TestHookAppRuleCASAndPermissions(t *testing.T) {
	e := setup(t)
	r := createAppHook(t, e, hooks.Handler{Type: "notify", Text: "x"}, false)
	if _, err := e.a.HookAction(context.Background(), mainSID, "fake:guest", HookRequest{Action: "list"}); err == nil {
		t.Fatal("guest")
	}
	if _, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "disable", ID: r.ID, Revision: 9}); err == nil {
		t.Fatal("stale")
	}
	out, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "delete", ID: r.ID, Revision: r.Revision})
	if err != nil || !out.Rule.Deleted {
		t.Fatal(out, err)
	}
	spec := hooks.Spec{On: []string{"device.online"}, Handler: hooks.Handler{Type: "notify", Text: "x"}}
	if _, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", ID: "different", OperationID: "one", Spec: &spec}); err == nil {
		t.Fatal("revoked operation changed identity")
	}
}
func TestHookScriptPinnedInsideAgentDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "default", "hooks")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte("cat"), 0600)
	raw, err := hookScript(root, "default", "check.sh")
	if err != nil || scriptDigest(raw) == "" {
		t.Fatal(string(raw), err)
	}
	for _, name := range []string{"../../../secret", filepath.Join(root, "outside"), "ads:stream"} {
		if _, err := hookScript(root, "default", name); err == nil {
			t.Fatal(name)
		}
	}
}

type hookRealHosts struct {
	local    host.Host
	fallback *fakeHosts
}

func (h hookRealHosts) Host(name string) (host.Host, host.Info, bool) {
	if name == host.LocalName {
		return h.local, host.Info{Name: name, Kind: "local", Ready: true, Landlock: 7, OS: "linux"}, true
	}
	return h.fallback.Host(name)
}
func (h hookRealHosts) Infos() []host.Info { return h.fallback.Infos() }
func TestHookCommandActuallyReadsEventInCExecutor(t *testing.T) {
	executable, err := host.NewExecutor("../../../kernel/nano-exec")
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	e.a.SetHosts(hookRealHosts{host.NewLocal(executable, nil), e.hosts})
	e.a.mu.Lock()
	e.a.cfg.Workspace.Mode = "auto"
	e.a.cfg.Sandbox.Mode = "off"
	e.a.mu.Unlock()
	handler := hooks.Handler{Type: "command", Command: `IFS= read -r event || true; case "$event" in *device.online*) printf '{"trigger":true,"summary":"C 读取到了真实事件"}';; *) exit 1;; esac`}
	r := createAppHook(t, e, handler, true)
	event := appHookEvent(t, e, 1, mainSID)
	e.ch.wait(t, "C 读取到了真实事件")
	result := hookDelivery(t, e, r, event, "succeeded")
	if result.Attempt != 1 {
		t.Fatal(result)
	}
}
func TestHookSilentEventKeepsModelApprovalAndResultAwayFromSource(t *testing.T) {
	e := setup(t)
	r := createAppHook(t, e, hooks.Handler{Type: "agent", Conversation: "temporary", Prompt: "分析安静事件"}, true)
	event := eventlog.Event{Name: "device.online", AgentID: "default", OriginSID: mainSID, SourceID: "silent", SourceEpoch: "boot", Seq: 1, OccurredAt: 100, ObservedAt: 100, Silent: true}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Event: &event}); err != nil {
		t.Fatal(err)
	}
	e.a.hookEngine.Wake()
	in := e.k.expect(proto.TInput)
	if !in.Silent || in.From != "fake:u1" {
		t.Fatal(in)
	}
	e.k.send(proto.Msg{T: proto.TApproveReq, SID: in.SID, ID: "write", Name: "Write", Args: json.RawMessage(`{"path":"x","content":"x"}`)})
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	hookDelivery(t, e, r, event, "succeeded")
	select {
	case text := <-e.ch.sent:
		t.Fatal("silent Hook pushed to original chat", text)
	default:
	}
}

type preflightShellModel struct{ command string }

func (m preflightShellModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "preflight finished: " + req.Messages[len(req.Messages)-1].Content}, nil
	}
	args, _ := json.Marshal(map[string]string{"command": m.command})
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "guarded-shell", Type: "function", Function: llm.FunctionCall{Name: "Shell", Arguments: string(args)}}}}, nil
}
func TestHookRealCPreflightDenialAndExtraApprovalInFullMode(t *testing.T) {
	for _, decision := range []string{"deny", "approval"} {
		t.Run(decision, func(t *testing.T) {
			e, _ := setupRealKernel(t)
			work := filepath.Join(e.dir, "home", "guarded-work")
			if err := os.MkdirAll(work, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(work, "ran")
			e.a.mu.Lock()
			node := ""
			for name := range e.a.nodes {
				node = name
				break
			}
			ws := Workspace{ID: "guarded-work", Host: e.a.machineOf(node), Path: work, SandboxMode: "off"}
			e.a.wss = []Workspace{ws}
			settings := e.a.store.Settings(mainSID)
			settings.Mode = "full"
			settings.WorkspacesSet = true
			settings.Workspaces = []string{ws.ID}
			err := e.a.store.SaveSettings(mainSID, settings)
			e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
			e.a.llm = preflightShellModel{command: "printf ran > '" + marker + "'"}
			e.a.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			spec := hooks.Spec{Name: "guard", On: []string{"tool.before"}, Handler: hooks.Handler{Type: "command", Decision: decision}}
			out, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "guard", Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			e.say("execute guarded shell")
			if decision == "approval" {
				e.ch.wait(t, "需要审批")
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("executed before extra approval", err)
				}
				approvals := e.a.Approvals()
				if len(approvals) != 1 {
					t.Fatal(approvals)
				}
				if err := e.a.Approve(approvals[0].N, true); err != nil {
					t.Fatal(err)
				}
			}
			e.ch.wait(t, "preflight finished")
			if decision == "deny" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("denied Shell executed", err)
				}
			} else if content, err := os.ReadFile(marker); err != nil || string(content) != "ran" {
				t.Fatal(string(content), err)
			}
			runs, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "runs", ID: out.Rule.ID})
			if err != nil || len(runs.Deliveries) != 1 || runs.Deliveries[0].Result != decision {
				t.Fatal(runs, err)
			}
		})
	}
}

func TestHookPreflightLateVerdictAfterStopHasNoEffect(t *testing.T) {
	e, _ := setupRealKernel(t)
	work := filepath.Join(e.dir, "home", "stale-work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(work, "ran")
	e.a.mu.Lock()
	node := ""
	for name := range e.a.nodes {
		node = name
		break
	}
	ws := Workspace{ID: "stale-work", Host: e.a.machineOf(node), Path: work, SandboxMode: "off"}
	e.a.wss = []Workspace{ws}
	settings := e.a.store.Settings(mainSID)
	settings.Mode = "full"
	settings.WorkspacesSet = true
	settings.Workspaces = []string{ws.ID}
	err := e.a.store.SaveSettings(mainSID, settings)
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.llm = preflightShellModel{command: "printf ran > '" + marker + "'"}
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	spec := hooks.Spec{Name: "guard", On: []string{"tool.before"}, Handler: hooks.Handler{Type: "command", Decision: "approval"}}
	if _, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "stale-guard", Spec: &spec}); err != nil {
		t.Fatal(err)
	}
	e.say("execute guarded shell")
	e.ch.wait(t, "需要审批")
	e.a.mu.Lock()
	runID := e.a.sessions[mainSID].run.runID
	e.a.mu.Unlock()
	e.say("/stop")
	e.ch.wait(t, "（已停止）")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("停止后仍然执行", err)
	}
	pending := len(e.a.Approvals())
	e.a.hub.Send(node, proto.Msg{T: proto.THookResult, SID: mainSID, RunID: runID, ID: "guarded-shell", RequestID: "guarded-shell",
		OK: proto.Bool(true), ForceApproval: true, Detail: "迟到的判断"})
	time.Sleep(300 * time.Millisecond)
	if got := len(e.a.Approvals()); got != pending {
		t.Fatal("迟到的判断复活了审批", pending, got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("迟到的判断执行了调用", err)
	}
	e.a.mu.Lock()
	running := e.a.sessions[mainSID].run != nil
	e.a.mu.Unlock()
	if running {
		t.Fatal("迟到的判断留下了新的 run")
	}
}
