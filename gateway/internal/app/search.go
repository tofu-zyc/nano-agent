package app

import (
	"context"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/search"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type SearchUsage struct {
	Date   string `json:"date"`
	Search int    `json:"search"`
	Fetch  int    `json:"fetch"`
}

func searchKey(cfg config.Search, endpoints []models.Endpoint) string {
	if !config.On(cfg.Enabled) {
		return ""
	}
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	for _, e := range endpoints {
		u, err := url.Parse(e.BaseURL)
		if err == nil && strings.EqualFold(u.Hostname(), "api.deepseek.com") && e.APIKey != "" {
			return e.APIKey
		}
	}
	return ""
}
func (a *App) searchKey() string {
	var endpoints []models.Endpoint
	if a.models != nil {
		endpoints = a.models.Endpoints()
	}
	return searchKey(a.cfg.Search, endpoints)
}
func (a *App) searchTools() []proto.ToolSpec {
	if a.searchKey() == "" {
		return nil
	}
	return []proto.ToolSpec{
		{Name: "WebSearch", Description: "搜索互联网网页，返回带编号的标题、链接和摘要。回答时必须附上来源链接。", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"count":{"type":"integer","minimum":1,"maximum":10}},"required":["query"]}`)},
		{Name: "WebFetch", Description: "抓取网页 Markdown，每次最多 20000 字，按返回的 offset 继续读。回答时必须附上来源链接。", InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"offset":{"type":"integer","minimum":0}},"required":["url"]}`)},
	}
}
func (a *App) searchProvider() (search.Provider, error) {
	key := a.searchKey()
	if key == "" {
		return nil, fmt.Errorf("未配置可用的 DeepSeek 搜索 API key")
	}
	fingerprint := key + "\n" + a.cfg.Network.Proxy
	if a.searchClient != nil && fingerprint == a.searchFingerprint {
		return a.searchClient, nil
	}
	d, err := search.NewDeepSeek(key, a.cfg.Network.Proxy)
	if err != nil {
		return nil, err
	}
	d.OnSuccess = func(op string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.loadSearchUsage()
		if op == "search" {
			a.searchUsage.Search++
		} else {
			a.searchUsage.Fetch++
		}
		b, _ := json.Marshal(a.searchUsage)
		path := filepath.Join(a.store.Dir(), "search-usage.json")
		if err := os.WriteFile(path+".tmp", b, 0600); err == nil {
			err = os.Rename(path+".tmp", path)
			if err != nil {
				a.log.Warn("保存搜索统计失败", "err", err)
			}
		}
	}
	a.searchClient = d
	a.searchFingerprint = fingerprint
	return d, nil
}
func (a *App) loadSearchUsage() {
	date := a.now().In(a.loc).Format("2006-01-02")
	if a.searchUsage.Date == "" {
		b, _ := os.ReadFile(filepath.Join(a.store.Dir(), "search-usage.json"))
		_ = json.Unmarshal(b, &a.searchUsage)
	}
	if a.searchUsage.Date != date {
		a.searchUsage = SearchUsage{Date: date}
	}
}
func (a *App) webTool(s *sess, m *proto.Msg, reply func(bool, string)) {
	var args struct {
		Query, URL    string
		Count, Offset int
	}
	if err := json.Unmarshal(m.Args, &args); err != nil {
		reply(false, "搜索参数格式不正确")
		return
	}
	if args.Count == 0 {
		args.Count = a.cfg.Search.Count
		if args.Count == 0 {
			args.Count = 6
		}
	}
	provider, err := a.searchProvider()
	if err != nil {
		reply(false, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	r := s.run
	r.execs[m.ID] = cancel
	go func() {
		defer cancel()
		var text string
		var err error
		if m.Name == "WebSearch" {
			var results []search.Result
			results, err = provider.Search(ctx, args.Query, args.Count)
			if err == nil {
				text = search.FormatResults(results)
			}
		} else {
			var page search.Page
			page, err = provider.Fetch(ctx, args.URL)
			if err == nil {
				text, err = search.FormatPage(page, args.Offset)
			}
		}
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, text)
		}
	}()
}
