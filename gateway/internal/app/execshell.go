package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// 网关发起的执行：不经过模型。Hook 的命令处理器和工单（建 worktree、验收、提交）都走这里：
// 在某台机器上、按给定的读写范围跑一条命令。授权和审批由调用方自己做。

// leasedResult 是 leasedExec 的结果：拿租约失败、准备请求失败，或者执行器的结果。
type leasedResult struct {
	Result     host.Result
	AcquireErr error // 没拿到执行租约（可以稍后重试）
	BuildErr   error // 准备请求时出错（租约已经释放，没有执行）
}

// leasedExec 拿执行租约（内核自己执行的不用），让 build 做最后的准备并给出请求，执行，最后释放租约。
// 调用方不持有 App.mu。
func (a *App) leasedExec(ctx context.Context, h host.Host, native bool, lease dispatch.Request, build func() ([]byte, error)) leasedResult {
	var release func(bool)
	if !native {
		var err error
		release, err = a.acquireExecution(ctx, lease)
		if err != nil {
			return leasedResult{AcquireErr: err}
		}
		a.executionLeaseStarted(lease)
	}
	request, err := build()
	if err != nil {
		if release != nil {
			release(false)
		}
		return leasedResult{BuildErr: err}
	}
	result := h.Exec(ctx, request)
	if release != nil {
		release(result.Lost)
	}
	return leasedResult{Result: result}
}

// shellRun 是一次网关发起的命令。
type shellRun struct {
	Machine  string              // 在哪台机器上跑
	Spec     proto.WorkspaceSpec // 读写范围：Dirs 可写，RO 只读
	Sandbox  string              // 执行隔离请求：auto、on、off
	Command  string              // shell 命令
	Timeout  time.Duration       // 超时
	Key      string              // 临时目录键（同一件事用同一个）
	Agent    string              // 归属的 agent
	Session  string              // 记在哪个会话名下（租约和事件日志用）
	By       string              // 谁发起的
	Priority int
	Env      map[string]string // 网关发起的执行才能带（5c 的推送、5d 的外部 agent）；Hook 命令不填
}

// gatewayShell 在 r.Machine 上跑一条命令，返回执行器的输出。没有机器、机器不可用、没拿到租约都返回错误。
// 调用方不持有 App.mu。
func (a *App) gatewayShell(ctx context.Context, r shellRun) (host.Result, error) {
	if err := checkEnvNUL(r.Env); err != nil {
		return host.Result{}, err
	}
	a.mu.Lock()
	h, _, why := a.hostFor(r.Machine)
	if h == nil {
		a.mu.Unlock()
		return host.Result{}, errors.New("执行机器不可用：" + why)
	}
	_, native := h.(kernelHost)
	windows := a.machineOS(r.Machine) == "windows"
	id := "gw-" + r.Key + fmt.Sprintf("-%d", a.hostSeq+1)
	a.hostSeq++
	origin := proto.LeaseSpec{SID: r.Session, RunID: id, CallID: id, Name: "Shell", Agent: r.Agent, CreatedBy: r.By, Silent: true, Priority: r.Priority}
	lease := dispatch.Request{ID: id, Name: "Shell", Owner: "gateway/" + a.dispatchBoot, Machine: r.Machine, Session: r.Session, RunID: id, CallID: id,
		Agent: r.Agent, Shell: true, Priority: r.Priority, Exclusive: []string{"machine:files"}, Windows: windows, At: time.Now()}
	a.mu.Unlock()
	timeout := max(1, int(r.Timeout/time.Second))
	run := a.leasedExec(ctx, h, native, lease, func() ([]byte, error) {
		req, _ := json.Marshal(withEnv(map[string]any{
			"tool": "Shell", "origin": origin, "args": map[string]any{"command": r.Command, "timeout": timeout},
			"dirs": r.Spec.Dirs, "home": r.Spec.Home, "ro": r.Spec.RO, "rw": r.Spec.RW, "write": true,
			"sandbox": r.Sandbox, "tmp_key": r.Key, "max_output": 64 << 10, "watch_stdin": true,
		}, r.Env))
		return append(req, '\n'), nil
	})
	if run.AcquireErr != nil {
		return host.Result{}, run.AcquireErr
	}
	if run.BuildErr != nil {
		return host.Result{}, run.BuildErr
	}
	return run.Result, nil
}

// withEnv 给请求加顶层 env（空就不加）。
func withEnv(req map[string]any, env map[string]string) map[string]any {
	if len(env) > 0 {
		req["env"] = env
	}
	return req
}

// checkEnvNUL 拒绝名字或值里有 NUL 的 env：执行器用的 JSON 库把 \u0000 之后的截掉，执行器自己看不出来，所以网关填的时候就查。
func checkEnvNUL(env map[string]string) error {
	for k, v := range env {
		if strings.ContainsRune(k, 0) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %q 的名字或值里有 NUL，不执行", k)
		}
	}
	return nil
}
