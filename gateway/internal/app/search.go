package app

import (
	"context"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
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
	key, _ := searchKeySource(cfg, endpoints)
	return key
}

// searchKeySource 找搜索用的 key，并说明从哪来：设置里单独填的，或者借用的哪个模型接口（地址是 api.deepseek.com）
func searchKeySource(cfg config.Search, endpoints []models.Endpoint) (key, from string) {
	if !config.On(cfg.Enabled) {
		return "", ""
	}
	if cfg.APIKey != "" {
		return cfg.APIKey, "settings"
	}
	for _, e := range endpoints {
		u, err := url.Parse(e.BaseURL)
		if err == nil && strings.EqualFold(u.Hostname(), "api.deepseek.com") && e.APIKey != "" {
			name := e.Name
			if name == "" {
				name = e.ID
			}
			return e.APIKey, "endpoint:" + name
		}
	}
	return "", ""
}
func (a *App) searchEndpoints() []models.Endpoint {
	if a.models != nil {
		return a.models.Endpoints()
	}
	return nil
}
func (a *App) searchKey() string { return searchKey(a.cfg.Search, a.searchEndpoints()) }

// SearchInfo 是网页「搜索」节点要显示的状态（不含 key 本身）
type SearchInfo struct {
	Enabled  bool        `json:"enabled"`
	Provider string      `json:"provider"`
	Key      string      `json:"key"` // settings：设置里填的；endpoint:<接口名>：借用模型接口的；空：没有可用的 key，不提供搜索工具
	Today    SearchUsage `json:"today"`
}

func (a *App) SearchInfo() SearchInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.searchInfo()
}
func (a *App) searchInfo() SearchInfo {
	a.loadSearchUsage()
	_, from := searchKeySource(a.cfg.Search, a.searchEndpoints())
	provider := a.cfg.Search.Provider
	if provider == "" {
		provider = "deepseek"
	}
	return SearchInfo{Enabled: config.On(a.cfg.Search.Enabled), Provider: provider, Key: from, Today: a.searchUsage}
}

// TestSearch 真的搜一次（计费一次），看 key 和网络通不通
func (a *App) TestSearch(ctx context.Context) (int, error) {
	a.mu.Lock()
	provider, err := a.searchProvider()
	a.mu.Unlock()
	if err != nil {
		return 0, err
	}
	results, err := provider.Search(ctx, "DeepSeek", 1)
	return len(results), err
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
		a.publish(events.Event{T: "search", Data: a.searchInfo()})
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
