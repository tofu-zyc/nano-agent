package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/outbox"
)

// openOutbox 打开外部平台渠道的可靠出站，文件和 events.jsonl 在同一个目录。
// 打开失败（存储损坏）时用返回的那个拒绝受理的对象：消息退回内存直发，健康条目报 error，
// 出站有自己的健康条目，不再往存储问题里记，同一个故障只报一处。
func (a *App) openOutbox(ctx context.Context, dir string) {
	h, _ := a.health.Claim("outbox")
	ob, err := outbox.Open(ctx, filepath.Join(dir, "runtime", "outbox.jsonl"), outbox.Deps{
		Channel: func(name string) channel.Channel { // 渠道快照，不拿 a.mu
			if snap := a.chSnap.Load(); snap != nil {
				return (*snap)[name]
			}
			return nil
		},
		Media: func(id string) ([]byte, error) {
			m := a.mediaRef.Load()
			if m == nil {
				return nil, errors.New("没有媒体库")
			}
			return m.Read(id)
		},
		MediaNote: mediaNote,
		Log:       a.log,
		Health:    h,
	})
	if err != nil {
		a.log.Error("出站存储不可用，外部渠道的消息改为内存直发，原文件保留", "err", err)
	}
	a.outbox = ob
}

// OutboxInfo 是出站的统计和最近的失败、丢弃，放在 Status 里。
type OutboxInfo struct {
	outbox.Stats
	Recent []outbox.Record `json:"recent,omitempty"`
}

func (a *App) outboxInfo() OutboxInfo {
	if a.outbox == nil {
		return OutboxInfo{}
	}
	return OutboxInfo{Stats: a.outbox.Stats(), Recent: a.outbox.Recent(5)}
}

// Summary 是一行概括：有待发或有问题才有内容，没事返回空。
func (o OutboxInfo) Summary(now time.Time) string {
	pending, failing := 0, 0
	var oldest time.Time
	for _, c := range o.Channels {
		pending += c.Pending
		failing += c.Failing
		if !c.Oldest.IsZero() && (oldest.IsZero() || c.Oldest.Before(oldest)) {
			oldest = c.Oldest
		}
	}
	dropped := 0
	var reasons []string
	for r, n := range o.Dropped24h {
		dropped += n
		reasons = append(reasons, fmt.Sprintf("%s %d", r, n))
	}
	sort.Strings(reasons)
	if pending == 0 && dropped == 0 && o.Full24h == 0 && o.TooLarge24h == 0 && o.Storage == "" {
		return ""
	}
	var parts []string
	if pending > 0 {
		s := fmt.Sprintf("待发 %d 条", pending)
		if !oldest.IsZero() {
			s += "（最老的 " + shortDuration(now.Sub(oldest).Milliseconds()) + "前入队）"
		}
		parts = append(parts, s)
	}
	if failing > 0 {
		parts = append(parts, fmt.Sprintf("%d 条在重试", failing))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("24 小时内丢弃 %d 条（%s）", dropped, strings.Join(reasons, "、")))
	}
	if o.Full24h > 0 {
		parts = append(parts, fmt.Sprintf("队列满 %d 次", o.Full24h))
	}
	if o.TooLarge24h > 0 {
		parts = append(parts, fmt.Sprintf("超过单条上限 %d 次", o.TooLarge24h))
	}
	if o.Storage != "" {
		parts = append(parts, "存储不可用："+o.Storage)
	}
	return strings.Join(parts, "；")
}
