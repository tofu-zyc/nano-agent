// Package fakegithub 是测试用的假 GitHub：一个仓库的议题、评论、PR、检查、审阅，放在内存里，
// 用 httptest 起来，只实现网关用到的那几个端点。不连真的 GitHub，不用真 token。
package fakegithub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/repos"
)

// Request 是收到的一次请求（断言用）。
type Request struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

// Server 是一个假 GitHub。
type Server struct {
	*httptest.Server
	Owner, Name, Token, Bot string
	DefaultBranch           string

	mu        sync.Mutex
	now       func() time.Time
	nextID    int64
	nextNum   int
	issues    []*repos.Issue
	comments  []repos.Comment
	pcomments []repos.Comment
	reviews   map[int][]repos.Review
	pulls     map[int]*repos.Pull
	checks    map[string][]repos.CheckRun
	requests  []Request
	fail      []failRule
}

type failRule struct {
	contains string
	status   int
	msg      string
	headers  map[string]string
	once     bool
}

// New 起一个假 GitHub。token 是它认的；bot 是这个 token 对应的账号。
func New(owner, name, token, bot string) *Server {
	s := &Server{Owner: owner, Name: name, Token: token, Bot: bot, DefaultBranch: "main", now: time.Now, nextID: 1000, nextNum: 0,
		reviews: map[int][]repos.Review{}, pulls: map[int]*repos.Pull{}, checks: map[string][]repos.CheckRun{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// SetClock 换时钟（让 since、updated_at 可预测）。
func (s *Server) SetClock(f func() time.Time) { s.mu.Lock(); s.now = f; s.mu.Unlock() }

func (s *Server) id() int64 { s.nextID++; return s.nextID }
func (s *Server) num() int  { s.nextNum++; return s.nextNum }

// Requests 是收到过的请求（副本）。
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// FailNext 让下一个路径里包含 contains 的请求失败（once）：status 和消息，headers 可以带 Retry-After 之类。
func (s *Server) FailNext(contains string, status int, msg string, headers map[string]string) {
	s.mu.Lock()
	s.fail = append(s.fail, failRule{contains: contains, status: status, msg: msg, headers: headers, once: true})
	s.mu.Unlock()
}

// ---- 造数据 ----

// AddIssue 开一个议题，返回它。
func (s *Server) AddIssue(author, title, body string) repos.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := &repos.Issue{ID: s.id(), Number: s.num(), Title: title, Body: body, State: "open", User: repos.User{Login: author},
		UpdatedAt: s.now()}
	i.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", s.Owner, s.Name, i.Number)
	s.issues = append(s.issues, i)
	return *i
}

// AddComment 在议题或 PR 上加一条评论，议题的 updated_at 跟着变。
func (s *Server) AddComment(number int, author, body string) repos.Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(number, author, body)
}

func (s *Server) addComment(number int, author, body string) repos.Comment {
	now := s.now()
	c := repos.Comment{ID: s.id(), Body: body, User: repos.User{Login: author}, CreatedAt: now, UpdatedAt: now,
		IssueURL: fmt.Sprintf("%s/repos/%s/%s/issues/%d", s.URL, s.Owner, s.Name, number)}
	c.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d#issuecomment-%d", s.Owner, s.Name, number, c.ID)
	s.comments = append(s.comments, c)
	for _, i := range s.issues {
		if i.Number == number {
			i.UpdatedAt = now
		}
	}
	return c
}

// Comments 是某个议题或 PR 上的评论（含网关发的）。
func (s *Server) Comments(number int) []repos.Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []repos.Comment
	suffix := fmt.Sprintf("/issues/%d", number)
	for _, c := range s.comments {
		if strings.HasSuffix(c.IssueURL, suffix) {
			out = append(out, c)
		}
	}
	return out
}

// AddReview 给 PR 加一条审阅。
func (s *Server) AddReview(pr int, author, state, body string) repos.Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := repos.Review{ID: s.id(), Body: body, State: state, User: repos.User{Login: author}, SubmittedAt: s.now()}
	r.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", s.Owner, s.Name, pr, r.ID)
	s.reviews[pr] = append(s.reviews[pr], r)
	return r
}

// AddReviewComment 给 PR 加一条行内审阅意见。
func (s *Server) AddReviewComment(pr int, author, body string) repos.Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := repos.Comment{ID: s.id(), Body: body, User: repos.User{Login: author}, CreatedAt: now, UpdatedAt: now,
		PullRequestURL: fmt.Sprintf("%s/repos/%s/%s/pulls/%d", s.URL, s.Owner, s.Name, pr)}
	s.pcomments = append(s.pcomments, c)
	return c
}

// OpenPull 开一个 PR（给没有经过网关的情况用）。
func (s *Server) OpenPull(title, head, headSHA string) repos.Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openPull(title, "", head, headSHA)
}

func (s *Server) openPull(title, body, head, headSHA string) repos.Pull {
	p := &repos.Pull{Number: s.num(), State: "open", Title: title}
	p.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.Owner, s.Name, p.Number)
	p.Head.Ref, p.Head.SHA = head, headSHA
	s.pulls[p.Number] = p
	// PR 也是议题：出现在 issues 列表里
	pr := &repos.Issue{ID: s.id(), Number: p.Number, Title: title, Body: body, State: "open", User: repos.User{Login: s.Bot}, HTMLURL: p.HTMLURL, UpdatedAt: s.now()}
	pr.PullRequest = &struct {
		URL string `json:"url"`
	}{URL: s.URL}
	s.issues = append(s.issues, pr)
	return *p
}

// Pull 是现在的 PR。
func (s *Server) Pull(n int) (repos.Pull, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pulls[n]
	if !ok {
		return repos.Pull{}, false
	}
	return *p, true
}

// Pulls 是全部 PR。
func (s *Server) Pulls() []repos.Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []repos.Pull
	for _, p := range s.pulls {
		out = append(out, *p)
	}
	return out
}

// MergePull 把 PR 合并（mergeSHA 是合并提交）。
func (s *Server) MergePull(n int, mergeSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[n]; p != nil {
		p.State, p.Merged, p.MergeCommitSHA = "closed", true, mergeSHA
	}
}

// ClosePull 关掉 PR（没合并）。
func (s *Server) ClosePull(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[n]; p != nil {
		p.State = "closed"
	}
}

// SetHead 换 PR 的 head 提交（推了新提交）。
func (s *Server) SetHead(n int, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[n]; p != nil {
		p.Head.SHA = sha
	}
}

// SetChecks 设一个提交上的检查。
func (s *Server) SetChecks(sha string, runs ...repos.CheckRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[sha] = runs
}

// Check 造一个检查；suite 是它的套件。
func Check(name string, suite int64, status, conclusion, summary string) repos.CheckRun {
	r := repos.CheckRun{ID: suite*10 + int64(len(name)), Name: name, Status: status, Conclusion: conclusion}
	r.Output.Title, r.Output.Summary = name+" "+conclusion, summary
	r.CheckSuite.ID = suite
	return r
}

// ---- HTTP ----

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := ""
	if r.Body != nil {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		body = b.String()
	}
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body})
	reply := func(status int, v any, hdr map[string]string) {
		for k, x := range hdr {
			w.Header().Set(k, x)
		}
		raw, _ := json.Marshal(v)
		if status == 200 && r.Method == "GET" {
			sum := sha256.Sum256(raw)
			tag := `"` + hex.EncodeToString(sum[:8]) + `"`
			w.Header().Set("ETag", tag)
			if r.Header.Get("If-None-Match") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(raw)
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		reply(401, map[string]string{"message": "Bad credentials"}, nil)
		return
	}
	for i, f := range s.fail {
		if strings.Contains(r.URL.Path, f.contains) {
			if f.once {
				s.fail = append(s.fail[:i], s.fail[i+1:]...)
			}
			reply(f.status, map[string]string{"message": f.msg}, f.headers)
			return
		}
	}
	prefix := "/repos/" + s.Owner + "/" + s.Name
	path := r.URL.Path
	since := func() time.Time {
		t, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		return t
	}
	switch {
	case path == "/user":
		reply(200, repos.User{Login: s.Bot}, nil)
	case path == prefix && r.Method == "GET":
		reply(200, map[string]string{"default_branch": s.DefaultBranch}, nil)
	case path == prefix+"/issues" && r.Method == "GET":
		var out []repos.Issue
		sn := since()
		for _, i := range s.issues {
			if !i.UpdatedAt.Before(sn) {
				out = append(out, *i)
			}
		}
		reply(200, out, nil)
	case path == prefix+"/issues/comments" && r.Method == "GET":
		var out []repos.Comment
		sn := since()
		for _, c := range s.comments {
			if !c.UpdatedAt.Before(sn) {
				out = append(out, c)
			}
		}
		reply(200, out, nil)
	case path == prefix+"/pulls/comments" && r.Method == "GET":
		var out []repos.Comment
		sn := since()
		for _, c := range s.pcomments {
			if !c.UpdatedAt.Before(sn) {
				out = append(out, c)
			}
		}
		reply(200, out, nil)
	case strings.HasPrefix(path, prefix+"/issues/") && strings.HasSuffix(path, "/comments") && r.Method == "POST":
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, prefix+"/issues/"), "/comments"))
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal([]byte(body), &in)
		reply(201, s.addComment(n, s.Bot, in.Body), nil)
	case strings.HasPrefix(path, prefix+"/issues/") && r.Method == "GET":
		n, _ := strconv.Atoi(strings.TrimPrefix(path, prefix+"/issues/"))
		for _, i := range s.issues {
			if i.Number == n {
				reply(200, i, nil)
				return
			}
		}
		reply(404, map[string]string{"message": "Not Found"}, nil)
	case path == prefix+"/pulls" && r.Method == "GET":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), s.Owner+":")
		var out []repos.Pull
		for _, p := range s.pulls {
			if p.Head.Ref == head {
				out = append(out, *p)
			}
		}
		reply(200, out, nil)
	case path == prefix+"/pulls" && r.Method == "POST":
		var in struct{ Title, Body, Head, Base string }
		_ = json.Unmarshal([]byte(body), &in)
		reply(201, s.openPull(in.Title, in.Body, in.Head, "sha-of-"+in.Head), nil)
	case strings.HasPrefix(path, prefix+"/pulls/") && strings.HasSuffix(path, "/reviews"):
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, prefix+"/pulls/"), "/reviews"))
		reply(200, s.reviews[n], nil)
	case strings.HasPrefix(path, prefix+"/pulls/") && r.Method == "GET":
		n, _ := strconv.Atoi(strings.TrimPrefix(path, prefix+"/pulls/"))
		if p := s.pulls[n]; p != nil {
			reply(200, p, nil)
			return
		}
		reply(404, map[string]string{"message": "Not Found"}, nil)
	case strings.HasPrefix(path, prefix+"/commits/") && strings.HasSuffix(path, "/check-runs"):
		sha := strings.TrimSuffix(strings.TrimPrefix(path, prefix+"/commits/"), "/check-runs")
		reply(200, map[string]any{"total_count": len(s.checks[sha]), "check_runs": s.checks[sha]}, nil)
	default:
		reply(404, map[string]string{"message": "Not Found"}, nil)
	}
}
