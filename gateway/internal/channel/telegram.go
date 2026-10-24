package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/media"
)

// Telegram 通过 Bot API 长轮询收消息，不需要公网地址。
type Telegram struct {
	statusBox
	token    string
	base     string
	hc       *http.Client
	log      *slog.Logger
	groupAll bool

	botID   int64
	botUser string
	name    string       // 渠道名：内置的是 tg，另外加的机器人是 tg.<id>
	media   *media.Store // 用户发来的图片存这里，nil 时不收图片
}

func NewTelegram(c config.Telegram, store *media.Store, log *slog.Logger) (*Telegram, error) {
	hc, err := httpx.NewClient(c.Proxy, 0) // 每个请求自己带超时
	if err != nil {
		return nil, fmt.Errorf("telegram.proxy：%w", err)
	}
	return &Telegram{
		name:     "tg",
		token:    c.Token,
		base:     strings.TrimRight(c.APIBase, "/"),
		hc:       hc,
		log:      log.With("channel", "tg"),
		groupAll: c.GroupReplyAll,
	}, nil
}

func (t *Telegram) Name() string { return t.name }

// SetName 给另外加的机器人换渠道名（tg.<id>），内置的机器人不用调。
func (t *Telegram) SetName(name string) {
	if name == "" || name == t.name {
		return
	}
	t.name = name
	t.log = t.log.With("channel", name)
}

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type tgChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private | group | supergroup | channel
	Title string `json:"title"`
}

type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type tgDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgMessage struct {
	MessageID int64         `json:"message_id"`
	From      *tgUser       `json:"from"`
	Chat      tgChat        `json:"chat"`
	Text      string        `json:"text"`
	Caption   string        `json:"caption"`
	ReplyTo   *tgMessage    `json:"reply_to_message"`
	Photo     []tgPhotoSize `json:"photo"`    // 同一个图片的几个尺寸，最后一个是最大的
	Document  *tgDocument   `json:"document"` // 以文件发的图片
}

type tgUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *tgMessage       `json:"message"`
	Callback *tgCallbackQuery `json:"callback_query"`
}

// tgCallbackQuery 是点了消息下面的按钮（停止）
type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgError struct {
	Method     string
	Code       int
	Desc       string
	RetryAfter int
}

func (e *tgError) Error() string {
	return fmt.Sprintf("telegram %s：%d %s", e.Method, e.Code, e.Desc)
}

func (t *Telegram) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return t.redact(err) // 错误信息里带着 URL，不能把 token 打进日志
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s：HTTP %d，响应无法解析", method, resp.StatusCode)
	}
	if !r.OK {
		e := &tgError{Method: method, Code: r.ErrorCode, Desc: r.Description}
		if r.Parameters != nil {
			e.RetryAfter = r.Parameters.RetryAfter
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (t *Telegram) redact(err error) error {
	if t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
}

// callRetry 遇到限流（429）时按服务端要求等待后重试。
func (t *Telegram) callRetry(ctx context.Context, method string, params, out any) error {
	for attempt := 0; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := t.call(cctx, method, params, out)
		cancel()
		var te *tgError
		if err == nil || !errors.As(err, &te) || te.Code != http.StatusTooManyRequests || attempt >= 2 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(max(te.RetryAfter, 1)) * time.Second):
		}
	}
}

func (t *Telegram) Run(ctx context.Context, h Handler) error {
	t.setState(StateStarting, "")
	defer func() {
		if st := t.Status(); st.State != StateError || ctx.Err() != nil {
			t.setState(StateStopped, "")
		}
	}()
	for {
		var me tgUser
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := t.call(cctx, "getMe", map[string]any{}, &me)
		cancel()
		if err == nil {
			t.botID, t.botUser = me.ID, me.Username
			t.setIdentity("@" + me.Username)
			t.setState(StateOnline, "")
			t.log.Info("Telegram 机器人已就绪", "username", "@"+me.Username)
			break
		}
		var te *tgError
		if errors.As(err, &te) && te.Code == http.StatusUnauthorized {
			t.setState(StateError, "token 无效")
			return fmt.Errorf("telegram token 无效：%w", err)
		}
		t.setState(StateError, err.Error())
		t.log.Warn("连接 Telegram 失败，5 秒后重试", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}

	commands := []map[string]string{
		{"command": "history", "description": "查看当前会话的聊天记录"},
		{"command": "sync", "description": "同步其他窗口的聊天记录"},
		{"command": "session", "description": "查看和切换会话"},
		{"command": "stop", "description": "停止当前任务"},
		{"command": "undo", "description": "撤回最近一条消息"},
		{"command": "help", "description": "查看全部命令"},
	}
	if err := t.callRetry(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); err != nil {
		t.log.Warn("设置 Telegram 命令菜单失败", "err", err)
	}
	var offset int64
	backoff := time.Second
	webhookCleared := false
	for ctx.Err() == nil {
		var ups []tgUpdate
		cctx, cancel := context.WithTimeout(ctx, 70*time.Second)
		err := t.call(cctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
		}, &ups)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var te *tgError
			if errors.As(err, &te) && te.Code == http.StatusConflict && strings.Contains(te.Desc, "webhook") && !webhookCleared {
				t.log.Warn("这个机器人设置了 webhook，与长轮询冲突，正在删除 webhook")
				webhookCleared = true
				t.callRetry(ctx, "deleteWebhook", map[string]any{}, nil)
				continue
			}
			t.setState(StateError, err.Error())
			t.log.Warn("拉取 Telegram 消息失败", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		t.setState(StateOnline, "")
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				if m, ok := t.convert(u.Message); ok {
					m.Images = t.fetchImages(ctx, u.Message)
					h(t, m)
				}
			}
			if u.Callback != nil {
				t.onButton(ctx, u.Callback, h)
			}
		}
	}
	return nil
}

func (t *Telegram) convert(m *tgMessage) (Message, bool) {
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if (text == "" && !hasImage(m)) || m.From == nil || m.From.IsBot {
		return Message{}, false
	}
	group := m.Chat.Type == "group" || m.Chat.Type == "supergroup"
	if !group && m.Chat.Type != "private" {
		return Message{}, false
	}
	mentioned := !group || t.groupAll
	if t.botUser != "" {
		if s, ok := stripMention(text, t.botUser); ok {
			text, mentioned = s, true
		}
	}
	if m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == t.botID {
		mentioned = true
	}
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	chatName := m.Chat.Title
	if !group {
		chatName = name
	}
	return Message{
		ChatID:     strconv.FormatInt(m.Chat.ID, 10),
		SenderID:   strconv.FormatInt(m.From.ID, 10),
		SenderName: name,
		ChatName:   chatName,
		Text:       strings.TrimSpace(text),
		MsgID:      strconv.FormatInt(m.MessageID, 10),
		Group:      group,
		Mentioned:  mentioned,
	}, true
}

// hasImage 判断这条消息是不是带了图片（照片，或者以文件发的图片）。
func hasImage(m *tgMessage) bool {
	if len(m.Photo) > 0 {
		return true
	}
	return m.Document != nil && strings.HasPrefix(m.Document.MimeType, "image/")
}

// fetchImages 把消息里的图片下载下来存进图片目录。失败只记日志，文字照常处理。
func (t *Telegram) fetchImages(ctx context.Context, m *tgMessage) []media.Attachment {
	if t.media == nil {
		return nil
	}
	type job struct{ id, name string }
	var jobs []job
	if len(m.Photo) > 0 {
		p := m.Photo[len(m.Photo)-1] // 最大的一张
		jobs = append(jobs, job{p.FileID, fmt.Sprintf("telegram-%d.jpg", m.MessageID)})
	}
	if d := m.Document; d != nil && strings.HasPrefix(d.MimeType, "image/") {
		jobs = append(jobs, job{d.FileID, d.FileName})
	}
	var out []media.Attachment
	for _, j := range jobs {
		if len(out) >= media.MaxPerMessage {
			break
		}
		a, err := t.download(ctx, j.id, j.name)
		if err != nil {
			t.log.Warn("下载 Telegram 图片失败", "err", err)
			continue
		}
		out = append(out, a)
	}
	return out
}

// download 用 getFile 拿到文件路径，再从文件接口下载一张图片。
func (t *Telegram) download(ctx context.Context, fileID, name string) (media.Attachment, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := t.call(cctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return media.Attachment{}, err
	}
	if f.FilePath == "" {
		return media.Attachment{}, errors.New("没有拿到文件路径")
	}
	if f.FileSize > media.MaxImageBytes {
		return media.Attachment{}, fmt.Errorf("图片太大（%d 字节）", f.FileSize)
	}
	u := t.base + "/file/bot" + t.token + "/" + f.FilePath
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return media.Attachment{}, t.redact(err)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return media.Attachment{}, t.redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Attachment{}, fmt.Errorf("下载图片失败：HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return media.Attachment{}, err
	}
	return t.media.Save(data, name, "telegram")
}

// onButton：点了消息下面的按钮（停止、批准、拒绝）。先回应按钮（不然客户端一直转圈），再交给网关。
func (t *Telegram) onButton(ctx context.Context, q *tgCallbackQuery, h Handler) {
	action, text, toast := "", "", ""
	switch {
	case q.Data == tgStopData:
		action, toast = ActionStop, "正在停止…"
	case strings.HasPrefix(q.Data, tgApprovePref):
		n, ok, _ := strings.Cut(strings.TrimPrefix(q.Data, tgApprovePref), ":")
		action, text, toast = ActionDeny, n, "已拒绝"
		if ok == "1" {
			action, toast = ActionApprove, "已批准"
		}
	}
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		t.call(cctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID, "text": toast}, nil)
	}()
	if action == "" || q.Message == nil || q.From.IsBot || q.Message.Chat.Type != "private" {
		return
	}
	name := strings.TrimSpace(q.From.FirstName + " " + q.From.LastName)
	h(t, Message{
		ChatID:     strconv.FormatInt(q.Message.Chat.ID, 10),
		SenderID:   strconv.FormatInt(q.From.ID, 10),
		SenderName: name,
		ChatName:   name,
		Text:       text,
		Mentioned:  true,
		Action:     action,
	})
}

// stripMention 去掉文本里所有 @username（不区分大小写，且后面不能紧跟用户名字符）。
func stripMention(text, user string) (string, bool) {
	var b strings.Builder
	found := false
	for i := 0; i < len(text); {
		end := i + 1 + len(user)
		if text[i] == '@' && end <= len(text) && strings.EqualFold(text[i+1:end], user) &&
			(end == len(text) || !isNameByte(text[end])) {
			found = true
			i = end
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String(), found
}

func isNameByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (t *Telegram) Send(ctx context.Context, chatID, text, replyTo string) error {
	for i, part := range Split(text, 4000) {
		p := map[string]any{"chat_id": chatID, "text": part}
		if i == 0 && replyTo != "" {
			if id, err := strconv.ParseInt(replyTo, 10, 64); err == nil {
				p["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
			}
		}
		if err := t.callRetry(ctx, "sendMessage", p, nil); err != nil {
			return err
		}
	}
	return nil
}

func (t *Telegram) Typing(ctx context.Context, chatID string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return t.call(cctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}
