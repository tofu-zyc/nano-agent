package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"nano-agent/gateway/internal/llm"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// Enable only this contract fixture until the production durable inbox is
// wired. A production App must not acknowledge Job ownership without it.
type jobContractHandler struct {
	*App
	observed chan proto.Msg
}

func (*jobContractHandler) JobProtocol() int { return 1 }
func (h *jobContractHandler) KernelMsg(node string, m *proto.Msg) {
	if m.T == proto.TJobState || m.T == proto.TJobsSync {
		h.observed <- *m
	}
	h.App.KernelMsg(node, m)
}
func TestRealCJobControlFramesThroughGoHub(t *testing.T) {
	var handler *jobContractHandler
	e, _ := setupRealKernel(t, func(a *App) kernel.Handler {
		handler = &jobContractHandler{App: a, observed: make(chan proto.Msg, 128)}
		return handler
	})
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	e.a.mu.Unlock()
	work := filepath.Join(e.dir, "job-work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(work, "release")
	req, _ := json.Marshal(map[string]any{
		"tool": "Shell", "args": map[string]any{"command": "printf real-job; while [ ! -f '" + gate + "' ]; do sleep 0.02; done; printf done", "execution": "background"},
		"origin": map[string]any{"agent": "default", "sid": "job-contract-source", "run_id": "job-contract-run", "turn_id": 1, "call_id": "shell", "created_by": "fake:u1"},
		"dirs":   []string{work}, "write": true, "sandbox": "off",
	})
	ctx, cancel := context.WithTimeout(e.a.ctx, 10*time.Second)
	defer cancel()
	result := (kernelHost{a: e.a, node: nodeName}).Exec(ctx, req)
	if !result.OK {
		t.Fatal(result)
	}
	var receipt struct{ Status, JobID string }
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.Content), &raw); err != nil {
		t.Fatal(err, result)
	}
	json.Unmarshal(raw["status"], &receipt.Status)
	json.Unmarshal(raw["job_id"], &receipt.JobID)
	if receipt.Status != "backgrounded" || receipt.JobID == "" {
		t.Fatal(result)
	}
	// Returning from the original host request detaches its cancellation context.
	cancel()
	await := func(predicate func(proto.Msg) bool) proto.Msg {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case message := <-handler.observed:
				if predicate(message) {
					return message
				}
			case <-timer.C:
				t.Fatal("Job frame missing through real Go decoder")
				return proto.Msg{}
			}
		}
	}
	control := func(action, request string) proto.Msg {
		t.Helper()
		if !e.a.hub.Send(nodeName, proto.Msg{T: proto.TJobControl, RequestID: request, Action: action, JobID: receipt.JobID, MaxBytes: 65536}) {
			t.Fatal("node lost")
		}
		return await(func(m proto.Msg) bool { return m.T == proto.TJobState && m.RequestID == request })
	}
	var running proto.Msg
	deadline := time.Now().Add(5 * time.Second)
	for {
		running = control("status", "status")
		if running.OK == nil || !*running.OK || running.Job == nil {
			t.Fatal(running)
		}
		// 内核先落盘再提交 running：这之前快照里已经有 pid 和输出，但状态还是 starting
		if running.Job.Status == "running" && running.Job.PID > 0 && running.Job.OutputCursor >= 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real command did not start", running)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if running.Job.Status != "running" || !running.Job.Background || running.Job.CreatedBy != "fake:u1" ||
		running.Job.OwnerAgent != "default" || running.Job.Origin.SID != "job-contract-source" ||
		running.Job.Lease == nil || running.Job.Lease.JobID != receipt.JobID {
		t.Fatal(running.Job)
	}
	output := control("output", "output")
	var log proto.JobOutput
	if err := json.Unmarshal(output.JobData, &log); err != nil {
		t.Fatal(err, output)
	}
	var bytes []byte
	for _, chunk := range log.Chunks {
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		bytes = append(bytes, data...)
	}
	if string(bytes) != "real-job" || log.NextCursor != 8 {
		t.Fatal(string(bytes), log)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	terminal := await(func(m proto.Msg) bool { return m.Kind == "terminal" && m.JobID == receipt.JobID })
	if terminal.Job == nil || terminal.Job.Status != "succeeded" || terminal.EventSeq != 1 ||
		terminal.Job.Result == nil || !terminal.Job.Result.OK || terminal.Job.OutputCursor != 12 {
		t.Fatal(terminal)
	}
	// Production intake must persist the fact before sending this ACK itself.
	ack := await(func(m proto.Msg) bool { return m.Kind == "ack" && m.JobID == receipt.JobID })
	if ack.Job == nil || ack.Job.NotificationState != "delivered" {
		t.Fatal(ack)
	}
	saved, ok := e.a.jobEvents.Job(nodeName, receipt.JobID)
	if !ok || saved.Creator != "fake:u1" || saved.TerminalEventID == "" || saved.Reserved {
		t.Fatal("node ACK preceded durable Gateway ownership/event", saved)
	}
	fact, exists := e.a.jobEvents.Event(saved.TerminalEventID)
	if !exists || fact.Name != "job.completed" || len(fact.DeliveryIDs) != 0 {
		t.Fatal(fact, exists)
	}
	// A typed array projection is distinct from the string data used by pipes.
	listing := control("list", "list")
	var rows []proto.JobSnapshot
	if err := json.Unmarshal(listing.JobData, &rows); err != nil || len(rows) != 1 || rows[0].JobID != receipt.JobID {
		t.Fatal(rows, err)
	}

	// Rebuild actual storage independently: no in-memory hook/event cache can
	// substitute for the source's durable completion proof.
	e.a.jobEvents.Close()
	rebuilt, err := eventlog.Open(context.Background(), filepath.Join(e.a.store.Dir(), "runtime", "events.jsonl"), eventlog.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	recovered, exists := rebuilt.Job(nodeName, receipt.JobID)
	_, hasFact := rebuilt.Event(saved.TerminalEventID)
	if !exists || !hasFact || recovered.Receipt.Status != "succeeded" {
		t.Fatal(recovered, hasFact)
	}
}

// Native C model/tool loop -> durable Job -> silent branch -> completion inbox.
// The source run finishes while the same command remains blocked on its gate.
func TestRealCNativeSilentBranchJobRetainsIdentityAfterRun(t *testing.T) {
	var handler *jobContractHandler
	e, _ := setupRealKernel(t, func(a *App) kernel.Handler {
		handler = &jobContractHandler{App: a, observed: make(chan proto.Msg, 128)}
		return handler
	})
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	work := filepath.Join(e.dir, "home", "branch-job")
	e.a.wss = []Workspace{{ID: "native-job", Host: e.a.machineOf(nodeName), Path: work, SandboxMode: "off"}}
	st := e.a.store.Settings(e.sid)
	st.Mode = "full"
	st.WorkspacesSet = true
	st.Workspaces = []string{"native-job"}
	err := e.a.store.SaveSettings(e.sid, st)
	if err == nil {
		err = e.a.store.Append(e.sid, session.Msg{Role: "user", Content: "source context"})
	}
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(work, "release")
	branch, err := e.a.SessionFork(SessionForkRequest{SourceSID: e.sid, Title: "quiet native job", RequestID: "job-native-branch", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"command": "printf quiet; while [ ! -f '" + gate + "' ]; do sleep 0.02; done; printf done",
		"workspace": e.a.wss[0].Label(), "execution": "background"})
	if _, err = e.a.SessionBranchAction("continue", branch.BranchSID, "!call Shell "+string(args), "fake:u1"); err != nil {
		t.Fatal(err)
	}
	var receipt proto.Msg
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for receipt.Job == nil || !receipt.Job.Background {
		select {
		case m := <-handler.observed:
			if m.Job != nil && m.Job.OriginSID == branch.BranchSID {
				receipt = m
			}
		case <-timer.C:
			t.Fatal("native branch did not detach", readFileTail(filepath.Join(e.dir, "kernel.log")))
		}
	}
	if !receipt.Job.Origin.Silent || receipt.Job.CreatedBy != "fake:u1" || receipt.Job.Lease == nil || !receipt.Job.Lease.Silent {
		t.Fatal("native policy lost silent/creator", receipt.Job)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.a.mu.Lock()
		source := e.a.sessions[branch.BranchSID]
		running := source != nil && source.run != nil
		e.a.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground barrier did not close after detach")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = os.Stat(gate); !os.IsNotExist(err) {
		t.Fatal("fixture released too early", err)
	}
	if err = os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		saved, ok := e.a.jobEvents.Job(nodeName, receipt.Job.JobID)
		if ok && saved.TerminalEventID != "" {
			fact, exists := e.a.jobEvents.Event(saved.TerminalEventID)
			if !exists || !saved.Silent || !fact.Silent || saved.Receipt.Status != "succeeded" {
				t.Fatal(saved, fact)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached native completion lost")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case text := <-e.ch.sent:
		t.Fatal("silent Job pushed source chat", text)
	default:
	}
}

type jobToolModel struct {
	execution          string
	command, workspace string
	mu                 sync.Mutex
	jobID              string
}

func (m *jobToolModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	offered := false
	for _, tool := range req.Tools {
		if tool.Function.Name == "Job" {
			offered = true
		}
	}
	if !offered {
		return nil, fmt.Errorf("Job tool was not capability-gated into the real model request")
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		if message.Role != "tool" {
			continue
		}
		if message.ToolCallID == "job-status" {
			var out JobResponse
			at := strings.Index(message.Content, "{")
			if at < 0 || json.NewDecoder(strings.NewReader(message.Content[at:])).Decode(&out) != nil || out.Job == nil || out.Job.JobID != m.jobID {
				return nil, fmt.Errorf("Job tool did not reach Gateway: %s", message.Content)
			}
			return &llm.Result{Reason: llm.ReasonFinal, Content: "Job tool path completed"}, nil
		}
		if message.ToolCallID == "job-shell" {
			var receipt struct {
				JobID string `json:"job_id"`
			}
			at := strings.Index(message.Content, "{")
			if at < 0 || json.NewDecoder(strings.NewReader(message.Content[at:])).Decode(&receipt) != nil || receipt.JobID == "" {
				return nil, fmt.Errorf("background receipt missing: %s", message.Content)
			}
			m.jobID = receipt.JobID
			raw, _ := json.Marshal(JobRequest{Action: "status", JobID: m.jobID})
			return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "job-status", Type: "function",
				Function: llm.FunctionCall{Name: "Job", Arguments: string(raw)}}}}, nil
		}
	}
	execution := m.execution
	if execution == "" {
		execution = "background"
	}
	raw, _ := json.Marshal(map[string]string{"command": m.command, "workspace": m.workspace, "execution": execution})
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: "job-shell", Type: "function",
		Function: llm.FunctionCall{Name: "Shell", Arguments: string(raw)}}}}, nil
}
func TestRealCJobToolAndGatewayCancel(t *testing.T) {
	e, _ := setupRealKernel(t, func(a *App) kernel.Handler { return &jobContractHandler{App: a, observed: make(chan proto.Msg, 128)} })
	work := filepath.Join(e.dir, "home", "job-tool")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	workspace := Workspace{ID: "job-tool-work", Host: e.a.machineOf(nodeName), Path: work, SandboxMode: "off"}
	e.a.wss = []Workspace{workspace}
	st := e.a.store.Settings(e.sid)
	st.Mode = "full"
	st.WorkspacesSet = true
	st.Workspaces = []string{workspace.ID}
	err := e.a.store.SaveSettings(e.sid, st)
	model := &jobToolModel{command: "printf started; while [ ! -f '" + filepath.Join(work, "never-release") + "' ]; do sleep 0.02; done", workspace: workspace.Label()}
	e.a.llm = model
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.say("Run a managed Shell then inspect it using Job")
	e.ch.wait(t, "Job tool path completed")
	model.mu.Lock()
	jobID := model.jobID
	model.mu.Unlock()
	result, err := e.a.JobAction(context.Background(), "fake:u1", e.sid, JobRequest{Action: "cancel", JobID: jobID})
	if err != nil || !result.Accepted {
		t.Fatal(result, err)
	}
	result, err = e.a.JobAction(context.Background(), "fake:u1", e.sid, JobRequest{Action: "wait", JobID: jobID, WaitMS: 5000})
	if err != nil || result.Job == nil || result.Job.Status != "cancelled" {
		t.Fatal(result, err)
	}
}

func TestRealCForegroundHintButtonDetachesSameExecution(t *testing.T) {
	e, _ := setupRealKernel(t, func(a *App) kernel.Handler { return &jobContractHandler{App: a, observed: make(chan proto.Msg, 128)} })
	work := filepath.Join(e.dir, "home", "hint-button")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	gate, marker := filepath.Join(work, "release"), filepath.Join(work, "launches")
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	ws := Workspace{ID: "hint-work", Host: e.a.machineOf(nodeName), Path: work, SandboxMode: "off"}
	e.a.wss = []Workspace{ws}
	st := e.a.store.Settings(e.sid)
	st.Mode = "full"
	st.WorkspacesSet = true
	st.Workspaces = []string{ws.ID}
	err := e.a.store.SaveSettings(e.sid, st)
	e.a.llm = &jobToolModel{execution: "foreground", workspace: ws.Label(),
		command: "printf launch >> '" + marker + "'; while [ ! -f '" + gate + "' ]; do sleep 0.02; done; printf completed"}
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	presenter := &jobPresenterCh{fakeCh: e.ch, cards: make(chan channel.JobCard, 8), updates: make(chan channel.JobCard, 8)}
	e.a.AddChannel(presenter)
	e.say("Start a foreground command and keep it running")
	var info JobInfo
	waitJobHint(t, e, "native", func() bool {
		for _, watch := range e.a.jobHints {
			info = watch.info
			return true
		}
		return false
	})
	before, ok := e.a.jobEvents.Job(info.Node, info.JobID)
	if !ok || before.Receipt.PID <= 0 {
		t.Fatal("no real running PID", before)
	}
	advanceHint(e, 10*time.Second)
	var card channel.JobCard
	select {
	case card = <-presenter.cards:
	case <-time.After(2 * time.Second):
		t.Fatal("native foreground had no hint")
	}
	waitJobHint(t, e, info.JobID, func() bool {
		p := e.a.jobCards[jobCardKey(info, endpoint{"fake", "c1"})]
		return p != nil && p.ref != ""
	})
	e.a.OnMessage(presenter, channel.Message{ChatID: "c1", SenderID: "u1", Action: channel.ActionJob, Text: card.Buttons[0].Token, MsgID: "job-card"})
	e.ch.wait(t, "Job tool path completed")
	after, ok := e.a.jobEvents.Job(info.Node, info.JobID)
	if !ok || !after.Receipt.Background || after.Receipt.PID != before.Receipt.PID {
		t.Fatal("button restarted or lost execution", before, after)
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := e.a.JobAction(context.Background(), "fake:u1", e.sid, JobRequest{Action: "wait", Node: info.Node, JobID: info.JobID, WaitMS: 5000})
	if err != nil || result.Job == nil || result.Job.Status != "succeeded" {
		t.Fatal(result, err)
	}
	launched, err := os.ReadFile(marker)
	if err != nil || string(launched) != "launch" {
		t.Fatal("command executed more than once", string(launched), err)
	}
}

// 用户路径：聊天里让模型起一个后台 Shell → Job 工具查到它 → /jobs 能看到 →
// 完成后经可靠日志投递给 job.completed 的 Hook。全程用真正的 C 内核，不带测试包装。
func TestRealCJobUserPathFromChatToHook(t *testing.T) {
	e, _ := setupRealKernel(t)
	work := filepath.Join(e.dir, "home", "job-user")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	negotiated := e.a.nodes[nodeName].info.Features["job_manager"] >= 1
	e.a.mu.Unlock()
	if !negotiated {
		t.Skip("内核没有协商开启后台 Job")
	}
	workspace := Workspace{ID: "job-user", Host: e.a.machineOf(nodeName), Path: work, SandboxMode: "off"}
	e.a.mu.Lock()
	e.a.wss = []Workspace{workspace}
	st := e.a.store.Settings(mainSID)
	st.Mode = "full"
	st.WorkspacesSet = true
	st.Workspaces = []string{workspace.ID}
	err := e.a.store.SaveSettings(mainSID, st)
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	model := &jobToolModel{command: "printf started; sleep 0.2; printf done", workspace: workspace.Label()}
	e.a.llm = model
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	spec := hooks.Spec{Name: "任务完成", On: []string{"job.completed"}, Handler: hooks.Handler{Type: "notify", Text: "任务完成 {{event_name}}"}, Deliver: "none"}
	view, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "job-done", Spec: &spec})
	if err != nil || view.Rule == nil {
		t.Fatal(view, err)
	}
	e.say("起一个后台命令，然后用 Job 看看它")
	e.ch.wait(t, "Job tool path completed")
	model.mu.Lock()
	jobID := model.jobID
	model.mu.Unlock()
	if jobID == "" {
		t.Fatal("后台任务没有经过网关登记")
	}
	e.say("/jobs")
	e.ch.wait(t, jobID)
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows, _ := e.a.jobEvents.Deliveries("", 128)
		done := false
		for _, d := range rows {
			if d.HookID == view.Rule.ID && d.Status == "succeeded" {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job.completed 没有投递给 Hook（%d 条投递）", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
	saved, ok := e.a.jobEvents.Job(nodeName, jobID)
	if !ok || saved.Receipt.Status != "succeeded" || saved.TerminalEventID == "" {
		t.Fatal(saved, ok)
	}
}
