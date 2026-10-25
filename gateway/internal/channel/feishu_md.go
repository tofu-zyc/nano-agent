package channel

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// ---- 回复正文按 Markdown 发：富文本消息（post）里的 md 段落。看起来还是普通的聊天气泡，不是卡片 ----

// 富文本消息的请求体最多 30 KB：中文一个字 3 字节，按 6000 字一段切
const feishuPostLimit = 6000

var mdHeading = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)

// feishuMD 把飞书 md 段落不支持的写法换掉：标题变成加粗（代码块里的不动）。
func feishuMD(text string) string {
	lines := strings.Split(text, "\n")
	fence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			continue
		}
		if !fence {
			if m := mdHeading.FindStringSubmatch(l); m != nil {
				lines[i] = "**" + strings.Trim(m[1], "*") + "**"
			}
		}
	}
	return strings.Join(lines, "\n")
}

func feishuPost(text string) string {
	b, _ := json.Marshal(map[string]any{"zh_cn": map[string]any{
		"content": [][]map[string]string{{{"tag": "md", "text": feishuMD(text)}}},
	}})
	return string(b)
}

// SendMarkdown 发 agent 的回复正文：富文本消息，加粗、列表、代码块、链接都能渲染。发不出去时退回纯文本。
func (f *Feishu) SendMarkdown(ctx context.Context, chatID, text, replyTo string) error {
	for i, part := range Split(text, feishuPostLimit) {
		reply := ""
		if i == 0 {
			reply = replyTo
		}
		if _, err := f.sendMsg(ctx, chatID, reply, "post", feishuPost(part)); err != nil {
			f.log.Warn("富文本消息发送失败，改发纯文本", "err", err)
			if err := f.Send(ctx, chatID, part, reply); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 处理时的状态：飞书没有给机器人的“正在输入”，在用户那条消息上贴一个“敲键盘”表情，回完撤掉 ----

const feishuBusyEmoji = "Typing"

func (f *Feishu) Busy(chatID, msgID string) func() {
	if msgID == "" || f.client == nil {
		return func() {}
	}
	id := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r, err := f.client.Im.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().MessageId(msgID).
			Body(larkim.NewCreateMessageReactionReqBodyBuilder().
				ReactionType(larkim.NewEmojiBuilder().EmojiType(feishuBusyEmoji).Build()).Build()).Build())
		switch {
		case err != nil:
			f.log.Debug("贴“正在处理”表情失败", "err", err)
			id <- ""
		case !r.Success() || r.Data == nil:
			f.log.Debug("贴“正在处理”表情失败", "code", r.Code, "msg", r.Msg)
			id <- ""
		default:
			id <- deref(r.Data.ReactionId)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			go func() {
				rid := <-id
				if rid == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				r, err := f.client.Im.MessageReaction.Delete(ctx, larkim.NewDeleteMessageReactionReqBuilder().
					MessageId(msgID).ReactionId(rid).Build())
				if err != nil {
					f.log.Debug("撤掉“正在处理”表情失败", "err", err)
				} else if !r.Success() {
					f.log.Debug("撤掉“正在处理”表情失败", "code", r.Code, "msg", r.Msg)
				}
			}()
		})
	}
}
