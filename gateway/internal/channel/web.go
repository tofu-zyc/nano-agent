package channel

import (
	"context"
	"errors"
	"sync"

	"nano-agent/gateway/internal/media"
)

// Web 是网页聊天渠道：网页可以往任何一个会话里发消息，网页用户都是登录过的管理员。
// 回复和处理过程由调度层统一发布到事件总线，网页从那里订阅，所以这里的 Send 和 RunView 什么都不用做；
// 实现 Streamer 是为了告诉调度层“正文已经实时显示过了”，不要在结束时再发一遍。
type Web struct {
	mu sync.Mutex
	h  Handler
}

func NewWeb() *Web { return &Web{} }

func (w *Web) Name() string { return "web" }

func (w *Web) Run(ctx context.Context, h Handler) error {
	w.mu.Lock()
	w.h = h
	w.mu.Unlock()
	<-ctx.Done()
	return nil
}

// Post 把网页上输入的一条消息发到会话 sid。
func (w *Web) Post(sid, text string, images []media.Attachment) error {
	w.mu.Lock()
	h := w.h
	w.mu.Unlock()
	if h == nil {
		return errors.New("网页渠道还没启动")
	}
	h(w, Message{ChatID: "browser", SenderID: "admin", SenderName: "我", Session: sid, Text: text, Images: images, Mentioned: true})
	return nil
}

func (w *Web) Send(context.Context, string, string, string) error { return nil }

func (w *Web) OpenRun(string, string) RunView { return nopView{} }

func (w *Web) Status() Status        { return Status{State: StateOnline, Identity: "浏览器"} }
func (w *Web) OnStatus(func(Status)) {}

type nopView struct{}

func (nopView) Reasoning(string)        {}
func (nopView) Content(string)          {}
func (nopView) EndMessage()             {}
func (nopView) Tool(string, string)     {}
func (nopView) ToolResult(bool, string) {}
func (nopView) Note(string)             {}
func (nopView) Close()                  {}
