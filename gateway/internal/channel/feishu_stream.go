package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	larkcard "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (f *Feishu) OpenRun(chatID, replyTo string) RunView {
	return newStreamRun(&feishuStream{f: f, chat: chatID, reply: replyTo}, func(ctx context.Context, text string) error {
		return f.Send(ctx, chatID, text, replyTo)
	}, f.log, 500*time.Millisecond)
}

type streamCard struct {
	id   string
	seq  int
	text string
}

type feishuStream struct {
	f     *Feishu
	chat  string
	reply string
	cards []streamCard
}

func (s *feishuStream) create(ctx context.Context, text string) error {
	card, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"streaming_mode": true, "update_multi": true, "summary": map[string]string{"content": "生成中…"}},
		"body":   map[string]any{"elements": []any{map[string]string{"tag": "markdown", "element_id": "reply", "content": text}}},
	})
	resp, err := s.f.client.Cardkit.V1.Card.Create(ctx, larkcard.NewCreateCardReqBuilder().
		Body(larkcard.NewCreateCardReqBodyBuilder().Type("card_json").Data(string(card)).Build()).Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("创建流式卡片失败：%d %s", resp.Code, resp.Msg)
	}
	id := deref(resp.Data.CardId)
	s.cards = append(s.cards, streamCard{id: id}) // 发送失败时也要关闭已创建的卡片
	content, _ := json.Marshal(map[string]any{"type": "card", "data": map[string]string{"card_id": id}})
	if len(s.cards) == 1 && s.reply != "" {
		r, err := s.f.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(s.reply).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(string(content)).Build()).Build())
		if err != nil {
			return err
		}
		if !r.Success() {
			return fmt.Errorf("发送流式卡片失败：%d %s", r.Code, r.Msg)
		}
	} else {
		r, err := s.f.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(s.chat).MsgType("interactive").Content(string(content)).Build()).Build())
		if err != nil {
			return err
		}
		if !r.Success() {
			return fmt.Errorf("发送流式卡片失败：%d %s", r.Code, r.Msg)
		}
	}
	s.cards[len(s.cards)-1].text = text
	return nil
}

func (s *feishuStream) Write(ctx context.Context, text string) error {
	for i, part := range streamChunks(text, 8000) {
		if i == len(s.cards) {
			if err := s.create(ctx, part); err != nil {
				return err
			}
			continue
		}
		c := &s.cards[i]
		if part == c.text {
			continue
		}
		c.seq++
		resp, err := s.f.client.Cardkit.V1.CardElement.Content(ctx, larkcard.NewContentCardElementReqBuilder().
			CardId(c.id).ElementId("reply").Body(larkcard.NewContentCardElementReqBodyBuilder().
			Content(part).Sequence(c.seq).Build()).Build())
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("更新流式卡片失败：%d %s", resp.Code, resp.Msg)
		}
		c.text = part
	}
	return nil
}

func (s *feishuStream) Close(ctx context.Context) error {
	var lastErr error
	for i := range s.cards {
		c := &s.cards[i]
		c.seq++
		settings, _ := json.Marshal(map[string]any{"config": map[string]any{
			"streaming_mode": false, "summary": map[string]string{"content": "本轮处理结束"},
		}})
		resp, err := s.f.client.Cardkit.V1.Card.Settings(ctx, larkcard.NewSettingsCardReqBuilder().CardId(c.id).
			Body(larkcard.NewSettingsCardReqBodyBuilder().Settings(string(settings)).Sequence(c.seq).Build()).Build())
		if err != nil {
			lastErr = err
		} else if !resp.Success() {
			lastErr = fmt.Errorf("关闭流式卡片失败：%d %s", resp.Code, resp.Msg)
		}
	}
	return lastErr
}
