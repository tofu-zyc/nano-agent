package repos

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// Intake 把事件交给处理函数：先记下（按键去重）、再处理、处理完记“处理了”。
// 处理失败的事件留在“没处理完”里，下次重放；轮询和 webhook 共用。
type Intake struct {
	State  *State
	Handle func(Event) error
}

// Submit 受理一个事件：见过的键直接丢（返回 false）；新的先记下再处理。
func (in *Intake) Submit(ev Event) (bool, error) {
	fresh, err := in.State.Record(ev)
	if err != nil || !fresh {
		return false, err
	}
	return true, in.process(ev)
}

func (in *Intake) process(ev Event) error {
	if err := in.Handle(ev); err != nil {
		return err
	}
	return in.State.Done(ev.Key)
}

// Replay 把记下了但没处理完的事件接着处理（启动时调用）。
func (in *Intake) Replay() error {
	var first error
	for _, ev := range in.State.Pending() {
		if err := in.process(ev); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Poller 是一个仓库的轮询：用 since 和 ETag 取新议题、新评论，以及工单相关 PR 的状态、检查和审阅。
type Poller struct {
	Client     *Client
	Intake     *Intake
	TrackedPRs func() []int // 工单相关的 PR（开着的）；可以为空
	Now        func() time.Time
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Poll 做一轮。第一次（没有游标）只定下基线，不把历史当新事件。
// 被限流时返回 wait：要等这么久再试，不加快重试；wait 为 0 表示按正常间隔。
func (p *Poller) Poll(ctx context.Context) (wait time.Duration, err error) {
	defer func() {
		var ae *APIError
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			wait = ae.RetryAfter
		}
	}()
	st := p.Intake.State
	if err := p.Intake.Replay(); err != nil {
		return 0, err
	}
	cur := st.Cursor()
	if cur.IssuesSince.IsZero() {
		now := p.now()
		cur.IssuesSince, cur.CommentsSince, cur.PullCommentsSince = now, now, now
		return 0, st.SetCursor(cur)
	}
	// 议题
	issues, tag, err := p.Client.Issues(ctx, cur.IssuesSince, cur.ETags["issues"])
	switch {
	case errors.Is(err, ErrNotModified):
	case err != nil:
		return 0, err
	default:
		sort.SliceStable(issues, func(i, j int) bool { return issues[i].UpdatedAt.Before(issues[j].UpdatedAt) })
		for _, i := range issues {
			if i.PullRequest == nil { // PR 的正文不当议题受理
				if _, err := p.Intake.Submit(issueEvent(i)); err != nil {
					return 0, err
				}
			}
			if i.UpdatedAt.After(cur.IssuesSince) {
				cur.IssuesSince = i.UpdatedAt
			}
		}
		setTag(&cur, "issues", tag)
	}
	// 评论（议题和 PR 对话里的）
	comments, tag, err := p.Client.IssueComments(ctx, cur.CommentsSince, cur.ETags["comments"])
	switch {
	case errors.Is(err, ErrNotModified):
	case err != nil:
		return 0, err
	default:
		for _, c := range comments {
			if _, err := p.Intake.Submit(commentEvent(c, false)); err != nil {
				return 0, err
			}
			if c.UpdatedAt.After(cur.CommentsSince) {
				cur.CommentsSince = c.UpdatedAt
			}
		}
		setTag(&cur, "comments", tag)
	}
	// PR 的行内审阅意见
	pcs, tag, err := p.Client.PullComments(ctx, cur.PullCommentsSince, cur.ETags["pcomments"])
	switch {
	case errors.Is(err, ErrNotModified):
	case err != nil:
		return 0, err
	default:
		for _, c := range pcs {
			if _, err := p.Intake.Submit(rcommentEvent(c)); err != nil {
				return 0, err
			}
			if c.UpdatedAt.After(cur.PullCommentsSince) {
				cur.PullCommentsSince = c.UpdatedAt
			}
		}
		setTag(&cur, "pcomments", tag)
	}
	if err := st.SetCursor(cur); err != nil { // 事件都记下了，游标才往前走
		return 0, err
	}
	// 工单相关的 PR：状态、检查、审阅
	var tracked []int
	if p.TrackedPRs != nil {
		tracked = p.TrackedPRs()
	}
	for _, n := range tracked {
		if err := p.pollPull(ctx, n); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func setTag(c *Cursor, name, tag string) {
	if tag == "" {
		return
	}
	if c.ETags == nil {
		c.ETags = map[string]string{}
	}
	c.ETags[name] = tag
}

func (p *Poller) pollPull(ctx context.Context, n int) error {
	pull, err := p.Client.Pull(ctx, n)
	if err != nil {
		return err
	}
	if ev, ok := prEvent(pull); ok {
		_, err := p.Intake.Submit(ev)
		return err
	}
	runs, err := p.Client.CheckRuns(ctx, pull.Head.SHA)
	if err != nil {
		return err
	}
	// 按套件分组：套件里的检查都完成了才算一个结果；有失败的就是失败
	type suite struct {
		done, failed bool
	}
	suites := map[int64]*suite{}
	var order []int64
	for _, r := range runs {
		s := suites[r.CheckSuite.ID]
		if s == nil {
			s = &suite{done: true}
			suites[r.CheckSuite.ID] = s
			order = append(order, r.CheckSuite.ID)
		}
		if r.Status != "completed" {
			s.done = false
		}
		switch r.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
			s.failed = true
		}
	}
	for _, id := range order {
		s := suites[id]
		if !s.done {
			continue
		}
		concl := "success"
		if s.failed {
			concl = "failure"
		}
		if _, err := p.Intake.Submit(checkEvent(n, id, concl, pull.Head.SHA)); err != nil {
			return err
		}
	}
	reviews, err := p.Client.Reviews(ctx, n)
	if err != nil {
		return err
	}
	for _, r := range reviews {
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		if _, err := p.Intake.Submit(reviewEvent(n, r)); err != nil {
			return err
		}
	}
	return nil
}
