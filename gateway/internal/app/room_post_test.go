package app

import (
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
)

// threeRoom 建一个三人群（default 小纳、bob 小鲍、cat 小猫），bob 和 cat 有一个私下频道 s「密谈」。
func threeRoom(t *testing.T, e *env, r scene.Rules) (rooms.Room, *rooms.Store) {
	t.Helper()
	store, _ := withAgents(t, e)
	addAgent(t, e, store, agents.Agent{ID: "bob", Name: "小鲍"}, "你是小鲍")
	addAgent(t, e, store, agents.Agent{ID: "cat", Name: "小猫"}, "你是小猫")
	st, _ := rooms.Open(filepath.Join(e.dir, "data", "rooms.json"))
	e.a.SetRooms(st)
	r.Channels = []scene.Channel{{ID: "s", Name: "密谈", Members: []string{"bob", "cat"}}}
	room, err := st.Create(rooms.Room{Name: "群", Members: []string{"default", "bob", "cat"}, Rules: r})
	if err != nil {
		t.Fatal(err)
	}
	return room, st
}

// lines 是某个会话历史里的所有内容。
func lines(e *env, sid string) []string {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	var out []string
	for _, m := range e.a.store.History(sid) {
		out = append(out, m.Content)
	}
	return out
}

func has(e *env, sid, want string) bool {
	for _, l := range lines(e, sid) {
		if l == want {
			return true
		}
	}
	return false
}

// 分发矩阵：公开、频道、cue、旁白四种情况下，每个成员会话收到什么、群记录里是什么格式。
func TestRoomPostMatrix(t *testing.T) {
	e := setup(t)
	room, _ := threeRoom(t, e, scene.Rules{})
	log := roomLogSID(room.ID)
	member := func(id string) string { return e.a.roomMemberSID(id, room.ID) }

	e.a.mu.Lock()
	e.a.roomHumanSay(room, "张三", "大家好")
	e.a.mu.Unlock()
	for _, sid := range []string{log, member("default"), member("bob"), member("cat")} {
		if !has(e, sid, "[张三] 大家好") {
			t.Errorf("公开的话 %s 应该收到：%v", sid, lines(e, sid))
		}
	}

	if err := e.a.RoomSay(room.ID, "只告诉你们", "张三", "s", "我"); err != nil {
		t.Fatal(err)
	}
	for sid, want := range map[string]bool{log: true, member("bob"): true, member("cat"): true, member("default"): false} {
		if has(e, sid, "[#密谈][张三] 只告诉你们") != want {
			t.Errorf("频道里的话 %s 收到=%v，应为 %v", sid, !want, want)
		}
	}

	if err := e.a.RoomSay(room.ID, "夜深了", "旁白", "", "我"); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{log, member("default"), member("bob"), member("cat")} {
		if !has(e, sid, "[旁白] 夜深了") {
			t.Errorf("旁白 %s 应该收到", sid)
		}
	}

	e.a.mu.Lock()
	e.a.roomPost(room, roomLine{Kind: lineCue, To: "bob", Text: "去开门"}, postOpt{})
	e.a.mu.Unlock()
	if !has(e, log, "[导演→小鲍] 去开门") || !has(e, member("bob"), "[导演] 去开门") {
		t.Errorf("cue：群记录 %v，小鲍 %v", lines(e, log), lines(e, member("bob")))
	}
	for _, id := range []string{"default", "cat"} {
		for _, l := range lines(e, member(id)) {
			if strings.Contains(l, "去开门") {
				t.Errorf("cue 只有对象收到，%s 也收到了：%q", id, l)
			}
		}
	}

	// agent 的话：公开的话说话的人自己不再收；频道里的话只有频道里别的成员收
	e.a.mu.Lock()
	e.a.roomAgentSay(room, "bob", "我在", "")
	e.a.roomAgentSay(room, "bob", "你听见了吗", "s")
	e.a.mu.Unlock()
	if !has(e, log, "[小鲍] 我在") || !has(e, member("default"), "[小鲍] 我在") || !has(e, member("cat"), "[小鲍] 我在") || has(e, member("bob"), "[小鲍] 我在") {
		t.Errorf("公开的 agent 发言：%v", lines(e, log))
	}
	if !has(e, log, "[#密谈][小鲍] 你听见了吗") || !has(e, member("cat"), "[#密谈][小鲍] 你听见了吗") ||
		has(e, member("default"), "[#密谈][小鲍] 你听见了吗") || has(e, member("bob"), "[#密谈][小鲍] 你听见了吗") {
		t.Errorf("频道里的 agent 发言：default=%v", lines(e, member("default")))
	}
	// 群记录里 agent 的话是助手消息，带 Agent
	e.a.mu.Lock()
	var ok bool
	for _, m := range e.a.store.History(log) {
		if m.Content == "[小鲍] 我在" && m.Role == "assistant" && m.Agent == "bob" {
			ok = true
		}
	}
	e.a.mu.Unlock()
	if !ok {
		t.Error("agent 的话在群记录里应是助手消息")
	}
}

// 校验：频道成员必须是群成员，退群时一并去掉；say 不存在的频道报错。
func TestRoomChannelsFollowMembers(t *testing.T) {
	e := setup(t)
	room, st := threeRoom(t, e, scene.Rules{})
	if err := e.a.RoomSay(room.ID, "x", "", "nope", "我"); err == nil {
		t.Fatal("没有的频道要报错")
	}
	_, err := e.a.UpdateRoom(room.ID, RoomPatch{ScenePatch: ScenePatch{Channels: &[]scene.Channel{{ID: "s", Name: "密谈", Members: []string{"bob", "ghost"}}}}})
	if err == nil {
		t.Fatal("频道里有不在群里的人应该拒绝")
	}
	members := []string{"default", "bob"}
	_, err = e.a.UpdateRoom(room.ID, RoomPatch{Members: &members})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(room.ID)
	if len(got.Channels) != 1 || len(got.Channels[0].Members) != 1 || got.Channels[0].Members[0] != "bob" {
		t.Fatalf("小猫退群后频道里只剩小鲍：%+v", got.Channels)
	}
}
