package channel

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// Parter 按渠道实际的发法，把一条消息切成要逐段发送的段：plain 为 true 时每段用 Send 发，
// 否则用 SendMarkdown 发。
// 实现必须保证：逐段发送和整条调用 Send/SendMarkdown，平台收到的内容和顺序完全一样。
// 没实现的渠道不切段，出站整条重试。
type Parter interface {
	Parts(text string, markdown bool) (parts []string, plain bool)
}

// ErrGone 表示目标永远发不到（机器人被拉黑或踢出、会话不存在）。发送失败的错误满足
// errors.Is(err, ErrGone) 时，出站不再重试。拿不准的错误不要标成 ErrGone。
var ErrGone = errors.New("目标永久不可达")

// RetryDelayer 是限流错误实现的接口：平台要求等多久之后再发。
type RetryDelayer interface{ RetryDelay() time.Duration }

// RetryAfter 取出错误里平台要求的等待时间；没有（不是限流）时 ok 为 false。
func RetryAfter(err error) (d time.Duration, ok bool) {
	var r RetryDelayer
	if errors.As(err, &r) {
		if d = r.RetryDelay(); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// Telegram 没有 Markdown 发法：都按 4000 字切，用 Send 发。
const tgTextLimit = 4000

func (t *Telegram) Parts(text string, _ bool) ([]string, bool) {
	return Split(text, tgTextLimit), true
}

// RetryDelay 是 429 时 Bot API 要求的等待时间。
func (e *tgError) RetryDelay() time.Duration {
	if e.Code != http.StatusTooManyRequests {
		return 0
	}
	return time.Duration(max(e.RetryAfter, 1)) * time.Second
}

// Is 让 403（机器人被拉黑或踢出）和 400 chat not found（会话不存在）满足 errors.Is(err, ErrGone)。
func (e *tgError) Is(target error) bool {
	if target != ErrGone {
		return false
	}
	return e.Code == http.StatusForbidden ||
		e.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Desc), "chat not found")
}

// 飞书：Markdown 走富文本消息（6000 字一段，见 feishuPostLimit），纯文字 8000 字一段。
const feishuTextLimit = 8000

func (f *Feishu) Parts(text string, markdown bool) ([]string, bool) {
	if markdown {
		return Split(text, feishuPostLimit), false
	}
	return Split(text, feishuTextLimit), true
}

// QQ：纯文字和 Markdown 都按 4000 字切。
func (q *QQ) Parts(text string, markdown bool) ([]string, bool) {
	return Split(text, qqTextLimit), !markdown
}

// 微信只能显示纯文字：Markdown 先转成文字，再按 2000 字切，每段用 Send 发。
func (w *Weixin) Parts(text string, markdown bool) ([]string, bool) {
	if markdown {
		text = PlainText(text)
	}
	return Split(text, wxTextLimit), true
}
