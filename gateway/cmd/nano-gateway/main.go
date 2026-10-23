// nano-gateway：nano-agent 的网关。持有对话上下文，对接模型和聊天平台，
// 等内核连上来驱动循环；另外提供一个网页管理界面。
//
//	nano-gateway -c gateway.toml        运行网关
//	nano-gateway chat -c gateway.toml   在终端里和正在运行的网关聊天
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/control"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/secret"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/version"
	"nano-agent/gateway/internal/web"
)

// testFailStart 只在测试构建里用 -ldflags -X 设置：模拟新版本启动失败，测试自动退回。
var testFailStart = ""

func newClient(c config.LLM) (llm.Client, error) {
	if c.Provider == "openai" {
		return llm.NewOpenAI(c)
	}
	return &llm.Mock{}, nil
}

func main() {
	if len(os.Args) > 1 {
		arg := os.Args[1]
		switch arg {
		case "chat":
			os.Exit(runChat(os.Args[2:]))
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "start":
			os.Exit(runStart(os.Args[2:]))
		case "stop":
			os.Exit(runStop(os.Args[2:]))
		case "restart":
			os.Exit(runRestart(os.Args[2:]))
		case "logs":
			os.Exit(runLogs(os.Args[2:]))
		case "url":
			os.Exit(runURL(os.Args[2:]))
		case "doctor":
			os.Exit(runDoctor(os.Args[2:]))
		case "version":
			os.Exit(runVersion())
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "uninstall":
			os.Exit(runUninstall(os.Args[2:]))
		case "check-config":
			os.Exit(runCheckConfig(os.Args[2:]))
		case "export":
			os.Exit(runExport(os.Args[2:]))
		case "import":
			os.Exit(runImport(os.Args[2:]))
		case "supervise":
			os.Exit(runSupervisor(os.Args[2:]))
		}
		if !strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "不认识的子命令：%s\n", arg)
			fmt.Fprintln(os.Stderr, "用法：nano-gateway [-c 配置文件] | status|start|stop|restart|logs|url|doctor|version|update|uninstall|chat|export|import")
			os.Exit(2)
		}
	}
	cfgPath := flag.String("c", "", "配置文件路径（默认 $NANO_HOME/gateway.toml）")
	verbose := flag.Bool("v", false, "输出调试日志")
	flag.Parse()
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// 刚更新过：记一次启动；新版本起不来好几次就在这里退回旧版本（所以要在读配置之前）
	upd := updateGuard(log)
	if testFailStart != "" {
		log.Error("测试构建：模拟新版本启动失败")
		os.Exit(1)
	}

	if err := secret.Init(paths.Home()); err != nil {
		log.Error("读取密钥文件失败", "err", err)
		os.Exit(1)
	}
	conf, warnings, err := config.Open(*cfgPath)
	for _, w := range warnings {
		log.Warn(w)
	}
	if err != nil {
		log.Error("配置有误", "err", err)
		os.Exit(1)
	}
	cfg := conf.Current()
	for _, name := range []string{"gateway.log", "kernel.log"} {
		_ = os.Chmod(filepath.Join(cfg.Session.DataDir, name), 0o600)
	}

	store, err := session.New(cfg.Session.DataDir)
	if err != nil {
		log.Error("打开数据目录失败", "dir", cfg.Session.DataDir, "err", err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(cfg.Persona.Dir, "default.md")); err != nil {
		log.Warn("没有找到默认人格文件，将不带系统提示词", "file", filepath.Join(cfg.Persona.Dir, "default.md"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	restartCh := make(chan app.RestartRequest, 1)

	personas := persona.New(cfg.Persona.Dir, log)
	// 模型全部在网页的模型节点里配置；没有模型时网关照样启动，聊天会提示去添加。
	// 旧版 [llm] 里的模型会在 models.Open 时一次性导入成普通模型。
	a := app.New(ctx, cfg, nil, store, personas, log)
	a.SetConfigStore(conf)
	a.SetRuntime(version.Version, runtimeMode(), "")
	a.SetRestarter(func(req app.RestartRequest) error {
		return requestRestart(*cfgPath, cfg.Session.DataDir, req, restartCh)
	})
	a.SetUpdater(&gatewayUpdater{cfgPath: *cfgPath, conf: conf, log: log, restart: func(req app.RestartRequest) error {
		// 更新后的重启不写重启标记：新进程按 update.json 回话（成功或退回）
		select {
		case restartCh <- req:
			return nil
		default:
			return errors.New("已经在重启了")
		}
	}})
	go func() {
		if err := control.Serve(ctx, control.Socket(cfg.Session.DataDir), func() any { return a.Status() }, log); err != nil {
			log.Warn("控制 socket 用不了", "err", err)
		}
	}()
	modelStore, err := models.Open(cfg.Session.DataDir, cfg)
	if err != nil {
		log.Error("读取模型列表失败", "err", err)
		os.Exit(1)
	}
	a.SetModels(modelStore, newClient)
	// 用户发来的图片（网页上传、Telegram、飞书下载）存在数据目录的 uploads 下
	mediaStore, err := media.Open(filepath.Join(cfg.Session.DataDir, "uploads"))
	if err != nil {
		log.Error("打开图片目录失败", "err", err)
		os.Exit(1)
	}
	a.SetMedia(mediaStore)
	hub := kernel.NewHub(cfg.Kernel.Token, a, log)
	a.SetHub(hub)
	ln, err := kernel.Listen(cfg.Kernel.Listen)
	if err != nil {
		log.Error("监听失败", "listen", cfg.Kernel.Listen, "err", err)
		os.Exit(1)
	}
	go hub.Serve(ctx, ln)
	if err := hub.EnablePairing(filepath.Join(cfg.Session.DataDir, "paired.json")); err != nil {
		log.Error("读取配对记录失败", "err", err)
		os.Exit(1)
	}
	hub.OnPairChange(func() { a.Bus().Publish(events.Event{T: "pairing"}) })
	// 网关能直接执行工具的机器：本机和 SSH 服务器（工作目录不在内核所在的机器上时用）
	exe, err := host.FindExecutor(cfg.Dir)
	if err != nil {
		log.Warn("本机和 SSH 服务器上的工作目录暂时用不了", "err", err)
	}
	devices, err := host.New(ctx, cfg.Session.DataDir, exe, []string{cfg.Dir, cfg.Session.DataDir}, hub, log)
	if err != nil {
		log.Error("初始化机器管理失败", "err", err)
		os.Exit(1)
	}
	a.SetHosts(devices)
	devices.OnChange(a.HostsChanged)
	kernelDone := startLocalKernel(ctx, cfg, exe, hub, a, log)
	if err := devices.SetListener(cfg.Kernel.DeviceListen); err != nil {
		log.Warn("电脑接入地址用不了", "err", err)
	}

	if config.On(cfg.CLI.Enabled) {
		if sock, err := channel.CLISocket(cfg.Session.DataDir); err != nil {
			log.Error("终端聊天用不了", "err", err)
		} else {
			a.SetCLI(channel.NewCLI(sock, config.On(cfg.CLI.ShowReasoning), config.On(cfg.CLI.ShowUsage), log))
		}
	}

	var ws *web.Server
	if cfg.Web.Listen != "off" {
		if ws, err = web.New(a, conf, personas, newClient, log); err != nil {
			log.Error("初始化网页管理界面失败", "err", err)
			os.Exit(1)
		}
		ws.SetDevices(devices)
		ws.SetMedia(mediaStore)
		go func() {
			if err := ws.Serve(ctx, cfg.Web.Listen); err != nil {
				log.Error("网页管理界面启动失败", "listen", cfg.Web.Listen, "err", err)
			}
		}()
		url := ws.LoginURL(cfg.Web.Listen)
		public := publicURL(url)
		a.SetRuntime(version.Version, runtimeMode(), public)
		log.Info("网页管理界面", "url", public)
		fmt.Printf("网页管理界面：%s\n", public)
		if err := writePrivate(filepath.Join(cfg.Session.DataDir, "web.url"), url+"\n"); err != nil {
			log.Warn("写不了网页登录链接文件", "err", err)
		}
	}

	// 网页里改了配置：能立即生效的立即生效，其余的等重启
	conf.OnChange(func(old, cur *config.Config) {
		a.SetConfig(cur)
		modelStore.SetConfig(cur)
		if old.Kernel.DeviceListen != cur.Kernel.DeviceListen {
			if err := devices.SetListener(cur.Kernel.DeviceListen); err != nil {
				log.Warn("更新电脑接入地址失败", "err", err)
			}
		}
		a.ReconcileChannels(cur)
		if ws != nil {
			ws.ConfigChanged(old, cur)
		}
	})
	a.ReconcileChannels(cfg)
	if req, elapsed, ok := readRestartMarker(cfg.Session.DataDir); ok {
		a.RestartDone(req, elapsed)
	}
	finishUpdate(ctx, a, upd, log)
	if config.On(cfg.Update.Check) {
		go checkUpdatesDaily(ctx, a, log)
	}

	names := []string{}
	for _, c := range a.Channels() {
		if c.Enabled {
			names = append(names, c.Name)
		}
	}
	log.Info("网关已启动", "listen", cfg.Kernel.Listen, "model", cfg.LLM.Model, "provider", cfg.LLM.Provider,
		"channels", strings.Join(names, ","), "persona", cfg.Persona.Dir, "data", cfg.Session.DataDir)
	if config.On(cfg.CLI.Enabled) {
		fmt.Printf("终端聊天：另开一个终端运行 make chat（或 %s chat -c %s）\n", os.Args[0], *cfgPath)
	}

	restarting := false
	var restartReq app.RestartRequest
	select {
	case <-ctx.Done():
	case restartReq = <-restartCh:
		restarting = true
		log.Info("重启网关", "by", restartReq.By, "sid", restartReq.SID)
		time.Sleep(300 * time.Millisecond)
		stop()
	}
	log.Info("退出")
	if kernelDone != nil {
		<-kernelDone
	}
	if p, ok := strings.CutPrefix(cfg.Kernel.Listen, "unix:"); ok {
		os.Remove(p)
	}
	if restarting {
		if err := reexec(os.Args); err != nil {
			log.Error("重启失败", "err", err)
			os.Exit(1)
		}
	}
}

// checkUpdatesDaily 每天查一次新版本，只记下来（status、网页、/status 里提示），不自动更新。
func checkUpdatesDaily(ctx context.Context, a *app.App, log *slog.Logger) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		info, err := a.CheckUpdate(cctx)
		cancel()
		if err != nil {
			log.Debug("查新版本失败", "err", err)
		} else if info.Available {
			log.Info("有新版本", "current", info.Current, "latest", info.Latest)
		}
		t.Reset(24 * time.Hour)
	}
}

// publicURL 去掉 # 后面的登录密码，只用于终端输出和日志。
func publicURL(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		return u[:i]
	}
	return u
}

// writePrivate 原子写一个只有自己能读的文件；已存在的文件也收紧到 0600。
func writePrivate(path, text string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
