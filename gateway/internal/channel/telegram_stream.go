package channel

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

func (t *Telegram) OpenRun(chatID, replyTo string) RunView {
	return newStreamRun(&telegramStream{t: t, chat: chatID, reply: replyTo}, func(ctx context.Context, text string) error {
		return t.Send(ctx, chatID, text, replyTo)
	}, t.log, time.Second)
}
func (t *Telegram) OpenMirror(chat, via, text string) RunView {
	return t.OpenRun(chat, "").(*streamRun).mirror(via, text)
}

type telegramStream struct {
	t           *Telegram
	chat, reply string
	ids         []int64
	texts       []string
	processID   int64
	processText string
	buttons     map[int64]bool // 带着“停止”按钮的消息
}

// tgStopData 是“停止”按钮的回调数据
const tgStopData = "stop"

var tgStopKeyboard = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "⏹ 停止", "callback_data": tgStopData}}}}

// send 发一条新消息（id 为 0）或者改一条。button 表示这条消息下面带“停止”按钮；
// 改消息时不带按钮，原来的按钮就没了。
func (s *telegramStream) send(ctx context.Context, text, parse string, id int64, reply, button bool) (int64, error) {
	p := map[string]any{"chat_id": s.chat, "text": text}
	if parse != "" {
		p["parse_mode"] = parse
	}
	if button {
		p["reply_markup"] = tgStopKeyboard
	}
	if id != 0 {
		p["message_id"] = id
		err := s.t.callRetry(ctx, "editMessageText", p, nil)
		if err == nil {
			s.mark(id, button)
		}
		return id, err
	}
	if reply && s.reply != "" {
		target, _ := strconv.ParseInt(s.reply, 10, 64)
		p["reply_parameters"] = map[string]any{"message_id": target, "allow_sending_without_reply": true}
	}
	var msg tgMessage
	err := s.t.callRetry(ctx, "sendMessage", p, &msg)
	if err == nil {
		s.mark(msg.MessageID, button)
	}
	return msg.MessageID, err
}

func (s *telegramStream) mark(id int64, button bool) {
	if s.buttons == nil {
		s.buttons = map[int64]bool{}
	}
	s.buttons[id] = button
}

// Write 更新这一轮的显示。处理中时“停止”按钮挂在过程消息下面，没有过程消息时挂在正文第一段下面。
func (s *telegramStream) Write(ctx context.Context, snap streamSnapshot) error {
	running := !snap.Closed
	// 过程是一条独立消息，正文始终单独发送，不把原始工具记录混进去。
	if snap.Reasoning != "" || snap.Tools != "" || snap.Notes != "" || snap.Source != "" {
		text := telegramProcess(snap)
		if text != s.processText {
			id, err := s.send(ctx, text, "HTML", s.processID, false, running)
			if err != nil {
				return err
			}
			s.processID = id
			s.processText = text
		}
	}
	for i, part := range telegramChunks(snap.Content, 3900) {
		button := running && i == 0 && s.processID == 0
		if i == len(s.ids) {
			id, err := s.send(ctx, part, "", 0, i == 0, button)
			if err != nil {
				return err
			}
			s.ids = append(s.ids, id)
			s.texts = append(s.texts, part)
		} else if part != s.texts[i] || (i == 0 && s.buttons[s.ids[0]] != button) {
			if _, err := s.send(ctx, part, "", s.ids[i], false, button); err != nil {
				return err
			}
			s.texts[i] = part
		}
	}
	return nil
}

// Close：这一轮结束了，去掉还留着的“停止”按钮
func (s *telegramStream) Close(ctx context.Context) error {
	var errs []error
	for id, on := range s.buttons {
		if !on {
			continue
		}
		err := s.t.callRetry(ctx, "editMessageReplyMarkup", map[string]any{
			"chat_id": s.chat, "message_id": id, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
		if err != nil {
			errs = append(errs, err)
		} else {
			s.buttons[id] = false
		}
	}
	return errors.Join(errs...)
}

func telegramProcess(s streamSnapshot) string {
	state := "处理中"
	if s.Closed {
		state = "处理结束"
	}
	parts := []string{fmt.Sprintf("<b>%s · %d 次工具调用</b>", state, s.ToolCount)}
	if s.Source != "" {
		parts = append(parts, html.EscapeString(telegramClip(s.Source, 600)))
	}
	if s.Reasoning != "" {
		parts = append(parts, "<b>思考过程</b>\n<blockquote expandable>"+html.EscapeString(telegramClip(s.Reasoning, 900))+"</blockquote>")
	}
	if s.Tools != "" {
		parts = append(parts, "<b>工具记录</b>\n<blockquote expandable>"+html.EscapeString(telegramClip(s.Tools, 1500))+"</blockquote>")
	}
	if s.Notes != "" {
		parts = append(parts, html.EscapeString(telegramClip(s.Notes, 400)))
	}
	return strings.Join(parts, "\n\n")
}

// Telegram 的长度和实体偏移按 UTF-16 计算，emoji 通常占两个单位。
func telegramChunks(text string, limit int) []string {
	var out []string
	start, units := 0, 0
	for i, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > limit {
			out = append(out, text[start:i])
			start = i
			units = 0
		}
		units += n
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}
func telegramClip(text string, limit int) string {
	parts := telegramChunks(text, limit)
	if len(parts) > 1 {
		return parts[0] + "…（完整过程见网页）"
	}
	return text
}
