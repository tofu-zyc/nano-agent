package channel

import (
	"context"
	"strconv"
	"time"
)

func (t *Telegram) OpenRun(chatID, replyTo string) RunView {
	return newStreamRun(&telegramStream{t: t, chat: chatID, reply: replyTo}, func(ctx context.Context, text string) error {
		return t.Send(ctx, chatID, text, replyTo)
	}, t.log, time.Second)
}

type telegramStream struct {
	t     *Telegram
	chat  string
	reply string
	ids   []int64
	texts []string
}

func (s *telegramStream) Write(ctx context.Context, text string) error {
	for i, part := range streamChunks(text, 4000) {
		p := map[string]any{"chat_id": s.chat, "text": part}
		if i == len(s.ids) {
			if i == 0 && s.reply != "" {
				id, _ := strconv.ParseInt(s.reply, 10, 64)
				p["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
			}
			var msg tgMessage
			if err := s.t.callRetry(ctx, "sendMessage", p, &msg); err != nil {
				return err
			}
			s.ids = append(s.ids, msg.MessageID)
			s.texts = append(s.texts, part)
		} else if part != s.texts[i] {
			p["message_id"] = s.ids[i]
			if err := s.t.callRetry(ctx, "editMessageText", p, nil); err != nil {
				return err
			}
			s.texts[i] = part
		}
	}
	return nil
}

func (s *telegramStream) Close(context.Context) error { return nil }
