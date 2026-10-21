package app

import (
	"strings"
	"testing"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

func msgID(id string) func(*channel.Message) {
	return func(m *channel.Message) { m.MsgID = id }
}

func historyText(h []session.Msg) string {
	var b strings.Builder
	for _, m := range h {
		b.WriteString(m.Role + ":" + m.Content + "\n")
	}
	return b.String()
}

func TestUndo(t *testing.T) {
	e := setup(t)
	e.say("第一句", msgID("m1"))
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.say("第二句", msgID("m2"))
	e.k.expect(proto.TInput)
	e.roundTrip(t)

	// 处理完的一轮：连同回复从上下文里删掉，磁盘上也截掉
	e.say("撤回")
	e.ch.wait(t, "已撤回：「第二句」")
	if h := e.a.store.History(e.sid); len(h) != 2 || strings.Contains(historyText(h), "第二句") {
		t.Fatalf("撤回后的历史：\n%s", historyText(h))
	}
	disk, _ := session.New(e.a.store.Dir())
	if h := disk.History(e.sid); len(h) != 2 {
		t.Fatalf("磁盘上的历史没有截掉：\n%s", historyText(h))
	}
	e.say("第三句", msgID("m3"))
	e.k.expect(proto.TInput)
	msgs := e.roundTrip(t)
	var all strings.Builder
	for _, m := range msgs {
		all.WriteString(m.Content)
	}
	if strings.Contains(all.String(), "第二句") || !strings.Contains(all.String(), "第一句") || !strings.Contains(all.String(), "第三句") {
		t.Fatalf("模型看到的上下文不对：%s", all.String())
	}

	// 正在处理的：先停下，结束时删掉这一轮
	e.say("长任务", msgID("m4"))
	e.k.expect(proto.TInput)
	e.say("/undo")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "已撤回：「长任务」")
	if h := e.a.store.History(e.sid); strings.Contains(historyText(h), "长任务") {
		t.Fatalf("正在处理的一轮没有撤回：\n%s", historyText(h))
	}

	// 还在排队的：直接拿掉，正在处理的不受影响
	e.say("任务A", msgID("m5"))
	e.k.expect(proto.TInput)
	e.say("任务B", msgID("m6"))
	e.say("撤回。")
	e.ch.wait(t, "已撤回还没处理的消息：「任务B」")
	e.k.none()
	e.roundTrip(t)
	e.k.none() // 任务B 不会再处理

	// 平台上撤回：更早的消息只说明；最近一轮的撤回
	recall := func(id string) func(*channel.Message) {
		return func(m *channel.Message) { m.Text, m.MsgID, m.SenderID, m.Action = "", id, "", channel.ActionRecall }
	}
	e.say("", recall("m1"))
	e.ch.wait(t, "后面已经接着聊了")
	e.say("", recall("m5"))
	e.ch.wait(t, "已撤回：「任务A」")
	e.say("", recall("unknown"))
	e.k.none()
	if who := e.a.eps["fake:c1"].Who; who != "fake:u1" {
		t.Fatalf("撤回事件改了窗口的记录：%q", who)
	}

	e.say("/reset")
	e.ch.wait(t, "上下文已清空")
	e.say("撤回")
	e.ch.wait(t, "没有可以撤回的对话")
}

func TestStopWords(t *testing.T) {
	e := setup(t)
	e.say("长任务")
	e.k.expect(proto.TInput)
	e.say("停")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: e.sid, Status: proto.StatusCancelled})
	e.ch.wait(t, "（已停止）")

	// 没有任务时，「停」是普通的话
	e.say("停")
	if in := e.k.expect(proto.TInput); in.SID != e.sid {
		t.Fatalf("input: %+v", in)
	}
	e.roundTrip(t)

	// 停止按钮（终端 Ctrl-C）：没有任务时什么也不说
	stop := func(m *channel.Message) { m.Text, m.Action = "", channel.ActionStop }
	e.say("", stop)
	e.k.none()
	select {
	case msg := <-e.ch.sent:
		t.Fatalf("没有任务时不该回话：%s", msg)
	default:
	}
	e.say("又一个任务")
	e.k.expect(proto.TInput)
	e.say("", stop)
	e.k.expect(proto.TCancel)
}

func TestUndoRestatesEnvironment(t *testing.T) {
	e := setup(t)
	dir := t.TempDir()
	e.say("/dir add " + dir)
	e.ch.wait(t, dir)
	e.say("看看目录")
	in := e.k.expect(proto.TInput)
	if len(in.Dirs) == 0 {
		t.Fatalf("input: %+v", in)
	}
	msgs := e.roundTrip(t)
	if !strings.Contains(lastUser(msgs), "工作目录") {
		t.Fatalf("第一条消息前面应当有环境说明：%s", lastUser(msgs))
	}
	e.say("撤回")
	e.ch.wait(t, "已撤回")
	// 环境说明跟着那一轮删掉了，下一条消息前面重新说
	e.say("再看看")
	e.k.expect(proto.TInput)
	if msgs := e.roundTrip(t); !strings.Contains(lastUser(msgs), "工作目录") {
		t.Fatalf("撤回后没有重新说明环境：%s", lastUser(msgs))
	}
}
