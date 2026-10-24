package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/media"
)

// Feishu 通过飞书开放平台的长连接（WebSocket）收事件，不需要公网地址。
type Feishu struct {
	statusBox
	cfg      config.Feishu
	base     string
	client   *lark.Client
	log      *slog.Logger
	groupAll bool

	botOpenID string
	name      string       // 渠道名：内置的是 feishu，另外加的机器人是 feishu.<id>
	media     *media.Store // 用户发来的图片存这里，nil 时不收图片
	queue     chan any     // *larkim.P2MessageReceiveV1 或 *larkim.P2MessageRecalledV1，按收到的顺序处理

	mu    sync.Mutex
	names map[string]string // open_id → 姓名
	seen  map[string]time.Time
}

func NewFeishu(c config.Feishu, store *media.Store, log *slog.Logger) (*Feishu, error) {
	hc, err := httpx.NewClient(c.Proxy, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("feishu.proxy：%w", err)
	}
	base := lark.FeishuBaseUrl
	if c.Domain == "lark" {
		base = lark.LarkBaseUrl
	}
	f := &Feishu{
		name:     "feishu",
		cfg:      c,
		base:     base,
		log:      log.With("channel", "feishu"),
		groupAll: c.GroupReplyAll,
		media:    store,
		queue:    make(chan any, 256),
		names:    map[string]string{},
		seen:     map[string]time.Time{},
	}
	f.client = lark.NewClient(c.AppID, c.AppSecret,
		lark.WithOpenBaseUrl(base),
		lark.WithHttpClient(hc),
		lark.WithLogLevel(larkcore.LogLevelWarn),
		lark.WithLogger(sdkLogger{f.log}))
	return f, nil
}

func (f *Feishu) Name() string { return f.name }

// SetName 给另外加的机器人换渠道名（feishu.<id>），内置的机器人不用调。
func (f *Feishu) SetName(name string) {
	if name == "" || name == f.name {
		return
	}
	f.name = name
	f.log = f.log.With("channel", name)
}

func (f *Feishu) Run(ctx context.Context, h Handler) error {
	f.setState(StateStarting, "")
	f.fetchBotInfo(ctx)

	// SDK 的事件回调要尽快返回，真正的处理（查用户名等）放到单独的协程里按顺序做
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-f.queue:
				var m Message
				ok := false
				switch ev := ev.(type) {
				case *larkim.P2MessageReceiveV1:
					m, ok = f.convert(ctx, ev)
				case *larkim.P2MessageRecalledV1:
					m, ok = recalled(ev)
				case Message: // 卡片上的按钮
					m, ok = ev, true
				}
				if ok {
					h(f, m)
				}
			}
		}
	}()

	d := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			select {
			case f.queue <- ev:
			default:
				f.log.Warn("消息队列已满，丢弃一条飞书消息")
			}
			return nil
		}).
		OnP2MessageRecalledV1(func(_ context.Context, ev *larkim.P2MessageRecalledV1) error {
			select {
			case f.queue <- ev:
			default:
			}
			return nil
		}).
		OnP2CardActionTrigger(func(_ context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			m, toast := cardAction(ev)
			if m.Action != "" {
				select {
				case f.queue <- m:
				default:
				}
			}
			return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: toast}}, nil
		})

	pf, err := httpx.ProxyFunc(f.cfg.Proxy)
	if err != nil {
		return err
	}
	hc, _ := httpx.NewClient(f.cfg.Proxy, 30*time.Second)
	ws := larkws.NewClient(f.cfg.AppID, f.cfg.AppSecret,
		larkws.WithEventHandler(d),
		larkws.WithDomain(f.base),
		larkws.WithHttpClient(hc),
		larkws.WithWebSocketDialer(&websocket.Dialer{Proxy: pf, HandshakeTimeout: 45 * time.Second}),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithLogger(sdkLogger{f.log}),
		larkws.WithOnReady(func() { f.setState(StateOnline, ""); f.log.Info("飞书长连接已建立") }))
	err = ws.Start(ctx)
	if err != nil && ctx.Err() == nil {
		f.setState(StateError, err.Error())
	} else {
		f.setState(StateStopped, "")
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// fetchBotInfo 取机器人自己的 open_id，用来判断群消息是不是 @ 了自己。失败不致命。
func (f *Feishu) fetchBotInfo(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := f.client.Get(cctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		f.log.Warn("获取机器人信息失败（群聊里会把任何 @ 都当成 @ 机器人）", "err", err)
		return
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &r); err != nil || r.Code != 0 {
		f.log.Warn("获取机器人信息失败（群聊里会把任何 @ 都当成 @ 机器人）", "code", r.Code, "msg", r.Msg)
		return
	}
	f.botOpenID = r.Bot.OpenID
	f.setIdentity(r.Bot.AppName)
	f.log.Info("飞书机器人已就绪", "name", r.Bot.AppName)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// recalled：用户撤回了一条消息（需要订阅 im.message.recalled_v1，扫码创建的机器人自动订阅）
func recalled(ev *larkim.P2MessageRecalledV1) (Message, bool) {
	if ev == nil || ev.Event == nil || deref(ev.Event.MessageId) == "" || deref(ev.Event.ChatId) == "" {
		return Message{}, false
	}
	return Message{ChatID: deref(ev.Event.ChatId), MsgID: deref(ev.Event.MessageId), Mentioned: true, Action: ActionRecall}, true
}

func (f *Feishu) convert(ctx context.Context, ev *larkim.P2MessageReceiveV1) (Message, bool) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil || ev.Event.Sender == nil {
		return Message{}, false
	}
	msg, sender := ev.Event.Message, ev.Event.Sender
	if deref(sender.SenderType) != "user" || sender.SenderId == nil {
		return Message{}, false
	}
	id := deref(msg.MessageId)
	if f.duplicate(id) { // 飞书的事件至少投递一次，可能重复
		return Message{}, false
	}
	text := extractText(deref(msg.MessageType), deref(msg.Content))
	images := f.fetchImages(ctx, id, msg)
	if text == "" && len(images) == 0 {
		return Message{}, false
	}

	group := deref(msg.ChatType) == "group"
	mentioned := !group || f.groupAll
	for _, mt := range msg.Mentions {
		key := deref(mt.Key)
		if key == "" {
			continue
		}
		isBot := false
		if f.botOpenID != "" {
			isBot = mt.Id != nil && deref(mt.Id.OpenId) == f.botOpenID
		} else {
			isBot = deref(mt.MentionedType) == "bot" || group // 不知道自己的 open_id 时只能宽松处理
		}
		if isBot {
			mentioned = true
			text = strings.ReplaceAll(text, key, "")
		} else {
			text = strings.ReplaceAll(text, key, "@"+deref(mt.Name))
		}
	}

	openID := deref(sender.SenderId.OpenId)
	senderName := f.userName(ctx, openID)
	chatName := ""
	if !group {
		chatName = senderName
	}
	return Message{
		ChatID:     deref(msg.ChatId),
		SenderID:   openID,
		SenderName: senderName,
		ChatName:   chatName,
		Text:       strings.TrimSpace(text),
		MsgID:      id,
		Group:      group,
		Mentioned:  mentioned,
		Images:     images,
	}, true
}

// extractImageKeys 从 image 消息或者富文本 post 里取出图片的 image_key。
func extractImageKeys(msgType, content string) []string {
	switch msgType {
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if json.Unmarshal([]byte(content), &c) == nil && c.ImageKey != "" {
			return []string{c.ImageKey}
		}
	case "post":
		var c struct {
			Content [][]struct {
				Tag      string `json:"tag"`
				ImageKey string `json:"image_key"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(content), &c) == nil {
			var keys []string
			for _, para := range c.Content {
				for _, el := range para {
					if el.Tag == "img" && el.ImageKey != "" {
						keys = append(keys, el.ImageKey)
					}
				}
			}
			return keys
		}
	}
	return nil
}

// fetchImages 把消息里的图片下载下来存进图片目录。失败只记日志，文字照常处理。
func (f *Feishu) fetchImages(ctx context.Context, messageID string, msg *larkim.EventMessage) []media.Attachment {
	if f.media == nil || messageID == "" {
		return nil
	}
	keys := extractImageKeys(deref(msg.MessageType), deref(msg.Content))
	var out []media.Attachment
	for _, key := range keys {
		if len(out) >= media.MaxPerMessage {
			break
		}
		a, err := f.download(ctx, messageID, key)
		if err != nil {
			f.log.Warn("下载飞书图片失败", "err", err)
			continue
		}
		out = append(out, a)
	}
	return out
}

// download 下载消息里的一张图片（im/v1/messages/{id}/resources/{key}?type=image）。
func (f *Feishu) download(ctx context.Context, messageID, key string) (media.Attachment, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := f.client.Im.MessageResource.Get(cctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(key).Type("image").Build())
	if err != nil {
		return media.Attachment{}, err
	}
	if !resp.Success() {
		return media.Attachment{}, fmt.Errorf("飞书返回 %d %s", resp.Code, resp.Msg)
	}
	if resp.File == nil {
		return media.Attachment{}, errors.New("飞书没有返回图片内容")
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, media.MaxImageBytes+1))
	if err != nil {
		return media.Attachment{}, err
	}
	return f.media.Save(data, "feishu-"+key, "feishu")
}

// extractText 从消息内容里取出纯文本。目前支持 text 和 post（富文本）。
func extractText(msgType, content string) string {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &c) == nil {
			return c.Text
		}
	case "post":
		var c struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
				Href string `json:"href"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(content), &c) != nil {
			return ""
		}
		var lines []string
		if c.Title != "" {
			lines = append(lines, c.Title)
		}
		for _, para := range c.Content {
			var b strings.Builder
			for _, el := range para {
				switch el.Tag {
				case "text", "a", "at", "code_block", "md":
					b.WriteString(el.Text)
				}
			}
			lines = append(lines, b.String())
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

func (f *Feishu) duplicate(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if _, ok := f.seen[id]; ok {
		return true
	}
	f.seen[id] = now
	if len(f.seen) > 2000 {
		for k, t := range f.seen {
			if now.Sub(t) > time.Hour {
				delete(f.seen, k)
			}
		}
	}
	return false
}

// userName 查用户姓名（需要通讯录权限），查不到就用 open_id 后几位代替。结果会缓存。
func (f *Feishu) userName(ctx context.Context, openID string) string {
	f.mu.Lock()
	name, ok := f.names[openID]
	f.mu.Unlock()
	if ok {
		return name
	}
	name = "用户" + openID[max(0, len(openID)-4):]
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := f.client.Contact.User.Get(cctx, larkcontact.NewGetUserReqBuilder().
		UserId(openID).UserIdType("open_id").Build())
	if err == nil && resp.Success() && resp.Data != nil && resp.Data.User != nil && deref(resp.Data.User.Name) != "" {
		name = deref(resp.Data.User.Name)
	} else if err == nil && !resp.Success() {
		f.log.Debug("查询用户姓名失败（可能没开通讯录权限）", "code", resp.Code, "msg", resp.Msg)
	}
	f.mu.Lock()
	f.names[openID] = name
	f.mu.Unlock()
	return name
}

func (f *Feishu) Send(ctx context.Context, chatID, text, replyTo string) error {
	for i, part := range Split(text, 8000) {
		content, _ := json.Marshal(map[string]string{"text": part})
		if i == 0 && replyTo != "" {
			resp, err := f.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
				MessageId(replyTo).
				Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(string(content)).Build()).
				Build())
			if err == nil && resp.Success() {
				continue
			}
			f.log.Warn("引用回复失败，改为直接发送", "err", err)
		}
		resp, err := f.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
			ReceiveIdType("chat_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("text").Content(string(content)).Build()).
			Build())
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("飞书发送消息失败：%d %s", resp.Code, resp.Msg)
		}
	}
	return nil
}

// cardAction 把卡片按钮的回调转成消息：停止、批准、拒绝（需要在开放平台订阅 card.action.trigger 回调）
func cardAction(ev *callback.CardActionTriggerEvent) (Message, string) {
	e := ev.Event
	if e == nil || e.Action == nil || e.Operator == nil || e.Context == nil {
		return Message{}, ""
	}
	val := func(k string) string { v, _ := e.Action.Value[k].(string); return v }
	m := Message{ChatID: e.Context.OpenChatID, SenderID: e.Operator.OpenID, Mentioned: true}
	switch val(fsAction) {
	case "stop":
		m.Action = ActionStop
		return m, "正在停止…"
	case "approve":
		m.Action, m.Text = ActionApprove, val(fsApprove)
		return m, "已批准"
	case "deny":
		m.Action, m.Text = ActionDeny, val(fsApprove)
		return m, "已拒绝"
	}
	return Message{}, ""
}

// sdkLogger 把 SDK 的日志接到 slog。
type sdkLogger struct{ l *slog.Logger }

func (s sdkLogger) msg(args []any) string                { return strings.TrimSpace(fmt.Sprintln(args...)) }
func (s sdkLogger) Debug(_ context.Context, args ...any) { s.l.Debug(s.msg(args)) }
func (s sdkLogger) Info(_ context.Context, args ...any)  { s.l.Debug(s.msg(args)) }
func (s sdkLogger) Warn(_ context.Context, args ...any)  { s.l.Warn(s.msg(args)) }
func (s sdkLogger) Error(_ context.Context, args ...any) { s.l.Error(s.msg(args)) }
