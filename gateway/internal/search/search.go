// Package search provides gateway-owned read-only web tools.
package search

import (
	"context"
	"fmt"
	"strings"
)

type Result struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Summary  string `json:"summary"`
	Date     string `json:"date"`
	SiteName string `json:"site_name"`
}
type Page struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}
type Provider interface {
	Search(context.Context, string, int) ([]Result, error)
	Fetch(context.Context, string) (Page, error)
}

func FormatResults(results []Result) string {
	if len(results) == 0 {
		return "没有找到搜索结果"
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] %s — %s（%s · %s）\n%s\n\n", i+1, r.Title, r.URL, r.Date, r.SiteName, r.Summary)
	}
	return strings.TrimSpace(b.String())
}
func FormatPage(page Page, offset int) (string, error) {
	r := []rune(page.Content)
	if offset < 0 || offset > len(r) {
		return "", fmt.Errorf("offset 超出范围（0–%d）", len(r))
	}
	end := min(offset+20000, len(r))
	text := page.Title + "\n\n" + string(r[offset:end])
	if end < len(r) {
		text += fmt.Sprintf("\n\n还有 %d 字，用 offset=%d 继续读", len(r)-end, end)
	}
	return text, nil
}
