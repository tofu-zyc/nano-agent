package app

import (
	"fmt"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
)

// 回合制（轮流和导演）：时钟按回合走，不按真实时间；没有随机数，也不等定时器。
// 轮数（Round）和这一轮下一个轮到谁（Next）存在 scenes.json 的场景条目里；
// 这一串是不是正在转（TurnActive）只在内存里：重启或取消暂停后不自动接着跑，等下一句人话或旁白。

// roomRunDone 在群里的成员一轮结束时调用：把它说的话记进群，再按回合制决定下一步。调用方持有锁。
func (a *App) roomRunDone(room rooms.Room, s *sess, r *run, status, body string) {
	if roomLogSID(room.ID) == s.sid {
		return
	}
	member := roomSender(s.sid)
	spoke := !isSilence(body)
	if spoke {
		s.groupSpokeAt = a.now()
		s.groupSpeaks = append(s.groupSpeaks, s.groupSpokeAt)
		a.roomAgentSay(room, member, body, r.roomChan)
	}
	// 用量已经算进场景（可能刚把群暂停了）：用最新的群记录接着判断
	if fresh, ok := a.roomStore.Get(room.ID); ok {
		a.roomTurnDone(fresh, member, status, spoke)
	}
}

// roomOrder 是轮流的发言顺序：设了 Order 就按它，否则按成员顺序。
func roomOrder(room rooms.Room) []string {
	if len(room.Order) == 0 {
		return append([]string(nil), room.Members...)
	}
	var out []string
	for _, m := range room.Order {
		if slicesContains(room.Members, m) {
			out = append(out, m)
		}
	}
	return out
}

// roomTurnStart 人或旁白在公开频道说了一句：轮流没在转就从这里开始。调用方持有锁。
func (a *App) roomTurnStart(room rooms.Room) {
	st := a.roomState(room.ID)
	if st.TurnActive || room.Paused || room.Mode != scene.ModeTurns {
		return
	}
	st.TurnActive, st.RoundTurns, st.RoundSilent, st.ErrRun = true, 0, 0, 0
	a.roomTurnNext(room)
}

// roomTurnStop 停下来，等下一句人话或旁白。调用方持有锁。
func (a *App) roomTurnStop(roomID string) {
	st := a.roomState(roomID)
	st.TurnActive, st.Current = false, ""
}

// roomTurnNext 叫下一位：一整轮结束时先记轮数，到了 Rounds 就暂停，全员沉默就停下。调用方持有锁。
func (a *App) roomTurnNext(room rooms.Room) {
	st := a.roomState(room.ID)
	if !st.TurnActive {
		return
	}
	order := roomOrder(room)
	if room.Paused || len(order) == 0 {
		a.roomTurnStop(room.ID)
		return
	}
	key := roomLogSID(room.ID)
	u := a.sceneUseOf(key)
	for range len(order) + 1 {
		if u.Next >= len(order) { // 一整轮结束
			u.Next = 0
			u.Round++
			allSilent := st.RoundTurns > 0 && st.RoundSilent == st.RoundTurns
			st.RoundTurns, st.RoundSilent = 0, 0
			a.saveSceneUses()
			if room.Rounds > 0 && u.Round >= room.Rounds {
				a.roomTurnStop(room.ID)
				a.pauseRoom(room, fmt.Sprintf("这一场到了 %d 轮，先暂停，清零累计后再恢复", room.Rounds))
				return
			}
			if allSilent {
				a.roomTurnStop(room.ID)
				return
			}
		}
		member := order[u.Next]
		u.Next++
		a.saveSceneUses()
		if room.MutedAgent(member) {
			continue // 被禁言的跳过
		}
		sid := a.roomMemberSID(member, room.ID)
		s := a.sessOf(sid)
		if s.run != nil || len(s.queue) > 0 {
			continue
		}
		st.Current = member
		hint := fmt.Sprintf("[系统] 第 %d 轮，轮到你。不想说就只回 [不回复]。", u.Round+1)
		a.roomStart(s, member, room, hint, "")
		return
	}
	a.roomTurnStop(room.ID)
}

// roomTurnDone 轮流里轮到的成员说完了（或出错）。调用方持有锁。
func (a *App) roomTurnDone(room rooms.Room, member, status string, spoke bool) {
	st := a.roomState(room.ID)
	if !st.TurnActive || st.Current != member {
		return
	}
	st.Current = ""
	switch status {
	case proto.StatusOK:
		st.ErrRun = 0
		st.RoundTurns++
		if !spoke {
			st.RoundSilent++
		}
	case proto.StatusCancelled:
		a.roomTurnStop(room.ID) // 人停下了这一轮：不再往下叫
		return
	default:
		// 出错算跳过；同一轮里连续两位出错就暂停
		st.ErrRun++
		if st.ErrRun >= 2 {
			a.roomTurnStop(room.ID)
			a.pauseRoom(room, "轮流时连续两位成员出错，先暂停")
			return
		}
	}
	a.roomTurnNext(room)
}
