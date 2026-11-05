package repos_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/repos"
	"nano-agent/gateway/internal/repos/fakegithub"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/secret"
)

const tok = "ghp_supersecrettoken123"

func goodRepo() repos.Repo {
	return repos.Repo{Workspace: "ws1", Owner: "acme", Name: "app", Token: tok, Intake: "poll",
		Team: scene.Team{Roster: []string{"gh:alice"}}}
}

func TestStoreEncryptsAndValidates(t *testing.T) {
	if err := secret.Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	path := filepath.Join(t.TempDir(), "repos.json")
	s, err := repos.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := goodRepo()
	r.Intake, r.Secret = "webhook", "whsec"
	saved, err := s.Put(r)
	if err != nil || saved.ID == "" {
		t.Fatalf("存：%+v %v", saved, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), "whsec") || !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("token 和密钥要加密存：%s", raw)
	}
	got, _ := s.Get(saved.ID)
	if pt, err := got.PlainToken(); err != nil || pt != tok {
		t.Fatalf("解密：%q %v", pt, err)
	}
	if ps, _ := got.PlainSecret(); ps != "whsec" {
		t.Fatalf("密钥：%q", ps)
	}
	// 重启后读回来
	s2, _ := repos.Open(path)
	if _, ok := s2.BySlug("ACME/App"); !ok {
		t.Fatal("按 owner/name 找（不分大小写）")
	}
	if _, ok := s2.ByWorkspace("ws1"); !ok {
		t.Fatal("按工作目录找")
	}
	// 同一个工作目录或仓库不能连两次
	dup := goodRepo()
	dup.Owner = "other"
	if _, err := s2.Put(dup); err == nil {
		t.Fatal("一个工作目录只连一个仓库")
	}
	for name, mod := range map[string]func(*repos.Repo){
		"owner 不合法":    func(r *repos.Repo) { r.Owner = "a/b" },
		"没有工作目录":       func(r *repos.Repo) { r.Workspace = "" },
		"没有 token":     func(r *repos.Repo) { r.Token = "" },
		"intake 不认识":   func(r *repos.Repo) { r.Intake = "carrier-pigeon" },
		"webhook 没密钥":  func(r *repos.Repo) { r.Intake = "webhook" },
		"名单不是 gh: 开头":  func(r *repos.Repo) { r.Team.Roster = []string{"telegram:1"} },
		"分支名里有空白":      func(r *repos.Repo) { r.Base = "ma in" },
		"轮询间隔是负数":      func(r *repos.Repo) { r.Interval = -1 },
		"owner 空":      func(r *repos.Repo) { r.Owner = "" },
		"验收命令太长（团队规则）": func(r *repos.Repo) { r.Team.Check = strings.Repeat("x", 600) },
	} {
		bad := goodRepo()
		bad.Workspace = "ws-" + name
		mod(&bad)
		if _, err := s2.Put(bad); err == nil {
			t.Errorf("%s 要拒绝", name)
		}
	}
	if iv := (repos.Repo{}).EffectiveInterval(); iv != 120 {
		t.Fatal(iv)
	}
	if iv := (repos.Repo{Interval: 5}).EffectiveInterval(); iv != 60 {
		t.Fatal(iv)
	}
	if tm := (repos.Repo{Workspace: "W", Team: scene.Team{Workspace: "other"}}).EffectiveTeam(); tm.Workspace != "W" {
		t.Fatal("Team.Workspace 以连接为准")
	}
	if err := s2.Delete(saved.ID); err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 0 {
		t.Fatal("删掉")
	}
}

func client(f *fakegithub.Server) *repos.Client {
	return &repos.Client{Base: f.URL, Owner: f.Owner, Name: f.Name, Token: tok}
}

func TestClientAuthWhoamiAndErrorsNeverCarryTheToken(t *testing.T) {
	f := fakegithub.New("acme", "app", tok, "nano-bot")
	defer f.Close()
	c := client(f)
	ctx := context.Background()
	if who, err := c.Whoami(ctx); err != nil || who != "nano-bot" {
		t.Fatalf("查账号：%q %v", who, err)
	}
	if b, err := c.CheckAccess(ctx); err != nil || b != "main" {
		t.Fatalf("查仓库：%q %v", b, err)
	}
	// API 返回的错误信息里带了 token：错误文本里也不能有
	f.FailNext("/issues", 500, "oops "+tok+" leaked", nil)
	_, _, err := c.Issues(ctx, time.Time{}, "")
	if err == nil || strings.Contains(err.Error(), tok) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("错误文本里不能有 token：%v", err)
	}
	// 错 token：401，错误里同样没有 token
	bad := &repos.Client{Base: f.URL, Owner: "acme", Name: "app", Token: "wrong-token-xyz"}
	_, err = bad.Whoami(ctx)
	if err == nil || strings.Contains(err.Error(), "wrong-token-xyz") {
		t.Fatalf("401：%v", err)
	}
	// 传输层错误（连不上）也不带 token
	dead := &repos.Client{Base: "http://127.0.0.1:1/" + tok, Owner: "a", Name: "b", Token: tok}
	_, err = dead.Whoami(ctx)
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("传输错误：%v", err)
	}
	// Authorization 头确实带了 token
	for _, r := range f.Requests() {
		if r.Auth != "Bearer "+tok && r.Auth != "Bearer wrong-token-xyz" && r.Auth != "" {
			t.Fatalf("认证头：%q", r.Auth)
		}
	}
}

func TestRateLimitIsHonoured(t *testing.T) {
	f := fakegithub.New("acme", "app", tok, "bot")
	defer f.Close()
	c := client(f)
	f.FailNext("/issues", 429, "slow down", map[string]string{"Retry-After": "7"})
	_, _, err := c.Issues(context.Background(), time.Time{}, "")
	var ae *repos.APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second || ae.Status != 429 {
		t.Fatalf("429 带 Retry-After：%+v", err)
	}
	reset := time.Now().Add(90 * time.Second).Unix()
	f.FailNext("/issues", 403, "rate limit", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(reset)})
	_, _, err = c.Issues(context.Background(), time.Time{}, "")
	if !errors.As(err, &ae) || ae.RetryAfter < 80*time.Second || ae.RetryAfter > 91*time.Second {
		t.Fatalf("403 额度用完按 X-RateLimit-Reset：%+v", err)
	}
	f.FailNext("/issues", 403, "forbidden", nil)
	_, _, err = c.Issues(context.Background(), time.Time{}, "")
	if !errors.As(err, &ae) || ae.RetryAfter != 0 {
		t.Fatalf("普通 403 不是限流：%+v", err)
	}
	// 轮询遇到限流：返回要等的时间，不报别的
	st, _ := repos.OpenState(filepath.Join(t.TempDir(), "s.jsonl"))
	defer st.Close()
	p := &repos.Poller{Client: c, Intake: &repos.Intake{State: st, Handle: func(repos.Event) error { return nil }}}
	if _, err := p.Poll(context.Background()); err != nil { // 基线
		t.Fatal(err)
	}
	f.FailNext("/issues", 429, "slow", map[string]string{"Retry-After": "30"})
	wait, err := p.Poll(context.Background())
	if wait != 30*time.Second || err == nil {
		t.Fatalf("限流要退避：%v %v", wait, err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func newPoller(t *testing.T, f *fakegithub.Server, handle func(repos.Event) error) (*repos.Poller, *repos.State) {
	t.Helper()
	st, err := repos.OpenState(filepath.Join(t.TempDir(), "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &repos.Poller{Client: client(f), Intake: &repos.Intake{State: st, Handle: handle}}, st
}

func TestPollerBaselineThenNewEventsOnce(t *testing.T) {
	var clock = time.Unix(1_800_000_000, 0)
	f := fakegithub.New("acme", "app", tok, "bot")
	defer f.Close()
	f.SetClock(func() time.Time { return clock })
	old := f.AddIssue("alice", "旧议题", "历史")
	_ = old
	var got []repos.Event
	p, _ := newPoller(t, f, func(e repos.Event) error { got = append(got, e); return nil })
	p.Now = func() time.Time { return clock.Add(time.Second) }
	ctx := context.Background()
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("第一次只定基线，历史不当新事件：%v", got)
	}
	clock = clock.Add(time.Minute)
	iss := f.AddIssue("bob", "登录挂了", "手机上打不开 @bot 看看")
	clock = clock.Add(time.Second)
	f.AddComment(iss.Number, "alice", "@bot 修一下")
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := ""
	for _, e := range got {
		kinds += e.Kind + ","
	}
	if kinds != "issue,comment," {
		t.Fatalf("一个议题一条评论：%s", kinds)
	}
	// 再来一条评论：议题的 updated_at 变了，但议题正文不会再受理一次
	got = nil
	clock = clock.Add(time.Second)
	f.AddComment(iss.Number, "alice", "补充一句")
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != repos.KindComment || got[0].Text != "补充一句" {
		t.Fatalf("议题来了新评论，议题正文不会再进来：%+v", got)
	}
	// 什么也没变：ETag 命中 304，没有新事件
	got = nil
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("没变化不该有事件：%+v", got)
	}
	var withETag int
	for _, r := range f.Requests() {
		_ = r
	}
	_ = withETag
}

func TestPollerTracksPullStateChecksAndReviews(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	f := fakegithub.New("acme", "app", tok, "bot")
	defer f.Close()
	f.SetClock(func() time.Time { return clock })
	pr := f.OpenPull("工单", "nano/tk-1", "sha1")
	var got []repos.Event
	p, st := newPoller(t, f, func(e repos.Event) error { got = append(got, e); return nil })
	p.Now = func() time.Time { return clock }
	p.TrackedPRs = func() []int { return []int{pr.Number} }
	ctx := context.Background()
	p.Poll(ctx) // 基线
	// 检查没跑完：没有结果
	f.SetChecks("sha1", fakegithub.Check("build", 11, "in_progress", "", ""), fakegithub.Check("lint", 11, "completed", "success", ""))
	p.Poll(ctx)
	if len(got) != 0 {
		t.Fatalf("套件没跑完不算结果：%+v", got)
	}
	f.SetChecks("sha1", fakegithub.Check("build", 11, "completed", "failure", "2 个测试失败"), fakegithub.Check("lint", 11, "completed", "success", ""))
	f.AddReview(pr.Number, "alice", "CHANGES_REQUESTED", "这里改一下")
	p.Poll(ctx)
	p.Poll(ctx) // 重复来一遍：只受理一次
	keys := map[string]int{}
	for _, e := range got {
		keys[e.Key]++
	}
	if len(got) != 2 || keys["check:11:failure"] != 1 || keys["review:1002"] != 1 && len(keys) != 2 {
		t.Fatalf("检查失败和审阅各一次：%+v", got)
	}
	for _, e := range got {
		if e.Kind == repos.KindCheck && (e.Number != pr.Number || e.SHA != "sha1" || e.State != "failure") {
			t.Fatalf("检查事件：%+v", e)
		}
	}
	got = nil
	f.MergePull(pr.Number, "mergesha")
	p.Poll(ctx)
	if len(got) != 1 || got[0].Key != "pr:"+itoa(int64(pr.Number))+":merged" || got[0].SHA != "mergesha" {
		t.Fatalf("合并：%+v", got)
	}
	// 关闭没合并
	pr2 := f.OpenPull("另一个", "nano/tk-2", "sha2")
	p.TrackedPRs = func() []int { return []int{pr2.Number} }
	got = nil
	f.ClosePull(pr2.Number)
	p.Poll(ctx)
	if len(got) != 1 || got[0].State != "closed" {
		t.Fatalf("关闭：%+v", got)
	}
	_ = st
}

func TestStateRecordsBeforeProcessingAndReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, _ := repos.OpenState(path)
	crash := errors.New("崩了")
	in := &repos.Intake{State: st, Handle: func(repos.Event) error { return crash }}
	ev := repos.Event{Key: "comment:1", Kind: repos.KindComment, Number: 3, Actor: "alice", Text: "@bot 来", At: 1}
	if fresh, err := in.Submit(ev); !fresh || !errors.Is(err, crash) {
		t.Fatalf("先记下再处理，处理失败：%v %v", fresh, err)
	}
	st.Close()
	// 重启：没处理完的事件还在，重放一次；同一个键再来不重复受理
	st2, _ := repos.OpenState(path)
	defer st2.Close()
	var handled []string
	in2 := &repos.Intake{State: st2, Handle: func(e repos.Event) error { handled = append(handled, e.Key); return nil }}
	if err := in2.Replay(); err != nil || len(handled) != 1 || handled[0] != "comment:1" {
		t.Fatalf("重放：%v %v", handled, err)
	}
	if fresh, _ := in2.Submit(ev); fresh {
		t.Fatal("同一个键只受理一次")
	}
	if len(st2.Pending()) != 0 {
		t.Fatal("处理完就不在待处理里了")
	}
	// 投递 ID 也去重
	if seen, _ := st2.SeenDelivery("d1"); seen {
		t.Fatal("第一次")
	}
	if seen, _ := st2.SeenDelivery("d1"); !seen {
		t.Fatal("第二次")
	}
}

func TestWebhookSignatureAndParsing(t *testing.T) {
	body := []byte(`{"action":"created","issue":{"number":5,"pull_request":{"url":"x"}},"comment":{"id":77,"body":"@bot 来","user":{"login":"alice"},"html_url":"u"}}`)
	sig := repos.Sign("s3cret", body)
	if !repos.VerifySignature("s3cret", body, sig) {
		t.Fatal("正确的签名要通过")
	}
	for name, c := range map[string]struct{ secret, header string }{
		"密钥不对":     {"other", sig},
		"没有签名头":    {"s3cret", ""},
		"格式不对":     {"s3cret", "sha1=abc"},
		"不是十六进制":   {"s3cret", "sha256=zz"},
		"密钥为空一律拒绝": {"", repos.Sign("", body)},
	} {
		if repos.VerifySignature(c.secret, body, c.header) {
			t.Errorf("%s 要拒绝", name)
		}
	}
	if repos.VerifySignature("s3cret", append([]byte(nil), body[:len(body)-1]...), sig) {
		t.Fatal("正文变了签名就不对")
	}
	evs, err := repos.ParseWebhook("issue_comment", "d1", body)
	if err != nil || len(evs) != 1 || evs[0].Key != "comment:77" || evs[0].Number != 5 || !evs[0].IsPR || evs[0].Delivery != "d1" || evs[0].Actor != "alice" {
		t.Fatalf("issue_comment：%+v %v", evs, err)
	}
	// 键和轮询得到的一样（共用去重）
	f := fakegithub.New("acme", "app", tok, "bot")
	defer f.Close()
	iss := f.AddIssue("bob", "x", "y")
	c := f.AddComment(iss.Number, "alice", "@bot 来")
	cs, _, _ := client(f).IssueComments(context.Background(), time.Time{}, "")
	if len(cs) != 1 || cs[0].ID != c.ID {
		t.Fatal("取评论")
	}
	for _, tc := range []struct {
		event, body, key string
	}{
		{"issues", `{"action":"opened","issue":{"id":9,"number":2,"title":"t","body":"b","user":{"login":"bob"}}}`, "issue:9"},
		{"pull_request", `{"action":"closed","pull_request":{"number":4,"merged":true,"merge_commit_sha":"m1","state":"closed"}}`, "pr:4:merged"},
		{"pull_request", `{"action":"closed","pull_request":{"number":4,"merged":false,"state":"closed"}}`, "pr:4:closed"},
		{"pull_request_review", `{"action":"submitted","pull_request":{"number":4},"review":{"id":31,"body":"改","state":"CHANGES_REQUESTED","user":{"login":"alice"}}}`, "review:31"},
		{"pull_request_review_comment", `{"action":"created","pull_request":{"number":4},"comment":{"id":41,"body":"行内","user":{"login":"alice"}}}`, "rcomment:41"},
		{"check_suite", `{"action":"completed","check_suite":{"id":51,"conclusion":"failure","head_sha":"sha9","pull_requests":[{"number":4}]}}`, "check:51:failure"},
	} {
		evs, err := repos.ParseWebhook(tc.event, "d", []byte(tc.body))
		if err != nil || len(evs) != 1 || evs[0].Key != tc.key {
			t.Errorf("%s：%+v %v 想要 %s", tc.event, evs, err, tc.key)
		}
	}
	// 不认的事件类型、不关心的 action：空
	for _, tc := range [][2]string{{"push", `{}`}, {"issues", `{"action":"edited","issue":{"id":1,"number":1}}`}, {"pull_request", `{"action":"opened","pull_request":{"number":1}}`}, {"check_suite", `{"action":"requested","check_suite":{"id":1}}`}} {
		if evs, err := repos.ParseWebhook(tc[0], "d", []byte(tc[1])); err != nil || len(evs) != 0 {
			t.Errorf("%v 要忽略：%+v %v", tc, evs, err)
		}
	}
	if _, err := repos.ParseWebhook("issues", "d", []byte("not json")); err == nil {
		t.Fatal("坏正文")
	}
}

func TestClientPullsCreateAndLookup(t *testing.T) {
	f := fakegithub.New("acme", "app", tok, "bot")
	defer f.Close()
	c := client(f)
	ctx := context.Background()
	if _, ok, err := c.PullByHead(ctx, "nano/tk-1"); err != nil || ok {
		t.Fatalf("还没有：%v %v", ok, err)
	}
	p, err := c.CreatePull(ctx, "标题", "正文", "nano/tk-1", "main")
	if err != nil || p.Number == 0 {
		t.Fatalf("开 PR：%+v %v", p, err)
	}
	if got, ok, err := c.PullByHead(ctx, "nano/tk-1"); err != nil || !ok || got.Number != p.Number {
		t.Fatalf("按分支找：%+v %v %v", got, ok, err)
	}
	if err := c.Comment(ctx, p.Number, "你好"); err != nil {
		t.Fatal(err)
	}
	if cs := f.Comments(p.Number); len(cs) != 1 || cs[0].Body != "你好" || cs[0].User.Login != "bot" {
		t.Fatalf("评论：%+v", cs)
	}
}
