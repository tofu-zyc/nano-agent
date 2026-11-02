package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/session"
)

// 平行世界和倒带（设计 §14.2）。

func sessionText(msgs []session.Msg) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Role+"|"+m.Content)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// worldState 是一场戏现在的全部状态：群记录、每个成员的会话、场景记录。
type worldState struct {
	log     []string
	members map[string][]string
	stage   []StageRec
}

func snapshotWorld(t *testing.T, e *env, id string) worldState {
	t.Helper()
	e.a.mu.Lock()
	ws := worldState{log: sessionText(e.a.store.Archive(roomLogSID(id))), members: map[string][]string{}}
	for _, m := range []string{"default", "bob", "cat"} {
		ws.members[m] = sessionText(e.a.store.Archive(e.a.roomMemberSID(m, id)))
	}
	e.a.mu.Unlock()
	ws.stage = timeline(t, e, id)
	return ws
}

func sameWorld(t *testing.T, what string, a, b worldState) {
	t.Helper()
	if !sameStrings(a.log, b.log) {
		t.Fatalf("%s：群记录变了\n%q\n%q", what, a.log, b.log)
	}
	for m := range a.members {
		if !sameStrings(a.members[m], b.members[m]) {
			t.Fatalf("%s：%s 的会话变了", what, m)
		}
	}
	if len(a.stage) != len(b.stage) {
		t.Fatalf("%s：场景记录从 %d 条变成 %d 条", what, len(a.stage), len(b.stage))
	}
	for i := range a.stage {
		x, y := a.stage[i], b.stage[i]
		if x.Seq != y.Seq || x.Kind != y.Kind || x.Text != y.Text || x.Line != y.Line {
			t.Fatalf("%s：场景记录第 %d 条变了：%+v / %+v", what, i+1, x, y)
		}
	}
}

// firstLineOf 是某个成员第一次说话的句子号。
func firstLineOf(t *testing.T, recs []StageRec, member string) int {
	t.Helper()
	for _, r := range kindsOf(recs, "line") {
		if r.From == member && r.Say == "say" {
			return r.Line
		}
	}
	t.Fatalf("%s 没说过话", member)
	return 0
}

func TestStageForkTakesPrefixAndLeavesOriginalAlone(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, stageRules(scene.Rules{Order: []string{"default", "bob", "cat"}, Rounds: 3, Inner: true}), m)
	e.a.RoomSay(id, "夜深了", "旁白", "", "我")
	runRounds(t, e, id, 3, "default", "bob", "cat")
	orig := snapshotWorld(t, e, id)

	at := firstLineOf(t, orig.stage, "cat") // 第一轮结束的那一句
	info, err := e.a.ForkRoom(id, ForkRequest{At: at})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == id || !info.Stage || !info.Inner || info.Paused || info.Mode != scene.ModeTurns {
		t.Fatalf("新群：%+v", info)
	}
	if info.Name != "群 · 平行 1" {
		t.Fatalf("默认名字：%q", info.Name)
	}
	if info.Use.Total != 0 || info.Use.Round != 0 {
		t.Fatalf("用量从零开始：%+v", info.Use)
	}

	fork := snapshotWorld(t, e, info.ID)
	// 群记录是原群记录的前 at 句
	if !sameStrings(fork.log, orig.log[:at]) {
		t.Fatalf("群记录：\n%q\n应为\n%q", fork.log, orig.log[:at])
	}
	// 每个成员的会话是原会话的前缀，长度等于记录里的 cursors
	var cursors map[string]int
	for _, r := range kindsOf(orig.stage, "cursors") {
		if r.Line == at {
			cursors = r.Cursors
		}
	}
	for _, mem := range []string{"default", "bob", "cat"} {
		want := orig.members[mem][:cursors[mem]]
		if !sameStrings(fork.members[mem], want) || len(want) == 0 {
			t.Fatalf("%s 的会话：\n%q\n应为\n%q", mem, fork.members[mem], want)
		}
	}
	// 场景记录：到这一句为止相同，最后一条是 fork
	cut := 0
	for i, r := range orig.stage {
		if r.Kind == "cursors" && r.Line == at {
			cut = i + 1
		}
	}
	if len(fork.stage) != cut+1 {
		t.Fatalf("场景记录 %d 条，应为 %d+1", len(fork.stage), cut)
	}
	for i := 0; i < cut; i++ {
		x, y := orig.stage[i], fork.stage[i]
		if x.Kind != y.Kind || x.Text != y.Text || x.Line != y.Line || x.Hash != y.Hash || x.Run != y.Run || x.TS != y.TS {
			t.Fatalf("场景记录第 %d 条不同：%+v / %+v", i+1, x, y)
		}
		if y.Seq != uint64(i+1) {
			t.Fatalf("新文件的序号要连续：%d", y.Seq)
		}
	}
	last := fork.stage[cut]
	if last.Kind != "fork" || !strings.Contains(string(last.Detail), id) || !strings.Contains(string(last.Detail), `"at":`) {
		t.Fatalf("最后一条是 fork：%+v %s", last, last.Detail)
	}
	// 原来那场没变
	sameWorld(t, "分出之后的原群", orig, snapshotWorld(t, e, id))

	// 在新群里说一句不同的旁白，接着跑：原群的一切仍然没变
	e.a.RoomSay(info.ID, "天亮了", "旁白", "", "我")
	for _, who := range []string{"default", "bob", "cat"} {
		step(t, e, e.a.roomMemberSID(who, info.ID))
	}
	after := snapshotWorld(t, e, info.ID)
	if len(after.log) != at+1+3 || !strings.HasSuffix(after.log[at], "天亮了") {
		t.Fatalf("新群接着跑：%q", after.log)
	}
	if got := kindsOf(after.stage, "line"); got[len(got)-1].Line != at+4 {
		t.Fatalf("新群的句子号从原来的位置接着数：%+v", got[len(got)-1])
	}
	sameWorld(t, "新群跑了之后的原群", orig, snapshotWorld(t, e, id))
}

func TestStageForkRefusesBusyLineAndNamesNearest(t *testing.T) {
	e := setup(t)
	e.a.SetLLM(&scriptModel{reply: say})
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
	e.a.RoomSay(room.ID, "@小鲍 在吗", "张三", "", "我") // 第 1 句：没人在跑，能分出
	in := e.k.expect(proto.TInput)
	e.a.RoomSay(room.ID, "对了还有件事", "张三", "", "我") // 第 2 句：小鲍在跑，不能分出
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitRunDone(t, e.a, in.SID)
	before := len(e.a.Rooms())
	_, err := e.a.ForkRoom(room.ID, ForkRequest{At: 2})
	if err == nil || !strings.Contains(err.Error(), "最近一个可以分出的是第 1 句") {
		t.Fatalf("应该拒绝并给出最近能分出的句子：%v", err)
	}
	if _, err := e.a.ForkRoom(room.ID, ForkRequest{At: 1}); err != nil {
		t.Fatal("第 1 句可以分出：", err)
	}
	if _, err := e.a.ForkRoom(room.ID, ForkRequest{At: 99}); err == nil {
		t.Fatal("超出的句子要拒绝")
	}
	if _, err := e.a.ForkRoom(room.ID, ForkRequest{At: 0}); err == nil {
		t.Fatal("at 要从 1 起")
	}
	if len(e.a.Rooms()) != before+1 {
		t.Fatalf("只有成功的那次多了一个群：%d → %d", before, len(e.a.Rooms()))
	}
}

func TestStageForkRewindPausesOriginal(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, stageRules(scene.Rules{Order: []string{"default", "bob", "cat"}, Rounds: 3}), m)
	e.a.RoomSay(id, "夜深了", "旁白", "", "我")
	step(t, e, e.a.roomMemberSID("default", id)) // 小纳说完，小鲍已经被叫起来了
	// 先让这一轮走完，找一个能分出的句子
	step(t, e, e.a.roomMemberSID("bob", id))
	step(t, e, e.a.roomMemberSID("cat", id))
	recs := timeline(t, e, id)
	at := firstLineOf(t, recs, "default")
	info, err := e.a.ForkRoom(id, ForkRequest{At: at, Rewind: true, Name: "重来"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "重来" {
		t.Fatalf("名字：%q", info.Name)
	}
	if p, _ := paused(e, id); !p {
		t.Fatal("倒带要暂停原来那场")
	}
	if p, _ := paused(e, info.ID); p {
		t.Fatal("新群不暂停")
	}
	ops := kindsOf(timeline(t, e, id), "op")
	if len(ops) == 0 || ops[len(ops)-1].Op != "pause" {
		t.Fatalf("原群记一条 pause：%+v", ops)
	}
	// 原群里已经在跑的那一轮照常说完，之后不再叫人
	step(t, e, e.a.roomMemberSID("default", id))
	e.k.none()
	// 新群可以接着跑
	e.a.RoomSay(info.ID, "换个说法", "旁白", "", "我")
	step(t, e, e.a.roomMemberSID("default", info.ID))
}

func TestStageForkKeepsStageIsolation(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: say}
	id, _ := turnRoom(t, e, stageRules(scene.Rules{Order: []string{"default", "bob", "cat"}, Rounds: 2, Inner: true}), m)
	e.a.RoomSay(id, "夜深了", "旁白", "", "我")
	runRounds(t, e, id, 2, "default", "bob", "cat")
	at := firstLineOf(t, timeline(t, e, id), "default")
	info, err := e.a.ForkRoom(id, ForkRequest{At: at})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	sid := e.a.roomMemberSID("default", info.ID)
	isolated := e.a.sceneIsolated(sid)
	system, tools, _, _ := e.a.contextParts(sid)
	st := session.Settings{}
	dirty := false
	note := e.a.memoryNotes(sid, "room:default", "x", &st, &dirty)
	_, stageOK := e.a.stageFor(roomLogSID(info.ID))
	e.a.mu.Unlock()
	if !isolated || len(tools) != 0 || note != "" || !strings.Contains(system, innerGuide) || stageOK.ID != info.ID {
		t.Fatalf("新群也是一场戏：isolated=%v tools=%v note=%q", isolated, tools, note)
	}
	ok, msg := true, ""
	done := make(chan struct{})
	go func() {
		e.a.messageTool(e.a.roomMemberSID("bob", id), `{"to":"room:`+info.Name+`","text":"偷偷说"}`, func(o bool, m string) { ok, msg = o, m; close(done) })
	}()
	<-done
	if ok || !strings.Contains(msg, "导演台") {
		t.Fatalf("Message 不能发进新群：%v %q", ok, msg)
	}
}

// 分出到一半失败：新群不出现，已经写的会话和场景记录都删掉。
func TestStageForkFailureLeavesNothingBehind(t *testing.T) {
	for _, step := range []string{"seed", "stage", "create"} {
		e := setup(t)
		e.a.SetLLM(&scriptModel{reply: say})
		room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeMention}))
		e.a.RoomSay(room.ID, "第一句", "张三", "", "我")
		before := len(e.a.Rooms())
		forkHook = func(s string) error {
			if s == step {
				return errors.New("故意失败")
			}
			return nil
		}
		_, err := e.a.ForkRoom(room.ID, ForkRequest{At: 1})
		forkHook = nil
		if err == nil {
			t.Fatalf("%s：应该失败", step)
		}
		if len(e.a.Rooms()) != before {
			t.Fatalf("%s：失败了不能多出群", step)
		}
		// 没有留下别的 room: 会话和场景记录文件
		e.a.mu.Lock()
		for _, sid := range e.a.store.List() {
			if strings.Contains(sid, "room:") && !strings.Contains(sid, room.ID) {
				t.Errorf("%s：留下了会话 %s", step, sid)
			}
		}
		e.a.mu.Unlock()
		entries, _ := os.ReadDir(e.a.stagePath(room.ID) + "/..")
		for _, en := range entries {
			if en.Name() != room.ID+".jsonl" {
				t.Errorf("%s：留下了文件 %s", step, en.Name())
			}
		}
	}
}
