package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
)

// dialKernel 再连一个名叫 name 的内核上来
func (e *env) dialKernel(t *testing.T, name string) *fakeKernel {
	t.Helper()
	return e.dialKernelWith(t, name, nil)
}

func (e *env) dialKernelWith(t *testing.T, name string, remotes []proto.Remote) *fakeKernel {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(e.dir, "k.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	k := &fakeKernel{t: t, conn: conn, sc: bufio.NewScanner(conn)}
	k.send(proto.Msg{T: proto.THello, V: proto.Version, Token: "tok", Node: name,
		Tools:   []proto.ToolSpec{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Remotes: remotes})
	k.expect(proto.TWelcome)
	waitNode(e.a, name, true)
	return k
}

// 会话的机器掉线了：别的内核顶上，这一轮只聊天，并告诉模型原因；机器回来后照常用工具
func TestSubstituteWhenNodeOffline(t *testing.T) {
	e := setup(t)
	e.a.cfg.Workspace.Dirs = []string{e.dir}
	phone := e.dialKernel(t, "phone")

	e.say("你好")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk || len(in.Dirs) != 1 {
		t.Fatalf("会话的机器在线时由它来跑：%+v", in)
	}
	e.roundTrip(t)

	e.k.conn.Close()
	waitNode(e.a, "test", false)
	e.say("还在吗")
	in := phone.expect(proto.TInput)
	if in.SID != mainSID || in.Mode != proto.ModeNone || len(in.Dirs) != 0 {
		t.Fatalf("替补只聊天：%+v", in)
	}
	phone.send(proto.Msg{T: proto.TModel, SID: mainSID})
	phone.expect(proto.TTurn)
	req := e.llm.lastReq()
	if len(req.Tools) != 0 || !strings.HasPrefix(lastUser(req.Messages), "[系统] 工作目录所在的机器 test 现在不在线，这一轮由 phone 接手") {
		t.Fatalf("request: tools=%d %q", len(req.Tools), lastUser(req.Messages))
	}
	phone.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "收到：还在吗")
	if info, _ := e.a.Session(mainSID); info.Node != "test" || info.NodeOnline {
		t.Fatalf("info: %+v", info)
	}

	// test 回来了：又由它来跑，环境说明重新给一次
	e.k = e.dialKernel(t, "test")
	e.say("回来了吗")
	if in := e.k.expect(proto.TInput); in.Mode != proto.ModeAsk {
		t.Fatalf("input: %+v", in)
	}
	if got := lastUser(e.roundTrip(t)); !strings.HasPrefix(got, "[系统] 在 test 上工作。工作目录：") {
		t.Fatalf("env note: %q", got)
	}
}

func TestNodeCommandAndRouting(t *testing.T) {
	e := setup(t)
	phone := e.dialKernel(t, "phone")
	e.say("/node")
	if msg := e.ch.wait(t, "这个会话在 test 上工作（在线）"); !strings.Contains(msg, "在线的机器：phone（和网关同一台机器，没有沙箱）、test（和网关同一台机器，没有沙箱）") {
		t.Fatalf("node: %q", msg)
	}
	e.say("/dir add " + e.dir + "/work")
	e.ch.wait(t, "工作目录")
	e.say("/node phone", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能切换机器")
	e.say("/node phone")
	e.ch.wait(t, "这个会话改在 phone 上工作了。原来的工作目录是 test 上的路径，已经清掉了")

	e.say("在手机上")
	if in := phone.expect(proto.TInput); in.Mode != proto.ModeNone {
		t.Fatalf("phone 上还没有工作目录，只聊天：%+v", in)
	}
	// 别的内核冒充这一轮发消息：不理，并告诉它没有这个任务
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	if m := e.k.expect(proto.TTurn); m.Reason != proto.ReasonError {
		t.Fatalf("turn: %+v", m)
	}
	e.say("/stop")
	if m := phone.expect(proto.TCancel); m.SID != mainSID {
		t.Fatalf("停止发给跑这一轮的内核：%+v", m)
	}
	phone.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	e.ch.wait(t, "（已停止）")

	e.say("/node default")
	e.ch.wait(t, "这个会话改在 test 上工作了。")
	if info, _ := e.a.Session(mainSID); info.Node != "test" || info.NodeSet || !info.NodeOnline {
		t.Fatalf("info: %+v", info)
	}
}

// 服务器（内核通过 SSH 执行工具）也是节点：会话选它，这一轮发给能连到它的内核，带上 node
func TestRemoteServerNode(t *testing.T) {
	e := setup(t)
	pc := e.dialKernelWith(t, "pc", []proto.Remote{{Name: "prod", Target: "deploy@prod", OK: true, Landlock: 7}})
	e.say("/node prod")
	e.ch.wait(t, "这个会话改在 prod 上工作了。")
	e.say("/dir add /srv/app")
	e.ch.wait(t, "/srv/app")
	e.say("看看服务器")
	in := pc.expect(proto.TInput)
	if in.Node != "prod" || in.Mode != proto.ModeAsk || len(in.Dirs) != 1 || in.Dirs[0] != "/srv/app" {
		t.Fatalf("input: %+v", in)
	}
	pc.send(proto.Msg{T: proto.TModel, SID: mainSID})
	pc.expect(proto.TTurn)
	if got := lastUser(e.llm.lastReq().Messages); !strings.HasPrefix(got, "[系统] 在 prod 上工作。工作目录：/srv/app") {
		t.Fatalf("env note: %q", got)
	}
	pc.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "收到：看看服务器")

	// 列服务器上的目录：发给 pc，带上 node
	go func() {
		m := pc.expect(proto.TLs)
		ok := m.Node == "prod" && m.Path == "~"
		pc.send(proto.Msg{T: proto.TLsResult, ID: m.ID, OK: proto.Bool(ok), Path: "/home/deploy", Home: "/home/deploy"})
	}()
	if l, err := e.a.ListDir(context.Background(), "prod", "~"); err != nil || l.Path != "/home/deploy" {
		t.Fatalf("ls: %+v %v", l, err)
	}

	// 服务器连不上了：替补只聊天，告诉模型原因
	pc.send(proto.Msg{T: proto.TRemotes, Remotes: []proto.Remote{{Name: "prod", OK: false, Detail: "连不上：Connection refused"}}})
	waitFor(t, func() bool { info, _ := e.a.Session(mainSID); return !info.NodeOnline })
	e.say("还能用吗")
	in = pc.expect(proto.TInput) // 两个内核都在本机，最近连上来的 pc 当替补
	if in.Node != "" || in.Mode != proto.ModeNone {
		t.Fatalf("input: %+v", in)
	}
	pc.send(proto.Msg{T: proto.TModel, SID: mainSID})
	pc.expect(proto.TTurn)
	if got := lastUser(e.llm.lastReq().Messages); !strings.HasPrefix(got, "[系统] 工作目录所在的服务器 prod 现在用不了（连不上：Connection refused）") {
		t.Fatalf("note: %q", got)
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

func TestSkillToolsUpdateWithoutReconnect(t *testing.T) {
	e := setup(t)
	e.a.cfg.Workspace.Dirs = []string{e.dir}
	initial := e.a.Snapshot().Kernels[0]
	skills := []proto.SkillSpec{{Name: "demo", Description: "示例技能"}}
	tools := []proto.ToolSpec{{Name: "Skill", Description: "先加载 demo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	e.k.send(proto.Msg{T: proto.TTools, Tools: tools, Skills: skills})
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Skills) == 1 })
	updated := e.a.Snapshot().Kernels[0]
	if updated.Since != initial.Since || !updated.Local || updated.Skills[0].Name != "demo" {
		t.Fatalf("更新工具不该重建节点：%+v", updated)
	}
	e.say("用一下技能")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if got := e.llm.lastReq().Tools; len(got) != 1 || got[0].Function.Name != "Skill" || got[0].Function.Description != "先加载 demo" {
		t.Fatalf("模型没收到新的技能工具：%+v", got)
	}

	// 删掉技能后，后续请求也不再带过期的工具定义。
	e.k.send(proto.Msg{T: proto.TTools})
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Skills) == 0 })
	e.say("技能删掉了")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if len(e.llm.lastReq().Tools) != 0 {
		t.Fatal("模型仍然带着已经删除的工具")
	}
}
