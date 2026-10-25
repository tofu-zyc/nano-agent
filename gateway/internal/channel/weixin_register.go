package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/httpx"
)

// 扫码连接微信 ClawBot：
//  1. POST ilink/bot/get_bot_qrcode?bot_type=3 拿二维码（local_token_list 带上已经连过的 bot_token，
//     服务端据此认出“这个微信已经连过了”）；
//  2. GET ilink/bot/get_qrcode_status 长轮询：wait → scaned → confirmed（拿到 bot_token）；
//     中间可能要输入手机上显示的数字（need_verifycode）、换机房地址（scaned_but_redirect）、二维码过期（expired，换一个）。

const (
	wxBotType      = "3"
	wxMaxRefresh   = 3
	wxStatusPoll   = 35 * time.Second
	wxLoginTimeout = 8 * time.Minute
)

// WeixinLoginBase 是拿二维码的地址（测试时换成假服务）
var WeixinLoginBase = wxAPI

type wxRegistration struct {
	hc     *http.Client
	known  []string
	qrcode string
	url    string
	base   string // 轮询状态用的地址（可能被换到别的机房）

	mu   sync.Mutex
	code string // 用户输的验证数字，下次轮询带上
	got  chan struct{}
}

// BeginWeixinRegistration 开始一次扫码。known 是已经连过的机器人的 bot_token。
func BeginWeixinRegistration(ctx context.Context, proxy string, known []string) (Registration, error) {
	hc, err := httpx.NewClient(proxy, 0)
	if err != nil {
		return nil, err
	}
	if known == nil {
		known = []string{}
	}
	r := &wxRegistration{hc: hc, known: known, base: WeixinLoginBase, got: make(chan struct{}, 1)}
	if err := r.newQR(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *wxRegistration) QRURL() string { return r.url }

func (r *wxRegistration) Verify(code string) {
	r.mu.Lock()
	r.code = strings.TrimSpace(code)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *wxRegistration) newQR(ctx context.Context) error {
	var resp struct {
		QRCode string `json:"qrcode"`
		Img    string `json:"qrcode_img_content"`
	}
	err := wxCall(ctx, r.hc, WeixinLoginBase, "ilink/bot/get_bot_qrcode?bot_type="+wxBotType, "",
		map[string]any{"local_token_list": r.known}, &resp, 20*time.Second)
	if err != nil {
		return fmt.Errorf("向微信要二维码：%w", err)
	}
	if resp.QRCode == "" || resp.Img == "" {
		return errors.New("微信没有给二维码")
	}
	r.qrcode, r.url, r.base = resp.QRCode, resp.Img, WeixinLoginBase
	return nil
}

type wxQRStatus struct {
	Status   string `json:"status"`
	Token    string `json:"bot_token"`
	BotID    string `json:"ilink_bot_id"`
	BaseURL  string `json:"baseurl"`
	UserID   string `json:"ilink_user_id"`
	Redirect string `json:"redirect_host"`
}

func (r *wxRegistration) status(ctx context.Context, code string) (wxQRStatus, error) {
	q := "ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(r.qrcode)
	if code != "" {
		q += "&verify_code=" + url.QueryEscape(code)
	}
	cctx, cancel := context.WithTimeout(ctx, wxStatusPoll+5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, r.base+"/"+q, nil)
	if err != nil {
		return wxQRStatus{}, err
	}
	req.Header.Set("iLink-App-Id", wxAppID)
	req.Header.Set("iLink-App-ClientVersion", wxClientVersion())
	resp, err := r.hc.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			return wxQRStatus{Status: "wait"}, nil // 长轮询超时或者网络抖一下：当作还在等
		}
		return wxQRStatus{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var st wxQRStatus
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &st) != nil {
		return wxQRStatus{Status: "wait"}, nil
	}
	return st, nil
}

func (r *wxRegistration) Wait(ctx context.Context, progress func(stage, url string)) (*BotCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, wxLoginTimeout)
	defer cancel()
	refreshed := 0
	scanned := false
	for {
		r.mu.Lock()
		code := r.code
		r.mu.Unlock()
		st, err := r.status(ctx, code)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errors.New("等太久了，请重新生成二维码")
			}
			return nil, err
		}
		switch st.Status {
		case "scaned":
			r.mu.Lock()
			r.code = "" // 带着数字问到了 scaned：数字对了
			r.mu.Unlock()
			if !scanned {
				scanned = true
				progress(RegScanned, "")
			}
		case "need_verifycode":
			stage := RegVerify
			if code != "" {
				stage = RegVerifyWrong
			}
			progress(stage, "")
			select {
			case <-ctx.Done():
				return nil, errors.New("等太久了，请重新生成二维码")
			case <-r.got:
			}
			continue
		case "scaned_but_redirect":
			if st.Redirect != "" {
				r.base = "https://" + st.Redirect
			}
		case "expired", "verify_code_blocked":
			if refreshed >= wxMaxRefresh {
				if st.Status == "verify_code_blocked" {
					return nil, errors.New("数字输错太多次了，过一会儿再试")
				}
				return nil, errors.New("二维码过期了，请重新生成")
			}
			refreshed++
			scanned = false
			r.mu.Lock()
			r.code = ""
			r.mu.Unlock()
			if err := r.newQR(ctx); err != nil {
				return nil, err
			}
			progress(RegRefreshed, r.url)
		case "binded_redirect":
			return nil, errors.New("这个微信已经连过这个网关了（在机器人列表里），不用重复连")
		case "confirmed":
			if st.Token == "" || st.BotID == "" {
				return nil, errors.New("微信没有返回机器人的凭据")
			}
			return &BotCredentials{Token: st.Token, BaseURL: st.BaseURL, BotID: st.BotID, Owner: st.UserID}, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("等太久了，请重新生成二维码")
		case <-time.After(time.Second):
		}
	}
}
