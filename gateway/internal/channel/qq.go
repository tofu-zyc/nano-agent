package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/media"
)

// QQ 是 QQ 开放平台的官方机器人（可以扫码创建，见 qq_register.go）：出站 WebSocket 收事件、HTTP 发消息，
// 不需要公网地址。私聊（C2C）和群聊都支持；机器人有“接收群里所有消息”的权限时，没 @ 它的话也能听到。
//
// 被动回复：同一条消息 1 小时内最多回 4 次（带上 msg_id），超过了就是主动消息，QQ 会限量拦截。
// 所以渠道记住每个聊天最近收到的一条消息，发消息时自动带上。
//
// 聊天 ID：私聊是对方的 user_openid，群是 g.<group_openid>（发消息时要知道是哪种）。

// 开放平台的地址（测试时换成假服务）
var (
	QQAPIBase  = "https://api.sgroup.qq.com"
	QQTokenURL = "https://bots.qq.com/app/getAppAccessToken"
)

const (
	qqIntents  = 1 << 25 // GROUP_AND_C2C：私聊、群（@ 机器人的和所有消息）
	qqGroupPre = "g."

	qqReplyTTL   = time.Hour
	qqReplyLimit = 4
	qqTextLimit  = 4000 // 一条消息最多多少字（再长就拆）
)

// QQConfig 是一个 QQ 机器人的凭据。
type QQConfig struct {
	AppID     string
	AppSecret string
	Proxy     string
	APIBase   string // 默认 https://api.sgroup.qq.com（测试时换成假服务）
	TokenURL  string // 默认 https://bots.qq.com/app/getAppAccessToken
}

type QQ struct {
	statusBox
	name   string
	c      QQConfig
	hc     *http.Client
	dialer *websocket.Dialer
	log    *slog.Logger
	media  *media.Store

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	botID    string
	session  string // 断线后 RESUME 用
	seq      int64
	last     map[string]*qqLast   // 聊天 → 最近收到的一条消息（被动回复用）
	seen     map[string]time.Time // 收到过的消息 ID：重连后可能重发
	noMD     bool                 // 发 Markdown 被拒过：这个机器人没有 Markdown 权限
}

type qqLast struct {
	id    string
	at    time.Time
	used  int  // 已经回了几次
	typed bool // 已经显示过“正在输入”
}

func NewQQ(c QQConfig, store *media.Store, log *slog.Logger) (*QQ, error) {
	hc, err := httpx.NewClient(c.Proxy, 0)
	if err != nil {
		return nil, fmt.Errorf("qq 代理：%w", err)
	}
	proxy, err := httpx.ProxyFunc(c.Proxy)
	if err != nil {
		return nil, fmt.Errorf("qq 代理：%w", err)
	}
	if c.APIBase == "" {
		c.APIBase = QQAPIBase
	}
	if c.TokenURL == "" {
		c.TokenURL = QQTokenURL
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")
	return &QQ{name: "qq", c: c, hc: hc, dialer: &websocket.Dialer{Proxy: proxy, HandshakeTimeout: 30 * time.Second},
		log: log.With("channel", "qq"), media: store, last: map[string]*qqLast{}, seen: map[string]time.Time{}}, nil
}

func (q *QQ) Name() string { return q.name }

// SetName 是渠道名 qq.<机器人 ID>。
func (q *QQ) SetName(name string) {
	if name != "" && name != q.name {
		q.name = name
		q.log = q.log.With("channel", name)
	}
}

// ---- 鉴权 ----

var errQQAuth = errors.New("AppID 或 AppSecret 不对")

// accessToken 换一个调用凭证，快过期（一分钟内）时换新的。
func (q *QQ) accessToken(ctx context.Context) (string, error) {
	q.mu.Lock()
	if q.token != "" && time.Until(q.tokenExp) > time.Minute {
		t := q.token
		q.mu.Unlock()
		return t, nil
	}
	q.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"appId": q.c.AppID, "clientSecret": q.c.AppSecret})
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, q.c.TokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Token   string          `json:"access_token"`
		Expires json.RawMessage `json:"expires_in"` // 有时是字符串
		Code    int             `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("换调用凭证：HTTP %d", resp.StatusCode)
	}
	if r.Token == "" {
		if r.Code == 100016 || strings.Contains(r.Message, "secret") || strings.Contains(r.Message, "appid") {
			return "", fmt.Errorf("%w（%d %s）", errQQAuth, r.Code, r.Message)
		}
		return "", fmt.Errorf("换调用凭证失败：%d %s", r.Code, r.Message)
	}
	sec, _ := strconv.Atoi(strings.Trim(string(r.Expires), `"`))
	if sec <= 0 {
		sec = 7200
	}
	q.mu.Lock()
	q.token, q.tokenExp = r.Token, time.Now().Add(time.Duration(sec)*time.Second)
	q.mu.Unlock()
	return r.Token, nil
}

// api 调一个开放平台接口。out 为 nil 时不解析结果。
func (q *QQ) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := q.accessToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, q.c.APIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := q.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if resp.StatusCode == http.StatusUnauthorized {
			q.mu.Lock()
			q.token = "" // 下次重新换
			q.mu.Unlock()
		}
		return &qqError{Status: resp.StatusCode, Code: e.Code, Msg: e.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type qqError struct {
	Status, Code int
	Msg          string
}

func (e *qqError) Error() string {
	return fmt.Sprintf("QQ 接口出错：HTTP %d，%d %s", e.Status, e.Code, e.Msg)
}

// ---- 收消息：WebSocket 网关 ----

type qqPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

func (q *QQ) Run(ctx context.Context, h Handler) error {
	q.setState(StateStarting, "")
	defer func() {
		if st := q.Status(); st.State != StateError || ctx.Err() != nil {
			q.setState(StateStopped, "")
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := q.connect(ctx, h)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errQQAuth) {
			q.setState(StateError, err.Error())
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // 连上过一阵：从头退避
		}
		msg := "连接断了"
		if err != nil {
			msg = err.Error()
		}
		q.setState(StateError, msg)
		q.log.Warn("QQ 连接断开，稍后重连", "err", err, "wait", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

// connect 连一次网关，直到断开。
func (q *QQ) connect(ctx context.Context, h Handler) error {
	tok, err := q.accessToken(ctx)
	if err != nil {
		return err
	}
	var g struct {
		URL string `json:"url"`
	}
	if err := q.api(ctx, http.MethodGet, "/gateway", nil, &g); err != nil {
		return fmt.Errorf("取网关地址：%w", err)
	}
	conn, _, err := q.dialer.DialContext(ctx, g.URL, nil)
	if err != nil {
		return fmt.Errorf("连接网关：%w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	var wmu sync.Mutex
	send := func(p any) error {
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteJSON(p)
	}

	// 第一条是 HELLO，带心跳间隔
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var hello qqPayload
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("等网关的 HELLO：%w", err)
	}
	var hd struct {
		Interval int `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(hello.D, &hd)
	interval := time.Duration(hd.Interval) * time.Millisecond
	if interval <= 0 {
		interval = 40 * time.Second
	}
	q.mu.Lock()
	session, seq := q.session, q.seq
	q.mu.Unlock()
	if session != "" {
		err = send(map[string]any{"op": 6, "d": map[string]any{"token": "QQBot " + tok, "session_id": session, "seq": seq}})
	} else {
		err = send(map[string]any{"op": 2, "d": map[string]any{"token": "QQBot " + tok, "intents": qqIntents, "shard": []int{0, 1}}})
	}
	if err != nil {
		return err
	}

	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				q.mu.Lock()
				s := q.seq
				q.mu.Unlock()
				var d any
				if s > 0 {
					d = s
				}
				if err := send(map[string]any{"op": 1, "d": d}); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(2*interval + 15*time.Second))
		var p qqPayload
		if err := conn.ReadJSON(&p); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return q.closed(ce.Code, ce.Text)
			}
			return err
		}
		if p.S > 0 {
			q.mu.Lock()
			q.seq = p.S
			q.mu.Unlock()
		}
		switch p.Op {
		case 0:
			q.dispatch(ctx, p, h)
		case 7: // 网关要我们重连（带着会话 RESUME）
			return errors.New("网关要求重连")
		case 9: // 会话失效
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				q.mu.Lock()
				q.session, q.seq = "", 0
				q.mu.Unlock()
			}
			return errors.New("会话失效，重新连接")
		}
	}
}

// closed 按网关的关闭码决定：凭证失效换新的，会话失效重新 IDENTIFY，没权限就报错。
func (q *QQ) closed(code int, text string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch code {
	case 4004:
		q.token = ""
	case 4006, 4007, 4009:
		q.session, q.seq = "", 0
	case 4914, 4915:
		return fmt.Errorf("这个机器人没有收私聊和群消息的权限（%d %s）", code, text)
	}
	return fmt.Errorf("网关断开：%d %s", code, text)
}

type qqAttachment struct {
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ASR         string `json:"asr_refer_text"` // 语音转的文字
}

type qqMessage struct {
	ID          string         `json:"id"`
	Content     string         `json:"content"`
	GroupOpenID string         `json:"group_openid"`
	Attachments []qqAttachment `json:"attachments"`
	Author      struct {
		UserOpenID   string `json:"user_openid"`
		MemberOpenID string `json:"member_openid"`
		Username     string `json:"username"`
		Bot          bool   `json:"bot"`
	} `json:"author"`
	Mentions []struct {
		IsYou bool `json:"is_you"`
	} `json:"mentions"`
}

func (q *QQ) dispatch(ctx context.Context, p qqPayload, h Handler) {
	switch p.T {
	case "READY":
		var d struct {
			SessionID string `json:"session_id"`
			User      struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		}
		_ = json.Unmarshal(p.D, &d)
		q.mu.Lock()
		q.session, q.botID = d.SessionID, d.User.ID
		q.mu.Unlock()
		q.setIdentity(d.User.Username)
		q.setState(StateOnline, "")
		q.log.Info("QQ 机器人已就绪", "name", d.User.Username)
	case "RESUMED":
		q.setState(StateOnline, "")
	case "C2C_MESSAGE_CREATE", "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE":
		var m qqMessage
		if err := json.Unmarshal(p.D, &m); err != nil || m.ID == "" || q.duplicate(m.ID) {
			return
		}
		msg, ok := q.convert(ctx, p.T, m)
		if !ok {
			return
		}
		q.mu.Lock()
		q.last[msg.ChatID] = &qqLast{id: m.ID, at: time.Now()}
		q.mu.Unlock()
		h(q, msg)
	}
}

var qqMention = regexp.MustCompile(`<@!?([0-9A-Za-z_-]+)>`)

func (q *QQ) convert(ctx context.Context, t string, m qqMessage) (Message, bool) {
	q.mu.Lock()
	botID := q.botID
	q.mu.Unlock()
	msg := Message{MsgID: m.ID, Mentioned: true}
	text := m.Content
	if t == "C2C_MESSAGE_CREATE" {
		msg.ChatID, msg.SenderID = m.Author.UserOpenID, m.Author.UserOpenID
	} else {
		if m.Author.Bot {
			return Message{}, false
		}
		msg.ChatID, msg.SenderID, msg.SenderName, msg.Group = qqGroupPre+m.GroupOpenID, m.Author.MemberOpenID, m.Author.Username, true
		msg.Mentioned = t == "GROUP_AT_MESSAGE_CREATE"
		for _, x := range m.Mentions {
			msg.Mentioned = msg.Mentioned || x.IsYou
		}
		if botID != "" && regexp.MustCompile(`<@!?`+regexp.QuoteMeta(botID)+`>`).MatchString(text) {
			msg.Mentioned = true
		}
	}
	// 去掉 @机器人；@ 别人的写成 @某人
	text = qqMention.ReplaceAllStringFunc(text, func(s string) string {
		if id := qqMention.FindStringSubmatch(s)[1]; id == botID {
			return ""
		}
		return "@某人"
	})
	var notes []string
	for _, a := range m.Attachments {
		switch {
		case strings.HasPrefix(a.ContentType, "image/") || a.ContentType == "image":
			if img, err := q.download(ctx, a); err == nil {
				msg.Images = append(msg.Images, img)
			} else {
				q.log.Warn("下载图片失败", "err", err)
				notes = append(notes, "[图片没收到："+err.Error()+"]")
			}
		case a.ContentType == "voice" || strings.HasPrefix(a.ContentType, "audio/"):
			if a.ASR != "" {
				notes = append(notes, "[语音] "+a.ASR)
			} else {
				notes = append(notes, "[一段语音，没有转出文字]")
			}
		default:
			name := a.Filename
			if name == "" {
				name = a.ContentType
			}
			notes = append(notes, "[文件："+name+"]")
		}
	}
	msg.Text = strings.TrimSpace(strings.Join(append([]string{strings.TrimSpace(text)}, notes...), "\n"))
	if msg.Text == "" && len(msg.Images) == 0 {
		return Message{}, false
	}
	return msg, true
}

func (q *QQ) download(ctx context.Context, a qqAttachment) (media.Attachment, error) {
	if q.media == nil {
		return media.Attachment{}, errors.New("没有地方存图片")
	}
	if a.Size > media.MaxImageBytes {
		return media.Attachment{}, fmt.Errorf("图片太大（%d 字节）", a.Size)
	}
	u := a.URL
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return media.Attachment{}, err
	}
	resp, err := q.hc.Do(req)
	if err != nil {
		return media.Attachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Attachment{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return media.Attachment{}, err
	}
	name := a.Filename
	if name == "" {
		name = "image"
	}
	return q.media.Save(data, name, "qq")
}

// duplicate 记下消息 ID，见过的返回 true（重连后网关可能重发）。
func (q *QQ) duplicate(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.seen[id]; ok {
		return true
	}
	now := time.Now()
	q.seen[id] = now
	if len(q.seen) > 2000 {
		for k, t := range q.seen {
			if now.Sub(t) > 10*time.Minute {
				delete(q.seen, k)
			}
		}
	}
	return false
}

// ---- 发消息 ----

func qqPath(chat string) string {
	if g, ok := strings.CutPrefix(chat, qqGroupPre); ok {
		return "/v2/groups/" + g + "/messages"
	}
	return "/v2/users/" + chat + "/messages"
}

// passive 是这个聊天还能用的被动回复：最近一条消息的 ID 和下一个序号；没有就是主动消息。
func (q *QQ) passive(chat string) (string, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.last[chat]
	if l == nil || time.Since(l.at) > qqReplyTTL || l.used >= qqReplyLimit {
		return "", 0
	}
	l.used++
	return l.id, l.used // 同一条消息的几次回复序号不能重复
}

func (q *QQ) post(ctx context.Context, chat string, body map[string]any) error {
	if id, seq := q.passive(chat); id != "" {
		body["msg_id"], body["msg_seq"] = id, seq
	}
	return q.api(ctx, http.MethodPost, qqPath(chat), body, nil)
}

func (q *QQ) Send(ctx context.Context, chatID, text, _ string) error {
	for _, part := range Split(text, qqTextLimit) {
		if err := q.post(ctx, chatID, map[string]any{"msg_type": 0, "content": part}); err != nil {
			return err
		}
	}
	return nil
}

// SendMarkdown 发 agent 的回复：QQ 原生的 Markdown 消息。机器人没有 Markdown 权限时退回纯文本，之后都发纯文本。
func (q *QQ) SendMarkdown(ctx context.Context, chatID, text, replyTo string) error {
	q.mu.Lock()
	noMD := q.noMD
	q.mu.Unlock()
	if noMD {
		return q.Send(ctx, chatID, text, replyTo)
	}
	for _, part := range Split(text, qqTextLimit) {
		err := q.post(ctx, chatID, map[string]any{"msg_type": 2, "markdown": map[string]string{"content": part}})
		var qe *qqError
		if errors.As(err, &qe) && qe.Status/100 == 4 {
			q.log.Warn("发 Markdown 被拒，以后改发纯文本", "err", err)
			q.mu.Lock()
			q.noMD = true
			q.mu.Unlock()
			err = q.Send(ctx, chatID, part, "")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Typing 是私聊里的“对方正在输入”：每条消息只显示一次（一次能撑 60 秒），群里没有。
func (q *QQ) Typing(ctx context.Context, chatID string) error {
	if strings.HasPrefix(chatID, qqGroupPre) {
		return nil
	}
	q.mu.Lock()
	l := q.last[chatID]
	if l == nil || l.typed || time.Since(l.at) > qqReplyTTL {
		q.mu.Unlock()
		return nil
	}
	l.typed = true
	id := l.id
	q.mu.Unlock()
	return q.api(ctx, http.MethodPost, qqPath(chatID), map[string]any{
		"msg_type": 6, "input_notify": map[string]int{"input_type": 1, "input_second": 60},
		"msg_id": id, "msg_seq": 1000 + rand.IntN(60000), // 和回复的序号（1–4）错开
	}, nil)
}
