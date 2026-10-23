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

// Telegram 上一轮的显示：
//   - 过程消息（有思考、工具调用时才有）：一行状态，下面是可以展开的思考和工具记录，处理中带“停止”按钮；
//   - 回复：模型写的 Markdown 转成 Telegram 的 HTML，边生成边编辑，太长时分成几条。

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

// 按钮的回调数据
const (
	tgStopData    = "stop"
	tgApprovePref = "ap:" // ap:<编号>:1 批准，ap:<编号>:0 拒绝
)

var tgStopKeyboard = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "⏹ 停止", "callback_data": tgStopData}}}}

// send 发一条新消息（id 为 0）或者改一条。button 表示这条消息下面带“停止”按钮；改消息时不带按钮，原来的就没了。
// 用 HTML 发不出去（转出来的标签 Telegram 不认）时，改成纯文本再发一次。
func (s *telegramStream) send(ctx context.Context, text string, html bool, id int64, reply, button bool) (int64, error) {
	p := map[string]any{"chat_id": s.chat, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if html {
		p["parse_mode"] = "HTML"
	}
	if button {
		p["reply_markup"] = tgStopKeyboard
	}
	method := "sendMessage"
	if id != 0 {
		p["message_id"] = id
		method = "editMessageText"
	} else if reply && s.reply != "" {
		target, _ := strconv.ParseInt(s.reply, 10, 64)
		p["reply_parameters"] = map[string]any{"message_id": target, "allow_sending_without_reply": true}
	}
	var msg tgMessage
	err := s.t.callRetry(ctx, method, p, &msg)
	var te *tgError
	if html && errors.As(err, &te) && strings.Contains(te.Desc, "can't parse entities") {
		delete(p, "parse_mode")
		p["text"] = htmlText(text)
		err = s.t.callRetry(ctx, method, p, &msg)
	}
	if err != nil {
		if errors.As(err, &te) && strings.Contains(te.Desc, "message is not modified") {
			return id, nil
		}
		return 0, err
	}
	if id == 0 {
		id = msg.MessageID
	}
	if s.buttons == nil {
		s.buttons = map[int64]bool{}
	}
	s.buttons[id] = button
	return id, nil
}

// htmlText 去掉标签、还原转义，得到纯文本（HTML 发不出去时用）
func htmlText(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}

// Write 更新这一轮的显示。处理中时“停止”按钮挂在过程消息下面，没有过程消息时挂在回复的第一条下面。
func (s *telegramStream) Write(ctx context.Context, snap streamSnapshot) error {
	running := !snap.Closed
	if snap.Concise && snap.Closed && s.processID != 0 {
		if err := s.t.callRetry(ctx, "deleteMessage", map[string]any{"chat_id": s.chat, "message_id": s.processID}, nil); err != nil {
			return err
		}
		delete(s.buttons, s.processID)
		s.processID = 0
		s.processText = ""
	}
	if snap.Concise && !snap.Closed && snap.Transient != "" {
		text := html.EscapeString(snap.Transient)
		if text != s.processText {
			id, err := s.send(ctx, text, true, s.processID, false, true)
			if err != nil {
				return err
			}
			s.processID = id
			s.processText = text
		}
	}

	if !snap.Concise && (snap.Reasoning != "" || len(snap.Tools) > 0 || snap.Source != "" || (snap.Notes != "" && snap.Content == "")) {
		text := telegramProcess(snap)
		if text != s.processText {
			id, err := s.send(ctx, text, true, s.processID, false, running)
			if err != nil {
				return err
			}
			s.processID, s.processText = id, text
		}
	}
	for i, part := range tgMarkdownChunks(snap.Content, 3800) {
		text := tgHTML(part)
		button := running && i == 0 && s.processID == 0
		if i == len(s.ids) {
			id, err := s.send(ctx, text, true, 0, i == 0, button)
			if err != nil {
				return err
			}
			s.ids = append(s.ids, id)
			s.texts = append(s.texts, text)
		} else if text != s.texts[i] || (i == 0 && s.buttons[s.ids[0]] != button) {
			if _, err := s.send(ctx, text, true, s.ids[i], false, button); err != nil {
				return err
			}
			s.texts[i] = text
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

// telegramProcess 是过程消息：状态一行，思考和工具记录放在可以展开的引用块里（默认收起）
func telegramProcess(s streamSnapshot) string {
	var parts []string
	if s.Source != "" {
		parts = append(parts, "📨 "+html.EscapeString(telegramClip(s.Source, 300)))
	}
	head := "⏳ <b>" + html.EscapeString(s.status()) + "</b>"
	if s.Closed {
		head = "✅ <b>完成</b>"
	}
	if n := len(s.Tools); n > 0 {
		head += fmt.Sprintf(" · %d 次工具调用", n)
	}
	parts = append(parts, head)
	if s.Reasoning != "" {
		parts = append(parts, "<blockquote expandable>💭 "+html.EscapeString(telegramClip(strings.TrimSpace(s.Reasoning), 900))+"</blockquote>")
	}
	if len(s.Tools) > 0 {
		var lines []string
		for _, t := range s.Tools {
			line := "🔧 <b>" + html.EscapeString(t.Name) + "</b>"
			if t.Detail != "" {
				line += " " + html.EscapeString(t.Detail)
			}
			switch {
			case !t.Done:
				line += "\n   …"
			case t.OK:
				line += "\n   ✓ " + html.EscapeString(t.Result)
			default:
				line += "\n   ✗ " + html.EscapeString(t.Result)
			}
			lines = append(lines, line)
		}
		parts = append(parts, "<blockquote expandable>"+telegramClipLines(lines, 1500)+"</blockquote>")
	}
	if s.Notes != "" {
		parts = append(parts, "<i>"+html.EscapeString(telegramClip(s.Notes, 400))+"</i>")
	}
	if !s.Closed {
		parts = append(parts, "<i>发「停」或点下面的按钮可以中断，发「撤回」撤回这一轮</i>")
	}
	return strings.Join(parts, "\n")
}

// telegramClipLines 拼起来太长时只留最后几条（最近的更有用），前面写省略了几条
func telegramClipLines(lines []string, limit int) string {
	total, keep := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		total += len([]rune(lines[i])) + 1
		if total > limit && keep < len(lines) {
			break
		}
		keep = i
	}
	out := strings.Join(lines[keep:], "\n")
	if keep > 0 {
		out = fmt.Sprintf("（前面 %d 次省略，完整记录见网页）\n", keep) + out
	}
	return out
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

// ---- 审批：消息下面带“批准 / 拒绝”按钮 ----

func tgApprovalText(c ApprovalCard) string {
	title := c.Title
	if title == "" {
		title = "执行工具"
	}
	return fmt.Sprintf("⚠️ <b>需要审批 #%d · %s</b>\n<pre>%s</pre>\n发起人：%s", c.N, html.EscapeString(title),
		html.EscapeString(telegramClip(c.Detail, 2500)), html.EscapeString(c.From))
}

func (t *Telegram) SendApproval(ctx context.Context, chatID string, c ApprovalCard) (string, error) {
	n := strconv.Itoa(c.N)
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ 批准", "callback_data": tgApprovePref + n + ":1"},
		{"text": "❌ 拒绝", "callback_data": tgApprovePref + n + ":0"},
	}}}
	var msg tgMessage
	err := t.callRetry(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": tgApprovalText(c), "parse_mode": "HTML", "reply_markup": kb}, &msg)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(msg.MessageID, 10), nil
}

func (t *Telegram) ResolveApproval(ctx context.Context, chatID, ref string, c ApprovalCard, result string) error {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return err
	}
	return t.callRetry(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": id, "parse_mode": "HTML",
		"text": tgApprovalText(c) + "\n\n<b>" + html.EscapeString(result) + "</b>", "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
}
