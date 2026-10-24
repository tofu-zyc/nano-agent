package app

import (
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
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
		return last.Role == "assistant" && strings.Contains(last.Content, "（mock）")
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
