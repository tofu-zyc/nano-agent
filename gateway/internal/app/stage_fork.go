package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/journal"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/session"
)

// 平行世界和倒带（设计 §14.2）：从一场戏的第 at 句分出新的一场。
// 新群的群记录、成员会话和场景记录都是原来的前缀；原来那场不受影响（记录只追加，不删、不截）。

// ForkRequest 是 POST /api/rooms/{id}/fork 的参数。
type ForkRequest struct {
	At     int    `json:"at"`     // 从群记录里的第几句分出
	Name   string `json:"name"`   // 空则取“<原名> · 平行 N”
	Rewind bool   `json:"rewind"` // 分出后暂停原来那场
}

// forkHook 只在测试里用：返回错误就让分出在这一步失败（"seed"、"stage"、"create"）。
var forkHook func(step string) error

func (a *App) forkName(base string) string {
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s · 平行 %d", base, n)
		if _, taken := a.roomStore.FindByName(name); !taken {
			return name
		}
	}
}

// ForkRoom 从第 at 句分出平行世界，返回新群。新群最后才出现在群列表里；中途失败，已经写的都删掉。
func (a *App) ForkRoom(id string, req ForkRequest) (RoomInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roomStore == nil {
		return RoomInfo{}, errors.New("房间列表还没准备好")
	}
	room, ok := a.roomStore.Get(id)
	if !ok {
		return RoomInfo{}, errors.New("没有这个群")
	}
	if !room.Stage {
		return RoomInfo{}, errors.New("只有一场戏可以分出平行世界")
	}
	if req.At <= 0 {
		return RoomInfo{}, errors.New("要说从第几句分出（at 从 1 起）")
	}
	a.stageFor(roomLogSID(id)) // 确保记录已经打开

	// 读场景记录：这一句能不能分出，截到哪里
	var recs []StageRec
	cut, lastLine, nearest := -1, 0, 0
	var cursors map[string]int
	var runRec *StageRec // 说出第 at 句的那一轮的 run：写在 cursors 后面，心里话和用量在里面
	if err := readStage(a.stagePath(id), func(r StageRec) bool {
		recs = append(recs, r)
		switch {
		case r.Kind == "run" && r.Said == req.At && runRec == nil:
			rr := r
			runRec = &rr
		case r.Kind == "line":
			lastLine = max(lastLine, r.Line)
		case r.Kind == "cursors" && r.Line == req.At:
			cut, cursors = len(recs), r.Cursors
		case r.Kind == "cursors" && r.Line < req.At:
			nearest = max(nearest, r.Line)
		}
		return true
	}); err != nil {
		return RoomInfo{}, err
	}
	if req.At > lastLine {
		return RoomInfo{}, fmt.Errorf("这场戏只有 %d 句", lastLine)
	}
	if cut < 0 {
		if nearest > 0 {
			return RoomInfo{}, fmt.Errorf("第 %d 句不能分出（那一刻有成员正在说话）；最近一个可以分出的是第 %d 句", req.At, nearest)
		}
		return RoomInfo{}, fmt.Errorf("第 %d 句不能分出（那一刻有成员正在说话），之前也没有可以分出的句子", req.At)
	}
	recs = recs[:cut]
	if runRec != nil {
		recs = append(recs, *runRec)
	}

	// 群记录和成员会话的前缀
	logMsgs := a.store.Archive(roomLogSID(id))
	if len(logMsgs) < req.At {
		return RoomInfo{}, errors.New("群记录比场景记录短，不能分出")
	}
	newID := rooms.NewID()
	type seed struct {
		sid  string
		msgs []session.Msg
	}
	seeds := []seed{{roomLogSID(newID), logMsgs[:req.At]}}
	for _, m := range room.Members {
		n, has := cursors[m]
		if !has {
			continue // 这一句之后才进群的成员：没听过前面的，从空白开始
		}
		msgs := a.store.Archive(a.roomMemberSID(m, id))
		if len(msgs) < n {
			return RoomInfo{}, fmt.Errorf("%s 的会话比记录里短（被清空过？），不能分出", a.agentDisplay(m))
		}
		seeds = append(seeds, seed{a.roomMemberSID(m, newID), msgs[:n]})
	}

	// 开始写：失败就删掉已经写的
	var written []string
	stagePath := a.stagePath(newID)
	undo := func() {
		for _, sid := range written {
			if err := a.store.Delete(sid); err != nil {
				a.log.Warn("分出失败，清理会话没成功", "sid", sid, "err", err)
			}
		}
		os.Remove(stagePath)
		os.Remove(stagePath + ".next")
	}
	step := func(name string) error {
		if forkHook != nil {
			return forkHook(name)
		}
		return nil
	}
	fail := func(err error) (RoomInfo, error) {
		undo()
		return RoomInfo{}, fmt.Errorf("分出失败，已清理：%w", err)
	}
	if err := step("seed"); err != nil {
		return fail(err)
	}
	for _, sd := range seeds {
		if err := a.store.Seed(sd.sid, slices.Clone(sd.msgs)); err != nil {
			return fail(err)
		}
		written = append(written, sd.sid)
		st := a.store.Settings(sd.sid)
		if sd.sid != roomLogSID(newID) {
			st.Group = true
		}
		if err := a.store.SaveSettings(sd.sid, st); err != nil {
			return fail(err)
		}
	}
	// 场景记录：到这一句为止的部分整份写进新文件，再追加一条 fork
	if err := step("stage"); err != nil {
		return fail(err)
	}
	j, err := journal.Open(stagePath, 1, journal.Limits{Record: stageRecMax, File: stageFileMax}, nil)
	if err != nil {
		return fail(err)
	}
	all := make([]any, 0, len(recs)+1)
	for _, r := range recs {
		r.Seq = 0
		all = append(all, r)
	}
	detail, _ := json.Marshal(map[string]any{"from": id, "at": req.At})
	all = append(all, StageRec{Kind: "fork", TS: a.now().UnixMilli(), Detail: detail})
	err = j.Rewrite(all)
	j.Close()
	if err != nil {
		return fail(err)
	}
	// 新群：成员、禁言、规则照抄（含 stage）；用量从零开始，轮流从顺序的第一位开始
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = a.forkName(room.Name)
	}
	if err := step("create"); err != nil {
		return fail(err)
	}
	got, err := a.roomStore.Create(rooms.Room{ID: newID, Name: name, Members: slices.Clone(room.Members), Muted: slices.Clone(room.Muted), Rules: room.Rules.Clone()})
	if err != nil {
		return fail(err)
	}
	if got.ID != newID {
		a.roomStore.Delete(got.ID)
		return fail(errors.New("群 ID 被占用"))
	}
	a.setRoomTitle(got.ID, got.Name)
	a.log.Info("分出平行世界", "from", id, "at", req.At, "new", got.ID)

	// 倒带：分出后暂停原来那场
	if req.Rewind && !room.Paused {
		room.Paused = true
		a.roomTurnStop(id)
		if err := a.roomStore.Update(room); err != nil {
			a.log.Warn("倒带时暂停原群失败", "room", id, "err", err)
		} else {
			a.stageOp(room, "pause", map[string]any{"rewind": true, "fork": got.ID, "at": req.At})
		}
	}
	a.publish(events.Event{T: "rooms"})
	return a.roomInfo(got), nil
}
