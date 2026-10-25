// Package channel 是聊天平台适配层：把各平台的消息统一成 Message，再把回复发回去。
package channel

import (
	"context"
	"strings"
	"unicode/utf8"

	"nano-agent/gateway/internal/media"
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
	Mentioned  bool               // 私聊恒为 true；群聊里 @ 了机器人或回复了机器人的消息
	Session    string             // 发到指定的会话（网页用）；空表示发到这个聊天窗口接着的会话
	Action     string             // 不是说话，而是界面上的操作：ActionStop、ActionRecall、ActionApprove、ActionDeny
	Images     []media.Attachment // 这条消息附带的图片（网页上传、Telegram、飞书下载好的）
}

const (
	ActionStop   = "stop"   // 停止按钮、终端里的 Ctrl-C：停下正在进行的任务，没有任务时不用回话
	ActionRecall = "recall" // 用户在平台上撤回了 MsgID 那条消息
	// 审批消息上的按钮：Text 是审批的编号
	ActionApprove = "approve"
	ActionDeny    = "deny"
)

// ApprovalCard 是一次等管理员审批的工具调用。
type ApprovalCard struct {
	N      int
	Title  string // 比如“执行命令”“写入文件”
	Detail string // 要执行的命令、要改的文件
	From   string // 发起人
}

// Approver 是能用按钮审批的渠道：审批请求下面带“批准 / 拒绝”按钮，点了之后渠道交回一条
// Action 为 ActionApprove / ActionDeny、Text 为编号的消息（和发 /approve 一样要检查是不是管理员）。
type Approver interface {
	// SendApproval 发审批消息，返回的 ref 用来之后更新它
	SendApproval(ctx context.Context, chatID string, c ApprovalCard) (ref string, err error)
	// ResolveApproval 把审批消息改成处理结果，去掉按钮
	ResolveApproval(ctx context.Context, chatID, ref string, c ApprovalCard, result string) error
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

// Markdowner 是可选能力：agent 的回复正文按 Markdown 渲染发出去（飞书：富文本消息里的 md）。
// 没有这个能力的渠道用 Send 发原文。系统说明、命令的回答仍然用 Send。
type Markdowner interface {
	SendMarkdown(ctx context.Context, chatID, text, replyTo string) error
}

// Busy 是可选能力：处理一条消息的时候在那条消息上显示“正在处理”（飞书没有“正在输入”，
// 贴一个“敲键盘”表情），返回的函数撤掉它。不能阻塞：真正的请求在后台发。
type Busy interface {
	Busy(chatID, msgID string) (done func())
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
