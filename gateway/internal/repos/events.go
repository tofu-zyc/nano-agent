package repos

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 归一化的事件：轮询和 webhook 收到的东西都变成它，由同一个处理函数消费。
// 去重的键是 GitHub 对象本身（设计 §16.3）：议题、评论、审阅意见按对象 ID，不带 updated_at
// （议题每来一条评论它的 updated_at 都会变，带上会把原来的正文当新消息再交给 agent 一次）；
// 状态按“对象加状态”。

// 事件的种类。
const (
	KindIssue    = "issue"    // 新议题（含正文）
	KindComment  = "comment"  // 议题或 PR 对话里的评论
	KindReview   = "review"   // PR 的审阅
	KindRComment = "rcomment" // PR 的行内审阅意见
	KindPR       = "pr"       // PR 的终态：State 是 merged 或 closed
	KindCheck    = "check"    // 一个检查套件完成：State 是结论
)

// maxText 是事件里文字的上限（字）。
const maxText = 8000

// Event 是一件归一化的事。
type Event struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Number   int    `json:"number"` // 议题或 PR 的号
	Actor    string `json:"actor"`  // 谁做的（GitHub 账号）
	Title    string `json:"title,omitempty"`
	Text     string `json:"text,omitempty"`
	URL      string `json:"url,omitempty"`
	SHA      string `json:"sha,omitempty"`   // check：这个检查对应的提交；pr merged：合并提交
	State    string `json:"state,omitempty"` // pr：merged | closed；check：结论；review：审阅状态
	IsPR     bool   `json:"is_pr,omitempty"` // 评论、议题属于 PR 的对话
	Delivery string `json:"delivery,omitempty"`
	At       int64  `json:"at,omitempty"`
}

func clipText(s string) string {
	r := []rune(s)
	if len(r) > maxText {
		return string(r[:maxText])
	}
	return s
}

func issueEvent(i Issue) Event {
	return Event{Key: fmt.Sprintf("issue:%d", i.ID), Kind: KindIssue, Number: i.Number, Actor: i.User.Login, Title: clipText(i.Title),
		Text: clipText(i.Body), URL: i.HTMLURL, IsPR: i.PullRequest != nil, At: i.UpdatedAt.Unix()}
}

// numberOfURL 取 .../issues/12 或 .../pulls/12 末尾的号。
func numberOfURL(u string) int {
	i := strings.LastIndex(u, "/")
	n, _ := strconv.Atoi(u[i+1:])
	return n
}

func commentEvent(c Comment, isPR bool) Event {
	return Event{Key: fmt.Sprintf("comment:%d", c.ID), Kind: KindComment, Number: numberOfURL(c.IssueURL), Actor: c.User.Login,
		Text: clipText(c.Body), URL: c.HTMLURL, IsPR: isPR, At: c.CreatedAt.Unix()}
}

func rcommentEvent(c Comment) Event {
	return Event{Key: fmt.Sprintf("rcomment:%d", c.ID), Kind: KindRComment, Number: numberOfURL(c.PullRequestURL), Actor: c.User.Login,
		Text: clipText(c.Body), URL: c.HTMLURL, IsPR: true, At: c.CreatedAt.Unix()}
}

func reviewEvent(pr int, r Review) Event {
	return Event{Key: fmt.Sprintf("review:%d", r.ID), Kind: KindReview, Number: pr, Actor: r.User.Login, Text: clipText(r.Body),
		URL: r.HTMLURL, State: strings.ToLower(r.State), IsPR: true, At: r.SubmittedAt.Unix()}
}

func prEvent(p Pull) (Event, bool) {
	switch {
	case p.Merged:
		return Event{Key: fmt.Sprintf("pr:%d:merged", p.Number), Kind: KindPR, Number: p.Number, State: "merged", SHA: p.MergeCommitSHA, URL: p.HTMLURL, IsPR: true}, true
	case p.State == "closed":
		return Event{Key: fmt.Sprintf("pr:%d:closed", p.Number), Kind: KindPR, Number: p.Number, State: "closed", URL: p.HTMLURL, IsPR: true}, true
	}
	return Event{}, false
}

func checkEvent(pr int, suite int64, conclusion, sha string) Event {
	return Event{Key: fmt.Sprintf("check:%d:%s", suite, conclusion), Kind: KindCheck, Number: pr, State: conclusion, SHA: sha, IsPR: true}
}

// ---- webhook ----

// MaxWebhookBody 是 webhook 正文的上限。
const MaxWebhookBody = 1 << 20

// ErrBadSignature 是签名不对。
var ErrBadSignature = errors.New("签名不对")

// VerifySignature 校验 X-Hub-Signature-256（sha256=<hex 的 HMAC>），恒定时间比较。密钥空一律不通过。
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	want, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

// Sign 是测试和假 GitHub 用的签名。
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// WebhookEvents 是 webhook 认的事件类型。
var WebhookEvents = map[string]bool{"issues": true, "issue_comment": true, "pull_request": true, "pull_request_review": true,
	"pull_request_review_comment": true, "check_suite": true}

// ParseWebhook 把一次 webhook 投递变成事件。不认的事件类型、不关心的 action 返回空。
func ParseWebhook(eventType, delivery string, body []byte) ([]Event, error) {
	if !WebhookEvents[eventType] {
		return nil, nil
	}
	var p struct {
		Action string `json:"action"`
		Issue  *struct {
			ID          int64  `json:"id"`
			Number      int    `json:"number"`
			Title       string `json:"title"`
			Body        string `json:"body"`
			User        User   `json:"user"`
			HTMLURL     string `json:"html_url"`
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"issue"`
		Comment *Comment `json:"comment"`
		Review  *Review  `json:"review"`
		PR      *Pull    `json:"pull_request"`
		Suite   *struct {
			ID           int64  `json:"id"`
			Conclusion   string `json:"conclusion"`
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errors.New("webhook 正文看不懂")
	}
	var out []Event
	switch eventType {
	case "issues":
		if p.Action == "opened" && p.Issue != nil {
			out = append(out, issueEvent(Issue{ID: p.Issue.ID, Number: p.Issue.Number, Title: p.Issue.Title, Body: p.Issue.Body, User: p.Issue.User,
				HTMLURL: p.Issue.HTMLURL, PullRequest: p.Issue.PullRequest}))
		}
	case "issue_comment":
		if p.Action == "created" && p.Issue != nil && p.Comment != nil {
			c := *p.Comment
			c.IssueURL = "/" + strconv.Itoa(p.Issue.Number)
			out = append(out, commentEvent(c, p.Issue.PullRequest != nil))
		}
	case "pull_request":
		if p.Action == "closed" && p.PR != nil {
			if ev, ok := prEvent(*p.PR); ok {
				out = append(out, ev)
			}
		}
	case "pull_request_review":
		if p.Action == "submitted" && p.Review != nil && p.PR != nil {
			out = append(out, reviewEvent(p.PR.Number, *p.Review))
		}
	case "pull_request_review_comment":
		if p.Action == "created" && p.Comment != nil && p.PR != nil {
			c := *p.Comment
			c.PullRequestURL = "/" + strconv.Itoa(p.PR.Number)
			out = append(out, rcommentEvent(c))
		}
	case "check_suite":
		if p.Action == "completed" && p.Suite != nil {
			for _, pr := range p.Suite.PullRequests {
				out = append(out, checkEvent(pr.Number, p.Suite.ID, p.Suite.Conclusion, p.Suite.HeadSHA))
			}
		}
	}
	for i := range out {
		out[i].Delivery = delivery
		if out[i].At == 0 {
			out[i].At = time.Now().Unix()
		}
	}
	return out, nil
}
