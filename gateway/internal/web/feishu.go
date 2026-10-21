package web

import (
	"context"
	"errors"
	"net/http"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
)

// 扫码创建飞书机器人：开始后立即返回二维码，后台等用户确认；拿到凭证就写进配置并启用飞书渠道。
// 进展通过事件 feishu_reg 推给网页：kind 是 done 或 error。

func (s *Server) feishuRegister(w http.ResponseWriter, r *http.Request) {
	cfg := s.conf.Current()
	reg, err := channel.BeginFeishuRegistration(r.Context(), cfg.ProxyFor(cfg.Feishu.Proxy),
		"{user}的 nano-agent", "由 nano-agent 扫码创建的个人助手")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	svg, err := channel.QRCodeSVG(reg.URL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.regCancel != nil {
		s.regCancel() // 同一时间只保留最新的一次
	}
	s.regCancel = cancel
	s.mu.Unlock()

	bus := s.app.Bus()
	go func() {
		defer cancel()
		res, err := reg.Wait(ctx)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err == nil {
			err = s.conf.Set(map[string]any{
				"feishu.app_id": res.AppID, "feishu.app_secret": res.AppSecret,
				"feishu.domain": res.Domain, "feishu.enabled": true,
			})
		}
		if err != nil {
			s.log.Warn("扫码创建飞书机器人失败", "err", err)
			bus.Publish(events.Event{T: "feishu_reg", Kind: "error", Text: err.Error()})
			return
		}
		s.log.Info("扫码创建了飞书机器人", "app_id", res.AppID)
		bus.Publish(events.Event{T: "feishu_reg", Kind: "done", Text: res.AppID})
		bus.Publish(events.Event{T: "config"})
	}()
	writeJSON(w, map[string]any{"url": reg.URL, "qr_svg": svg, "expire_in": reg.ExpireIn})
}

func (s *Server) feishuRegisterCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.regCancel != nil {
		s.regCancel()
		s.regCancel = nil
	}
	s.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}
