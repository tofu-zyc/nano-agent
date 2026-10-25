package channel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/media"
)

// Weixin 是微信官方的 ClawBot（iLink 接口，扫码登录，见 weixin_register.go）：长轮询收消息、HTTP 发消息，
// 不需要公网地址。只能和扫码的那个人私聊，不能进群，只能发纯文字（Markdown 会先转成干净的文字）。
//
// 回复要带上对方消息里的 context_token；主动找人时用最近一次的。收消息的游标和每个人的 context_token
// 存在 StateFile 里，网关重启后接着收、还能主动发。
//
// 协议见 https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md

const (
	wxAPI     = "https://ilinkai.weixin.qq.com"
	wxCDN     = "https://novac2c.cdn.weixin.qq.com/c2c"
	wxVersion = "2.4.10" // 协议跟着官方插件的这个版本
	wxAppID   = "bot"
	wxAgent   = "nano-agent/1.0"

	wxStaleToken = -14 // 登录失效：要重新扫码
	wxTextLimit  = 2000
)

// WeixinConfig 是一个微信机器人的凭据。
type WeixinConfig struct {
	Token     string // bot_token
	BaseURL   string // 扫码时返回的接口地址，空用默认
	CDN       string // 图片的 CDN 地址，空用默认（测试时换成假服务）
	Proxy     string
	StateFile string // 存游标和 context_token
}

type Weixin struct {
	statusBox
	name  string
	c     WeixinConfig
	hc    *http.Client
	log   *slog.Logger
	media *media.Store

	mu     sync.Mutex
	st     wxState
	ticket map[string]wxTicket // 对方 → “正在输入”要用的 typing_ticket
}

type wxState struct {
	Cursor   string            `json:"cursor"`
	Contexts map[string]string `json:"contexts"` // 对方 → 最近的 context_token
}

type wxTicket struct {
	t  string
	at time.Time
}

func NewWeixin(c WeixinConfig, store *media.Store, log *slog.Logger) (*Weixin, error) {
	hc, err := httpx.NewClient(c.Proxy, 0)
	if err != nil {
		return nil, fmt.Errorf("微信代理：%w", err)
	}
	if c.BaseURL == "" {
		c.BaseURL = wxAPI
	}
	if c.CDN == "" {
		c.CDN = wxCDN
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	w := &Weixin{name: "wx", c: c, hc: hc, log: log.With("channel", "wx"), media: store, ticket: map[string]wxTicket{}}
	w.st.Contexts = map[string]string{}
	if c.StateFile != "" {
		if b, err := os.ReadFile(c.StateFile); err == nil {
			_ = json.Unmarshal(b, &w.st)
			if w.st.Contexts == nil {
				w.st.Contexts = map[string]string{}
			}
		}
	}
	return w, nil
}

func (w *Weixin) Name() string { return w.name }

// SetName 是渠道名 wx.<机器人 ID>。
func (w *Weixin) SetName(name string) {
	if name != "" && name != w.name {
		w.name = name
		w.log = w.log.With("channel", name)
	}
}

// saveState 写游标和 context_token。调用方持有 w.mu。
func (w *Weixin) saveState() {
	if w.c.StateFile == "" {
		return
	}
	b, _ := json.Marshal(w.st)
	if err := os.MkdirAll(filepath.Dir(w.c.StateFile), 0o700); err != nil {
		return
	}
	tmp := w.c.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, w.c.StateFile)
	}
}

// ---- 接口 ----

func wxClientVersion() string {
	var n [3]int
	for i, p := range strings.SplitN(wxVersion, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return strconv.Itoa((n[0]&0xff)<<16 | (n[1]&0xff)<<8 | n[2]&0xff)
}

func wxUIN() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)))
}

func wxBaseInfo() map[string]string {
	return map[string]string{"channel_version": wxVersion, "bot_agent": wxAgent}
}

// wxCall 调一个接口（POST JSON）。token 为空时不带鉴权（拿二维码）。
func wxCall(ctx context.Context, hc *http.Client, base, path, token string, in, out any, timeout time.Duration) error {
	b, _ := json.Marshal(in)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, base+"/"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", wxUIN())
	req.Header.Set("iLink-App-Id", wxAppID)
	req.Header.Set("iLink-App-ClientVersion", wxClientVersion())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("微信接口 %s：HTTP %d", path, resp.StatusCode)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (w *Weixin) call(ctx context.Context, path string, in map[string]any, out any, timeout time.Duration) error {
	in["base_info"] = wxBaseInfo()
	return wxCall(ctx, w.hc, w.c.BaseURL, path, w.c.Token, in, out, timeout)
}

type wxResult struct {
	Ret     int    `json:"ret"`
	Errcode int    `json:"errcode"`
	Errmsg  string `json:"errmsg"`
}

func (r wxResult) code() int {
	if r.Ret != 0 {
		return r.Ret
	}
	return r.Errcode
}

// ---- 收消息 ----

type wxCDNMedia struct {
	Param   string `json:"encrypt_query_param"`
	AESKey  string `json:"aes_key"`
	FullURL string `json:"full_url"`
}

type wxItem struct {
	Type int `json:"type"`
	Text *struct {
		Text string `json:"text"`
	} `json:"text_item"`
	Image *struct {
		Media  *wxCDNMedia `json:"media"`
		AESKey string      `json:"aeskey"` // 十六进制，优先用
		URL    string      `json:"url"`
	} `json:"image_item"`
	Voice *struct {
		Text string `json:"text"`
	} `json:"voice_item"`
	File *struct {
		Name string `json:"file_name"`
	} `json:"file_item"`
	Video *struct{} `json:"video_item"`
}

type wxMessage struct {
	MessageID    json.Number `json:"message_id"`
	FromUser     string      `json:"from_user_id"`
	MessageType  int         `json:"message_type"` // 1 用户 2 机器人
	ContextToken string      `json:"context_token"`
	GroupID      string      `json:"group_id"`
	Items        []wxItem    `json:"item_list"`
}

func (w *Weixin) Run(ctx context.Context, h Handler) error {
	w.setState(StateStarting, "")
	defer func() {
		if st := w.Status(); st.State != StateError || ctx.Err() != nil {
			w.setState(StateStopped, "")
		}
	}()
	var r wxResult
	if err := w.call(ctx, "ilink/bot/msg/notifystart", map[string]any{}, &r, 15*time.Second); err != nil {
		w.log.Warn("通知微信开始收消息失败", "err", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.call(cctx, "ilink/bot/msg/notifystop", map[string]any{}, nil, 5*time.Second)
	}()
	poll := 35 * time.Second
	fails := 0
	for ctx.Err() == nil {
		w.mu.Lock()
		cursor := w.st.Cursor
		w.mu.Unlock()
		var resp struct {
			wxResult
			Msgs    []wxMessage `json:"msgs"`
			Cursor  string      `json:"get_updates_buf"`
			Timeout int         `json:"longpolling_timeout_ms"`
		}
		err := w.call(ctx, "ilink/bot/getupdates", map[string]any{"get_updates_buf": cursor}, &resp, poll+10*time.Second)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && resp.code() == wxStaleToken {
			w.setState(StateError, "登录失效了，请在网页上重新扫码")
			w.log.Warn("微信登录失效，一小时后再试", "code", resp.code())
			if !sleepCtx(ctx, time.Hour) {
				return nil
			}
			continue
		}
		if err == nil && resp.code() != 0 {
			err = fmt.Errorf("收消息失败：%d %s", resp.code(), resp.Errmsg)
		}
		if err != nil {
			fails++
			w.setState(StateError, err.Error())
			wait := 2 * time.Second
			if fails >= 3 {
				wait = 30 * time.Second
			}
			w.log.Warn("微信收消息出错", "err", err, "wait", wait)
			if !sleepCtx(ctx, wait) {
				return nil
			}
			continue
		}
		fails = 0
		w.setState(StateOnline, "")
		if resp.Timeout > 0 {
			poll = time.Duration(resp.Timeout) * time.Millisecond
		}
		w.mu.Lock()
		if resp.Cursor != "" {
			w.st.Cursor = resp.Cursor
		}
		for _, m := range resp.Msgs {
			if m.MessageType == 1 && m.FromUser != "" && m.ContextToken != "" {
				w.st.Contexts[m.FromUser] = m.ContextToken
			}
		}
		w.saveState()
		w.mu.Unlock()
		for _, m := range resp.Msgs {
			if msg, ok := w.convert(ctx, m); ok {
				h(w, msg)
			}
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (w *Weixin) convert(ctx context.Context, m wxMessage) (Message, bool) {
	if m.MessageType != 1 || m.FromUser == "" || m.GroupID != "" {
		return Message{}, false
	}
	msg := Message{ChatID: m.FromUser, SenderID: m.FromUser, MsgID: m.MessageID.String(), Mentioned: true}
	var parts []string
	for _, it := range m.Items {
		switch {
		case it.Text != nil:
			parts = append(parts, it.Text.Text)
		case it.Image != nil:
			if img, err := w.image(ctx, it); err == nil {
				msg.Images = append(msg.Images, img)
			} else {
				w.log.Warn("下载图片失败", "err", err)
				parts = append(parts, "[图片没收到："+err.Error()+"]")
			}
		case it.Voice != nil:
			if it.Voice.Text != "" {
				parts = append(parts, "[语音] "+it.Voice.Text)
			} else {
				parts = append(parts, "[一段语音，没有转出文字]")
			}
		case it.File != nil:
			parts = append(parts, "[文件："+it.File.Name+"]")
		case it.Video != nil:
			parts = append(parts, "[一段视频]")
		}
	}
	msg.Text = strings.TrimSpace(strings.Join(parts, "\n"))
	if msg.Text == "" && len(msg.Images) == 0 {
		return Message{}, false
	}
	return msg, true
}

// image 下载一张图片：CDN 上的是 AES-128-ECB 加密的。
func (w *Weixin) image(ctx context.Context, it wxItem) (media.Attachment, error) {
	if w.media == nil {
		return media.Attachment{}, errors.New("没有地方存图片")
	}
	img := it.Image
	m := img.Media
	if m == nil {
		m = &wxCDNMedia{}
	}
	u := m.FullURL
	if u == "" && m.Param != "" {
		u = w.c.CDN + "/download?encrypted_query_param=" + url.QueryEscape(m.Param)
	}
	if u == "" {
		u = img.URL
	}
	if u == "" {
		return media.Attachment{}, errors.New("没有下载地址")
	}
	var key []byte
	if img.AESKey != "" {
		key, _ = hex.DecodeString(img.AESKey)
	} else if m.AESKey != "" {
		key = wxParseKey(m.AESKey)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return media.Attachment{}, err
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return media.Attachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return media.Attachment{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+64))
	if err != nil {
		return media.Attachment{}, err
	}
	if len(key) == 16 {
		if data, err = aesECBDecrypt(data, key); err != nil {
			return media.Attachment{}, err
		}
	}
	return w.media.Save(data, "image", "weixin")
}

// wxParseKey：aes_key 是 base64 的 16 字节密钥，或者 base64 的 32 位十六进制字符串。
func wxParseKey(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	if len(b) == 32 {
		if k, err := hex.DecodeString(string(b)); err == nil {
			return k
		}
	}
	return b
}

func aesECBDecrypt(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("密文长度不对")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("填充不对")
	}
	return out[:len(out)-pad], nil
}

// ---- 发消息 ----

func (w *Weixin) Send(ctx context.Context, chatID, text, _ string) error {
	for _, part := range Split(text, wxTextLimit) {
		if err := w.sendItem(ctx, chatID, map[string]any{"type": 1, "text_item": map[string]string{"text": part}}); err != nil {
			return err
		}
	}
	return nil
}

// sendItem 发一条消息（一个 item），带上和对方最近的 context_token。
func (w *Weixin) sendItem(ctx context.Context, to string, item map[string]any) error {
	w.mu.Lock()
	ctxTok := w.st.Contexts[to]
	w.mu.Unlock()
	var cid [4]byte
	_, _ = rand.Read(cid[:])
	var r wxResult
	err := w.call(ctx, "ilink/bot/sendmessage", map[string]any{"msg": map[string]any{
		"from_user_id": "", "to_user_id": to,
		"client_id":    fmt.Sprintf("nano-agent:%d-%x", time.Now().UnixMilli(), cid),
		"message_type": 2, "message_state": 2, "context_token": ctxTok,
		"item_list": []map[string]any{item},
	}}, &r, 30*time.Second)
	if err == nil && r.code() != 0 {
		err = fmt.Errorf("微信发送失败：%d %s", r.code(), r.Errmsg)
	}
	return err
}

// SendMedia 发文件：AES-128-ECB 加密后传到微信的 CDN（getuploadurl 拿上传地址，响应头 x-encrypted-param
// 是下载参数），再发图片 / 视频 / 文件消息。语音官方插件也没做，按文件发。
func (w *Weixin) SendMedia(ctx context.Context, chatID string, att media.Attachment, data []byte) error {
	mediaType, kind := 3, media.Kind(att.MIME)
	switch kind {
	case "image":
		mediaType = 1
	case "video":
		mediaType = 2
	}
	key := make([]byte, 16)
	fk := make([]byte, 16)
	_, _ = rand.Read(key)
	_, _ = rand.Read(fk)
	hexKey, filekey := hex.EncodeToString(key), hex.EncodeToString(fk)
	enc := aesECBEncrypt(data, key)
	sum := md5.Sum(data)
	var up struct {
		wxResult
		Param   string `json:"upload_param"`
		FullURL string `json:"upload_full_url"`
	}
	if err := w.call(ctx, "ilink/bot/getuploadurl", map[string]any{"filekey": filekey, "media_type": mediaType, "to_user_id": chatID,
		"rawsize": len(data), "rawfilemd5": hex.EncodeToString(sum[:]), "filesize": len(enc), "no_need_thumb": true, "aeskey": hexKey},
		&up, 15*time.Second); err != nil {
		return err
	}
	u := strings.TrimSpace(up.FullURL)
	if u == "" {
		if up.Param == "" {
			return fmt.Errorf("微信没有给上传地址：%d %s", up.code(), up.Errmsg)
		}
		u = w.c.CDN + "/upload?encrypted_query_param=" + url.QueryEscape(up.Param) + "&filekey=" + url.QueryEscape(filekey)
	}
	param, err := w.uploadCDN(ctx, u, enc)
	if err != nil {
		return err
	}
	ref := map[string]any{"encrypt_query_param": param, "aes_key": base64.StdEncoding.EncodeToString([]byte(hexKey)), "encrypt_type": 1}
	switch kind {
	case "image":
		return w.sendItem(ctx, chatID, map[string]any{"type": 2, "image_item": map[string]any{"media": ref, "mid_size": len(enc)}})
	case "video":
		return w.sendItem(ctx, chatID, map[string]any{"type": 5, "video_item": map[string]any{"media": ref, "video_size": len(enc)}})
	}
	name := att.Name
	if name == "" {
		name = "file"
	}
	return w.sendItem(ctx, chatID, map[string]any{"type": 4, "file_item": map[string]any{"media": ref, "file_name": name, "len": strconv.Itoa(len(data))}})
}

// uploadCDN 把密文 POST 到 CDN，返回响应头里的下载参数。4xx 不重试，其他失败最多试三次。
func (w *Weixin) uploadCDN(ctx context.Context, u string, enc []byte) (string, error) {
	var last error
	for i := 0; i < 3; i++ {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, u, bytes.NewReader(enc))
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := w.hc.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			param := resp.Header.Get("x-encrypted-param")
			switch {
			case resp.StatusCode == http.StatusOK && param != "":
				cancel()
				return param, nil
			case resp.StatusCode/100 == 4:
				cancel()
				return "", fmt.Errorf("上传到微信 CDN 失败：HTTP %d %s", resp.StatusCode, resp.Header.Get("x-error-message"))
			}
			err = fmt.Errorf("上传到微信 CDN 失败：HTTP %d", resp.StatusCode)
		}
		cancel()
		last = err
	}
	return "", last
}

func aesECBEncrypt(data, key []byte) []byte {
	block, _ := aes.NewCipher(key)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	buf := make([]byte, len(data)+pad)
	copy(buf, data)
	for i := len(data); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	for i := 0; i < len(buf); i += aes.BlockSize {
		block.Encrypt(buf[i:i+aes.BlockSize], buf[i:i+aes.BlockSize])
	}
	return buf
}

// SendMarkdown：微信只能显示纯文字，先把 Markdown 转成干净的文字。
func (w *Weixin) SendMarkdown(ctx context.Context, chatID, text, replyTo string) error {
	return w.Send(ctx, chatID, PlainText(text), replyTo)
}

var (
	mdImage  = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdStrong = regexp.MustCompile(`(\*\*\*|\*\*|__)(\S(?:.*?\S)?)(\*\*\*|\*\*|__)`)
	mdCodeIn = regexp.MustCompile("`([^`\n]+)`")
	mdHead   = regexp.MustCompile(`^#{1,6}\s+`)
	mdQuote  = regexp.MustCompile(`^>\s?`)
	mdBullet = regexp.MustCompile(`^(\s*)[*+]\s+`)
)

// PlainText 把 Markdown 转成适合纯文字聊天的样子：去掉加粗、标题、代码块的围栏，链接写成“文字（地址）”。
func PlainText(md string) string {
	lines := strings.Split(md, "\n")
	fence := false
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			continue
		}
		if !fence {
			l = mdHead.ReplaceAllString(l, "")
			l = mdQuote.ReplaceAllString(l, "")
			l = mdBullet.ReplaceAllString(l, "$1- ")
			l = mdImage.ReplaceAllString(l, "$1")
			l = mdLink.ReplaceAllString(l, "$1（$2）")
			l = mdStrong.ReplaceAllString(l, "$2")
			l = mdCodeIn.ReplaceAllString(l, "$1")
			if strings.Trim(l, "-*_ ") == "" && strings.TrimSpace(l) != "" {
				l = "" // 分隔线
			}
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ---- “对方正在输入” ----

// typingTicket 拿“正在输入”要用的票（半小时内用缓存的）。
func (w *Weixin) typingTicket(ctx context.Context, user string) string {
	w.mu.Lock()
	t, ok := w.ticket[user]
	ctxTok := w.st.Contexts[user]
	w.mu.Unlock()
	if ok && time.Since(t.at) < 30*time.Minute {
		return t.t
	}
	var r struct {
		wxResult
		Ticket string `json:"typing_ticket"`
	}
	if err := w.call(ctx, "ilink/bot/getconfig", map[string]any{"ilink_user_id": user, "context_token": ctxTok}, &r, 10*time.Second); err != nil || r.code() != 0 || r.Ticket == "" {
		return ""
	}
	w.mu.Lock()
	w.ticket[user] = wxTicket{t: r.Ticket, at: time.Now()}
	w.mu.Unlock()
	return r.Ticket
}

// Busy 在处理消息的时候显示“对方正在输入”（每 5 秒续一次），处理完取消。
func (w *Weixin) Busy(chatID, _ string) func() {
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ticket := w.typingTicket(ctx, chatID)
		if ticket == "" {
			return
		}
		send := func(status int) {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = w.call(cctx, "ilink/bot/sendtyping", map[string]any{"ilink_user_id": chatID, "typing_ticket": ticket, "status": status}, nil, 10*time.Second)
		}
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			send(1)
			select {
			case <-done:
				send(2)
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
