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
	ChatName   string // 会话的名字：群名，或者私聊对方的名字；拿不到时为空
	Text       string // 已去掉 @机器人 的部分
	MsgID      string // 用于引用回复
	Group      bool
	Mentioned  bool   // 私聊恒为 true；群聊里 @ 了机器人或回复了机器人的消息
	Session    string // 发到指定的会话（网页用）；空表示发到这个聊天窗口接着的会话
	Action     string // 不是说话，而是界面上的操作：ActionStop、ActionRecall
}

const (
	ActionStop   = "stop"   // 停止按钮、终端里的 Ctrl-C：停下正在进行的任务，没有任务时不用回话
	ActionRecall = "recall" // 用户在平台上撤回了 MsgID 那条消息
)

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

// Noticer 是可选能力：用不同于机器人回复的样式显示系统说明（比如补上的聊天记录）。
// 没有这个能力的渠道直接用 Send 发。
type Noticer interface {
	Notice(chatID, text string)
}

// Watcher 同步显示同一会话里别的窗口发来的消息和本轮回复。
type Watcher interface {
	Streamer
	OpenMirror(chatID, via, text string) RunView
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
