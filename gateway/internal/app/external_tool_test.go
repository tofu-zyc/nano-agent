package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/extagent"
	"nano-agent/gateway/internal/proto"
)

// extEnv：一个开启了外部 agent claude 的工作目录，内核协商了后台 Job。
type extEnv struct {
	*env
	work, wsID string
}

func newExtEnv(t *testing.T) *extEnv {
	t.Helper()
	e := setup(t)
	work := e.useDir(t, "work")
	x := &extEnv{env: e, work: work}
	for _, w := range e.a.Workspaces() {
		if w.Path == work {
			x.wsID = w.ID
		}
	}
	ag := extagent.Agent{Name: "claude", Argv: []string{"claude", "-p", extagent.PromptSlot}, Dirs: []string{"~/.claude"},
		Env: map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude"}, Timeout: 3600}
	if _, err := e.a.SaveExternalAgent(ag); err != nil {
		t.Fatal(err)
	}
	if err := e.a.SetWorkspaceExternal(x.wsID, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	n := e.a.nodes["test"]
	n.info.Features = map[string]int{"job_manager": 1, "lease_detach": 1, "machine_leases": 1, "job_timeout_max_seconds": 600}
	n.info.LeasesReady = true
	e.a.mu.Unlock()
	return x
}

// call 在这一轮里调 External，收内核这边的回应：交给哪个内核的 run（没有就是空），和最后的 exec_result。
func (x *extEnv) call(t *testing.T, sid, args string, receipt string) (run proto.Msg, res proto.Msg) {
	t.Helper()
	x.a.mu.Lock()
	s := x.a.sessOf(sid)
	x.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: sid, ID: "c1", RunID: s.run.runID, TurnID: s.run.turnID, Name: "External", Args: json.RawMessage(args)})
	x.a.mu.Unlock()
	for {
		m, ok := x.k.recv(3 * time.Second)
		if !ok {
			t.Fatal("等不到 External 的结果")
		}
		switch m.T {
		case proto.TLs:
			x.k.send(proto.Msg{T: proto.TLsResult, ID: m.ID, OK: proto.Bool(true), Path: m.Path})
		case proto.TRun:
			run = m
			x.k.send(proto.Msg{T: proto.TRunResult, ID: m.ID, OK: proto.Bool(true), Content: receipt, Status: "backgrounded", JobID: "j_x-1"})
		case proto.TExecResult:
			return run, m
		}
	}
}

func (x *extEnv) finish(t *testing.T, sid string) {
	t.Helper()
	x.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, x.env, sid)
	drainSent(x.env)
}

func (x *extEnv) startRound(t *testing.T) proto.Msg {
	t.Helper()
	x.say("你好")
	in := x.k.expect(proto.TInput)
	x.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	x.k.expect(proto.TTurn)
	return in
}

// External 只在挂的工作目录上开启了外部 agent 的会话里有：开了、关了，下一轮的工具表跟着变。
func TestExternalOfferedOnlyWhereEnabled(t *testing.T) {
	x := newExtEnv(t)
	x.startRound(t)
	var spec string
	for _, tl := range x.llm.lastReq().Tools {
		if tl.Function.Name == "External" {
			b, _ := json.Marshal(tl)
			spec = string(b)
		}
	}
	if !strings.Contains(spec, `"enum":["claude"]`) || !strings.Contains(spec, "claude（本机:"+x.work+"）") || !strings.Contains(spec, "Job") {
		t.Fatalf("开启了就有 External，写明能用哪个、在哪里，用 Job 等它：%s", spec)
	}
	x.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	x.ch.wait(t, "（mock）")
	if err := x.a.SetWorkspaceExternal(x.wsID, nil); err != nil {
		t.Fatal(err)
	}
	x.startRound(t)
	if hasTool(x.llm.lastReq().Tools, "External") {
		t.Fatal("关了就没有")
	}
	x.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	x.ch.wait(t, "（mock）")
	// 只读模式和非管理员没有（聊天里只有管理员的这一轮能用，和 MCP 一样）
	if err := x.a.SetWorkspaceExternal(x.wsID, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	if x.a.externalOffered(mainSID, false) {
		t.Fatal("非管理员没有")
	}
	if !x.a.externalOffered(mainSID, true) {
		t.Fatal("管理员有")
	}
}

// 网关构造请求：Shell、后台执行、每一项单引号转义、只给这一个工作目录、登记的目录走 agent_rw、env 走顶层、自己的 tmp_key；
// 交给工作目录所在机器的内核；工具结果就是内核的回执。
func TestExternalCallBuildsTheBackgroundRun(t *testing.T) {
	x := newExtEnv(t)
	in := x.startRound(t)
	const receipt = `{"status":"backgrounded","job_id":"j_x-1","state":"queued","exit_code":null,"output_tail":""}`
	run, res := x.call(t, in.SID, `{"name":"claude","prompt":"修一下 it's $(touch pwned) `+"`id`"+`"}`, receipt)
	if res.OK == nil || !*res.OK || res.Content != receipt {
		t.Fatalf("工具结果是回执：%+v", res)
	}
	var req struct {
		Tool string `json:"tool"`
		Args struct {
			Command   string `json:"command"`
			Timeout   int    `json:"timeout"`
			Execution string `json:"execution"`
		} `json:"args"`
		Dirs    []string          `json:"dirs"`
		RO      []string          `json:"ro"`
		RW      []string          `json:"rw"`
		Write   bool              `json:"write"`
		AgentRW []string          `json:"agent_rw"`
		Env     map[string]string `json:"env"`
		TmpKey  string            `json:"tmp_key"`
		Sandbox string            `json:"sandbox"`
		Origin  proto.LeaseSpec   `json:"origin"`
	}
	if err := json.Unmarshal(run.Req, &req); err != nil {
		t.Fatal(err)
	}
	want := `'claude' '-p' '修一下 it'\''s $(touch pwned) ` + "`id`" + `'`
	if req.Tool != "Shell" || req.Args.Command != want || req.Args.Execution != "background" || req.Args.Timeout != 600 {
		t.Fatalf("命令（每项单引号转义；时限是登记的，不超过机器的上限）：%+v", req.Args)
	}
	if !slices.Equal(req.Dirs, []string{x.work}) || len(req.RO) != 0 || len(req.RW) != 0 || !req.Write {
		t.Fatalf("只给这一个工作目录，不带管理员的 RO /：dirs=%v ro=%v rw=%v", req.Dirs, req.RO, req.RW)
	}
	if !slices.Equal(req.AgentRW, []string{"~/.claude"}) || req.Env["CLAUDE_CONFIG_DIR"] != "~/.claude" {
		t.Fatalf("登记的目录和环境：%v %v", req.AgentRW, req.Env)
	}
	if req.TmpKey != "ext-claude-"+in.SID || req.Sandbox == "" {
		t.Fatalf("自己的 tmp_key、沙箱模式：%q %q", req.TmpKey, req.Sandbox)
	}
	if o := req.Origin; o.SID != in.SID || o.CallID != "c1" || o.CreatedBy != "fake:u1" || o.Agent == "" || o.RunID == "" {
		t.Fatalf("来源：%+v", o)
	}
	x.finish(t, in.SID)
}

// 拒绝的情况：写明原因，不交给内核。
func TestExternalRefusals(t *testing.T) {
	x := newExtEnv(t)
	in := x.startRound(t)
	refuse := func(args, want string) {
		t.Helper()
		run, res := x.call(t, in.SID, args, "")
		if run.T != "" || res.OK == nil || *res.OK || !strings.Contains(res.Content, want) {
			t.Fatalf("%s：要拒绝（%q），收到 %+v run=%v", args, want, res, run.T)
		}
	}
	refuse(`{"name":"claude","prompt":"  "}`, "prompt 不能是空的")
	refuse(`{"name":"claude","prompt":"`+strings.Repeat("x", 32<<10+1)+`"}`, "最长 32KB")
	refuse(`{"name":"claude","prompt":"a\u0000b"}`, "NUL")
	refuse(`{"name":"codex","prompt":"x"}`, "没有登记这个外部 agent：codex")
	refuse(`{"name":"claude","prompt":"x","workspace":"nope"}`, "没有工作目录 nope")
	x.a.mu.Lock()
	s := x.a.sessOf(in.SID)
	s.run.from = "fake:someone"
	x.a.mu.Unlock()
	refuse(`{"name":"claude","prompt":"x"}`, "只有管理员")
	x.a.mu.Lock()
	s.run.from = "fake:u1"
	n := x.a.nodes["test"]
	delete(n.info.Features, "lease_detach")
	x.a.mu.Unlock()
	refuse(`{"name":"claude","prompt":"x"}`, "没有协商后台任务")
	x.a.mu.Lock()
	n.info.Features["lease_detach"] = 1
	n.info.OS = "windows"
	x.a.mu.Unlock()
	refuse(`{"name":"claude","prompt":"x"}`, "Windows 上还不能用")
	x.a.mu.Lock()
	n.info.OS = "linux"
	x.a.mu.Unlock()
	x.finish(t, in.SID)
}

// 一个会话挂了几个工作目录、都开了这个名字：要给 workspace（写法和内核工具一样），没给就拒绝，列出可选的。
func TestExternalNeedsWorkspaceWhenAmbiguous(t *testing.T) {
	x := newExtEnv(t)
	other := x.useDir(t, "other")
	var otherID string
	for _, w := range x.a.Workspaces() {
		if w.Path == other {
			otherID = w.ID
		}
	}
	if err := x.a.SetWorkspaceExternal(otherID, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	x.a.mu.Lock()
	st := x.a.store.Settings(mainSID)
	st.WorkspacesSet, st.Workspaces = true, []string{x.wsID, otherID}
	err := x.a.store.SaveSettings(mainSID, st)
	x.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	in := x.startRound(t)
	run, res := x.call(t, in.SID, `{"name":"claude","prompt":"x"}`, "")
	if run.T != "" || *res.OK || !strings.Contains(res.Content, "用 workspace 参数指明") ||
		!strings.Contains(res.Content, "本机:"+x.work) || !strings.Contains(res.Content, "本机:"+other) {
		t.Fatalf("不明确：拒绝，列出可选的：%+v", res)
	}
	for _, w := range []string{"本机:" + other, other, "2"} {
		run, res = x.call(t, in.SID, `{"name":"claude","prompt":"x","workspace":"`+w+`"}`, `{"status":"backgrounded"}`)
		var req struct {
			Dirs []string `json:"dirs"`
		}
		json.Unmarshal(run.Req, &req)
		if !*res.OK || !slices.Equal(req.Dirs, []string{other}) {
			t.Fatalf("workspace=%s：%+v %v", w, res, req.Dirs)
		}
	}
	// 只开在一个上：不写也行；写了没开的那个，拒绝
	if err := x.a.SetWorkspaceExternal(otherID, nil); err != nil {
		t.Fatal(err)
	}
	if run, res = x.call(t, in.SID, `{"name":"claude","prompt":"x"}`, `{"status":"backgrounded"}`); run.T != proto.TRun || !*res.OK {
		t.Fatalf("只有一个开着：%+v", res)
	}
	if _, res = x.call(t, in.SID, `{"name":"claude","prompt":"x","workspace":"`+other+`"}`, ""); *res.OK || !strings.Contains(res.Content, "上没有开启 claude") {
		t.Fatalf("没开的工作目录：%+v", res)
	}
	x.finish(t, in.SID)
}

// SSH 服务器上没有内核：用不了，写明原因。
func TestExternalUnavailableOnSSHServers(t *testing.T) {
	x := newExtEnv(t)
	x.hosts.set("gpu", "ssh", true, 7, "")
	proj := filepath.Join(x.dir, "gpu-proj")
	os.MkdirAll(proj, 0o755)
	ws, err := x.a.AddWorkspace(context.Background(), "gpu", proj, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.a.SetWorkspaceExternal(ws.ID, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	x.a.mu.Lock()
	st := x.a.store.Settings(mainSID)
	st.WorkspacesSet, st.Workspaces = true, []string{ws.ID}
	err = x.a.store.SaveSettings(mainSID, st)
	x.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	in := x.startRound(t)
	if run, res := x.call(t, in.SID, `{"name":"claude","prompt":"x"}`, ""); run.T != "" || *res.OK || !strings.Contains(res.Content, "SSH 服务器上没有内核") {
		t.Fatalf("SSH：%+v", res)
	}
	x.finish(t, in.SID)
}

// 审批提示写出 agent 名字和完整的 prompt（不截断）；卡片放不下时改发文字。
func TestExternalApprovalShowsTheWholePrompt(t *testing.T) {
	prompt := strings.Repeat("把登录页修好。", 600)
	title, body := callCard("External", json.RawMessage(`{"name":"claude","prompt":"`+prompt+`","workspace":"本机:/w"}`))
	if !strings.Contains(title, "外部 agent claude") || !strings.Contains(title, "本机:/w") || body != prompt {
		t.Fatalf("审批提示：%q / %d 字", title, len([]rune(body)))
	}
	if !externalApprovalTooLong("External", body) || externalApprovalTooLong("External", "短的") || externalApprovalTooLong("Shell", body) {
		t.Fatal("放不下的长 prompt 改发文字")
	}
}
