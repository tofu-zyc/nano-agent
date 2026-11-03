package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/repos"
)

// GitHub 仓库连接（设计 §16.3）：data/repos.json 和每个仓库一份状态日志 data/runtime/repos/<id>.jsonl。
// 事件进来有两种方式（每个仓库选一种）：轮询，或 webhook；都变成同样的事件、按 GitHub 对象去重、
// 由 handleRepoEvent 处理，并写进事件日志（repo.*，Hook 可以订阅；不带正文）。

// repoRT 是一个仓库连接运行时的状态。
type repoRT struct {
	id     string
	state  *repos.State
	intake *repos.Intake
	stop   context.CancelFunc
	pollMu sync.Mutex // 一个仓库同一时刻只轮询一次
}

// openRepos 打开仓库连接。打不开就保留现场，网关其余照旧。
func (a *App) openRepos() {
	path := filepath.Join(a.store.Dir(), "repos.json")
	s, err := repos.Open(path)
	if err != nil {
		a.log.Error("repos.json 打不开，保留现场", "err", err)
		return
	}
	a.repos = s
	a.repoRTs = map[string]*repoRT{}
}

// startRepos 让每个连接开始接事件（启动时调用一次，调用方不持有锁）。
func (a *App) startRepos() {
	if a.repos == nil {
		return
	}
	for _, r := range a.repos.List() {
		a.ensureGitHubChannel()
		a.startRepo(r)
	}
}

func (a *App) repoStatePath(id string) string {
	return filepath.Join(a.store.Dir(), "runtime", "repos", id+".jsonl")
}

// repoClient 是这个连接的 API 客户端；token 解密只在这里。
func (a *App) repoClient(r repos.Repo) (*repos.Client, error) {
	tok, err := r.PlainToken()
	if err != nil {
		return nil, errors.New("token 解不开")
	}
	return &repos.Client{Base: a.repoAPIBase, Owner: r.Owner, Name: r.Name, Token: tok}, nil
}

// startRepo （重新）启动一个连接：打开状态、重放没处理完的事件、轮询的开轮询循环。
func (a *App) startRepo(r repos.Repo) {
	a.stopRepo(r.ID)
	st, err := repos.OpenState(a.repoStatePath(r.ID))
	if err != nil {
		a.log.Error("仓库的状态日志打不开", "repo", r.Slug(), "err", err)
		return
	}
	rt := &repoRT{id: r.ID, state: st}
	rt.intake = &repos.Intake{State: st, Handle: func(ev repos.Event) error { return a.handleRepoEvent(r.ID, ev) }}
	ctx, cancel := context.WithCancel(a.ctx)
	rt.stop = cancel
	a.mu.Lock()
	a.repoRTs[r.ID] = rt
	a.mu.Unlock()
	go func() {
		if err := rt.intake.Replay(); err != nil {
			a.log.Warn("重放仓库事件失败", "repo", r.Slug(), "err", err)
		}
	}()
	if r.Intake == "webhook" || a.repoNoLoop {
		return
	}
	go a.pollLoop(ctx, r.ID)
}

func (a *App) stopRepo(id string) {
	a.mu.Lock()
	rt := a.repoRTs[id]
	delete(a.repoRTs, id)
	a.mu.Unlock()
	if rt != nil {
		rt.stop()
		rt.pollMu.Lock()
		rt.state.Close()
		rt.pollMu.Unlock()
	}
}

// pollLoop 按间隔轮询；被限流就按它说的时间退避，不加快重试。
func (a *App) pollLoop(ctx context.Context, id string) {
	for {
		r, ok := a.repos.Get(id)
		if !ok {
			return
		}
		wait := time.Duration(r.EffectiveInterval()) * time.Second
		if w, err := a.PollRepo(ctx, id); err != nil {
			a.log.Warn("轮询仓库失败", "repo", r.Slug(), "err", err.Error())
			if w > wait {
				wait = w
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// PollRepo 做一轮轮询，返回要退避多久（被限流时）。测试里也直接调用。调用方不持有锁。
func (a *App) PollRepo(ctx context.Context, id string) (time.Duration, error) {
	a.mu.Lock()
	rt := a.repoRTs[id]
	a.mu.Unlock()
	r, ok := a.repos.Get(id)
	if rt == nil || !ok {
		return 0, errors.New("没有这个连接")
	}
	c, err := a.repoClient(r)
	if err != nil {
		return 0, err
	}
	rt.pollMu.Lock()
	defer rt.pollMu.Unlock()
	p := &repos.Poller{Client: c, Intake: rt.intake, Now: a.now, TrackedPRs: func() []int { return a.trackedPRs(r) }}
	return p.Poll(ctx)
}

// handleRepoEvent 处理一件事：写进事件日志（repo.*，不带正文），再交给议题渠道和工单。
func (a *App) handleRepoEvent(id string, ev repos.Event) error {
	a.mu.Lock()
	r, ok := a.repos.Get(id)
	if ok {
		a.emitRepoLog(r, ev, "repo."+ev.Kind)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	return a.dispatchRepoEvent(r, ev)
}

// emitRepoLog 把事件记进事件日志：只有种类、号、账号、键和状态，没有正文（名单外的人写的东西不进日志）。调用方持有锁。
func (a *App) emitRepoLog(r repos.Repo, ev repos.Event, name string) {
	if a.jobEvents == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"repo": r.Slug(), "number": ev.Number, "actor": ev.Actor, "key": ev.Key, "state": ev.State, "sha": ev.SHA})
	a.runtimeEventSeq++
	now := a.now().Unix()
	agent := r.Agent
	if agent == "" {
		agent = "default"
	}
	e := eventlog.Event{Name: name, SourceID: "repo:" + r.ID, SourceEpoch: a.dispatchBoot, Seq: a.runtimeEventSeq, OccurredAt: now, ObservedAt: now,
		AgentID: agent, Data: raw, Silent: true}
	e.ID = eventlog.EventID(e.SourceID, e.SourceEpoch, e.Seq)
	a.lastRepoEventID = e.ID
	engine := a.hookEngine
	if err := a.jobEvents.Submit(eventlog.Transaction{Event: &e}, func(err error) {
		if err != nil {
			a.log.Warn("仓库事件未落盘", "event", name, "err", err)
		} else if engine != nil {
			engine.Wake()
		}
	}); err != nil {
		a.log.Warn("仓库事件未入队", "event", name, "err", err)
	}
}

// 议题渠道和工单对事件的处理（后面几个提交接上）。
var dispatchRepoEventHook func(a *App, r repos.Repo, ev repos.Event) error

func (a *App) dispatchRepoEvent(r repos.Repo, ev repos.Event) error {
	if dispatchRepoEventHook != nil {
		return dispatchRepoEventHook(a, r, ev)
	}
	return a.routeRepoEvent(r, ev)
}

// trackedPRs 是工单相关的、还开着的 PR（轮询要看它们的状态和检查）。
func (a *App) trackedPRs(r repos.Repo) []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.repoTrackedPRsLocked(r)
}

// ---- webhook ----

// RepoWebhook 收一次 webhook 投递，返回 HTTP 状态码。签名校验、事件类型白名单、先记再回 2xx 都在这里（不在 web 层），
// 这样不会被别的入口绕过。只在这个仓库选了 webhook、配了密钥时才接，否则和 ID 不存在一样返回 404；签名不对 401。
func (a *App) RepoWebhook(id, event, delivery, signature string, body []byte) int {
	r, ok := a.repos.Get(id)
	if !ok || r.Intake != "webhook" || r.Secret == "" {
		return http.StatusNotFound
	}
	if len(body) > repos.MaxWebhookBody {
		return http.StatusRequestEntityTooLarge
	}
	secret, err := r.PlainSecret()
	if err != nil || !repos.VerifySignature(secret, body, signature) {
		return http.StatusUnauthorized
	}
	a.mu.Lock()
	rt := a.repoRTs[id]
	a.mu.Unlock()
	if rt == nil {
		return http.StatusServiceUnavailable
	}
	rt.pollMu.Lock() // 状态日志不加锁：和轮询串行
	defer rt.pollMu.Unlock()
	if seen, err := rt.state.SeenDelivery(delivery); err != nil {
		return http.StatusInternalServerError
	} else if seen {
		return http.StatusOK // 同一次投递重发：直接丢
	}
	events, err := repos.ParseWebhook(event, delivery, body)
	if err != nil {
		return http.StatusBadRequest
	}
	for _, ev := range events {
		// 先记下（Submit 先写状态日志再处理）；处理失败的留在待处理里，下次重放，投递照样回 2xx
		if _, err := rt.intake.Submit(ev); err != nil {
			a.log.Warn("处理 webhook 事件失败", "repo", r.Slug(), "event", ev.Key, "err", err.Error())
		}
	}
	return http.StatusAccepted
}

// ---- 网页上填仓库连接 ----

// RepoView 是网页上看到的一个连接：token 和密钥不回显（也不是加密的形式），只说有没有设。
type RepoView struct {
	ID          string   `json:"id"`
	Workspace   string   `json:"workspace"`
	Owner       string   `json:"owner"`
	Name        string   `json:"name"`
	Base        string   `json:"base"`
	Agent       string   `json:"agent,omitempty"`
	Bot         string   `json:"bot"`
	Intake      string   `json:"intake"`
	Interval    int      `json:"interval"`
	Roster      []string `json:"roster"`
	Check       string   `json:"check,omitempty"`
	Budget      int      `json:"budget,omitempty"`
	TokenSet    bool     `json:"token_set"`
	SecretSet   bool     `json:"secret_set"`
	WebhookPath string   `json:"webhook_path,omitempty"`
}

func repoView(r repos.Repo) RepoView {
	v := RepoView{ID: r.ID, Workspace: r.Workspace, Owner: r.Owner, Name: r.Name, Base: r.Base, Agent: r.Agent, Bot: r.Bot, Intake: r.Intake,
		Interval: r.EffectiveInterval(), Roster: append([]string{}, r.Team.Roster...), Check: r.Team.Check, Budget: r.Team.Budget,
		TokenSet: r.Token != "", SecretSet: r.Secret != ""}
	if r.Intake == "webhook" {
		v.WebhookPath = "/api/repos/" + r.ID + "/webhook"
	}
	return v
}

// Repos 列出全部连接。
func (a *App) Repos() []RepoView {
	out := []RepoView{}
	if a.repos == nil {
		return out
	}
	for _, r := range a.repos.List() {
		out = append(out, repoView(r))
	}
	return out
}

// RepoSave 是保存连接的请求：Token、Secret 空表示不改。
type RepoSave struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	Owner     string   `json:"owner"`
	Name      string   `json:"name"`
	Base      string   `json:"base"`
	Agent     string   `json:"agent"`
	Token     string   `json:"token"`
	Intake    string   `json:"intake"`
	Interval  int      `json:"interval"`
	Secret    string   `json:"secret"`
	Roster    []string `json:"roster"`
	Check     string   `json:"check"`
	Budget    int      `json:"budget"`
}

// SaveRepo 保存一个连接：先用 token 查一次账号（填 Bot）和这个仓库能不能访问，不通就不保存。
func (a *App) SaveRepo(req RepoSave) (RepoView, error) {
	if a.repos == nil {
		return RepoView{}, errors.New("仓库连接现在不可用")
	}
	r := repos.Repo{ID: req.ID, Workspace: strings.TrimSpace(req.Workspace), Owner: strings.TrimSpace(req.Owner), Name: strings.TrimSpace(req.Name),
		Base: strings.TrimSpace(req.Base), Agent: req.Agent, Token: req.Token, Intake: req.Intake, Interval: req.Interval, Secret: req.Secret}
	r.Team.Roster, r.Team.Check, r.Team.Budget = req.Roster, req.Check, req.Budget
	if old, ok := a.repos.Get(req.ID); ok && req.ID != "" {
		if r.Token == "" {
			r.Token = old.Token
		}
		if r.Secret == "" {
			r.Secret = old.Secret
		}
	}
	plainTok, err := r.PlainToken()
	if err != nil {
		return RepoView{}, errors.New("token 解不开")
	}
	if err := r.Validate(); err != nil {
		return RepoView{}, err
	}
	a.mu.Lock()
	ws := a.findWorkspace(r.Workspace)
	a.mu.Unlock()
	if ws == nil {
		return RepoView{}, errors.New("没有这个工作目录")
	}
	c := &repos.Client{Base: a.repoAPIBase, Owner: r.Owner, Name: r.Name, Token: plainTok}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	bot, err := c.Whoami(ctx)
	if err != nil {
		return RepoView{}, fmt.Errorf("用这个 token 查不到账号：%w", err)
	}
	if _, err := c.CheckAccess(ctx); err != nil {
		return RepoView{}, fmt.Errorf("这个 token 访问不了 %s/%s：%w", r.Owner, r.Name, err)
	}
	r.Bot = bot
	saved, err := a.repos.Put(r)
	if err != nil {
		return RepoView{}, err
	}
	a.ensureGitHubChannel()
	a.startRepo(saved)
	return repoView(saved), nil
}

// DeleteRepo 删掉一个连接。
func (a *App) DeleteRepo(id string) error {
	if a.repos == nil {
		return errors.New("仓库连接现在不可用")
	}
	if err := a.repos.Delete(id); err != nil {
		return err
	}
	a.stopRepo(id)
	return nil
}

func randHexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetRepoAPIBase 换 GitHub API 的地址（GitHub Enterprise；测试里是假 GitHub）。要在连接开始接事件之前调用。
func (a *App) SetRepoAPIBase(base string) {
	a.mu.Lock()
	a.repoAPIBase = base
	a.mu.Unlock()
}

// SetRepoNoLoop 不开轮询循环（测试里自己一轮一轮地轮询）。
func (a *App) SetRepoNoLoop(v bool) {
	a.mu.Lock()
	a.repoNoLoop = v
	a.mu.Unlock()
}
