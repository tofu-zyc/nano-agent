package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	larkcard "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// 飞书上一轮的显示是一张 CardKit 流式卡片：
//
//	（来自别的窗口时）一行来源
//	▸ 思考和工具调用（默认收起）
//	回复正文（Markdown，边生成边显示）
//	一行灰色的状态：处理中 / 用量
//	[停止]（处理中才有）
//
// 回复太长时后面接着发只有正文的卡片。没有思考和工具调用时，结束时把折叠面板去掉。

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
	gone  map[string]bool // 已经删掉的组件
}
type feishuStream struct {
	f           *Feishu
	chat, reply string
	cards       []*streamCard // 第一张是主卡片，后面是长回复接着发的
}

// 卡片按钮的回调数据
const (
	fsAction  = "a" // stop / approve / deny
	fsApprove = "n" // 审批的编号
)

func markdown(id, text string) map[string]any {
	m := map[string]any{"tag": "markdown", "content": text}
	if id != "" {
		m["element_id"] = id
	}
	return m
}
func notation(id, text string) map[string]any {
	m := markdown(id, text)
	m["text_size"] = "notation"
	return m
}
func fsButton(id, label, kind string, value map[string]string) map[string]any {
	b := map[string]any{"tag": "button", "text": map[string]string{"tag": "plain_text", "content": label}, "type": kind,
		"behaviors": []any{map[string]any{"type": "callback", "value": value}}}
	if id != "" {
		b["element_id"] = id
	}
	return b
}

func (s *feishuStream) create(ctx context.Context, summary string, elements []any) (*streamCard, error) {
	card, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"streaming_mode": true, "update_multi": true, "summary": map[string]string{"content": summary},
			"streaming_config": map[string]any{"print_frequency_ms": map[string]int{"default": 40}, "print_step": map[string]int{"default": 2}}},
		"body": map[string]any{"elements": elements},
	})
	resp, err := s.f.client.Cardkit.V1.Card.Create(ctx, larkcard.NewCreateCardReqBuilder().
		Body(larkcard.NewCreateCardReqBodyBuilder().Type("card_json").Data(string(card)).Build()).Build())
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, fmt.Errorf("创建卡片失败：%d %s", resp.Code, resp.Msg)
	}
	c := &streamCard{id: deref(resp.Data.CardId), parts: map[string]string{}, gone: map[string]bool{}}
	s.cards = append(s.cards, c) // 发送失败也要关闭已创建的卡片
	content, _ := json.Marshal(map[string]any{"type": "card", "data": map[string]string{"card_id": c.id}})
	if err := s.f.sendRaw(ctx, s.chat, s.reply, "interactive", string(content)); err != nil {
		return nil, err
	}
	s.reply = ""
	return c, nil
}

func (s *feishuStream) update(ctx context.Context, c *streamCard, id, text string) error {
	if c.parts[id] == text || c.gone[id] {
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

func (s *feishuStream) remove(ctx context.Context, c *streamCard, id string) error {
	if c.gone[id] {
		return nil
	}
	c.seq++
	resp, err := s.f.client.Cardkit.V1.CardElement.Delete(ctx, larkcard.NewDeleteCardElementReqBuilder().CardId(c.id).ElementId(id).
		Body(larkcard.NewDeleteCardElementReqBodyBuilder().Sequence(c.seq).Build()).Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("删除卡片组件失败：%d %s", resp.Code, resp.Msg)
	}
	c.gone[id] = true
	return nil
}

// 卡片各部分的内容
func feishuThink(s streamSnapshot) string {
	if s.Reasoning == "" {
		return "（没有思考内容）"
	}
	return "<font color='grey'>" + fsEscape(streamClip(strings.TrimSpace(s.Reasoning), 1200)) + "</font>"
}

func feishuTools(s streamSnapshot) string {
	if len(s.Tools) == 0 {
		return "（没有调用工具）"
	}
	var lines []string
	for _, t := range s.Tools {
		line := "🔧 **" + fsEscape(t.Name) + "**"
		if t.Detail != "" {
			line += "　" + fsEscape(streamClip(t.Detail, 80))
		}
		switch {
		case !t.Done:
			line += "\n<font color='grey'>　…</font>"
		case t.OK:
			line += "\n<font color='green'>　✓</font> <font color='grey'>" + fsEscape(streamClip(t.Result, 60)) + "</font>"
		default:
			line += "\n<font color='red'>　✗ " + fsEscape(streamClip(t.Result, 60)) + "</font>"
		}
		lines = append(lines, line)
	}
	// 卡片有大小限制：只留最近的几次
	if len(lines) > 12 {
		lines = append([]string{fmt.Sprintf("<font color='grey'>（前面 %d 次省略，完整记录见网页）</font>", len(lines)-12)}, lines[len(lines)-12:]...)
	}
	return strings.Join(lines, "\n")
}

func feishuFoot(s streamSnapshot) string {
	var head string
	if s.Closed {
		head = "✅ 完成"
	} else {
		head = "⏳ " + s.status()
	}
	if n := len(s.Tools); n > 0 {
		head += fmt.Sprintf(" · %d 次工具调用", n)
	}
	if s.Notes != "" {
		head += "\n" + s.Notes
	}
	if !s.Closed {
		head += "\n发「停」可以中断，发「撤回」撤回这一轮"
	}
	return "<font color='grey'>" + fsEscape(head) + "</font>"
}

// fsEscape：放进 <font> 里的文本，尖括号转义掉，免得被当成标签
func fsEscape(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}

func (s *feishuStream) Write(ctx context.Context, snap streamSnapshot) error {
	// 卡片有大小限制（30 KB 左右）：正文每张卡最多 3500 字，加上思考和工具记录，即使全是四字节 emoji 也放得下
	chunks := streamChunks(snap.Content, 3500)
	first := "…"
	if len(chunks) > 0 {
		first = chunks[0]
	}
	if len(s.cards) == 0 {
		var els []any
		if snap.Source != "" {
			els = append(els, notation("src", "📨 "+fsEscape(snap.Source)))
		}
		els = append(els,
			map[string]any{"tag": "collapsible_panel", "element_id": "proc", "expanded": false,
				"header":   map[string]any{"title": map[string]string{"tag": "plain_text", "content": "💭 思考和工具调用"}},
				"elements": []any{notation("think", feishuThink(snap)), markdown("tools", feishuTools(snap))}},
			markdown("reply", first),
			notation("foot", feishuFoot(snap)),
			fsButton("stop", "停止", "default", map[string]string{fsAction: "stop"}),
		)
		c, err := s.create(ctx, "正在处理…", els)
		if err != nil {
			return err
		}
		c.parts = map[string]string{"think": feishuThink(snap), "tools": feishuTools(snap), "reply": first, "foot": feishuFoot(snap)}
	}
	main := s.cards[0]
	for _, p := range [][2]string{{"think", feishuThink(snap)}, {"tools", feishuTools(snap)}, {"reply", first}, {"foot", feishuFoot(snap)}} {
		if err := s.update(ctx, main, p[0], p[1]); err != nil {
			return err
		}
	}
	for i := 1; i < len(chunks); i++ {
		if i == len(s.cards) {
			c, err := s.create(ctx, "回复（续）", []any{markdown("reply", chunks[i])})
			if err != nil {
				return err
			}
			c.parts["reply"] = chunks[i]
		} else if err := s.update(ctx, s.cards[i], "reply", chunks[i]); err != nil {
			return err
		}
	}
	if snap.Closed && len(snap.Tools) == 0 && snap.Reasoning == "" {
		return s.remove(ctx, main, "proc") // 简单的回答：不要空的折叠面板
	}
	return nil
}

func (s *feishuStream) Close(ctx context.Context) error {
	var lastErr error
	if len(s.cards) > 0 {
		if err := s.remove(ctx, s.cards[0], "stop"); err != nil {
			lastErr = err
		}
	}
	for i, c := range s.cards {
		c.seq++
		summary := "已回复"
		if i > 0 {
			summary = "回复（续）"
		}
		settings, _ := json.Marshal(map[string]any{"config": map[string]any{"streaming_mode": false, "summary": map[string]string{"content": summary}}})
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

// sendRaw 发一条消息（replyTo 不为空时引用回复），返回错误
func (f *Feishu) sendRaw(ctx context.Context, chat, replyTo, msgType, content string) error {
	_, err := f.sendMsg(ctx, chat, replyTo, msgType, content)
	return err
}

func (f *Feishu) sendMsg(ctx context.Context, chat, replyTo, msgType, content string) (string, error) {
	if replyTo != "" {
		r, err := f.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(replyTo).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content).Build()).Build())
		if err != nil {
			return "", err
		}
		if !r.Success() {
			return "", fmt.Errorf("发送消息失败：%d %s", r.Code, r.Msg)
		}
		return deref(r.Data.MessageId), nil
	}
	r, err := f.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chat).MsgType(msgType).Content(content).Build()).Build())
	if err != nil {
		return "", err
	}
	if !r.Success() {
		return "", fmt.Errorf("发送消息失败：%d %s", r.Code, r.Msg)
	}
	return deref(r.Data.MessageId), nil
}

// ---- 审批：卡片上带“批准 / 拒绝”按钮（需要在开放平台订阅卡片回调 card.action.trigger） ----

func feishuApprovalCard(c ApprovalCard, result string) string {
	title := c.Title
	if title == "" {
		title = "执行工具"
	}
	template, sub := "orange", "发起人："+c.From
	els := []any{markdown("", "```\n"+strings.ReplaceAll(streamClip(c.Detail, 2500), "```", "ˋˋˋ")+"\n```")}
	if result == "" {
		n := fmt.Sprint(c.N)
		els = append(els, map[string]any{"tag": "column_set", "flex_mode": "none", "columns": []any{
			map[string]any{"tag": "column", "width": "auto", "elements": []any{fsButton("", "批准", "primary", map[string]string{fsAction: "approve", fsApprove: n})}},
			map[string]any{"tag": "column", "width": "auto", "elements": []any{fsButton("", "拒绝", "danger", map[string]string{fsAction: "deny", fsApprove: n})}},
		}}, notation("", fmt.Sprintf("<font color='grey'>也可以回复 /approve %d 或 /deny %d</font>", c.N, c.N)))
	} else {
		template = "grey"
		if strings.Contains(result, "批准") {
			template = "green"
		} else if strings.Contains(result, "拒绝") {
			template = "red"
		}
		els = append(els, markdown("", "**"+result+"**"))
	}
	card, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "summary": map[string]string{"content": fmt.Sprintf("需要审批 #%d", c.N)}},
		"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": fmt.Sprintf("需要审批 #%d · %s", c.N, title)},
			"subtitle": map[string]string{"tag": "plain_text", "content": sub}, "template": template},
		"body": map[string]any{"elements": els},
	})
	return string(card)
}

func (f *Feishu) SendApproval(ctx context.Context, chatID string, c ApprovalCard) (string, error) {
	return f.sendMsg(ctx, chatID, "", "interactive", feishuApprovalCard(c, ""))
}

func (f *Feishu) ResolveApproval(ctx context.Context, chatID, ref string, c ApprovalCard, result string) error {
	r, err := f.client.Im.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().MessageId(ref).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(feishuApprovalCard(c, result)).Build()).Build())
	if err != nil {
		return err
	}
	if !r.Success() {
		return fmt.Errorf("更新审批卡片失败：%d %s", r.Code, r.Msg)
	}
	return nil
}
