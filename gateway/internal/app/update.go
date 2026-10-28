package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/release"
)

// UpdateInfo 是版本检查的结果（status、网页、/update 用）。
type UpdateInfo struct {
	Installed bool   `json:"installed"`
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`          // 有比现在新的版本
	Updatable bool   `json:"updatable"`          // 这个网关能自动更新（用安装脚本装的）
	Why       string `json:"why,omitempty"`      // 不能自动更新的原因
	Checked   int64  `json:"checked,omitempty"`  // 毫秒
	Err       string `json:"error,omitempty"`    // 上次检查失败的原因
	Updating  string `json:"updating,omitempty"` // 正在更新到的版本
}

// Updater 由 main 实现：查新版本；更新到某个版本（换好程序后让网关重启，新进程起来后回话）；
// 取某个系统和架构的内核程序（推给内核用）。
type Updater interface {
	Check(ctx context.Context) (UpdateInfo, error)
	Apply(ctx context.Context, version string, req RestartRequest) error
	KernelFiles(ctx context.Context, version, osName, arch string) ([]release.File, error)
}

// SetUpdater 接上更新的实现。
func (a *App) SetUpdater(u Updater) {
	a.mu.Lock()
	a.updater = u
	a.mu.Unlock()
}

// UpdateStatus 返回上次检查的结果。
func (a *App) UpdateStatus() UpdateInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	info := a.update
	info.Current = a.version
	return info
}

// CheckUpdate 查一次新版本并记下来。调用方不持有 a.mu。
func (a *App) CheckUpdate(ctx context.Context) (UpdateInfo, error) {
	a.mu.Lock()
	u := a.updater
	a.mu.Unlock()
	if u == nil {
		return UpdateInfo{}, fmt.Errorf("这个网关没有配置更新")
	}
	info, err := u.Check(ctx)
	info.Checked = time.Now().UnixMilli()
	if err != nil {
		info.Err = err.Error()
	}
	a.mu.Lock()
	info.Current = a.version
	info.Updating = a.update.Updating
	a.update = info
	a.mu.Unlock()
	a.Bus().Publish(events.Event{T: "update"})
	return info, err
}

// RunUpdate 查新版本，有就更新（下载、校验、换程序后网关自己重启）。返回给人看的结果。
// 调用方不持有 a.mu；会下载，可能要一会儿。
func (a *App) RunUpdate(ctx context.Context, req RestartRequest) (string, error) {
	info, err := a.CheckUpdate(ctx)
	if err != nil {
		return "", fmt.Errorf("查新版本失败：%w", err)
	}
	if !info.Updatable {
		return "", fmt.Errorf("这个网关不能自动更新：%s", info.Why)
	}
	if !info.Available {
		return fmt.Sprintf("已经是最新版本 %s", info.Current), nil
	}
	a.mu.Lock()
	if a.update.Updating != "" {
		a.mu.Unlock()
		return "", fmt.Errorf("已经在更新到 %s 了", a.update.Updating)
	}
	a.update.Updating = info.Latest
	u := a.updater
	a.mu.Unlock()
	a.Bus().Publish(events.Event{T: "update"})
	if req.At == 0 {
		req.At = time.Now().UnixMilli()
	}
	if err := u.Apply(ctx, info.Latest, req); err != nil {
		a.mu.Lock()
		a.update.Updating = ""
		a.mu.Unlock()
		a.Bus().Publish(events.Event{T: "update"})
		return "", err
	}
	return fmt.Sprintf("新版本 %s 已经下载、校验并换好，网关正在重启（现在是 %s）。起来后会在这里告诉你；新版本起不来会自动退回。",
		info.Latest, info.Current), nil
}

// Notify 给发起重启或更新的会话回一条消息（找不到具体窗口就发给这个会话接着的窗口）。
func (a *App) Notify(req RestartRequest, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.SID != "" {
		if req.Channel != "" {
			if req.Channel == "cli" && (a.cli == nil || !a.cli.Connected()) {
				a.pendingRestart = text
				return
			}
			a.tell(req.SID, endpoint{ch: req.Channel, chat: req.Chat}, text)
			return
		}
		// GatewayAdmin 只有会话，没有具体窗口：发给这个会话接着的窗口
		for key, r := range a.eps {
			if r.Session != req.SID {
				continue
			}
			ep := parseEndpoint(key)
			if ep.ch == "cli" && (a.cli == nil || !a.cli.Connected()) {
				a.pendingRestart = text
				continue
			}
			a.deliver(ep, text, "")
		}
		a.publish(events.Event{T: "say", SID: req.SID, Text: text})
		return
	}
	a.publish(events.Event{T: "say", Text: text})
}

// maybeUpdateKernel：内核的版本和网关不一样时，把网关这个版本的内核推给它（内核换好后自己重启）。
// 网关自己启动的本机内核随网关一起更新，不推；网关是源码编译的（不是发布版本）也不推。
func (a *App) maybeUpdateKernel(name string, hello *proto.Msg) {
	a.mu.Lock()
	u, ver, cfg := a.updater, a.version, a.cfg
	managed := a.managedKernel != "" && name == a.managedKernel
	key := name + "@" + ver
	tried := a.kernelUpdates[key]
	switch {
	case u == nil, managed, !config.On(cfg.Update.Kernels), !release.IsRelease(ver),
		hello.Version == "" || hello.Version == ver, hello.UpdateFailed == ver, tried:
		a.mu.Unlock()
		return
	}
	if a.kernelUpdates == nil {
		a.kernelUpdates = map[string]bool{}
	}
	a.kernelUpdates[key] = true // 每次网关运行期间每个内核只自动推一次
	a.mu.Unlock()

	a.log.Info("内核版本和网关不同，推送网关的版本", "node", name, "kernel", hello.Version, "gateway", ver,
		"os", hello.OS, "arch", hello.Arch)
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
		defer cancel()
		files, err := u.KernelFiles(ctx, ver, hello.OS, hello.Arch)
		if err != nil {
			a.log.Warn("取不到内核的新版本，没有更新", "node", name, "err", err)
			return
		}
		msg := proto.Msg{T: proto.TUpdate, Version: ver}
		for _, f := range files {
			msg.Files = append(msg.Files, proto.SkillFile{Path: f.Name, Data: base64.StdEncoding.EncodeToString(f.Data), Exec: true})
		}
		a.mu.Lock()
		hub := a.hub
		a.mu.Unlock()
		if hub == nil || !hub.Send(name, msg) {
			a.log.Warn("内核不在线了，没有推送更新", "node", name)
		}
	}()
}
