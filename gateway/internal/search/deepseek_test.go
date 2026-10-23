package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fake(t *testing.T, fn http.HandlerFunc) *DeepSeek {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	d, err := NewDeepSeek("test-key", "")
	if err != nil {
		t.Fatal(err)
	}
	d.base = srv.URL
	d.delays = []time.Duration{0, 0}
	return d
}
func TestSearchAndFetch(t *testing.T) {
	var searches, fetches, success atomic.Int32
	d := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Method != "POST" {
			t.Error("auth/method")
		}
		var v map[string]any
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/search" {
			searches.Add(1)
			if v["query"] != "新闻" || v["count"] != float64(6) || v["need_content"] != false {
				t.Error(v)
			}
			w.Write([]byte(`{"results":[{"title":"标题","url":"https://example.org","summary":"摘要","date":null,"site_name":"站点"}]}`))
		} else {
			fetches.Add(1)
			json.NewEncoder(w).Encode(Page{Title: "网页", Content: strings.Repeat("字", 40001)})
		}
	})
	d.OnSuccess = func(string) { success.Add(1) }
	results, err := d.Search(context.Background(), " 新闻 ", 6)
	if err != nil {
		t.Fatal(err)
	}
	if s := FormatResults(results); !strings.Contains(s, "[1] 标题 — https://example.org") || !strings.Contains(s, "摘要") {
		t.Fatal(s)
	}
	p, err := d.Fetch(context.Background(), "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := FormatPage(p, 0)
	if !strings.Contains(s, "还有 20001 字，用 offset=20000") {
		t.Fatal("pagination")
	}
	p, err = d.Fetch(context.Background(), "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	s, _ = FormatPage(p, 40000)
	if s != "网页\n\n字" {
		t.Fatal(s)
	}
	if searches.Load() != 1 || fetches.Load() != 1 || success.Load() != 2 {
		t.Fatal("unexpected billed calls")
	}
	if _, err = FormatPage(p, -1); err == nil {
		t.Fatal("negative offset")
	}
	if _, err = FormatPage(p, 40002); err == nil {
		t.Fatal("oversized offset")
	}
}
func TestErrorsRetriesAndValidation(t *testing.T) {
	for _, tc := range []struct {
		code, wantCalls int
		want            string
	}{{401, 1, "鉴权"}, {402, 1, "余额"}, {429, 3, "过快"}, {404, 1, "抓取"}, {400, 1, "400"}, {408, 3, "408"}, {500, 3, "500"}} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			var calls int
			d := fake(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.code)
				w.Write([]byte("private-key-do-not-echo"))
			})
			_, err := d.Search(context.Background(), "a", 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-key") || calls != tc.wantCalls {
				t.Fatalf("%d %v", calls, err)
			}
		})
	}
	d := fake(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid input sent") })
	for _, q := range []string{"", strings.Repeat("字", 129)} {
		if _, err := d.Search(context.Background(), q, 6); err == nil {
			t.Fatal(q)
		}
	}
	if _, err := d.Search(context.Background(), "ok", 11); err == nil {
		t.Fatal("count")
	}
	for _, u := range []string{"file:///tmp/a", "https://user:pass@example.org", "relative"} {
		if _, err := d.Fetch(context.Background(), u); err == nil {
			t.Fatal(u)
		}
	}
}
func TestRetrySuccessEmptyAndCancel(t *testing.T) {
	calls, success := 0, 0
	d := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"results":[]}`))
	})
	d.OnSuccess = func(string) { success++ }
	r, err := d.Search(context.Background(), "q", 6)
	if err != nil || len(r) != 0 || success != 0 || calls != 3 || FormatResults(r) != "没有找到搜索结果" {
		t.Fatal(r, err, calls)
	}
	d.delays = []time.Duration{time.Hour}
	calls = 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = d.Search(ctx, "q", 6); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
