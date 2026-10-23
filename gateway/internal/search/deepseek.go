package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"nano-agent/gateway/internal/httpx"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type cachedPage struct {
	page Page
	at   time.Time
}
type DeepSeek struct {
	base, key string
	hc        *http.Client
	delays    []time.Duration
	OnSuccess func(string)
	mu        sync.Mutex
	pages     map[string]cachedPage
}

func NewDeepSeek(key, proxy string) (*DeepSeek, error) {
	hc, err := httpx.NewClient(proxy, 45*time.Second)
	if err != nil {
		return nil, err
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DeepSeek{base: "https://api.deepseek.com/v1/tools", key: key, hc: hc, delays: []time.Duration{time.Second, 3 * time.Second}, pages: map[string]cachedPage{}}, nil
}
func (d *DeepSeek) post(ctx context.Context, op string, body, out any) error {
	b, _ := json.Marshal(body)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", d.base+"/"+op, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+d.key)
		req.Header.Set("Content-Type", "application/json")
		res, err := d.hc.Do(req)
		if err != nil {
			return fmt.Errorf("搜索服务连接失败：%w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, (16<<20)+1))
		res.Body.Close()
		if readErr != nil {
			return fmt.Errorf("读取搜索结果失败：%w", readErr)
		}
		if len(raw) > 16<<20 {
			return fmt.Errorf("搜索结果超过 16MB")
		}
		if res.StatusCode == 200 {
			if err = json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("搜索服务返回格式不正确")
			}
			return nil
		}
		retry := res.StatusCode == 408 || res.StatusCode == 429 || res.StatusCode >= 500
		if retry && attempt < len(d.delays) {
			timer := time.NewTimer(d.delays[attempt])
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		switch res.StatusCode {
		case 401:
			return fmt.Errorf("DeepSeek 搜索鉴权失败，请检查 API key（401）")
		case 402:
			return fmt.Errorf("DeepSeek 搜索余额不足，请充值（402）")
		case 429:
			return fmt.Errorf("DeepSeek 搜索请求过快，请稍后重试（429）")
		case 404:
			return fmt.Errorf("未能抓取这个网页（404）")
		}
		return fmt.Errorf("DeepSeek 搜索服务返回 HTTP %d", res.StatusCode)
	}
}
func (d *DeepSeek) Search(ctx context.Context, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 1 || utf8.RuneCountInString(query) > 128 {
		return nil, fmt.Errorf("搜索词需要 1–128 个字")
	}
	if count < 1 || count > 10 {
		return nil, fmt.Errorf("搜索条数需要 1–10")
	}
	var out struct {
		Results []Result `json:"results"`
	}
	if err := d.post(ctx, "search", map[string]any{"query": query, "count": count, "need_content": false}, &out); err != nil {
		return nil, err
	}
	if len(out.Results) > count {
		out.Results = out.Results[:count]
	}
	if len(out.Results) > 0 && d.OnSuccess != nil {
		d.OnSuccess("search")
	}
	return out.Results, nil
}
func (d *DeepSeek) Fetch(ctx context.Context, raw string) (Page, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || len(raw) > 8192 {
		return Page{}, fmt.Errorf("url 必须是 HTTP/HTTPS 网页地址，不能含用户名或密码")
	}
	d.mu.Lock()
	cached, ok := d.pages[raw]
	d.mu.Unlock()
	if ok && time.Since(cached.at) < 10*time.Minute {
		return cached.page, nil
	}
	var page Page
	if err := d.post(ctx, "fetch", map[string]string{"url": raw}, &page); err != nil {
		return Page{}, err
	}
	if page.Content != "" {
		if d.OnSuccess != nil {
			d.OnSuccess("fetch")
		}
		d.mu.Lock()
		for key, p := range d.pages {
			if time.Since(p.at) > 10*time.Minute {
				delete(d.pages, key)
			}
		}
		if len(d.pages) >= 32 {
			for key := range d.pages {
				delete(d.pages, key)
				break
			}
		}
		d.pages[raw] = cachedPage{page, time.Now()}
		d.mu.Unlock()
	}
	return page, nil
}
