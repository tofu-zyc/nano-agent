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
	"os"
	"strings"
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

// EnvProxy 是网关进程环境变量里的代理（HTTPS_PROXY、ALL_PROXY 等），没有返回空。
// 网关作为服务运行时，终端里设的代理变量传不进来，网页上要让用户看到这一点。
func EnvProxy() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return RedactURL(v)
		}
	}
	return ""
}

// RedactURL 去掉地址里的密码（http://user:pass@host → http://user:***@host）。
func RedactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// Describe 用一句话说明 proxy（网关的代理设置）实际是怎么走的，写进错误和网页提示里。
func Describe(proxy string) string {
	switch proxy {
	case "":
		if env := EnvProxy(); env != "" {
			return "走环境变量里的代理 " + env
		}
		return "没走代理：默认代理是跟随环境变量，但网关进程的环境变量里没有代理"
	case "none", "direct":
		return "直连，没走代理"
	}
	return "走代理 " + RedactURL(proxy)
}
