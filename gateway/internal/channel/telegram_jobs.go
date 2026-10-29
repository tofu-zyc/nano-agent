package channel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func telegramJobKeyboard(c JobCard) map[string]any {
	buttons := []map[string]string{}
	for _, button := range c.Buttons {
		if len(buttons) == 3 {
			break
		}
		if validJobButton(button.Token) {
			buttons = append(buttons, map[string]string{"text": telegramClip(button.Label, 30), "callback_data": button.Token})
		}
	}
	rows := [][]map[string]string{}
	if len(buttons) > 0 {
		rows = append(rows, buttons)
	}
	return map[string]any{"inline_keyboard": rows}
}
func (t *Telegram) SendJob(ctx context.Context, chat string, c JobCard) (string, error) {
	var msg tgMessage
	err := t.callRetry(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": telegramClip(JobCardText(c), 3800), "reply_markup": telegramJobKeyboard(c)}, &msg)
	if err != nil {
		return "", err
	}
	if msg.MessageID == 0 {
		return "", fmt.Errorf("任务卡片缺少消息编号")
	}
	return strconv.FormatInt(msg.MessageID, 10), nil
}
func (t *Telegram) UpdateJob(ctx context.Context, chat, ref string, c JobCard) error {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return err
	}
	err = t.callRetry(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": id,
		"text": telegramClip(JobCardText(c), 3800), "reply_markup": telegramJobKeyboard(c)}, nil)
	var te *tgError
	if errors.As(err, &te) && strings.Contains(te.Desc, "message is not modified") {
		return nil
	}
	return err
}
