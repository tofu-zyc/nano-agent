package app

import (
	"slices"

	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/session"
)

// 图片输入：用户发来的图片存在 data/uploads，会话历史里只留引用。请求模型时，如果这一轮的模型
// 能接收图片（modalities 里有 image），就把图片读出来做成 data URL（见 session.BuildMessages），
// 否则只发一个 [图片×N] 的文字说明——图片仍然留在历史里，换成支持图片的模型再聊还能看到。
// 图片不做压缩、不改写旧消息（前缀缓存），长期对话的图片会一直占 token，压缩摘要时只留文字说明。

// SetMedia 接上图片存储（网页上传、Telegram、飞书下载的图片都放这里）。
func (a *App) SetMedia(m *media.Store) {
	a.mu.Lock()
	a.media = m
	a.mu.Unlock()
}

// MediaStore 是图片存储（创建聊天渠道时用）。
func (a *App) MediaStore() *media.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.media
}

// modelSupportsImages 是这个会话现在用的模型能不能接收图片。
func (a *App) modelSupportsImages(sid string) bool {
	if m, _, ok := a.modelOf(sid); ok {
		return slices.Contains(m.Modalities, "image")
	}
	return false
}

// imageLoader 把历史里的图片读成 data URL；没有图片存储时返回 nil（只发文字说明）。
func (a *App) imageLoader() session.ImageLoader {
	store := a.media
	if store == nil {
		return nil
	}
	return func(att media.Attachment) (string, bool) {
		url, err := store.DataURL(att)
		if err != nil {
			a.log.Warn("读不了图片", "id", att.ID, "err", err)
			return "", false
		}
		return url, true
	}
}
