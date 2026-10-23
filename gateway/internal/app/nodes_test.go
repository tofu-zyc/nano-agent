package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

var echoTool = []proto.ToolSpec{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object"}`)}}

// dialKernel 再连一个和网关同一台机器上的内核
func (e *env) dialKernel(t *testing.T, name string) *fakeKernel {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(e.dir, "k.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return e.hello(t, conn, name, "tok")
}

// dialRemote 连一个别的机器上的内核（网关自己建立的通道，不算本机）
func (e *env) dialRemote(t *testing.T, name string) *fakeKernel {
	t.Helper()
	hubSide, kernelSide := net.Pipe()
	e.a.hub.Attach(e.a.ctx, hubSide, false)
	return e.hello(t, kernelSide, name, "")
}

func (e *env) hello(t *testing.T, conn net.Conn, name, token string) *fakeKernel {
	t.Helper()
	t.Cleanup(func() { conn.Close() })
	k := &fakeKernel{t: t, conn: conn, sc: bufio.NewScanner(conn)}
	go k.send(proto.Msg{T: proto.THello, V: proto.Version, Token: token, Node: name, Tools: echoTool})
	k.expect(proto.TWelcome)
	waitNode(e.a, name, true)
	return k
}

// expectAll 收 n 条消息（顺序不定），按类型和 ID 分好
func (k *fakeKernel) expectAll(n int) map[string]proto.Msg {
	k.t.Helper()
	got := map[string]proto.Msg{}
	for i := 0; i < n; i++ {
		m, ok := k.recv(3 * time.Second)
		if !ok {
			k.t.Fatalf("少了消息，已经收到：%v", got)
		}
		got[m.T+":"+m.ID] = m
	}
	return got
}

// addSkill 在网关的技能目录里放一个技能
func (e *env) addSkill(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(e.dir, "skills", name)
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: 示例技能\n---\n照着做"), 0o644)
	os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("echo hi\n"), 0o755)
}

// 本机的内核掉线了：别的设备上的内核顶上，本机的工作目录照样能用工具（网关在本机执行）
func TestGatewayExecWhenLocalKernelOffline(t *testing.T) {
	e := setup(t)
	work := e.useDir(t, "work")
	phone := e.dialRemote(t, "phone")

	e.say("你好")
	if in := e.k.expect(proto.TInput); len(in.Workspaces) != 1 || !in.Workspaces[0].Local || in.Workspaces[0].Dirs[0] != work {
		t.Fatalf("同机：本机的内核自己执行：%+v", in)
	}
	e.roundTrip(t)

	e.k.conn.Close()
	waitNode(e.a, "test", false)
	e.say("还在吗")
	in := phone.expect(proto.TInput)
	if w := in.Workspaces; len(w) != 1 || w[0].Local || w[0].Landlock != 7 || w[0].Name != "本机:"+work || in.Mode != proto.ModeAsk {
		t.Fatalf("异机：工具交给网关执行：%+v", in)
	}
	phone.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c1", Name: "Read", Args: json.RawMessage(`{"path":"a.txt"}`),
		Workspace: "本机:" + work, TimeoutMs: 1000})
	res := phone.expect(proto.TExecResult)
	if res.ID != "c1" || res.OK == nil || !*res.OK || res.Content != "执行结果" {
		t.Fatalf("exec_result: %+v", res)
	}
	req := e.hosts.hosts[host.LocalName].lastReq(t)
	if req["tool"] != "Read" || req["write"] != true || req["tmp_key"] != mainSID || req["watch_stdin"] != true {
		t.Fatalf("执行器请求：%v", req)
	}
	if dirs := req["dirs"].([]any); len(dirs) != 1 || dirs[0] != work {
		t.Fatalf("约束由网关按会话的设置生成：%v", req["dirs"])
	}
	if ro := req["ro"].([]any); len(ro) != 1 || ro[0] != "/" {
		t.Fatalf("管理员可以读整个文件系统：%v", req["ro"])
	}
	phone.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c2", Name: "Read", Workspace: "gpu:~/x"})
	if r := phone.expect(proto.TExecResult); r.OK == nil || *r.OK || !strings.Contains(r.Content, "没有挂载") {
		t.Fatalf("没挂的工作目录：%+v", r)
	}
	phone.send(proto.Msg{T: proto.TModel, SID: mainSID})
	phone.expect(proto.TTurn)
	if n := len(e.llm.lastReq().Tools); n != 3 {
		t.Fatalf("替补也能用工具：%d", n)
	}
	phone.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "收到：还在吗")
	if info, _ := e.a.Session(mainSID); info.Kernel != "phone" || len(info.Workspaces) != 1 || info.Workspaces[0].How != "gateway" {
		t.Fatalf("info: %+v", info)
	}

	// 本机的执行器也用不了时，没有可用的工作目录，只能聊天
	e.hosts.set(host.LocalName, "local", false, 0, "执行器运行不了")
	e.say("现在呢")
	if in := phone.expect(proto.TInput); in.Mode != proto.ModeNone || len(in.Workspaces) != 0 {
		t.Fatalf("input: %+v", in)
	}
	phone.send(proto.Msg{T: proto.TModel, SID: mainSID})
	phone.expect(proto.TTurn)
	want := "[系统] 挂载的工作目录所在的机器现在都用不了（本机:" + work + "：本机 执行器运行不了），这一轮由 phone 接手：没有可用的工作目录，只能聊天"
	if got := lastUser(e.llm.lastReq().Messages); !strings.HasPrefix(got, want) {
		t.Fatalf("note: %q", got)
	}
}

// SSH 服务器上的工作目录：任意内核来跑，工具、技能文件交给网关在服务器上处理；服务器连不上时只聊天
func TestSSHWorkspace(t *testing.T) {
	e := setup(t)
	e.addSkill(t, "demo")
	gpu := e.hosts.set("gpu", "ssh", true, 0, "")
	remote := filepath.Join(e.dir, "remote")
	os.MkdirAll(remote, 0o755)
	ws, err := e.a.AddWorkspace(context.Background(), "gpu", remote, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.UpdateSession(mainSID, SessionPatch{Workspaces: &[]string{ws.ID}}); err != nil {
		t.Fatal(err)
	}
	e.say("在服务器上干活")
	in := e.k.expect(proto.TInput)
	if w := in.Workspaces; len(w) != 1 || w[0].Local || w[0].Landlock != 0 || w[0].Name != "gpu:"+remote {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c1", Name: "Shell", Args: json.RawMessage(`{"command":"ls"}`), Workspace: "gpu:" + remote})
	if r := e.k.expect(proto.TExecResult); r.OK == nil || !*r.OK {
		t.Fatalf("exec_result: %+v", r)
	}
	if req := gpu.lastReq(t); req["tool"] != "Shell" || req["dirs"].([]any)[0] != remote {
		t.Fatalf("req: %v", req)
	}

	// 技能在网关上：Skill 返回说明，附带文件放到服务器上
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c2", Name: "Skill", Args: json.RawMessage(`{"name":"demo"}`)})
	if r := e.k.expect(proto.TExecResult); r.OK == nil || !*r.OK || !strings.Contains(r.Content, "技能目录：~/.nano-agent/skills/demo") ||
		!strings.Contains(r.Content, "附带文件：scripts/run.sh") || !strings.HasSuffix(r.Content, "照着做") {
		t.Fatalf("skill: %+v", r)
	}
	if f := gpu.put["~/.nano-agent/skills/demo"]; len(f) != 2 || f[1].Path != "scripts/run.sh" || !f[1].Exec {
		t.Fatalf("put: %+v", gpu.put)
	}
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c3", Name: "Skill", Args: json.RawMessage(`{"name":"../evil"}`)})
	if r := e.k.expect(proto.TExecResult); r.OK == nil || *r.OK || !strings.Contains(r.Content, "没有这个技能") {
		t.Fatalf("没有的技能：%+v", r)
	}

	// 停止：正在服务器上执行的工具一起停下
	gpu.block = true
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c4", Name: "Shell", Args: json.RawMessage(`{"command":"sleep 100"}`), Workspace: "gpu:" + remote})
	gpu.lastReq(t)
	time.Sleep(50 * time.Millisecond)
	e.say("/stop")
	got := e.k.expectAll(2)
	if _, ok := got[proto.TCancel+":"]; !ok || got[proto.TExecResult+":c4"].Content != "已停止" {
		t.Fatalf("停止：%v", got)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	e.ch.wait(t, "（已停止）")

	// 服务器连不上：只读，说明原因
	e.hosts.set("gpu", "ssh", false, 0, "连不上：timeout")
	e.say("还能用吗")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk || !in.Elevate || len(in.Workspaces) != 1 || in.Workspaces[0].Name != "HOME" || !in.Workspaces[0].Home {
		t.Fatalf("input: %+v", in)
	}
	msgs := e.roundTrip(t)
	if got := lastUser(msgs); !strings.HasPrefix(got, "[系统] 挂载的工作目录所在的机器现在都用不了（gpu:"+remote+"：gpu 连不上：timeout），这一轮由 test 接手，在 HOME 下工作：") {
		t.Fatalf("note: %q", got)
	}
	e.say("/node")
	if msg := e.ch.wait(t, "机器："); !strings.Contains(msg, "· gpu（SSH 服务器，连不上：timeout）") || !strings.Contains(msg, "· 本机（网关所在的机器，内核 test）") {
		t.Fatalf("node: %q", msg)
	}
}

// 装了内核的电脑上的工作目录：由那台电脑的内核来跑；它不在线时只聊天
func TestKernelMachineWorkspace(t *testing.T) {
	e := setup(t)
	laptop := e.dialRemote(t, "laptop")
	done := make(chan Workspace, 1)
	go func() {
		ws, err := e.a.AddWorkspace(context.Background(), "laptop", "/data", true)
		if err != nil {
			t.Error(err)
		}
		done <- ws
	}()
	ls := laptop.expect(proto.TLs) // 电脑上的目录由它的内核列
	if ls.Path != "/data" {
		t.Fatalf("ls: %+v", ls)
	}
	laptop.send(proto.Msg{T: proto.TLsResult, ID: ls.ID, OK: proto.Bool(true), Path: "/data", Home: "/home/u"})
	<-done

	e.say("在电脑上")
	if in := laptop.expect(proto.TInput); len(in.Workspaces) != 1 || !in.Workspaces[0].Local || in.Workspaces[0].Dirs[0] != "/data" {
		t.Fatalf("同机：电脑的内核自己执行：%+v", in)
	}
	// 别的内核冒充这一轮发消息：不理，并告诉它没有这个任务
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	if m := e.k.expect(proto.TTurn); m.Reason != proto.ReasonError {
		t.Fatalf("turn: %+v", m)
	}
	// 同机的工作目录内核该自己执行
	laptop.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "x", Name: "Read", Workspace: "laptop:/data"})
	if r := laptop.expect(proto.TExecResult); r.OK == nil || *r.OK || !strings.Contains(r.Content, "内核自己执行") {
		t.Fatalf("exec: %+v", r)
	}
	laptop.send(proto.Msg{T: proto.TModel, SID: mainSID})
	laptop.expect(proto.TTurn)
	laptop.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "收到：在电脑上")

	laptop.conn.Close()
	waitNode(e.a, "laptop", false)
	e.say("电脑关了")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk || !in.Elevate || len(in.Workspaces) != 1 || in.Workspaces[0].Name != "HOME" || !in.Workspaces[0].Home {
		t.Fatalf("input: %+v", in)
	}
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 挂载的工作目录所在的机器现在都用不了（laptop:/data：laptop 不在线），这一轮由 test 接手，在 HOME 下工作：") {
		t.Fatalf("note: %q", got)
	}
	if ms := e.a.Snapshot().Machines; len(ms) != 2 || ms[1].Name != "laptop" || ms[1].Online {
		t.Fatalf("machines: %+v", ms)
	}
}

// 一个会话同时挂三台机器上的工作目录：本机（有内核）、另一台电脑（请它的内核代为执行）、SSH 服务器
func TestMountsOnSeveralMachines(t *testing.T) {
	e := setup(t)
	e.addSkill(t, "demo")
	work := e.useDir(t, "work")
	laptop := e.dialRemote(t, "laptop")
	gpu := e.hosts.set("gpu", "ssh", true, 7, "")
	remote := filepath.Join(e.dir, "remote")
	os.MkdirAll(remote, 0o755)
	gw, err := e.a.AddWorkspace(context.Background(), "gpu", remote, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Workspace, 1)
	go func() {
		ws, _ := e.a.AddWorkspace(context.Background(), "laptop", "/data", false)
		done <- ws
	}()
	ls := laptop.expect(proto.TLs)
	laptop.send(proto.Msg{T: proto.TLsResult, ID: ls.ID, OK: proto.Bool(true), Path: "/data"})
	lw := <-done
	e.say("/dir")
	e.ch.wait(t, "工作目录列表")
	ids := []string{e.a.Workspaces()[0].ID, lw.ID, gw.ID}
	if err := e.a.UpdateSession(mainSID, SessionPatch{Workspaces: &ids}); err != nil {
		t.Fatal(err)
	}

	e.say("三台机器一起干")
	in := e.k.expect(proto.TInput) // 第一个工作目录在本机：本机的内核来跑
	if len(in.Workspaces) != 3 || !in.Workspaces[0].Local || in.Workspaces[1].Local || in.Workspaces[2].Local || in.Workspaces[2].Landlock != 7 {
		t.Fatalf("input: %+v", in.Workspaces)
	}
	// 电脑上的工作目录：网关请电脑的内核代为执行
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c1", Name: "Shell", Args: json.RawMessage(`{"command":"ls"}`), Workspace: "laptop:/data"})
	run := laptop.expect(proto.TRun)
	var req map[string]any
	json.Unmarshal(run.Req, &req)
	if req["tool"] != "Shell" || req["dirs"].([]any)[0] != "/data" || req["write"] != true {
		t.Fatalf("run: %s", run.Req)
	}
	laptop.send(proto.Msg{T: proto.TRunResult, ID: run.ID, OK: proto.Bool(true), Content: "laptop 上的文件"})
	if r := e.k.expect(proto.TExecResult); r.ID != "c1" || r.Content != "laptop 上的文件" {
		t.Fatalf("exec_result: %+v", r)
	}
	// 技能文件放到挂着的每台机器上
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c2", Name: "Skill", Args: json.RawMessage(`{"name":"demo"}`)})
	put := laptop.expect(proto.TSkillPut)
	if put.Skill != "demo" || len(put.Files) != 2 {
		t.Fatalf("skill_put: %+v", put)
	}
	laptop.send(proto.Msg{T: proto.TRunResult, ID: put.ID, OK: proto.Bool(true)})
	if r := e.k.expect(proto.TExecResult); r.OK == nil || !*r.OK || strings.Contains(r.Content, "没能放到") {
		t.Fatalf("skill: %+v", r)
	}
	if len(gpu.put["~/.nano-agent/skills/demo"]) != 2 || len(e.hosts.hosts[host.LocalName].put["~/.nano-agent/skills/demo"]) != 2 {
		t.Fatalf("put: %+v %+v", gpu.put, e.hosts.hosts[host.LocalName].put)
	}
	if info, _ := e.a.Session(mainSID); info.Workspaces[0].How != "local" || info.Workspaces[1].How != "gateway" || info.Workspaces[2].How != "gateway" {
		t.Fatalf("info: %+v", info.Workspaces)
	}
	// 代执行时电脑断开了：结果说明连不上
	e.k.send(proto.Msg{T: proto.TExec, SID: mainSID, ID: "c3", Name: "Read", Args: json.RawMessage(`{"path":"a"}`), Workspace: "laptop:/data"})
	laptop.expect(proto.TRun)
	laptop.conn.Close()
	if r := e.k.expect(proto.TExecResult); r.ID != "c3" || r.OK == nil || *r.OK || !strings.Contains(r.Content, "断开") {
		t.Fatalf("exec_result: %+v", r)
	}
	_ = work
}

// 旧版本的设置（机器和目录分开设）迁移成工作目录列表
func TestWorkspaceMigration(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "conf")
	os.MkdirAll(conf, 0o755)
	os.WriteFile(filepath.Join(conf, "kernel.conf"), []byte("gateway = unix:/x\nnode = tofu-pc\n"), 0o600)
	store, _ := session.New(filepath.Join(dir, "data"))
	store.SaveSettings("a", session.Settings{Node: "tofu-pc", DirsSet: true, Dirs: []string{"/a", "/a2"}, Title: "A"})
	store.SaveSettings("b", session.Settings{Node: "gpu", DirsSet: true, Dirs: []string{"~/b"}})
	store.SaveSettings("c", session.Settings{Node: "gpu"})
	store.SaveSettings("d", session.Settings{Title: "默认"})
	cfg := &config.Config{Dir: conf, Workspace: config.Workspace{Dirs: []string{"/x"}, Mode: "ask"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(context.Background(), cfg, nil, store, persona.New(dir, log), log)

	labels := map[string]Workspace{}
	for _, w := range a.wss {
		labels[w.Label()] = w
	}
	if len(a.wss) != 4 || !labels["本机:/x"].Default || labels["本机:/a"].ID == "" || labels["gpu:~/b"].ID == "" {
		t.Fatalf("workspaces: %+v", a.wss)
	}
	check := func(sid, want string) {
		t.Helper()
		var labels []string
		for _, w := range a.workspaceOf(sid).wss {
			labels = append(labels, w.Label())
		}
		got := strings.Join(labels, ",")
		if got != want {
			t.Fatalf("%s: %q，应该是 %q", sid, got, want)
		}
		if st := a.store.Settings(sid); st.Node != "" || st.DirsSet || st.Dirs != nil {
			t.Fatalf("旧的设置没清掉：%+v", st)
		}
	}
	check("a", "本机:/a,本机:/a2")
	check("b", "gpu:~/b")
	check("c", "")
	check("d", "本机:/x")
	if b, err := os.ReadFile(filepath.Join(dir, "data", "workspaces.json")); err != nil || !strings.Contains(string(b), "gpu") {
		t.Fatalf("没有保存：%v %s", err, b)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等不到")
}

// 技能放在网关上：加上、删掉都不用重连内核，下一轮的工具定义跟着变
func TestGatewaySkills(t *testing.T) {
	e := setup(t)
	e.useDir(t, "work")
	e.addSkill(t, "demo")
	e.say("用一下技能")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	got := e.llm.lastReq().Tools
	if len(got) != 4 || got[0].Function.Name != "Gateway" || got[1].Function.Name != "GatewayAdmin" ||
		got[2].Function.Name != "Skill" || got[3].Function.Name != "echo" ||
		!strings.Contains(got[2].Function.Description, "- demo：示例技能") {
		t.Fatalf("模型没收到技能工具：%+v", got)
	}
	if s := e.a.Skills(); len(s) != 1 || s[0].Name != "demo" {
		t.Fatalf("skills: %+v", s)
	}
	// 内核报上来的 Skill（旧内核）不用
	e.k.send(proto.Msg{T: proto.TTools, Tools: []proto.ToolSpec{{Name: "Skill", Description: "旧的"}, echoTool[0]}})
	os.RemoveAll(filepath.Join(e.dir, "skills", "demo"))
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Tools) == 1 })
	e.say("技能删掉了")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if len(e.llm.lastReq().Tools) != 4 {
		t.Fatal("两次整理之间工具快照改变")
	}
	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	e.say("刷新后再看技能")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if got := e.llm.lastReq().Tools; len(got) != 3 || got[0].Function.Name != "Gateway" || got[1].Function.Name != "GatewayAdmin" || got[2].Function.Name != "echo" {
		t.Fatalf("模型仍然带着已经删除的技能：%+v", got)
	}
}
