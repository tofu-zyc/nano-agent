package app

import (
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"time"
)

// 房间：人在群里说话转给所有成员；被叫到的 2 秒后回一句；群记录里能看到。
func TestRoomFanout(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, err := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	room, err := st.Create(rooms.Room{Name: "产品组", Members: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	sid := roomLogSID(room.ID)
	e.a.OnMessage(e.a.Web(), channel.Message{ChatID: "local", SenderID: "u1", SenderName: "张三",
		Text: "@小纳 大家好", Session: sid})
	member := e.a.roomMemberSID("default", room.ID)
	in := e.k.expect(proto.TInput)
	if in.SID != member {
		t.Fatalf("应该由成员会话跑：%q，实际 %q", member, in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: member})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: member, Status: proto.StatusOK})
	waitFor(t, func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		h := e.a.store.History(sid)
		if len(h) < 2 {
			return false
		}
		last := h[len(h)-1]
		return last.Role == "assistant" && last.Agent == "default" && strings.Contains(last.Content, "（mock）")
	})
}

// 两个 agent 的私聊：第一次发消息时自动建成两人群。
func TestRoomDM(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, err := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	if err := e.a.agentStore.Create(testAgent("bob")); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	r, err := e.a.dmRoom("default", "bob")
	e.a.mu.Unlock()
	if err != nil {
		t.Fatalf("建私聊失败：%v", err)
	}
	if !r.IsDM() || len(r.Members) != 2 {
		t.Fatalf("私聊房间不对：%+v", r)
	}
	e.a.mu.Lock()
	r2, _ := e.a.dmRoom("bob", "default")
	e.a.mu.Unlock()
	if r2.ID != r.ID {
		t.Fatalf("同一个私聊应该复用一个房间：%s %s", r.ID, r2.ID)
	}
}

// /room 列出、暂停、恢复。
func TestRoomCommand(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, err := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetRooms(st)
	room, err := st.Create(rooms.Room{Name: "产品组", Members: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	c := in{s: e.a.sessOf(e.sid), ep: endpoint{ch: "fake", chat: "c1"}, who: "fake:u1"}
	e.a.mu.Lock()
	e.a.cmdRoom(c, "")
	e.a.mu.Unlock()
	got := e.ch.wait(t, "产品组")
	if !strings.Contains(got, "在聊") {
		t.Fatalf("/room 没列出状态：%q", got)
	}
	e.a.mu.Lock()
	e.a.cmdRoom(c, "产品组 pause")
	e.a.mu.Unlock()
	e.ch.wait(t, "已暂停")
	if r, _ := st.Get(room.ID); !r.Paused {
		t.Fatal("没真的暂停")
	}
	e.a.mu.Lock()
	e.a.cmdRoom(c, "产品组 resume")
	e.a.mu.Unlock()
	e.ch.wait(t, "恢复了")
	if r, _ := st.Get(room.ID); r.Paused {
		t.Fatal("没真的恢复")
	}
}

// 建群时把群记录会话的标题设成群名（会话列表里好看）。
func TestRoomTitle(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	name := "产品组"
	info, err := e.a.CreateRoom(RoomPatch{Name: &name, Members: &[]string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	title := e.a.store.Settings(info.SID).Title
	e.a.mu.Unlock()
	if title != "产品组" {
		t.Fatalf("会话标题没设成群名：%q", title)
	}
}

// Message 工具：给某个 agent 发消息会自动建两人私聊，并让对方那边跑一轮。
func TestMessageToolToAgent(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	if err := e.a.agentStore.Create(testAgent("bob")); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	e.a.messageTool(mainSID, "{\"to\":\"agent:bob\",\"text\":\"有空吗\"}", func(ok bool, text string) {
		done <- text
	})
	select {
	case text := <-done:
		if !strings.Contains(text, "发进") {
			t.Fatalf("Message 结果不对：%q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Message 没回复")
	}
	e.a.mu.Lock()
	var dm rooms.Room
	for _, r := range e.a.roomStore.List() {
		if r.IsDM() {
			dm = r
		}
	}
	member := e.a.roomMemberSID("bob", dm.ID)
	hist := e.a.store.History(member)
	e.a.mu.Unlock()
	if len(hist) == 0 || !strings.Contains(hist[len(hist)-1].Content, "有空吗") {
		t.Fatalf("对方没收到：%+v", hist)
	}
	e.a.mu.Lock()
	e.a.roomState(dm.ID).Pending = map[string][]chan string{}
	e.a.mu.Unlock()
}
