// nano-gateway：nano-agent 的网关。持有对话上下文，对接模型和聊天平台，
// 等内核连上来驱动循环；另外提供一个网页管理界面。
//
//	nano-gateway -c gateway.toml        运行网关
//	nano-gateway chat -c gateway.toml   在终端里和正在运行的网关聊天
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/web"
)

func newClient(c config.LLM) (llm.Client, error) {
	if c.Provider == "openai" {
		return llm.NewOpenAI(c)
	}
	return &llm.Mock{}, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "chat" {
		os.Exit(runChat(os.Args[2:]))
	}
	cfgPath := flag.String("c", "gateway.toml", "配置文件路径")
	verbose := flag.Bool("v", false, "输出调试日志")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	conf, warnings, err := config.Open(*cfgPath)
	for _, w := range warnings {
		log.Warn(w)
	}
	if err != nil {
		log.Error("配置有误", "err", err)
		os.Exit(1)
	}
	cfg := conf.Current()

	client, err := newClient(cfg.LLMConfig())
	if err != nil {
		log.Error("初始化模型客户端失败", "err", err)
		os.Exit(1)
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

	personas := persona.New(cfg.Persona.Dir, log)
	a := app.New(ctx, cfg, client, store, personas, log)
	// 可以用的模型：配置文件里的 [llm] 是内置端点，网页里加的端点和模型存在数据目录的 models.json
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
		log.Info("网页管理界面", "url", strings.Split(url, "#")[0])
		fmt.Printf("网页管理界面：%s\n", url)
		os.WriteFile(filepath.Join(cfg.Session.DataDir, "web.url"), []byte(url+"\n"), 0o600)
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
		if !reflect.DeepEqual(old.LLMConfig(), cur.LLMConfig()) {
			if c, err := newClient(cur.LLMConfig()); err != nil {
				log.Warn("新的模型配置有误，继续用原来的", "err", err)
			} else {
				a.SetLLM(c)
				log.Info("模型配置已更新", "model", cur.LLM.Model)
			}
		}
		a.ReconcileChannels(cur)
		a.ModelsChanged() // 配置文件里的模型可能变了，网页上的模型列表跟着刷新
		if ws != nil {
			ws.ConfigChanged(old, cur)
		}
	})
	a.ReconcileChannels(cfg)

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

	<-ctx.Done()
	log.Info("退出")
	if kernelDone != nil {
		<-kernelDone
	}
	if p, ok := strings.CutPrefix(cfg.Kernel.Listen, "unix:"); ok {
		os.Remove(p)
	}
}
