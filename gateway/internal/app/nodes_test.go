package app

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/proto"
)

// dialKernel 再连一个名叫 name 的内核上来
func (e *env) dialKernel(t *testing.T, name string) *fakeKernel {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(e.dir, "k.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	k := &fakeKernel{t: t, conn: conn, sc: bufio.NewScanner(conn)}
	k.send(proto.Msg{T: proto.THello, V: proto.Version, Token: "tok", Node: name,
		Tools: []proto.ToolSpec{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
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
