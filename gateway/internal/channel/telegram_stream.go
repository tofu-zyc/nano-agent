package channel

import (
	"context"
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
}

func (s *telegramStream) send(ctx context.Context, text, parse string, id int64, reply bool) (int64, error) {
	p := map[string]any{"chat_id": s.chat, "text": text}
	if parse != "" {
		p["parse_mode"] = parse
	}
	if id != 0 {
		p["message_id"] = id
		return id, s.t.callRetry(ctx, "editMessageText", p, nil)
	}
	if reply && s.reply != "" {
		target, _ := strconv.ParseInt(s.reply, 10, 64)
		p["reply_parameters"] = map[string]any{"message_id": target, "allow_sending_without_reply": true}
	}
	var msg tgMessage
	err := s.t.callRetry(ctx, "sendMessage", p, &msg)
	return msg.MessageID, err
}
func (s *telegramStream) Write(ctx context.Context, snap streamSnapshot) error {
	// 过程是一条独立消息，正文始终单独发送，不把原始工具记录混进去。
	if snap.Reasoning != "" || snap.Tools != "" || snap.Notes != "" || snap.Source != "" {
		text := telegramProcess(snap)
		if text != s.processText {
			id, err := s.send(ctx, text, "HTML", s.processID, false)
			if err != nil {
				return err
			}
			s.processID = id
			s.processText = text
		}
	}
	for i, part := range telegramChunks(snap.Content, 3900) {
		if i == len(s.ids) {
			id, err := s.send(ctx, part, "", 0, i == 0)
			if err != nil {
				return err
			}
			s.ids = append(s.ids, id)
			s.texts = append(s.texts, part)
		} else if part != s.texts[i] {
			if _, err := s.send(ctx, part, "", s.ids[i], false); err != nil {
				return err
			}
			s.texts[i] = part
		}
	}
	return nil
}
func (s *telegramStream) Close(context.Context) error { return nil }

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
