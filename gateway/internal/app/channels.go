package app

import (
	"context"
	"reflect"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
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

// SetCLI 登记命令行渠道（由 main 创建，它接着终端）。onEOF 在标准输入关闭时调用。
// 命令行窗口一开始就接在主会话上（拓扑图里能看到），不用等第一条消息。
func (a *App) SetCLI(c *channel.CLI, onEOF func()) {
	a.cli, a.cliEOF = c, onEOF
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
	a.chMu.Lock()
	defer a.chMu.Unlock()
	if a.runs == nil {
		a.runs, a.chErr = map[string]*chanRun{}, map[string]string{}
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
	a.reconcile("telegram", cfg.Telegram.Enabled, cfg.TelegramConfig(), func() (channel.Channel, error) {
		return channel.NewTelegram(cfg.TelegramConfig(), a.log)
	})
	a.reconcile("feishu", cfg.Feishu.Enabled, cfg.FeishuConfig(), func() (channel.Channel, error) {
		return channel.NewFeishu(cfg.FeishuConfig(), a.log)
	})
}

func (a *App) reconcile(name string, want bool, conf any, build func() (channel.Channel, error)) {
	cur := a.runs[name]
	if cur != nil && (!want || !reflect.DeepEqual(cur.conf, conf)) {
		a.stopChannel(name)
		cur = nil
	}
	if want && cur == nil {
		ch, err := build()
		if err != nil {
			a.log.Error("启动渠道失败", "channel", name, "err", err)
			a.chErr[name] = err.Error()
			a.publishChannel(name)
			return
		}
		delete(a.chErr, name)
		a.startChannel(name, ch, conf)
	}
	if !want {
		delete(a.chErr, name)
		a.publishChannel(name)
	}
}

func (a *App) startChannel(name string, ch channel.Channel, conf any) {
	ctx, cancel := context.WithCancel(a.ctx)
	r := &chanRun{ch: ch, conf: conf, cancel: cancel, done: make(chan struct{})}
	a.runs[name] = r
	a.AddChannel(ch)
	if sr, ok := ch.(channel.StatusReporter); ok {
		sr.OnStatus(func(channel.Status) { a.publishChannel(name) })
	}
	go func() {
		defer close(r.done)
		err := ch.Run(ctx, a.OnMessage)
		switch {
		case err != nil:
			a.log.Error("聊天渠道退出", "channel", name, "err", err)
		case name == "cli" && ctx.Err() == nil:
			a.log.Info("标准输入已关闭，命令行渠道停止")
			if a.cliEOF != nil {
				a.cliEOF()
			}
		}
		a.publishChannel(name)
	}()
	a.log.Info("渠道已启动", "channel", name)
	a.publishChannel(name)
}

func (a *App) stopChannel(name string) {
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
	a.log.Info("渠道已停止", "channel", name)
}

// Channels 返回所有渠道（包括没启用的）的情况。
func (a *App) Channels() []ChannelInfo {
	a.chMu.Lock()
	defer a.chMu.Unlock()
	out := make([]ChannelInfo, 0, len(channelOrder))
	for _, name := range channelOrder {
		out = append(out, a.channelInfo(name))
	}
	return out
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
