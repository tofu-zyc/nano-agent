package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
)

// helperMCP 接上一个真的 MCP 子进程（这个测试程序自己，见 mcpHelper），等它连好。
func helperMCP(t *testing.T, e *env, approval string) *mcp.Manager {
	t.Helper()
	if testing.Short() {
		t.Skip("要起子进程")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr)
	if err := e.a.SaveMCP([]mcp.Server{{Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true, Approval: approval}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.TestMCP("helper"); err != nil {
		t.Fatal(err)
	}
	return mgr
}

func (e *env) callMCP(sid, id, name string) {
	e.a.mu.Lock()
	s := e.a.sessOf(sid)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: sid, ID: id, Name: name, Args: json.RawMessage(`{}`)})
	e.a.mu.Unlock()
}

// 死锁回归：machine= 的服务正在连（内核在线但还没回话），这时有人拿着总锁算工具定义。
// 以前连接拿着 conn 的锁去等总锁（PipeOpen），算工具定义拿着总锁等 conn 的锁，永久死锁。
func TestMCPPipeConnectingNoDeadlock(t *testing.T) {
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]mcp.Server{{Name: "remote", Machine: "test", Command: "python3", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	mgr.SetLink(MCPLink(e.a))
	e.a.mu.Lock() // 网页刷新会话列表正好在这时候拿着总锁
	e.a.mcpStore, e.a.mcp = st, mgr
	mgr.Warm(2 * time.Second) // 假内核不会回 pipe_open：连接会一直等到超时
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		e.a.contextParts(mainSID)
		e.a.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("死锁：算工具定义在等正在进行的 MCP 连接")
	}
}

// 本机进程起得慢（npx 第一次要下载）：连接期间网关不能被卡住。
func TestMCPSlowStartDoesNotStall(t *testing.T) {
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	// sleep 起来了但不说 MCP：连接一直等到它退出
	if err := st.Save([]mcp.Server{{Name: "slow", Command: "sleep", Args: []string{"3"}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr) // 后台开始连
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	e.a.mu.Lock()
	e.a.contextParts(mainSID)
	e.a.mu.Unlock()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("算工具定义等了 %v（在等后台的 MCP 连接，整个网关跟着卡住）", d)
	}
}

// 调用到一半用户停下：内核发 exec_cancel，紧接着 done。以前收尾时 s.run 已经是 nil，网关直接崩溃。
// 取消不是连接坏了：连接要留着，下一轮还能接着用（computer use 这类服务的状态不丢）。
func TestMCPStopDuringCall(t *testing.T) {
	e := setup(t)
	mgr := helperMCP(t, e, "")
	for i := 0; i < 5; i++ {
		e.say("等一下")
		in := e.k.expect(proto.TInput)
		e.callMCP(in.SID, "w1", "helper__wait")
		time.Sleep(100 * time.Millisecond)
		e.k.send(proto.Msg{T: proto.TExecCancel, SID: in.SID, ID: "w1"})
		e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, RunID: in.RunID, Status: proto.StatusCancelled})
		// The cancelled RPC can reply before the peer's done frame obtains
		// App.mu. Finish that run before sending the next independent message;
		// otherwise this fixture accidentally exercises interjection recovery.
		deadline := time.Now().Add(3 * time.Second)
		for {
			e.a.mu.Lock()
			session := e.a.sessions[in.SID]
			active := session != nil && session.run != nil
			e.a.mu.Unlock()
			if !active {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cancelled run did not finish")
			}
			time.Sleep(time.Millisecond)
		}
		res := e.k.expect(proto.TExecResult)
		if res.OK == nil || *res.OK {
			t.Fatalf("取消的调用应该回失败：%+v", res)
		}
	}
	if online, _ := mgr.Status("helper"); !online {
		t.Fatal("用户停下不该把 MCP 连接也断掉")
	}
	e.say("再来")
	in := e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(in.SID)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: in.SID, ID: "c2", Name: "helper__echo", Args: json.RawMessage(`{"text":"还在"}`)})
	e.a.mu.Unlock()
	if res := e.k.expect(proto.TExecResult); res.OK == nil || !*res.OK || res.Content != "echo:还在" {
		t.Fatalf("停下以后再调：%+v", res)
	}
}

// 等审批的时候这一轮结束了：审批单撤掉，等着的调用马上回“没有执行”，以后批也不会执行。
func TestMCPApprovalReleasedWhenRunEnds(t *testing.T) {
	e := setup(t)
	helperMCP(t, e, mcp.ApprovalAlways)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.callMCP(in.SID, "c1", "helper__echo")
	waitFor(t, func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return len(e.a.approvals) == 1
	})
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusCancelled})
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK || !strings.Contains(res.Content, "没有执行") {
		t.Fatalf("这一轮结束了，等审批的调用应该马上回没有执行：%+v", res)
	}
	e.a.mu.Lock()
	left := len(e.a.approvals)
	e.a.mu.Unlock()
	if left != 0 {
		t.Fatalf("审批单应该撤掉了，还剩 %d 张", left)
	}
}

// 内核说这次调用不要了（超时）：审批单撤掉，管理员之后再批也不会执行。
func TestMCPCancelWithdrawsApproval(t *testing.T) {
	e := setup(t)
	helperMCP(t, e, mcp.ApprovalAlways)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.callMCP(in.SID, "c1", "helper__echo")
	var n int
	waitFor(t, func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		for k := range e.a.approvals {
			n = k
		}
		return len(e.a.approvals) == 1
	})
	e.k.send(proto.Msg{T: proto.TExecCancel, SID: in.SID, ID: "c1"})
	if res := e.k.expect(proto.TExecResult); res.OK == nil || *res.OK {
		t.Fatalf("取消以后应该回没有执行：%+v", res)
	}
	if err := e.a.Approve(n, true); err == nil {
		e.k.none() // 万一批得进去，也不能真的执行
	}
}

// 已经有工具快照的会话：服务一开始连不上（命令写错），改好以后连上了，工具要出现。
// 以前快照的签名只看「哪些服务开着、在哪台机器」，只改命令的话签名不变，工具永远出不来。
func TestMCPToolsAppearWhenServerComesUp(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	st, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr)
	if err := e.a.SaveMCP([]mcp.Server{{Name: "helper", Command: "/nonexistent-mcp", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")

	exe, _ := os.Executable()
	if err := e.a.SaveMCP([]mcp.Server{{Name: "helper", Command: exe, Args: []string{"-test.run=TestNoSuchTest"},
		Env: map[string]string{mcpHelperEnv: "1"}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(mgr.CachedTools("default")) > 0 })
	e.say("再来")
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	if !hasTool(e.llm.lastReq().Tools, "helper__echo") {
		t.Fatalf("服务连上以后已有会话还是看不到它的工具：%v", toolNames(e.llm.lastReq().Tools))
	}
}

// 只读模式下建的快照没有 MCP 工具；切回别的模式，工具要回来（不用 /new）。
func TestMCPModeSwitchRefreshesTools(t *testing.T) {
	e := setup(t)
	helperMCP(t, e, "")
	setMode := func(mode string) {
		e.a.mu.Lock()
		st := e.a.store.Settings(mainSID)
		st.Mode = mode
		e.a.store.SaveSettings(mainSID, st)
		e.a.mu.Unlock()
	}
	setMode(proto.ModeReadonly)
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	if hasTool(e.llm.lastReq().Tools, "helper__echo") {
		t.Fatal("只读模式不该有 MCP 工具")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")
	setMode(proto.ModeAuto)
	e.say("再来")
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	if !hasTool(e.llm.lastReq().Tools, "helper__echo") {
		t.Fatalf("切回自动模式以后 MCP 工具没回来：%v", toolNames(e.llm.lastReq().Tools))
	}
}

// 旧内核（协议 v9）不认识 pipe_open：别发过去干等，直接说清楚。
func TestPipeOpenOldKernel(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.nodes["test"].info.Proto = 9
	e.a.mu.Unlock()
	err := e.a.PipeOpen("test", "p1", []string{"python3"}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "太旧") {
		t.Fatalf("旧内核应该直接报错：%v", err)
	}
	e.k.none()
}

// 一次返回好几张图、模型又不能看图：说明只加一次。
func TestMCPImagesNoteOnce(t *testing.T) {
	e := setup(t)
	withMedia(t, e)
	helperMCP(t, e, "")
	e.say("你好")
	in := e.k.expect(proto.TInput)
	e.callMCP(in.SID, "c1", "helper__pics")
	res := e.k.expect(proto.TExecResult)
	if n := strings.Count(res.Content, "不能看图"); n != 1 {
		t.Fatalf("说明应该只有一次，实际 %d 次：%q", n, res.Content)
	}
}
