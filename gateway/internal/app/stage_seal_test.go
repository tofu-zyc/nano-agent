package app

import (
	"strings"
	"testing"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/scene"
)

// 一场戏的群记录和成员会话只追加：清空、撤回、删历史、删会话的入口都拒绝，而且不产生任何副作用。

func sealedErr(err error) bool { return err != nil && strings.Contains(err.Error(), "只追加") }

func TestStageSessionsAreAppendOnly(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	log, bob := roomLogSID(room.ID), e.a.roomMemberSID("bob", room.ID)
	e.a.RoomSay(room.ID, "@小鲍 在吗", "张三", "", "我")
	in := e.k.expect(proto.TInput) // 小鲍正在跑：拒绝不能把它停掉
	from, to := 0, 0
	for _, sid := range []string{log, bob} {
		for name, call := range map[string]func() error{
			"删全部历史": func() error { return e.a.DeleteHistory(sid, HistoryDelete{All: true}) },
			"删轮次":   func() error { return e.a.DeleteHistory(sid, HistoryDelete{From: &from, To: &to}) },
			"重置":    func() error { return e.a.ResetSession(sid) },
			"撤回":    func() error { _, err := e.a.UndoSession(sid); return err },
			"删会话":   func() error { return e.a.DeleteSession(sid) },
		} {
			if err := call(); !sealedErr(err) {
				t.Errorf("%s %s：应该拒绝，得到 %v", sid, name, err)
			}
		}
	}
	e.a.mu.Lock()
	s := e.a.sessions[bob]
	running := s != nil && s.run != nil && !s.resetAfter && !s.deleteAfter && len(e.a.store.History(log)) == 1
	exists := e.a.store.Exists(log) && e.a.store.Exists(bob)
	e.a.mu.Unlock()
	if !running || !exists {
		t.Fatalf("拒绝要在任何副作用之前：running=%v exists=%v", running, exists)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitRunDone(t, e.a, in.SID)
	// 只清摘要放过；压缩只挪 Base、不动文件，照常
	if err := e.a.DeleteHistory(bob, HistoryDelete{ClearSummary: true}); err != nil {
		t.Fatalf("只清摘要可以：%v", err)
	}
	// 记录没缺：第 1 句和小鲍的回复都在
	if got := lines(e, log); len(got) != 2 {
		t.Fatalf("群记录：%q", got)
	}
}

// 不是戏的群照旧能清空、撤回、删。
func TestNonStageSessionsStillClearable(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, scene.Rules{Mode: scene.ModeMention})
	log, bob := roomLogSID(room.ID), e.a.roomMemberSID("bob", room.ID)
	e.a.RoomSay(room.ID, "你们好", "张三", "", "我")
	if err := e.a.DeleteHistory(bob, HistoryDelete{All: true}); err != nil {
		t.Fatalf("普通群的成员会话可以清空：%v", err)
	}
	if err := e.a.ResetSession(bob); err != nil {
		t.Fatalf("可以重置：%v", err)
	}
	if _, err := e.a.UndoSession(log); sealedErr(err) {
		t.Fatalf("撤回不该因为戏被拒绝：%v", err)
	}
	if err := e.a.DeleteSession(e.a.roomMemberSID("cat", room.ID)); err != nil {
		t.Fatalf("可以删：%v", err)
	}
}
