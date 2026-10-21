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

// Streamer 是可选能力：边生成边显示。每一轮处理打开一个 RunView，直到这一轮结束；
// 回复正文已经通过 RunView 显示过，就不会再用 Send 发一遍。
type Streamer interface {
	OpenRun(chatID, replyTo string) RunView
}

// RunView 显示一轮处理的过程。方法可能在不同的协程里调用，实现要自己加锁，并且不能阻塞太久
// （命令行直接打印；聊天平台要自己缓冲、限频）。Close 之后的调用应当被忽略。
type RunView interface {
	Reasoning(text string)             // 思考内容的增量
	Content(text string)               // 回复正文的增量
	EndMessage()                       // 一次模型调用的输出结束
	Tool(name, detail string)          // 开始执行一个工具
	ToolResult(ok bool, detail string) // 工具执行完
	Note(text string)                  // 附加说明，比如这一轮的用量
	Close()
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
