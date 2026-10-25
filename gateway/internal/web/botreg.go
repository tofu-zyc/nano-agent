package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
)

// 扫码绑定 QQ、微信机器人：开始后立即返回二维码，后台等用户扫码确认；拿到凭据就加一个机器人（重新扫码时
// 更新那个机器人），并把扫码的人设成管理员。进展通过事件 bot_reg 推给网页，kind 是：
// scanned（扫了，等手机上确认）、verify / verify_wrong（要输手机上显示的数字）、
// refreshed（二维码过期换了新的，data.qr_svg）、done（data.id 是机器人 ID）、error。

type botRegRun struct {
	reg    channel.Registration
	cancel context.CancelFunc
}

type botRegReq struct {
	Kind  string `json:"kind"`  // qq | weixin
	ID    string `json:"id"`    // 重新扫码：已有的机器人
	Name  string `json:"name"`  // 新建时的名字
	Agent string `json:"agent"` // 新建时绑给谁
	Proxy string `json:"proxy"`
}

func (s *Server) botRegister(w http.ResponseWriter, r *http.Request) {
	var p botRegReq
	if !readJSON(w, r, &p) {
		return
	}
	cfg := s.conf.Current()
	proxy := cfg.ProxyFor(strings.TrimSpace(p.Proxy))
	var reg channel.Registration
	var err error
	switch p.Kind {
	case agents.KindQQ:
		reg, err = channel.BeginQQRegistration(r.Context(), proxy)
	case agents.KindWeixin:
		reg, err = channel.BeginWeixinRegistration(r.Context(), proxy, s.app.WeixinTokens(p.ID))
	default:
		writeErr(w, http.StatusBadRequest, "只有 QQ 和微信机器人能这样扫码（飞书在飞书节点里扫码）")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	svg, err := channel.QRCodeSVG(reg.QRURL())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.botReg != nil {
		s.botReg.cancel() // 同一时间只保留最新的一次
	}
	run := &botRegRun{reg: reg, cancel: cancel}
	s.botReg = run
	s.mu.Unlock()

	bus := s.app.Bus()
	go func() {
		defer cancel()
		defer func() {
			s.mu.Lock()
			if s.botReg == run {
				s.botReg = nil
			}
			s.mu.Unlock()
		}()
		creds, err := reg.Wait(ctx, func(stage, url string) {
			ev := events.Event{T: "bot_reg", Kind: stage}
			if url != "" {
				if svg, err := channel.QRCodeSVG(url); err == nil {
					ev.Data = map[string]any{"qr_svg": svg, "url": url}
				}
			}
			bus.Publish(ev)
		})
		if errors.Is(err, context.Canceled) {
			return
		}
		var view app.BotView
		if err == nil {
			view, err = s.saveScannedBot(p, creds)
		}
		if err != nil {
			s.log.Warn("扫码绑定机器人失败", "kind", p.Kind, "err", err)
			bus.Publish(events.Event{T: "bot_reg", Kind: "error", Text: err.Error()})
			return
		}
		s.log.Info("扫码绑定了机器人", "kind", p.Kind, "id", view.ID)
		bus.Publish(events.Event{T: "bot_reg", Kind: "done", Text: view.Name, Data: map[string]any{"id": view.ID}})
	}()
	writeJSON(w, map[string]any{"url": reg.QRURL(), "qr_svg": svg})
}

// saveScannedBot 把扫码拿到的凭据存成机器人：重新扫码（或者扫的是已经连过的同一个微信）就更新它，
// 否则新加一个。扫码的人设成管理员。
func (s *Server) saveScannedBot(p botRegReq, c *channel.BotCredentials) (app.BotView, error) {
	patch := app.BotPatch{Owner: &c.Owner}
	switch p.Kind {
	case agents.KindQQ:
		patch.AppID, patch.AppSecret = &c.AppID, &c.AppSecret
	case agents.KindWeixin:
		patch.Token, patch.APIBase, patch.BotID = &c.Token, &c.BaseURL, &c.BotID
	}
	id := p.ID
	if id == "" && p.Kind == agents.KindWeixin {
		id = s.app.BotByWeixinID(c.BotID)
	}
	var view app.BotView
	var err error
	if id != "" {
		view, err = s.app.UpdateBot(id, patch)
	} else {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = map[string]string{agents.KindQQ: "QQ 机器人", agents.KindWeixin: "微信"}[p.Kind]
		}
		agent := p.Agent
		if agent == "" {
			agent = agents.DefaultID
		}
		proxy := strings.TrimSpace(p.Proxy)
		patch.Name, patch.Agent, patch.Proxy = &name, &agent, &proxy
		view, err = s.app.CreateBot(p.Kind, patch)
	}
	if err != nil {
		return view, err
	}
	if view.Owner != "" {
		if err := s.addAdmin(view.Owner); err != nil {
			s.log.Warn("把扫码的人设成管理员失败", "err", err)
		}
	}
	return view, nil
}

// addAdmin 把一个身份写进 access.admins（已经在了就不动）。
func (s *Server) addAdmin(who string) error {
	admins := s.conf.Current().Access.Admins
	if slices.Contains(admins, who) {
		return nil
	}
	list := []any{}
	for _, a := range admins {
		list = append(list, a)
	}
	if err := s.conf.Set(map[string]any{"access.admins": append(list, who)}); err != nil {
		return err
	}
	s.app.Bus().Publish(events.Event{T: "config"})
	return nil
}

// botRegisterVerify 交上手机微信上显示的数字。
func (s *Server) botRegisterVerify(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &p) {
		return
	}
	s.mu.Lock()
	run := s.botReg
	s.mu.Unlock()
	if run == nil {
		writeErr(w, http.StatusBadRequest, "没有进行中的扫码")
		return
	}
	run.reg.Verify(p.Code)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) botRegisterCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.botReg != nil {
		s.botReg.cancel()
		s.botReg = nil
	}
	s.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}
