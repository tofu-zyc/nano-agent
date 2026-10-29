package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func hookScript(root, agent, name string) ([]byte, error) {
	if root == "" || filepath.IsAbs(name) || strings.ContainsAny(name, `\:`) {
		return nil, errors.New("脚本应为 agent/hooks 中的相对路径")
	}
	base, err := filepath.EvalSymlinks(filepath.Join(root, agent, "hooks"))
	if err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(base, name))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("脚本不在 agent 的 hooks 目录内")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<10 {
		return nil, errors.New("脚本最多 64 KiB")
	}
	return raw, nil
}
func scriptDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func shellLiteral(s string, windows bool) string {
	if windows {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
func (x hookExecutor) command(ctx context.Context, w hooks.Execution, done func(hooks.Result)) {
	a := x.a
	a.mu.Lock()
	if err := a.hookAuthority(recordOf(w), w.Spec); err != nil {
		a.mu.Unlock()
		done(hooks.Result{Err: err})
		return
	}
	scope := a.hookWorkspaces(&hookInvocation{work: w})
	mode := scope.mode
	if modeRank(mode) < modeRank(proto.ModeAsk) {
		a.mu.Unlock()
		done(hooks.Result{Err: errors.New("当前模式禁止 Hook 命令")})
		return
	}
	route := a.route(scope)
	a.freezeSandboxRoute(&route, a.sandboxModeFor(w.Spec.Scope.SID))
	if len(route.down) > 0 {
		a.mu.Unlock()
		done(hooks.Result{Err: errors.New(strings.Join(route.down, "；")), Retry: true})
		return
	}
	machine := host.LocalName
	spec := homeSpec()
	sandbox := a.sandboxModeFor(w.Spec.Scope.SID)
	if len(route.mounts) > 0 {
		mt := route.mounts[0]
		machine = mt.ws.Host
		spec = route.spec(mt, true)
		sandbox = mt.sandboxMode
	}
	h, landlock, why := a.hostFor(machine)
	if h == nil {
		a.mu.Unlock()
		done(hooks.Result{Err: errors.New("执行机器不可用：" + why), Retry: true})
		return
	}
	if target, ok := h.(kernelHost); ok {
		n := a.nodes[target.node]
		if n == nil || n.info.Features["hook_input"] < 1 {
			a.mu.Unlock()
			done(hooks.Result{Err: errors.New("目标节点尚不支持 Hook 事件输入，请更新内核")})
			return
		}
	}
	windows := a.machineOS(machine) == "windows"
	root := a.agentRoot
	input, _ := json.Marshal(map[string]any{"delivery_id": w.Delivery.ID, "event": w.Event})
	if len(input) > 4096 {
		a.mu.Unlock()
		done(hooks.Result{Err: errors.New("Hook 事件输入超过 4 KiB")})
		return
	}
	inv := &hookInvocation{work: w}
	fake := &sess{sid: hookSID(w.Delivery.AgentID, w.Delivery.ID), run: &run{hook: inv, from: w.Delivery.Creator, runID: w.Delivery.ID, inner: true}}
	var approval <-chan bool
	approvalN := 0
	if mode == proto.ModeAsk || (landlock == 0 && sandbox != "off" && mode != proto.ModeFull) {
		if !w.Delivery.Silent {
			for _, fixed := range w.Spec.Scope.Endpoints {
				ep := endpoint{fixed.Channel, fixed.Chat}
				if rec := a.eps[ep.key()]; rec != nil && rec.Session == w.Spec.Scope.SID && rec.Who == w.Delivery.Creator && a.hookFingerprint(ep.ch) == fixed.Fingerprint {
					fake.run.outs = append(fake.run.outs, out{ep: ep})
				}
			}
		}
		args, _ := json.Marshal(map[string]string{"command": w.Spec.Handler.Command, "script": w.Spec.Handler.Script})
		approval, approvalN = a.askMCPApproval(fake, w.Delivery.ID, "Shell", args)
	}
	origin := proto.LeaseSpec{SID: fake.sid, RunID: w.Delivery.ID, CallID: w.Delivery.ID, Name: "Shell", Agent: w.Delivery.AgentID, CreatedBy: w.Delivery.Creator, Silent: w.Delivery.Silent, Priority: 10}
	lease := dispatch.Request{ID: "hook-" + w.Delivery.ID, Name: "Shell", Owner: "gateway/" + a.dispatchBoot, Machine: machine, Session: fake.sid, RunID: w.Delivery.ID, CallID: w.Delivery.ID, Agent: w.Delivery.AgentID, Shell: true, Priority: 10, Exclusive: []string{"machine:files"}, Windows: windows, At: time.Now()}
	_, native := h.(kernelHost)
	a.mu.Unlock()
	if approval != nil {
		defer func() { a.mu.Lock(); a.dropApproval(approvalN, "Hook 命令审批结束"); a.mu.Unlock() }()
		select {
		case ok := <-approval:
			if !ok {
				done(hooks.Result{Err: errors.New("Hook 命令未获批准")})
				return
			}
		case <-ctx.Done():
			done(hooks.Result{Err: ctx.Err()})
			return
		}
	}
	a.mu.Lock()
	auth := a.hookAuthority(recordOf(w), w.Spec)
	a.mu.Unlock()
	if auth != nil {
		done(hooks.Result{Err: auth})
		return
	}
	var release func(bool)
	if !native {
		var err error
		release, err = a.acquireExecution(ctx, lease)
		if err != nil {
			done(hooks.Result{Err: err, Retry: true})
			return
		}
		a.executionLeaseStarted(lease)
	}
	command := w.Spec.Handler.Command
	if w.Spec.Handler.Script != "" {
		raw, err := hookScript(root, w.Delivery.AgentID, w.Spec.Handler.Script)
		if err != nil || scriptDigest(raw) != w.Spec.Handler.Digest {
			if release != nil {
				release(false)
			}
			done(hooks.Result{Err: errors.New("登记的 Hook 脚本已改变或不可读，需要更新规则修订")})
			return
		}
		name := "hook-" + w.Spec.Handler.Digest
		actual, err := h.PutFiles(ctx, skills.CacheDir+"/"+name, []host.File{{Path: "handler", Data: raw, Exec: true}})
		if err != nil {
			if release != nil {
				release(false)
			}
			done(hooks.Result{Err: err})
			return
		}
		path := strings.TrimRight(actual, `/\`) + "/handler"
		if windows {
			command = "& " + shellLiteral(path, true)
		} else {
			command = "/bin/sh " + shellLiteral(path, false)
		}
	}
	request, _ := json.Marshal(map[string]any{
		"tool": "Shell", "origin": origin, "args": map[string]any{"command": command, "timeout": max(1, (w.Spec.Handler.TimeoutMS+999)/1000)},
		"dirs": spec.Dirs, "home": spec.Home, "ro": spec.RO, "rw": spec.RW, "write": true,
		"sandbox": sandbox, "hook_input": string(input), "tmp_key": w.Delivery.ID, "max_output": 64 << 10, "watch_stdin": true,
	})
	a.noteHookCause(machine, w.Delivery.HookID, w.Spec.Handler.TimeoutMS)
	result := h.Exec(ctx, append(request, '\n'))
	if release != nil {
		release(result.Lost)
	}
	if !result.OK {
		done(hooks.Result{Err: errors.New(result.Content), Uncertain: result.Lost})
		return
	}
	var output struct {
		Trigger  *bool           `json:"trigger"`
		Summary  string          `json:"summary"`
		Data     json.RawMessage `json:"data"`
		Decision string          `json:"decision"`
	}
	dec := json.NewDecoder(strings.NewReader(result.Content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&output); err != nil || dec.Decode(new(any)) != io.EOF || len(result.Content) > 64<<10 {
		done(hooks.Result{Err: errors.New("Hook 命令必须输出单个有界 JSON 对象")})
		return
	}
	summary := cutRunes(output.Summary, 1000)
	if slices.Contains(w.Spec.On, "tool.before") {
		done(hooks.Result{Summary: summary, Decision: output.Decision})
		return
	}
	if output.Trigger != nil && *output.Trigger && summary != "" {
		result := x.notify(ctx, w, summary)
		if result.Err != nil {
			done(result)
			return
		}
	}
	done(hooks.Result{Summary: summary})
}
func recordOf(w hooks.Execution) eventlog.RuleRecord {
	return eventlog.RuleRecord{ID: w.Delivery.HookID, AgentID: w.Delivery.AgentID, Creator: w.Delivery.Creator, Silent: w.Delivery.Silent}
}
