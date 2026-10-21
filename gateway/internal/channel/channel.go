// Package channel 是聊天平台适配层：把各平台的消息统一成 Message，再把回复发回去。
package channel

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Message 是一条收到的用户消息。
type Message struct {
	ChatID     string // 平台的会话 ID（私聊、群聊）
	SenderID   string // 平台的用户 ID
	SenderName string
	Text       string // 已去掉 @机器人 的部分
	MsgID      string // 用于引用回复
	Group      bool
	Mentioned  bool // 私聊恒为 true；群聊里 @ 了机器人或回复了机器人的消息
}

// Handler 处理收到的消息，必须很快返回。
type Handler func(ch Channel, m Message)

type Channel interface {
	Name() string // 会话 ID 和身份的前缀，如 "tg"、"feishu"
	Run(ctx context.Context, h Handler) error
	Send(ctx context.Context, chatID, text, replyTo string) error
}

// Typer 是可选能力：显示“正在输入”。
type Typer interface {
	Typing(ctx context.Context, chatID string) error
}

// Split 把长文本按 rune 数切成多段，尽量在换行处断开。
func Split(text string, limit int) []string {
	var parts []string
	for utf8.RuneCountInString(text) > limit {
		runes := []rune(text)
		cut := limit
		if i := strings.LastIndex(string(runes[:limit]), "\n"); i > 0 {
			if n := utf8.RuneCountInString(string(runes[:limit])[:i]); n > limit*3/5 {
				cut = n
			}
		}
		parts = append(parts, strings.TrimRight(string(runes[:cut]), "\n"))
		text = strings.TrimLeft(string(runes[cut:]), "\n")
	}
	if text != "" || len(parts) == 0 {
		parts = append(parts, text)
	}
	return parts
}
