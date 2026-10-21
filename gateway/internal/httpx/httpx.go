// Package httpx 按配置创建 HTTP 客户端。
//
// 每个出站连接（模型接口、Telegram、飞书）可以单独指定代理：
//
//	""          跟随环境变量 HTTPS_PROXY / NO_PROXY
//	"none"      直连
//	"http://…"  使用指定代理
//
// 注意 Go 不认 NO_PROXY 里 "10.*" 这种通配写法，内网地址需要显式配置成 "none"。
package httpx

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ProxyFunc 返回给 http.Transport / websocket.Dialer 用的代理函数。
func ProxyFunc(proxy string) (func(*http.Request) (*url.URL, error), error) {
	switch proxy {
	case "":
		return http.ProxyFromEnvironment, nil
	case "none", "direct":
		return nil, nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("代理地址不合法：%q", proxy)
	}
	return http.ProxyURL(u), nil
}

// NewClient 创建一个使用指定代理的客户端。timeout 为 0 表示不限时（长轮询自己控制）。
func NewClient(proxy string, timeout time.Duration) (*http.Client, error) {
	pf, err := ProxyFunc(proxy)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = pf
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
