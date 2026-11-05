package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub API 客户端。token 只放在 Authorization 头里，错误信息里一律打码。

// DefaultBase 是 GitHub 的 API 地址；测试里换成假 GitHub。
const DefaultBase = "https://api.github.com"

// Client 是一个仓库的 API 客户端。
type Client struct {
	Base  string // 空表示 DefaultBase
	Owner string
	Name  string
	Token string
	HTTP  *http.Client
}

// APIError 是 API 返回的错误。RetryAfter 非零表示被限流，要等这么久再试，不加快重试。
type APIError struct {
	Status     int
	Msg        string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("GitHub %d：%s（被限流，%s 后再试）", e.Status, e.Msg, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("GitHub %d：%s", e.Status, e.Msg)
}

// IsNotModified 是 304（ETag 没变）。
var ErrNotModified = errors.New("没有变化")

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultBase
}

func (c *Client) repoPath(rest string) string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Name) + rest
}

// scrub 把 token 从文本里去掉。
func (c *Client) scrub(s string) string {
	if c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "***")
	}
	return s
}

// do 发一次请求。etag 非空时带 If-None-Match；返回新的 ETag。304 返回 ErrNotModified。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any, etag string) (newETag string, err error) {
	u := c.base() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return "", errors.New(c.scrub(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "nano-agent")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New(c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotModified {
		return etag, ErrNotModified
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return "", &APIError{Status: resp.StatusCode, Msg: c.scrub(msg), RetryAfter: retryAfter(resp)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return "", errors.New("GitHub 的回应看不懂")
		}
	}
	return resp.Header.Get("ETag"), nil
}

// retryAfter 看 403、429 要不要退避：Retry-After（秒），或者额度用完时的 X-RateLimit-Reset（时间戳）。
func retryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Until(time.Unix(n, 0)); d > 0 {
				return d
			}
			return time.Second
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return time.Minute
	}
	return 0
}

// ---- 数据 ----

type User struct {
	Login string `json:"login"`
}

type Issue struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// 审阅意见（pulls/comments）才有
	PullRequestURL string `json:"pull_request_url"`
}

type Review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type Pull struct {
	Number         int    `json:"number"`
	State          string `json:"state"` // open | closed
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	HTMLURL        string `json:"html_url"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Title string `json:"title"`
}

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // completed | in_progress | queued
	Conclusion string `json:"conclusion"` // success | failure | timed_out | cancelled | neutral | skipped ...
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
}

// ---- 调用 ----

// Whoami 用 token 查它对应的账号。
func (c *Client) Whoami(ctx context.Context) (string, error) {
	var u User
	if _, err := c.do(ctx, "GET", "/user", nil, nil, &u, ""); err != nil {
		return "", err
	}
	return u.Login, nil
}

// CheckAccess 查这个仓库能不能访问，返回默认分支。
func (c *Client) CheckAccess(ctx context.Context) (defaultBranch string, err error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.do(ctx, "GET", c.repoPath(""), nil, nil, &r, ""); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// Issues 取 since 之后更新过的议题和 PR（升序）。
func (c *Client) Issues(ctx context.Context, since time.Time, etag string) ([]Issue, string, error) {
	q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []Issue
	tag, err := c.do(ctx, "GET", c.repoPath("/issues"), q, nil, &out, etag)
	return out, tag, err
}

// IssueComments 取 since 之后的议题评论（含 PR 对话里的评论）。
func (c *Client) IssueComments(ctx context.Context, since time.Time, etag string) ([]Comment, string, error) {
	q := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []Comment
	tag, err := c.do(ctx, "GET", c.repoPath("/issues/comments"), q, nil, &out, etag)
	return out, tag, err
}

// PullComments 取 since 之后的 PR 审阅意见（行内评论）。
func (c *Client) PullComments(ctx context.Context, since time.Time, etag string) ([]Comment, string, error) {
	q := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []Comment
	tag, err := c.do(ctx, "GET", c.repoPath("/pulls/comments"), q, nil, &out, etag)
	return out, tag, err
}

// Reviews 取一个 PR 的审阅。
func (c *Client) Reviews(ctx context.Context, pr int) ([]Review, error) {
	var out []Review
	_, err := c.do(ctx, "GET", c.repoPath("/pulls/"+strconv.Itoa(pr)+"/reviews"), url.Values{"per_page": {"100"}}, nil, &out, "")
	return out, err
}

// Issue 取一个议题（标题、正文、作者）。
func (c *Client) Issue(ctx context.Context, n int) (Issue, error) {
	var out Issue
	_, err := c.do(ctx, "GET", c.repoPath("/issues/"+strconv.Itoa(n)), nil, nil, &out, "")
	return out, err
}

// Pull 取一个 PR。
func (c *Client) Pull(ctx context.Context, n int) (Pull, error) {
	var out Pull
	_, err := c.do(ctx, "GET", c.repoPath("/pulls/"+strconv.Itoa(n)), nil, nil, &out, "")
	return out, err
}

// PullByHead 找 head 是这个分支的、开着的 PR；没有返回 false。
func (c *Client) PullByHead(ctx context.Context, branch string) (Pull, bool, error) {
	var out []Pull
	q := url.Values{"head": {c.Owner + ":" + branch}, "state": {"all"}, "per_page": {"5"}}
	if _, err := c.do(ctx, "GET", c.repoPath("/pulls"), q, nil, &out, ""); err != nil {
		return Pull{}, false, err
	}
	for _, p := range out { // 只认开着的；合并了或关了的可能是以前留下的同名分支
		if p.State == "open" {
			return p, true, nil
		}
	}
	return Pull{}, false, nil
}

// CreatePull 开 PR。
func (c *Client) CreatePull(ctx context.Context, title, body, head, base string) (Pull, error) {
	var out Pull
	_, err := c.do(ctx, "POST", c.repoPath("/pulls"), nil, map[string]string{"title": title, "body": body, "head": head, "base": base}, &out, "")
	return out, err
}

// CheckRuns 取一个提交上的检查。
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	_, err := c.do(ctx, "GET", c.repoPath("/commits/"+url.PathEscape(sha)+"/check-runs"), url.Values{"per_page": {"100"}}, nil, &out, "")
	return out.CheckRuns, err
}

// Comment 往议题或 PR 发评论。
func (c *Client) Comment(ctx context.Context, n int, text string) error {
	_, err := c.do(ctx, "POST", c.repoPath("/issues/"+strconv.Itoa(n)+"/comments"), nil, map[string]string{"body": text}, nil, "")
	return err
}
