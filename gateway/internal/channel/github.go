package channel

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
)

// GitHub 渠道（设计 §16.3）：议题和 PR 是对话，正文和评论是消息。对话 ID 是 owner/name#号，
// 发消息就是发评论（replyTo 忽略）。收到什么、谁的话能进会话由网关决定（仓库连接的事件进来、过滤之后才调 Deliver），
// 这里只管把消息交给网关、把回复变成评论；评论怎么发（用哪个仓库的 token）由网关提供的 send 决定，token 不经过这个包。
type GitHub struct {
	send func(ctx context.Context, repo string, number int, text string) error

	mu sync.Mutex
	h  Handler
}

// GitHubName 是这个渠道的名字：会话 ID 和身份的前缀，身份是 gh:<账号>。
const GitHubName = "gh"

// NewGitHub 建一个 GitHub 渠道；send 把一条评论发到 owner/name 的第 number 号上。
func NewGitHub(send func(ctx context.Context, repo string, number int, text string) error) *GitHub {
	return &GitHub{send: send}
}

func (g *GitHub) Name() string { return GitHubName }

// Attach 先记下处理消息的函数（不等 Run 起来，免得刚登记就来消息时丢掉）。
func (g *GitHub) Attach(h Handler) {
	g.mu.Lock()
	g.h = h
	g.mu.Unlock()
}

// Run 记下处理消息的函数，等到 ctx 结束。
func (g *GitHub) Run(ctx context.Context, h Handler) error {
	g.mu.Lock()
	g.h = h
	g.mu.Unlock()
	<-ctx.Done()
	return nil
}

// Deliver 把一条已经过滤的消息交给网关。渠道还没开始运行时返回错误。
func (g *GitHub) Deliver(m Message) error {
	g.mu.Lock()
	h := g.h
	g.mu.Unlock()
	if h == nil {
		return errors.New("GitHub 渠道还没有开始运行")
	}
	h(g, m)
	return nil
}

// Send 往议题或 PR 发一条评论。
func (g *GitHub) Send(ctx context.Context, chatID, text, replyTo string) error {
	repo, n, ok := SplitGitHubChat(chatID)
	if !ok {
		return errors.New("不是 GitHub 的对话：" + chatID)
	}
	return g.send(ctx, repo, n, text)
}

// GitHubChat 是对话 ID：owner/name#号。
func GitHubChat(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

// SplitGitHubChat 拆开 owner/name#号。
func SplitGitHubChat(chat string) (repo string, number int, ok bool) {
	repo, num, found := strings.Cut(chat, "#")
	if !found || strings.Count(repo, "/") != 1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return repo, n, true
}
