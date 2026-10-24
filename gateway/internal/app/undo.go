package app

import (
	"errors"
	"fmt"
	"strings"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
)

// 停下和撤回
//
// 聊天平台上打斜杠命令不方便，所以任务进行中直接发「停」也能停下，任何时候发「撤回」撤回最近一条。
// 撤回只删历史的末尾，前面的部分不变，前缀缓存照样命中：
//   - 还在排队、没开始处理的消息：直接拿掉；
//   - 正在处理的：先停下，这一轮结束时连同已经产生的回复、工具调用一起从上下文里删掉；
//   - 已经处理完的：删掉最后一轮（最后一条用户消息和它后面的所有消息）。
// 平台上已经发出去的消息不动，只是模型不会再看到它们。

// stopWords：任务进行中，整条消息只有这些词时当成 /stop
var stopWords = map[string]bool{"停": true, "停止": true, "停下": true, "中断": true, "取消": true, "stop": true, "cancel": true}

// plainCommand 认出不带斜杠的「停」「撤回」。
func (a *App) plainCommand(s *sess, text string) string {
	w := strings.ToLower(strings.TrimRight(strings.TrimSpace(text), "。.!！~～"))
	switch {
	case w == "撤回":
		return "undo"
	case stopWords[w] && (s.run != nil || len(s.queue) > 0):
		return "stop"
	}
	return ""
}

// msgPos 记下一条平台消息进了历史的哪个位置（那一轮的用户消息）
type msgPos struct {
	id  string
	pos int
}

// remember 记下这一批消息在历史里的位置，只留最近的一些。调用方持有锁。
func (a *App) remember(s *sess, batch []queued, pos int) {
	for _, q := range batch {
		if q.m.MsgID != "" {
			s.recent = append(s.recent, msgPos{q.m.MsgID, pos})
		}
	}
	if n := len(s.recent); n > 50 {
		s.recent = append([]msgPos(nil), s.recent[n-50:]...)
	}
}

func (a *App) undo(c in) {
	_, note, pending, err := a.undoLatest(c.s)
	switch {
	case pending:
		c.s.run.addOut(c.ep) // 这一轮结束时告诉这个窗口
	case err != nil:
		a.answer(c, err.Error())
	default:
		a.answer(c, note)
	}
}

// UndoSession 撤回会话里最近的一条输入（网页上的撤回）。返回撤回的那段话，网页放回输入框，改了再发。
func (a *App) UndoSession(sid string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSession(sid) {
		return "", errors.New("没有这个会话")
	}
	text, note, pending, err := a.undoLatest(a.sessOf(sid))
	if err == nil && !pending {
		a.publish(events.Event{T: "say", SID: sid, Text: note})
	}
	return text, err
}

// undoLatest 撤回会话里最近的一条输入。正在处理的先停下，结束时再删（pending 为 true）。
// 返回撤回的那段话和给用户的说明。调用方持有锁。
func (a *App) undoLatest(s *sess) (text, note string, pending bool, err error) {
	if n := len(s.queue); n > 0 {
		q := s.queue[n-1]
		s.queue = s.queue[:n-1]
		a.publish(events.Event{T: "undo", SID: s.sid, Text: q.m.Text, Data: map[string]any{"queued": true}})
		return q.m.Text, "已撤回还没处理的消息：" + quoteText(q.m.Text), false, nil
	}
	if s.run != nil {
		s.undoAfter = true
		a.cancelRun(s)
		return a.lastUserText(s.sid), "", true, nil
	}
	return a.undoTurn(s)
}

// undoTurn 从上下文里删掉最近一轮。调用方持有锁，会话里没有任务在跑。
func (a *App) undoTurn(s *sess) (text, note string, pending bool, err error) {
	removed, err := a.store.Undo(s.sid)
	if errors.Is(err, session.ErrNothingToUndo) {
		if a.store.Settings(s.sid).Base > 0 {
			return "", "", false, errors.New("没有可以撤回的对话（更早的对话已经整理成摘要，不能再撤回）")
		}
		return "", "", false, err
	}
	if err != nil {
		a.log.Warn("撤回失败", "sid", s.sid, "err", err)
		return "", "", false, fmt.Errorf("撤回失败：%w", err)
	}
	// 环境说明是跟着删掉的那条消息说的：下一条消息前面重新说一次
	if st := a.store.Settings(s.sid); st.EnvNote != "" && strings.Contains(removed[0].Content, st.EnvNote) || strings.Contains(removed[0].Content, memNoteOpen) {
		if strings.Contains(removed[0].Content, st.EnvNote) {
			st.EnvNote = ""
		}
		if strings.Contains(removed[0].Content, memNoteOpen) {
			st.MemNote = "" // 记忆说明也是跟着这条说的
		}
		if err := a.store.SaveSettings(s.sid, st); err != nil {
			a.log.Warn("保存会话设置失败", "sid", s.sid, "err", err)
		}
	}
	// 窗口“看到了哪里”不能超过新的末尾，不然之后的对话补不上
	end := a.histEnd(s.sid)
	for _, rec := range a.eps {
		if n, ok := rec.Seen[s.sid]; ok && n > end {
			rec.Seen[s.sid] = end
		}
	}
	a.saveEndpoints()
	text = userText(removed[0])
	a.log.Info("撤回了一轮", "sid", s.sid, "messages", len(removed))
	a.publish(events.Event{T: "undo", SID: s.sid, Text: text})
	a.publishSession(s.sid)
	return text, fmt.Sprintf("已撤回：%s\n这一轮（%d 条消息）已从上下文里删掉，模型不会再看到。", quoteText(text), len(removed)), false, nil
}

// afterRun 做这一轮进行中被要求的清空或撤回，返回给窗口的说明。调用方持有锁。
func (a *App) afterRun(s *sess) (string, bool) {
	switch {
	case s.resetAfter:
		s.resetAfter, s.undoAfter = false, false
		if err := a.resetHistory(s); err != nil {
			a.log.Warn("清空上下文失败", "sid", s.sid, "err", err)
		}
		return "上下文已清空", true
	case s.undoAfter:
		s.undoAfter = false
		_, note, _, err := a.undoTurn(s)
		if err != nil {
			return err.Error(), true
		}
		return note, true
	}
	return "", false
}

// recall：用户在平台上撤回了一条消息。还在排队就拿掉；是最近一轮（正在处理或者刚处理完）就从上下文里撤回；
// 更早的消息后面已经接着聊了，只说明一下。不认识的消息（命令、网关重启前的）不管。
func (a *App) recall(c in, msgID string) {
	s := c.s
	for i, q := range s.queue {
		if q.m.MsgID == msgID {
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			a.publish(events.Event{T: "undo", SID: s.sid, Text: q.m.Text, Data: map[string]any{"queued": true}})
			a.answer(c, "已撤回还没处理的消息："+quoteText(q.m.Text))
			return
		}
	}
	pos := -1
	for _, r := range s.recent {
		if r.id == msgID {
			pos = r.pos
		}
	}
	if pos < 0 {
		return
	}
	if pos != a.lastUserPos(s.sid) {
		a.answer(c, "这条消息后面已经接着聊了，上下文里还留着它。要撤回最近一轮，发「撤回」。")
		return
	}
	if s.run != nil {
		s.undoAfter = true
		s.run.addOut(c.ep)
		a.cancelRun(s)
		return
	}
	if _, note, _, err := a.undoTurn(s); err != nil {
		a.answer(c, err.Error())
	} else {
		a.answer(c, note)
	}
}

// lastUserPos 是历史里最后一条用户消息的位置（算上裁剪掉的），没有时为 -1
func (a *App) lastUserPos(sid string) int {
	h := a.store.History(sid)
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			return a.store.Settings(sid).Base + i
		}
	}
	return -1
}

func (a *App) lastUserText(sid string) string {
	h := a.store.History(sid)
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			return userText(h[i])
		}
	}
	return ""
}

// userText 是用户消息里用户自己说的话（去掉环境说明和时间戳）
func userText(m session.Msg) string {
	if ts := turnsOf([]session.Msg{m}, 0); len(ts) > 0 {
		return ts[0].user
	}
	return m.Content
}

func quoteText(s string) string {
	return "「" + clip(strings.Join(strings.Fields(s), " "), 60) + "」"
}
