package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"strings"
	"testing"
)

func TestSandboxPrecedenceFrozenRemoteRequestAndActualResult(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.cfg.Sandbox.Mode = "off"
	e.a.nodes["test"].info.Features = map[string]int{"sandbox_policy": 2}
	e.a.mu.Unlock()
	if _, err := e.a.UpdateAgent("default", AgentPatch{Settings: map[string]any{"sandbox_mode": "on"}}); err != nil {
		t.Fatal(err)
	}
	remote := e.hosts.set("remote", "ssh", true, 7, "")
	remote.result = host.Result{OK: true, Content: "actual", Sandbox: &proto.Sandbox{Effective: "none"}}
	ws, err := e.a.AddWorkspace(context.Background(), "remote", e.dir, true, "off")
	if err != nil {
		t.Fatal(err)
	}
	e.say("work")
	input := e.k.expect(proto.TInput)
	if input.SandboxMode != "on" || len(input.Workspaces) != 1 || input.Workspaces[0].SandboxMode != "off" {
		t.Fatalf("wrong precedence: %+v", input)
	}
	// Edits apply to subsequent runs. Neither the coordinator frame nor model
	// arguments may override the accepted target's frozen request.
	if err := e.a.SetWorkspaceSandbox(ws.ID, "on"); err != nil {
		t.Fatal(err)
	}
	e.k.send(proto.Msg{T: proto.TExec, SID: e.sid, RunID: input.RunID, ID: "c", Name: "Shell", Workspace: ws.Label(), SandboxMode: "on", Args: json.RawMessage(`{"command":"echo actual","sandbox_mode":"on"}`)})
	result := e.k.expect(proto.TExecResult)
	if got := remote.lastReq(t)["sandbox"]; got != "off" {
		t.Fatalf("request overridden: %v", got)
	}
	if result.Sandbox == nil || result.Sandbox.Mode != "off" || result.Sandbox.Requested != "off" || result.Sandbox.Effective != "none" {
		t.Fatalf("lost actual state: %+v", result)
	}
	result.T = proto.TResult
	e.k.send(result)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, RunID: input.RunID, Status: proto.StatusOK})
	e.ch.wait(t, "模型没有返回内容")
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	h := e.a.store.History(e.sid)
	found := false
	for _, m := range h {
		if m.ToolCallID == "c" && m.Sandbox != nil && m.Sandbox.Effective == "none" {
			found = true
		}
	}
	if !found {
		t.Fatal("actual execution state not durable")
	}
}

func TestSandboxLegacyCapabilityFailsBeforeModelAndOffCanSelectHome(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.cfg.Sandbox.Mode = "off"
	e.a.mu.Unlock()
	e.say("work")
	e.ch.wait(t, "尚不支持所选执行隔离策略")
	e.k.none()
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, e.dir, false, "off")
	if err != nil || ws.SandboxMode != "off" {
		t.Fatal("explicit off blocked containing directory", err)
	}
	if err := e.a.SetWorkspaceSandbox(ws.ID, "invalid"); err == nil {
		t.Fatal("invalid policy accepted")
	}
	e.a.mu.Lock()
	desc := e.a.describeSandbox(e.sid)
	e.a.mu.Unlock()
	if !strings.Contains(desc, "能力探测不等于") {
		t.Fatal("probe confused with application")
	}
}
