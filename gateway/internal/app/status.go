package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/redact"
)

// Status 是 `nano-gateway status`、聊天 /status 和网页用的运行概况。
type Status struct {
	Running        bool          `json:"running"`
	PID            int           `json:"pid"`
	Version        string        `json:"version"`
	Started        int64         `json:"started"` // 毫秒
	UptimeMS       int64         `json:"uptime_ms"`
	Mode           string        `json:"mode"` // systemd | background | foreground
	WebURL         string        `json:"web_url,omitempty"`
	KernelListen   string        `json:"kernel_listen"`
	Kernels        []KernelInfo  `json:"kernels"`
	Machines       []MachineInfo `json:"machines"`
	Channels       []ChannelInfo `json:"channels"`
	DefaultModel   string        `json:"default_model,omitempty"`
	PendingRestart int           `json:"pending_restart"`
	Update         *UpdateInfo   `json:"update,omitempty"`
	RecentErrors   []string      `json:"recent_errors,omitempty"`
}

// RestartRequest 是一次网关自重启的发起信息。SID/Channel/Chat 用于重启完成后回话。
type RestartRequest struct {
	By      string `json:"by"`
	SID     string `json:"sid,omitempty"`
	Channel string `json:"channel,omitempty"`
	Chat    string `json:"chat,omitempty"`
	At      int64  `json:"at"`
}

// SetRuntime 记录只在启动时才知道的信息（版本、运行方式、带密码的网页链接）。
func (a *App) SetRuntime(version, mode, webURL string) {
	a.mu.Lock()
	a.version, a.mode, a.webURL = version, mode, webURL
	a.mu.Unlock()
}

// SetConfigStore 接上配置存储，用来统计“改了但还没重启”的项。
func (a *App) SetConfigStore(c *config.Store) {
	a.mu.Lock()
	a.confStore = c
	a.mu.Unlock()
}

// SetRestarter 接上真正的重启动作（main 里实现：检查配置、写标记、让主循环 re-exec）。
func (a *App) SetRestarter(f func(RestartRequest) error) {
	a.mu.Lock()
	a.restarter = f
	a.mu.Unlock()
}

// Status 返回当前概况。RecentErrors 只保留最近几条，并用网关自己的密钥脱敏。
func (a *App) Status() Status {
	a.mu.Lock()
	st := a.statusLocked()
	cfg, confStore := a.cfg, a.confStore
	a.mu.Unlock()
	st.Channels = a.Channels()
	if confStore != nil {
		for _, f := range confStore.Describe() {
			if f.Pending {
				st.PendingRestart++
			}
		}
	}
	st.RecentErrors = tailErrors(cfg.Session.DataDir, 5, a.knownSecrets())
	return st
}

// statusLocked 在持有 a.mu 时取基础字段；调用方再补渠道、待重启和日志。
func (a *App) statusLocked() Status {
	now := time.Now()
	st := Status{
		Running:      true,
		PID:          os.Getpid(),
		Version:      a.version,
		Started:      a.started.UnixMilli(),
		UptimeMS:     now.Sub(a.started).Milliseconds(),
		Mode:         a.mode,
		WebURL:       a.webURL,
		Kernels:      a.kernelList(),
		Machines:     a.machineList(),
		KernelListen: a.cfg.Kernel.Listen,
	}
	if a.updater != nil {
		u := a.update
		u.Current = a.version
		st.Update = &u
	}
	if a.models != nil {
		st.DefaultModel = a.models.Default()
	}
	return st
}

// statusBriefLocked 是聊天命令用的版本（OnMessage 已经持有 a.mu），不查渠道和日志。
func (a *App) statusBriefLocked() string {
	return formatStatusBrief(a.statusLocked())
}

// StatusBrief 是聊天里的简短版。
func (a *App) StatusBrief() string {
	return formatStatusBrief(a.Status())
}

func formatStatusBrief(st Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "网关运行中：版本 %s，已运行 %s，运行方式 %s，pid %d", st.Version, shortDuration(st.UptimeMS), st.Mode, st.PID)
	if st.WebURL != "" {
		fmt.Fprintf(&b, "\n网页：%s", st.WebURL)
	}
	fmt.Fprintf(&b, "\n内核监听：%s", st.KernelListen)
	if len(st.Kernels) > 0 {
		parts := make([]string, 0, len(st.Kernels))
		for _, k := range st.Kernels {
			parts = append(parts, fmt.Sprintf("%s（%s %s）", k.Node, k.OS, k.Version))
		}
		fmt.Fprintf(&b, "\n在线内核：%s", strings.Join(parts, "、"))
	} else {
		b.WriteString("\n在线内核：没有")
	}
	if st.DefaultModel != "" {
		fmt.Fprintf(&b, "\n默认模型：%s", st.DefaultModel)
	}
	for _, m := range st.Machines {
		if m.Online {
			fmt.Fprintf(&b, "\n机器：%s（%s，在线）", m.Name, m.Kind)
		}
	}
	for _, c := range st.Channels {
		if c.Enabled {
			fmt.Fprintf(&b, "\n渠道：%s（%s）", c.Name, c.Status.State)
		}
	}
	if u := st.Update; u != nil && u.Available {
		if u.Updatable {
			fmt.Fprintf(&b, "\n有新版本 %s（现在 %s），发 /update 更新", u.Latest, u.Current)
		} else {
			fmt.Fprintf(&b, "\n有新版本 %s（现在 %s）", u.Latest, u.Current)
		}
	}
	if st.PendingRestart > 0 {
		fmt.Fprintf(&b, "\n有 %d 项设置改了还没重启", st.PendingRestart)
	}
	if len(st.RecentErrors) > 0 {
		fmt.Fprintf(&b, "\n最近的错误：\n- %s", strings.Join(st.RecentErrors, "\n- "))
	}
	return b.String()
}

// RequestRestart 由网页和 GatewayAdmin 调用。调用方不持有 a.mu。
func (a *App) RequestRestart(req RestartRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restartLocked(req)
}

// restartLocked 是聊天命令用的版本（OnMessage 已经持有 a.mu）。
func (a *App) restartLocked(req RestartRequest) error {
	if a.restarter == nil {
		return fmt.Errorf("这个网关没有配置重启入口")
	}
	if req.At == 0 {
		req.At = time.Now().UnixMilli()
	}
	if err := a.restarter(req); err != nil {
		return err
	}
	if req.SID != "" {
		a.tell(req.SID, endpoint{ch: req.Channel, chat: req.Chat}, "网关正在重启…")
	}
	return nil
}

// RestartDone 在新进程启动完成后调用，给发起重启的会话回一条消息。
func (a *App) RestartDone(req RestartRequest, elapsed time.Duration) {
	a.Notify(req, fmt.Sprintf("网关已重启（用时 %s）", shortDuration(elapsed.Milliseconds())))
}

func shortDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return "不到 1 秒"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// tailErrors 读网关日志的最后几行错误，给 status / Gateway logs 用。
func tailErrors(dataDir string, n int, secrets []string) []string {
	out := []string{}
	for _, line := range logTail(dataDir, 1000, secrets) {
		if strings.Contains(strings.ToUpper(line), "ERROR") {
			out = append(out, line)
			if len(out) >= n {
				break
			}
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// logTail 读日志最后 n 行，并脱敏。
func logTail(dataDir string, n int, secrets []string) []string {
	b, err := os.ReadFile(filepath.Join(dataDir, "gateway.log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, redact.ToolResult(line, secrets))
	}
	return out
}
