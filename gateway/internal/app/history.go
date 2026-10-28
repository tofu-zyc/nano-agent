package app

import (
	"errors"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
	"strings"
)

func (a *App) resetHistory(s *sess) (err error) {
	defer func() {
		if err == nil {
			a.lifecycleExperience(s.sid, "清空了聊天上下文；清空前已经执行的操作仍是过去的事实，不是待办")
		}
	}()
	if s.compactCancel != nil {
		s.compactCancel()
		s.compactCancel = nil
		s.compacting = false
	}
	if s.deleteAfter {
		s.deleteAfter = false
		if s.deleteArchivesAfter {
			s.deleteArchivesAfter = false
			return a.store.DeleteAllHistory(s.sid)
		}
		return a.store.Reset(s.sid)
	}
	return a.store.ArchiveReset(s.sid, a.now())
}

type HistoryDelete struct {
	All          bool   `json:"all"`
	From         *int   `json:"from"`
	To           *int   `json:"to"`
	Archive      string `json:"archive"`
	ClearSummary bool   `json:"clear_summary"`
}

func (a *App) Archives(sid string) []session.ArchiveInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store.Archives(sid)
}
func (a *App) DeleteHistory(sid string, p HistoryDelete) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deleteHistory(sid, p, false)
}
func (a *App) deleteHistory(sid string, p HistoryDelete, fromTool bool) error {
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	s := a.sessOf(sid)
	if s.compacting {
		return errors.New("正在整理上下文，稍后再删除")
	}
	if s.run != nil && fromTool && p.From != nil && p.To != nil {
		n := 0
		for _, m := range a.store.Archive(sid) {
			if m.Role == "user" {
				n++
			}
		}
		if *p.To >= n-1 {
			return errors.New("不能删除正在执行的轮次，请在结束后删除")
		}
		if err := a.store.DeleteTurns(sid, *p.From, *p.To); err != nil {
			return err
		}
	} else if s.run != nil {
		if !fromTool || !p.All {
			return errors.New("任务正在进行，先停止再删除历史")
		}
		s.resetAfter = true
		s.deleteAfter = true
		s.deleteArchivesAfter = true
	} else if p.All {
		if err := a.store.DeleteAllHistory(sid); err != nil {
			return err
		}
	} else if p.Archive != "" {
		if err := a.store.DeleteArchive(sid, p.Archive); err != nil {
			return err
		}
	} else if p.From != nil && p.To != nil {
		if err := a.store.DeleteTurns(sid, *p.From, *p.To); err != nil {
			return err
		}
	} else if !p.ClearSummary {
		return errors.New("请指定全部历史、轮次范围或归档")
	}
	if p.ClearSummary {
		st := a.store.Settings(sid)
		st.Summary = ""
		st.Meter = session.Meter{}
		if err := a.store.SaveSettings(sid, st); err != nil {
			return err
		}
	}
	s.recent = nil
	for _, ep := range a.eps {
		delete(ep.Seen, sid)
	}
	a.saveEndpoints()
	a.publish(events.Event{T: "undo", SID: sid})
	a.publishSession(sid)
	return nil
}

// TurnInfo 是 Gateway {"what":"turns"} 里的一轮：编号从 0 起，和 history_delete 的 from/to 一样
type TurnInfo struct {
	Turn      int    `json:"turn"`
	At        int64  `json:"at,omitempty"`
	Text      string `json:"text"`
	InContext bool   `json:"in_context"` // false：已经整理进摘要，不在上下文里了（原始记录还在，可以删）
	Running   bool   `json:"running,omitempty"`
}

// Turns 列出这个会话最近 n 轮用户说的话（整个历史文件，含已整理的部分）
func (a *App) Turns(sid string, n int) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 {
		n = 20
	}
	h := a.store.Archive(sid)
	base := a.store.Settings(sid).Base
	var turns []TurnInfo
	for i, m := range h {
		if m.Role == "user" {
			turns = append(turns, TurnInfo{Turn: len(turns), At: m.TS, Text: clip(strings.Join(strings.Fields(userText(m)), " "), 80), InContext: i >= base})
		}
	}
	total := len(turns)
	if s := a.sessions[sid]; s != nil && s.run != nil && total > 0 {
		turns[total-1].Running = true // 正在执行的这一轮不能删
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return map[string]any{"total": total, "turns": turns}
}
