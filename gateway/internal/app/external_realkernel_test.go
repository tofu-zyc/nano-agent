package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/extagent"
	"nano-agent/gateway/internal/llm"
)

// externalModel 调 External，拿回执里的 job_id 用 Job wait 等它结束。
type externalModel struct {
	mu     sync.Mutex
	prompt string
	waited string
}

func (m *externalModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != "tool" {
			continue
		}
		switch msg.ToolCallID {
		case "wait":
			m.waited = msg.Content
			return &llm.Result{Reason: llm.ReasonFinal, Content: "external done"}, nil
		case "ext":
			var receipt struct {
				Status string `json:"status"`
				JobID  string `json:"job_id"`
			}
			at := strings.Index(msg.Content, "{")
			if at < 0 || json.Unmarshal([]byte(msg.Content[at:]), &receipt) != nil || receipt.Status != "backgrounded" || receipt.JobID == "" {
				return nil, fmt.Errorf("External 的结果不是后台任务的回执：%s", msg.Content)
			}
			raw, _ := json.Marshal(JobRequest{Action: "wait", JobID: receipt.JobID, WaitMS: 10000})
			return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "wait", Type: "function", Function: llm.FunctionCall{Name: "Job", Arguments: string(raw)}}}}, nil
		}
	}
	raw, _ := json.Marshal(map[string]string{"name": "fake", "prompt": m.prompt})
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "ext", Type: "function", Function: llm.FunctionCall{Name: "External", Arguments: string(raw)}}}}, nil
}

// 真内核：External 交给工作目录所在机器的内核，作为后台 Job 在工作目录里跑；prompt 原样交给命令（不经 shell 解释），
// 登记的目录可写、env 里的 ~/ 用真实的 HOME 展开，HOME 是它自己的私有目录；模型用 Job wait 等到它结束。
func TestRealKernelExternalRunsAsBackgroundJob(t *testing.T) {
	e, _ := setupRealKernel(t)
	home := filepath.Join(e.dir, "home")
	agentDir := filepath.Join(home, ".fake-agent")
	work := filepath.Join(e.dir, "ext-work")
	for _, d := range []string{agentDir, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `printf '%s' "$1" > prompt.txt; printf 'HOME=%s\nCFG=%s\n' "$HOME" "$FAKE_AGENT_DIR" > env.txt; date > "$FAKE_AGENT_DIR/last-run"`
	ag := extagent.Agent{Name: "fake", Argv: []string{"sh", "-c", script, "fake-agent", extagent.PromptSlot},
		Dirs: []string{"~/.fake-agent"}, Env: map[string]string{"FAKE_AGENT_DIR": "~/.fake-agent"}, Timeout: 60}
	if _, err := e.a.SaveExternalAgent(ag); err != nil {
		t.Fatal(err)
	}
	prompt := `把 a.txt 改好；it's "quoted" $(touch pwned) ` + "`touch pwned2`" + "\n第二行"
	model := &externalModel{prompt: prompt}
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
	}
	landlock := e.a.nodes[nodeName].info.Landlock
	ws := Workspace{ID: "ext-work", Host: e.a.machineOf(nodeName), Path: work, External: []string{"fake"}}
	e.a.wss = []Workspace{ws}
	st := e.a.store.Settings(e.sid)
	st.Mode = "full" // 和 Shell 一样跟着权限模式：这里不测审批
	st.WorkspacesSet, st.Workspaces = true, []string{ws.ID}
	err := e.a.store.SaveSettings(e.sid, st)
	e.a.llm = model
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.say("让外部 agent 改一下")
	deadline := time.After(30 * time.Second)
	for done := false; !done; {
		select {
		case s := <-e.ch.sent:
			done = strings.Contains(s, "external done")
		case <-deadline:
			t.Fatalf("等不到这一轮结束；内核日志：%s", readFileTail(filepath.Join(e.dir, "kernel.log")))
		}
	}
	model.mu.Lock()
	waited := model.waited
	model.mu.Unlock()
	if !strings.Contains(waited, `"status":"succeeded"`) {
		t.Fatalf("Job wait 等到它结束：%s", waited)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "prompt.txt")); string(b) != prompt {
		t.Fatalf("prompt 原样交给命令：%q", b)
	}
	for _, f := range []string{"pwned", "pwned2"} {
		if _, err := os.Stat(filepath.Join(work, f)); err == nil {
			t.Fatalf("prompt 被 shell 解释了：%s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(agentDir, "last-run")); err != nil {
		t.Fatalf("登记的目录可写：%v", err)
	}
	env, _ := os.ReadFile(filepath.Join(work, "env.txt"))
	if !strings.Contains(string(env), "CFG="+agentDir+"\n") {
		t.Fatalf("env 里的 ~/ 用真实的 HOME 展开：%s", env)
	}
	if landlock > 0 && (strings.Contains(string(env), "HOME="+home+"\n") || !strings.Contains(string(env), "/tmp/ext-fake-")) {
		t.Fatalf("HOME 是它自己的私有目录（按 ext-<名字>-<会话> 分）：%s", env)
	}
}

// 真内核：启动前就被执行器拒绝的 Job（登记的目录不存在），Job 工具的 status、wait 带上失败原因；
// 别的后台 Job 失败也一样（取执行器回的内容的前 500 字）；list 不带原因。
func TestRealKernelJobFailureReasonVisible(t *testing.T) {
	e, _ := setupRealKernel(t)
	work := filepath.Join(e.dir, "ext-work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	ag := extagent.Agent{Name: "fake", Argv: []string{"sh", "-c", "true", "fake-agent", extagent.PromptSlot}, Dirs: []string{"~/.missing-agent"}, Timeout: 60}
	if _, err := e.a.SaveExternalAgent(ag); err != nil {
		t.Fatal(err)
	}
	model := &externalModel{prompt: "x"}
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
	}
	ws := Workspace{ID: "ext-work", Host: e.a.machineOf(nodeName), Path: work, External: []string{"fake"}}
	e.a.wss = []Workspace{ws}
	st := e.a.store.Settings(e.sid)
	st.Mode = "full"
	st.WorkspacesSet, st.Workspaces = true, []string{ws.ID}
	err := e.a.store.SaveSettings(e.sid, st)
	e.a.llm = model
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.say("让外部 agent 改一下")
	deadline := time.After(30 * time.Second)
	for done := false; !done; {
		select {
		case s := <-e.ch.sent:
			done = strings.Contains(s, "external done")
		case <-deadline:
			t.Fatalf("等不到这一轮结束；内核日志：%s", readFileTail(filepath.Join(e.dir, "kernel.log")))
		}
	}
	model.mu.Lock()
	waited := model.waited
	model.mu.Unlock()
	var got JobResponse
	if err := json.Unmarshal([]byte(waited), &got); err != nil || got.Job == nil {
		t.Fatalf("Job wait 的结果：%s", waited)
	}
	if got.Job.Status != "failed" || !strings.Contains(got.Job.Reason, "agent_rw 里的 ~/.missing-agent") || !strings.Contains(got.Job.Reason, "目录不存在") {
		t.Fatalf("wait 带上失败原因：%+v", got.Job)
	}
	// 同一个任务再 status：还在；list 不带原因
	status, err := e.a.JobAction(context.Background(), "fake:u1", "", JobRequest{Action: "status", JobID: got.Job.JobID})
	if err != nil || status.Job == nil || status.Job.Reason != got.Job.Reason {
		t.Fatalf("status 带上失败原因：%+v %v", status, err)
	}
	list, err := e.a.JobAction(context.Background(), "fake:u1", "", JobRequest{Action: "list"})
	if err != nil || len(list.Jobs) == 0 {
		t.Fatalf("list：%+v %v", list, err)
	}
	for _, j := range list.Jobs {
		if j.Reason != "" {
			t.Fatalf("list 不带原因：%+v", j)
		}
	}
	if !strings.Contains(renderJobResponse(status), "原因：agent_rw") {
		t.Fatalf("/job 的文字也写出原因：%s", renderJobResponse(status))
	}
}
