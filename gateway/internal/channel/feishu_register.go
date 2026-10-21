package channel

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"

	"nano-agent/gateway/internal/httpx"
)

// 扫码创建飞书机器人：飞书开放平台的应用注册接口，走 OAuth 设备授权流程（RFC 8628），
// 和飞书官方 SDK 里的 registration.RegisterApp 是同一套接口。用户用飞书扫码、确认之后，
// 平台自动创建一个机器人应用，并返回 App ID 和 App Secret。
//
// 自己实现而不直接调 SDK：要走网关配置的代理，也不能往标准输出打印东西（命令行聊天在用它）。

const regPath = "/oauth/v1/app/registration"

var regDomains = map[string]string{"feishu": "https://accounts.feishu.cn", "lark": "https://accounts.larksuite.com"}

// 机器人收发私聊消息需要的权限和事件，扫码确认页上会预先勾好
var regAddons = map[string]any{
	"scopes": map[string][]string{"tenant": {
		"im:message", "im:message:send_as_bot", "im:message.p2p_msg:readonly", "contact:user.base:readonly",
		"cardkit:card:write",
	}},
	"events": map[string]any{"items": map[string][]string{"tenant": {"im.message.receive_v1", "im.message.recalled_v1"}}},
}

// FeishuRegistration 是一次进行中的扫码注册。
type FeishuRegistration struct {
	URL      string // 用飞书扫这个链接的二维码（或者在浏览器里打开）
	ExpireIn int    // 秒

	hc         *http.Client
	domain     string
	deviceCode string
	interval   int
}

// FeishuRegistered 是注册成功后拿到的凭证。
type FeishuRegistered struct {
	AppID     string
	AppSecret string
	Domain    string // feishu 或 lark（扫码的是国际版账号时自动切到 lark）
}

// BeginFeishuRegistration 开始一次扫码注册。name 和 desc 预填在创建页上，用户可以改。
func BeginFeishuRegistration(ctx context.Context, proxy, name, desc string) (*FeishuRegistration, error) {
	hc, err := httpx.NewClient(proxy, 20*time.Second)
	if err != nil {
		return nil, err
	}
	var r struct {
		DeviceCode string `json:"device_code"`
		URI        string `json:"verification_uri_complete"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
		ExpireIn   int    `json:"expire_in"` // 飞书 SDK 读的是这个名字，实际返回的是 expires_in，两个都认
		Error      string `json:"error"`
		Desc       string `json:"error_description"`
	}
	err = regCall(ctx, hc, regDomains["feishu"], url.Values{
		"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}, "request_user_info": {"open_id"},
	}, &r)
	if err != nil {
		return nil, err
	}
	if r.DeviceCode == "" || r.URI == "" {
		return nil, fmt.Errorf("飞书没有返回二维码：%s %s", r.Error, r.Desc)
	}
	u, err := url.Parse(r.URI)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("from", "sdk")
	q.Set("tp", "sdk")
	q.Set("source", "go-sdk/nano-agent")
	if name != "" {
		q.Set("name", name)
	}
	if desc != "" {
		q.Set("desc", desc)
	}
	if addons, err := encodeAddons(regAddons); err == nil {
		q.Set("addons", addons)
	}
	u.RawQuery = q.Encode()
	if r.ExpiresIn > 0 {
		r.ExpireIn = r.ExpiresIn
	}
	reg := &FeishuRegistration{URL: u.String(), ExpireIn: r.ExpireIn, hc: hc, domain: regDomains["feishu"], deviceCode: r.DeviceCode, interval: r.Interval}
	if reg.ExpireIn <= 0 {
		reg.ExpireIn = 600
	}
	if reg.interval <= 0 {
		reg.interval = 5
	}
	return reg, nil
}

// Wait 轮询直到用户确认、拒绝或者二维码过期。
func (r *FeishuRegistration) Wait(ctx context.Context) (*FeishuRegistered, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.ExpireIn)*time.Second)
	defer cancel()
	brand := "feishu"
	switched := false
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, errors.New("二维码过期了，请重新生成")
				}
				return nil, ctx.Err()
			case <-time.After(time.Duration(r.interval) * time.Second):
			}
		}
		var p struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			UserInfo     *struct {
				TenantBrand string `json:"tenant_brand"`
			} `json:"user_info"`
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		if err := regCall(ctx, r.hc, r.domain, url.Values{"action": {"poll"}, "device_code": {r.deviceCode}}, &p); err != nil {
			if ctx.Err() != nil {
				continue // 交给上面的 select 报过期或取消
			}
			return nil, err
		}
		// 扫码的是 Lark（国际版）账号：换到 Lark 的域名继续问
		if p.UserInfo != nil && p.UserInfo.TenantBrand == "lark" && !switched {
			r.domain, brand, switched = regDomains["lark"], "lark", true
			continue
		}
		if p.ClientID != "" && p.ClientSecret != "" {
			return &FeishuRegistered{AppID: p.ClientID, AppSecret: p.ClientSecret, Domain: brand}, nil
		}
		switch p.Error {
		case "", "authorization_pending":
		case "slow_down":
			r.interval += 5
		case "access_denied":
			return nil, errors.New("在飞书里取消了创建")
		case "expired_token":
			return nil, errors.New("二维码过期了，请重新生成")
		default:
			return nil, fmt.Errorf("飞书返回错误：%s %s", p.Error, p.Desc)
		}
	}
}

func regCall(ctx context.Context, hc *http.Client, domain string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, domain+regPath, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接飞书失败：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("飞书的响应看不懂（HTTP %d）：%.200s", resp.StatusCode, body)
	}
	return nil
}

// encodeAddons 按飞书 SDK 的格式编码预置的权限和事件：JSON → gzip → base64url（无填充）
func encodeAddons(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// QRCodeSVG 把文本编码成二维码的 SVG（黑色方块，透明背景外加白色边距）。
func QRCodeSVG(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	n := code.Size
	const quiet = 4
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n+2*quiet, n+2*quiet)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n+2*quiet, n+2*quiet)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
