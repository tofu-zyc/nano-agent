package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/repos"
	"nano-agent/gateway/internal/repos/fakegithub"
)

const testToken = "ghp_testtoken_abc123"

type repoEnv struct {
	*env
	gh   *fakegithub.Server
	ws   string
	mu   sync.Mutex
	seen []repos.Event
}

// newRepoEnv 起一个假 GitHub，连上一个本地工作目录；dispatch 钩子把收到的事件记下来。
func newRepoEnv(t *testing.T, intake string) (*repoEnv, RepoView) {
	t.Helper()
	e := setup(t)
	gh := fakegithub.New("acme", "app", testToken, "nano-bot")
	t.Cleanup(gh.Close)
	e.a.SetRepoAPIBase(gh.URL)
	e.a.SetRepoNoLoop(true)
	ws, err := e.a.AddWorkspace(context.Background(), host.LocalName, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	re := &repoEnv{env: e, gh: gh, ws: ws.ID}
	old := dispatchRepoEventHook
	dispatchRepoEventHook = func(a *App, r repos.Repo, ev repos.Event) error {
		re.mu.Lock()
		re.seen = append(re.seen, ev)
		re.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { dispatchRepoEventHook = old })
	req := RepoSave{Workspace: ws.ID, Owner: "acme", Name: "app", Token: testToken, Intake: intake, Roster: []string{"gh:alice"}}
	if intake == "webhook" {
		req.Secret = "whsec"
	}
	v, err := e.a.SaveRepo(req)
	if err != nil {
		t.Fatal(err)
	}
	return re, v
}

func (re *repoEnv) events() []repos.Event {
	re.mu.Lock()
	defer re.mu.Unlock()
	return append([]repos.Event(nil), re.seen...)
}

func TestSaveRepoChecksTheTokenAndNeverEchoesIt(t *testing.T) {
	re, v := newRepoEnv(t, "poll")
	if v.Bot != "nano-bot" || !v.TokenSet || v.ID == "" || v.Interval != 120 {
		t.Fatalf("保存时用 token 查出 Bot：%+v", v)
	}
	raw, _ := json.Marshal(re.a.Repos())
	if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), "enc:v1") {
		t.Fatalf("网页上看不到 token：%s", raw)
	}
	// 错的 token：不保存，错误里没有 token
	_, err := re.a.SaveRepo(RepoSave{Workspace: re.ws, Owner: "acme", Name: "other", Token: "wrong_token_zzz", Intake: "poll"})
	if err == nil || strings.Contains(err.Error(), "wrong_token_zzz") {
		t.Fatalf("错 token：%v", err)
	}
	if len(re.a.Repos()) != 1 {
		t.Fatal("不通就不保存")
	}
	// 不存在的工作目录
	if _, err := re.a.SaveRepo(RepoSave{Workspace: "no-such", Owner: "acme", Name: "x", Token: testToken}); err == nil {
		t.Fatal("没有这个工作目录")
	}
	// 更新时 token 留空表示不改
	v2, err := re.a.SaveRepo(RepoSave{ID: v.ID, Workspace: re.ws, Owner: "acme", Name: "app", Intake: "poll", Interval: 300, Roster: []string{"gh:alice", "gh:bob"}})
	if err != nil || v2.Interval != 300 || len(v2.Roster) != 2 || v2.Bot != "nano-bot" {
		t.Fatalf("更新：%+v %v", v2, err)
	}
	if err := re.a.DeleteRepo(v.ID); err != nil || len(re.a.Repos()) != 0 {
		t.Fatalf("删除：%v", err)
	}
}

func TestWebhookIsRefusedUnlessChosenSignedAndWithinSize(t *testing.T) {
	re, v := newRepoEnv(t, "webhook")
	body := []byte(`{"action":"created","issue":{"number":3},"comment":{"id":501,"body":"@nano-bot 来","user":{"login":"alice"}}}`)
	sig := repos.Sign("whsec", body)
	if code := re.a.RepoWebhook("no-such-id", "issue_comment", "d0", sig, body); code != 404 {
		t.Fatalf("没有这个连接：%d", code)
	}
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d1", repos.Sign("wrong", body), body); code != 401 {
		t.Fatalf("签名不对：%d", code)
	}
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d1", "", body); code != 401 {
		t.Fatalf("没有签名：%d", code)
	}
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d1", sig, make([]byte, repos.MaxWebhookBody+1)); code != 413 {
		t.Fatalf("太大：%d", code)
	}
	if len(re.events()) != 0 {
		t.Fatal("被拒的不处理")
	}
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d1", sig, body); code != 202 {
		t.Fatalf("正确的：%d", code)
	}
	if evs := re.events(); len(evs) != 1 || evs[0].Key != "comment:501" || evs[0].Actor != "alice" {
		t.Fatalf("事件：%+v", evs)
	}
	// 同一次投递重发：丢；换个投递 ID 但是同一条评论：键一样，也只受理一次
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d1", sig, body); code != 200 {
		t.Fatalf("重发：%d", code)
	}
	if code := re.a.RepoWebhook(v.ID, "issue_comment", "d2", sig, body); code != 202 {
		t.Fatalf("另一次投递：%d", code)
	}
	if len(re.events()) != 1 {
		t.Fatalf("同一件事只受理一次：%+v", re.events())
	}
	// 不认的事件类型：签名对也不处理
	push := []byte(`{"ref":"x"}`)
	if code := re.a.RepoWebhook(v.ID, "push", "d3", repos.Sign("whsec", push), push); code != 202 || len(re.events()) != 1 {
		t.Fatalf("不认的类型：%d", code)
	}
	// 事件日志里有 repo.comment，没有正文
	re.a.mu.Lock()
	id := re.a.lastRepoEventID
	re.a.mu.Unlock()
	if id == "" {
		t.Fatal("没写进事件日志")
	}
	// 选了轮询的仓库：webhook 和不存在一样 404
	re2, v2 := newRepoEnv(t, "poll")
	if code := re2.a.RepoWebhook(v2.ID, "issue_comment", "d1", repos.Sign("whsec", body), body); code != 404 {
		t.Fatalf("没选 webhook：%d", code)
	}
}

func TestPollRepoDeliversEventsOnceAndLogsWithoutBody(t *testing.T) {
	re, v := newRepoEnv(t, "poll")
	ctx := context.Background()
	if _, err := re.a.PollRepo(ctx, v.ID); err != nil { // 基线
		t.Fatal(err)
	}
	iss := re.gh.AddIssue("bob", "登录挂了", "机密正文 @nano-bot")
	re.gh.AddComment(iss.Number, "alice", "@nano-bot 看看")
	if _, err := re.a.PollRepo(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := re.a.PollRepo(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	evs := re.events()
	if len(evs) != 2 || evs[0].Kind != repos.KindIssue || evs[1].Kind != repos.KindComment {
		t.Fatalf("各一次：%+v", evs)
	}
	re.a.mu.Lock()
	rec, ok := re.a.jobEvents.Event(re.a.lastRepoEventID)
	re.a.mu.Unlock()
	if !ok || rec.Name != "repo.comment" {
		t.Fatalf("事件日志里有 repo.comment：%+v %v", rec, ok)
	}
	if b, _ := json.Marshal(rec); strings.Contains(string(b), "看看") || strings.Contains(string(b), "机密正文") {
		t.Fatalf("事件日志里不带正文：%s", b)
	}
	// 重启后基线和去重还在：重新打开状态，同样的事件不再受理
	re.a.stopRepo(v.ID)
	r, _ := re.a.repos.Get(v.ID)
	re.a.startRepo(r)
	if _, err := re.a.PollRepo(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if len(re.events()) != 2 {
		t.Fatalf("重启后不重复：%+v", re.events())
	}
}
