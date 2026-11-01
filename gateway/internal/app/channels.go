package app

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"nano-agent/gateway/internal/agents"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/health"
)

// 渠道管理：按配置启动、停止、重启各个聊天渠道。网页里改了渠道设置时调用 ReconcileChannels，
// 不用重启网关。命令行渠道接着终端，只在启动时打开。

// ChannelInfo 是一个渠道的情况，网页的拓扑图用。
type ChannelInfo struct {
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Status  channel.Status `json:"status"`
}

type chanRun struct {
	ch     channel.Channel
	conf   any // 启动时用的配置，变了就要重启
	cancel context.CancelFunc
	done   chan struct{}
}

// 拓扑图里渠道的顺序
var channelOrder = []string{"cli", "web", "telegram", "feishu"}

// SetCLI 登记终端渠道（由 main 创建）。终端窗口一开始就接在主会话上（拓扑图里能看到），
// 不用等第一条消息；终端连上来时打个招呼，附上最近的对话。
func (a *App) SetCLI(c *channel.CLI) {
	a.cli = c
	c.OnConnect(a.cliConnected)
	a.mu.Lock()
	defer a.mu.Unlock()
	if key := c.Name() + ":local"; a.eps[key] == nil {
		a.eps[key] = &epRec{Session: mainSID}
		a.saveEndpoints()
	}
}

// Web 返回网页聊天渠道。
func (a *App) Web() *channel.Web { return a.web }

// ReconcileChannels 让正在运行的渠道和配置一致：该开的开，该关的关，配置变了的重启。
func (a *App) ReconcileChannels(cfg *config.Config) {
	// 先取一份机器人列表（要拿 a.mu），再进 chMu：锁的顺序只能是 a.mu → chMu，
	// 反过来的话会和 Status()（拿着 a.mu 读渠道）互相卡住
	a.mu.Lock()
	bots := []agents.Bot{}
	if a.botStore != nil {
		bots = a.botStore.List()
	}
	a.mu.Unlock()
	a.chMu.Lock()
	defer a.chMu.Unlock()
	if a.runs == nil {
		a.runs, a.chErr = map[string]*chanRun{}, map[string]string{}
		a.chHealth = map[string]*health.Handle{}
	}
	if a.web == nil {
		a.web = channel.NewWeb()
		a.startChannel("web", a.web, nil)
	}
	if a.cli != nil && a.runs["cli"] == nil {
		a.startChannel("cli", a.cli, nil)
	}
	if a.cli != nil {
		a.cli.SetDisplay(config.On(cfg.CLI.ShowReasoning), config.On(cfg.CLI.ShowUsage))
	}
	a.reconcileBotsLocked(cfg, bots)
}

func (a *App) reconcile(name string, want bool, conf any, build func() (channel.Channel, error)) {
	cur := a.runs[name]
	if cur != nil && (!want || !reflect.DeepEqual(cur.conf, conf)) {
		a.stopChannel(name)
		cur = nil
	}
	if want && cur == nil {
		h := a.claimChannelHealth(name)
		ch, err := build()
		if err != nil {
			a.log.Error("启动渠道失败", "channel", name, "err", err)
			a.chErr[name] = err.Error()
			h.Set(health.Error, err.Error())
			a.publishChannel(name)
			return
		}
		delete(a.chErr, name)
		a.startChannel(name, ch, conf)
	}
	if !want {
		delete(a.chErr, name)
		a.releaseChannelHealth(name)
		a.publishChannel(name)
	}
}

// channel:<名字> 健康条目只在这里认领和释放：启用的渠道有条目（构建失败也有，显示 error），
// 停用或被删除的没有。调用方持有 chMu。
func (a *App) claimChannelHealth(name string) *health.Handle {
	a.releaseChannelHealth(name)
	h, _ := a.health.Claim("channel:" + name)
	a.chHealth[name] = h
	return h
}

func (a *App) releaseChannelHealth(name string) {
	a.chHealth[name].Release()
	delete(a.chHealth, name)
}

// channelErrorGrace 是渠道自己报的 error 要持续多久才算 error，之前算 degraded：
// 一次网络抖动不该让 nano-gateway status 返回 3。
const channelErrorGrace = time.Minute

// channelWatch 把一个渠道报来的状态写进它的健康条目，并给渠道自己报的 error 一段宽限。
type channelWatch struct {
	h     *health.Handle
	grace time.Duration

	mu       sync.Mutex
	since    time.Time // 这一次连续出错的开始；零值表示现在没在出错
	promoted bool      // 宽限已过，条目是 error
	detail   string
}

func (w *channelWatch) report(st channel.Status) {
	state, detail := channelHealth(st)
	w.mu.Lock()
	defer w.mu.Unlock()
	switch st.State {
	case channel.StateOnline, channel.StateIdle:
		w.since, w.promoted = time.Time{}, false // 只有这两种算恢复
		w.h.Set(state, detail)
		return
	case channel.StateStarting:
		if !w.since.IsZero() {
			return // 出错期间重试前报的“连接中”不算恢复：保持当前状态，计时照走
		}
		w.h.Set(state, detail)
		return
	case channel.StateError:
	default: // stopped 等：直接按状态写，不再有“正在出错”的计时
		w.since, w.promoted = time.Time{}, false
		w.h.Set(state, detail)
		return
	}
	w.detail = detail
	if w.since.IsZero() {
		w.since = time.Now()
		since := w.since
		time.AfterFunc(w.grace, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.since.Equal(since) { // 中间恢复过又出错的，是另一次，不能被前一次的定时器提前判成 error
				w.promoted = true
				w.h.Set(health.Error, w.detail)
			}
		})
	}
	if w.promoted {
		w.h.Set(health.Error, detail)
	} else {
		w.h.Set(health.Degraded, detail)
	}
}

// channelHealth 把渠道自己报的状态换成健康状态：出错和停止（启用的渠道不该停）是 error，
// 连接中和等人接入还算正常。
func channelHealth(st channel.Status) (health.State, string) {
	switch st.State {
	case channel.StateError:
		if st.Error == "" {
			return health.Error, "出错"
		}
		return health.Error, st.Error
	case channel.StateStopped:
		return health.Error, "已停止"
	case channel.StateStarting:
		return health.OK, "连接中"
	case channel.StateIdle:
		return health.OK, "等待接入"
	}
	return health.OK, ""
}

func (a *App) startChannel(name string, ch channel.Channel, conf any) {
	ctx, cancel := context.WithCancel(a.ctx)
	r := &chanRun{ch: ch, conf: conf, cancel: cancel, done: make(chan struct{})}
	a.runs[name] = r
	a.AddChannel(ch)
	// 网页和终端不经过 reconcile，没有健康条目（h 为 nil，什么也不做）。
	// 渠道停止之后迟到的回报写不到这里：stopChannel 先释放了 h。
	h := a.chHealth[name]
	if sr, ok := ch.(channel.StatusReporter); ok {
		w := &channelWatch{h: h, grace: a.channelGrace}
		sr.OnStatus(func(st channel.Status) {
			w.report(st)
			a.publishChannel(name)
		})
		w.report(sr.Status())
	}
	go func() {
		defer close(r.done)
		err := ch.Run(ctx, a.OnMessage)
		if err != nil {
			a.log.Error("聊天渠道退出", "channel", name, "err", err)
		}
		if ctx.Err() == nil {
			detail := "渠道已退出"
			if err != nil {
				detail += "：" + err.Error()
			}
			h.Set(health.Error, detail)
		}
		a.publishChannel(name)
	}()
	a.log.Info("渠道已启动", "channel", name)
	a.publishChannel(name)
}

func (a *App) stopChannel(name string) {
	a.releaseChannelHealth(name)
	r := a.runs[name]
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		a.log.Warn("渠道停止超时", "channel", name)
	}
	delete(a.runs, name)
	a.RemoveChannel(r.ch) // 待发消息留在出站里，渠道回来再发
	a.log.Info("渠道已停止", "channel", name)
}

// Channels 返回所有渠道（包括没启用的）的情况：终端、网页、内置的两个机器人，
// 再加另外加的机器人渠道（tg.<id>、feishu.<id>）。
func (a *App) Channels() []ChannelInfo {
	a.chMu.Lock()
	defer a.chMu.Unlock()
	out := make([]ChannelInfo, 0, len(channelOrder))
	for _, name := range channelOrder {
		out = append(out, a.channelInfo(name))
	}
	var extra []string
	for name := range a.runs {
		if isBotChannel(name) && !slices.Contains(channelOrder, name) && !slices.Contains(extra, name) {
			extra = append(extra, name)
		}
	}
	// 停用的机器人也要在图上：没在跑的时候 a.runs 里没有它。
	// 用 botRunKey：内置的 Telegram 在 runs 和图上叫 telegram，它的消息渠道名却是 tg，按渠道名会多出一个“Telegram”
	if a.botStore != nil {
		for _, b := range a.botStore.List() {
			name := botRunKey(b)
			if !isBotChannel(name) || slices.Contains(channelOrder, name) || slices.Contains(extra, name) {
				continue
			}
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, a.channelInfo(name))
	}
	return out
}

// reconcileBotsLocked 按机器人列表启动、停止、重启机器人渠道。调用方持有 chMu，不能再去拿 a.mu。
func (a *App) reconcileBotsLocked(cfg *config.Config, bots []agents.Bot) {
	seen := map[string]bool{}
	for _, b := range bots {
		key := botRunKey(b)
		seen[key] = true
		conf, build, want := a.botChannel(cfg, b)
		if build == nil {
			// 凭据还没填完：停掉，不报错
			a.reconcile(key, false, nil, nil)
			continue
		}
		a.reconcile(key, want, conf, build)
	}
	for name := range a.runs {
		if !isBotChannel(name) || seen[name] || name == "web" || name == "cli" {
			continue
		}
		a.stopChannel(name)
	}
	// 机器人被删除时，没在跑（比如构建失败）的也要撤掉条目
	for name := range a.chHealth {
		if isBotChannel(name) && !seen[name] {
			a.releaseChannelHealth(name)
			delete(a.chErr, name)
		}
	}
}

// botRunKey 是机器人在 runs 里的键：内置的 Telegram 是 telegram、飞书是 feishu（和老版本一致），
// 另外加的就是它的渠道名。
func botRunKey(b agents.Bot) string {
	if b.Legacy && b.Kind == agents.KindTelegram {
		return "telegram"
	}
	return agents.ChannelName(b)
}

func (a *App) channelInfo(name string) ChannelInfo {
	info := ChannelInfo{Name: name, Status: channel.Status{State: "disabled"}}
	if r := a.runs[name]; r != nil {
		info.Enabled = true
		if sr, ok := r.ch.(channel.StatusReporter); ok {
			info.Status = sr.Status()
		} else {
			info.Status = channel.Status{State: channel.StateOnline}
		}
	} else if e := a.chErr[name]; e != "" {
		info.Enabled = true
		info.Status = channel.Status{State: channel.StateError, Error: e}
	}
	if name == "cli" && a.cli == nil {
		info.Status.State = "disabled"
	}
	return info
}

func (a *App) publishChannel(name string) {
	// 渠道状态变化的回调可能来自渠道自己的协程，也可能是在持有 chMu 时调用；
	// 状态是从渠道里实时读的，这里在新协程里读，避免锁顺序问题
	go func() {
		a.chMu.Lock()
		info := a.channelInfo(name)
		a.chMu.Unlock()
		a.publish(events.Event{T: "channel", Name: name, Data: info})
	}()
}
