package app

import (
	"context"
	"encoding/json"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/search"
	"strings"
	"testing"
)

func TestSearchKeyFallback(t *testing.T) {
	es := []models.Endpoint{{BaseURL: "https://api.deepseek.com.evil/v1", APIKey: "bad"}, {BaseURL: "https://API.deepseek.com/v1", APIKey: "fallback"}}
	off := false
	for _, tc := range []struct {
		cfg  config.Search
		want string
	}{
		{config.Search{}, "fallback"}, {config.Search{APIKey: "explicit"}, "explicit"}, {config.Search{Enabled: &off, APIKey: "explicit"}, ""},
	} {
		if got := searchKey(tc.cfg, es); got != tc.want {
			t.Fatal(got)
		}
	}
	if searchKey(config.Search{}, es[:1]) != "" {
		t.Fatal("unsafe hostname fallback")
	}
}

type fakeSearch struct{ count int }

func (f *fakeSearch) Search(_ context.Context, _ string, n int) ([]search.Result, error) {
	f.count = n
	return []search.Result{{Title: "来源", URL: "https://example.org", Summary: "新闻"}}, nil
}
func (*fakeSearch) Fetch(context.Context, string) (search.Page, error) {
	return search.Page{Content: "正文"}, nil
}
func TestGuestWebSearchToolAndFrozenPrefix(t *testing.T) {
	e := setup(t)
	e.a.cfg.Search.APIKey = "test-key"
	e.a.searchClient = &fakeSearch{}
	e.a.searchFingerprint = "test-key\n"
	e.a.OnMessage(e.ch, channel.Message{ChatID: "visitor", SenderID: "u2", Text: "查新闻"})
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	req := e.llm.lastReq()
	if len(req.Tools) != 2 {
		t.Fatalf("guest tools: %+v", req.Tools)
	}
	e.k.send(proto.Msg{T: proto.TExec, SID: in.SID, ID: "web1", Name: "WebSearch", Args: json.RawMessage(`{"query":"news"}`)})
	res := e.k.expect(proto.TExecResult)
	if (res.OK == nil || !*res.OK) || !strings.Contains(res.Content, "https://example.org") {
		t.Fatal(res)
	}
	if e.a.searchClient.(*fakeSearch).count != 6 {
		t.Fatal("default count")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: "ok"})
	e.a.mu.Lock()
	e.a.cfg.Search.APIKey = ""
	_, tools, _, _ := e.a.contextParts(in.SID)
	e.a.mu.Unlock()
	if len(tools) != 2 {
		t.Fatal("prefix changed mid-session")
	}
}

func TestSearchUsagePersistenceAndDayRollover(t *testing.T) {
	e := setup(t)
	e.a.cfg.Search.APIKey = "test-key"
	provider, err := e.a.searchProvider()
	if err != nil {
		t.Fatal(err)
	}
	d := provider.(*search.DeepSeek)
	d.OnSuccess("search")
	d.OnSuccess("fetch")
	e.a.mu.Lock()
	e.a.searchUsage = SearchUsage{}
	e.a.loadSearchUsage()
	if e.a.searchUsage.Search != 1 || e.a.searchUsage.Fetch != 1 {
		t.Fatal(e.a.searchUsage)
	}
	e.a.searchUsage.Date = "2026-10-20"
	e.a.loadSearchUsage()
	if e.a.searchUsage.Search != 0 || e.a.searchUsage.Fetch != 0 {
		t.Fatal("daily reset")
	}
	e.a.mu.Unlock()
}
