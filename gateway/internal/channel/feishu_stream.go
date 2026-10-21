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
func (f *Feishu) OpenMirror(chat, via, text string) RunView {
	return f.OpenRun(chat, "").(*streamRun).mirror(via, text)
}

type streamCard struct {
	id    string
	seq   int
	parts map[string]string
}
type feishuStream struct {
	f           *Feishu
	chat, reply string
	cards       []*streamCard
	process     *streamCard
}

func markdown(id, text string) map[string]any {
	return map[string]any{"tag": "markdown", "element_id": id, "content": text}
}
func collapsed(id, title, text string) map[string]any {
	return map[string]any{"tag": "collapsible_panel", "expanded": false,
		"header":   map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}},
		"elements": []any{markdown(id, text)}}
}
func (s *feishuStream) create(ctx context.Context, title string, elements []any) (*streamCard, error) {
	card, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"streaming_mode": true, "update_multi": true, "summary": map[string]string{"content": title}},
		"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}, "template": "blue"},
		"body":   map[string]any{"elements": elements},
	})
	resp, err := s.f.client.Cardkit.V1.Card.Create(ctx, larkcard.NewCreateCardReqBuilder().
		Body(larkcard.NewCreateCardReqBodyBuilder().Type("card_json").Data(string(card)).Build()).Build())
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, fmt.Errorf("创建卡片失败：%d %s", resp.Code, resp.Msg)
	}
	c := &streamCard{id: deref(resp.Data.CardId), parts: map[string]string{}}
	s.cards = append(s.cards, c) // 发送失败也要关闭已创建的卡片
	content, _ := json.Marshal(map[string]any{"type": "card", "data": map[string]string{"card_id": c.id}})
	if s.reply != "" {
		r, err := s.f.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(s.reply).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(string(content)).Build()).Build())
		if err != nil {
			return nil, err
		}
		if !r.Success() {
			return nil, fmt.Errorf("发送卡片失败：%d %s", r.Code, r.Msg)
		}
		s.reply = ""
	} else {
		r, err := s.f.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(s.chat).MsgType("interactive").Content(string(content)).Build()).Build())
		if err != nil {
			return nil, err
		}
		if !r.Success() {
			return nil, fmt.Errorf("发送卡片失败：%d %s", r.Code, r.Msg)
		}
	}
	return c, nil
}
func (s *feishuStream) update(ctx context.Context, c *streamCard, id, text string) error {
	if c.parts[id] == text {
		return nil
	}
	c.seq++
	resp, err := s.f.client.Cardkit.V1.CardElement.Content(ctx, larkcard.NewContentCardElementReqBuilder().CardId(c.id).ElementId(id).
		Body(larkcard.NewContentCardElementReqBodyBuilder().Content(text).Sequence(c.seq).Build()).Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("更新卡片失败：%d %s", resp.Code, resp.Msg)
	}
	c.parts[id] = text
	return nil
}
func (s *feishuStream) Write(ctx context.Context, snap streamSnapshot) error {
	if snap.Reasoning != "" || snap.Tools != "" || snap.Notes != "" || snap.Source != "" {
		state, hint := "正在处理", "\n发「停」可以中断，发「撤回」撤回这一轮"
		if snap.Closed {
			state, hint = "处理结束", ""
		}
		status := fmt.Sprintf("**%s · %d 次工具调用**%s", state, snap.ToolCount, hint)
		if snap.Source != "" {
			status += "\n\n" + snap.Source
		}
		reasoning := streamClip(snap.Reasoning, 2000)
		if reasoning == "" {
			reasoning = "本轮没有思考内容"
		}
		tools := streamClip(snap.Tools, 3000)
		if tools == "" {
			tools = "本轮尚未调用工具"
		}
		notes := streamClip(snap.Notes, 500)
		if notes == "" {
			notes = "回复会在下方单独显示"
		}
		if s.process == nil {
			c, err := s.create(ctx, "处理过程", []any{markdown("status", status), collapsed("reasoning", "思考过程", reasoning),
				collapsed("tools", "工具记录", tools), markdown("notes", notes)})
			if err != nil {
				return err
			}
			s.process = c
			c.parts = map[string]string{"status": status, "reasoning": reasoning, "tools": tools, "notes": notes}
		} else {
			for _, p := range [][2]string{{"status", status}, {"reasoning", reasoning}, {"tools", tools}, {"notes", notes}} {
				if err := s.update(ctx, s.process, p[0], p[1]); err != nil {
					return err
				}
			}
		}
	}
	var replies []*streamCard
	for _, c := range s.cards {
		if c != s.process {
			replies = append(replies, c)
		}
	}
	// 预留卡片结构开销，即使整段都是四字节 emoji 也不把单卡撑得过大。
	for i, part := range streamChunks(snap.Content, 6000) {
		if i == len(replies) {
			c, err := s.create(ctx, "回复", []any{markdown("reply", part)})
			if err != nil {
				return err
			}
			c.parts["reply"] = part
			replies = append(replies, c)
		} else if err := s.update(ctx, replies[i], "reply", part); err != nil {
			return err
		}
	}
	return nil
}
func (s *feishuStream) Close(ctx context.Context) error {
	var lastErr error
	for _, c := range s.cards {
		c.seq++
		settings, _ := json.Marshal(map[string]any{"config": map[string]any{"streaming_mode": false, "summary": map[string]string{"content": "本轮处理结束"}}})
		resp, err := s.f.client.Cardkit.V1.Card.Settings(ctx, larkcard.NewSettingsCardReqBuilder().CardId(c.id).
			Body(larkcard.NewSettingsCardReqBodyBuilder().Settings(string(settings)).Sequence(c.seq).Build()).Build())
		if err != nil {
			lastErr = err
		} else if !resp.Success() {
			lastErr = fmt.Errorf("关闭卡片失败：%d %s", resp.Code, resp.Msg)
		}
	}
	return lastErr
}
