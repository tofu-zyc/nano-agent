// nano-gateway：nano-agent 的网关。持有对话上下文，对接模型和聊天平台，
// 等内核连上来驱动循环；另外提供一个网页管理界面。
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
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
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
	hub := kernel.NewHub(cfg.Kernel.Token, a, log)
	a.SetHub(hub)
	ln, err := kernel.Listen(cfg.Kernel.Listen)
	if err != nil {
		log.Error("监听失败", "listen", cfg.Kernel.Listen, "err", err)
		os.Exit(1)
	}
	go hub.Serve(ctx, ln)

	if cfg.CLI.Enabled {
		// 只有命令行一个聊天渠道时（网页不算），Ctrl-D 就是退出
		onlyCLI := !cfg.Telegram.Enabled && !cfg.Feishu.Enabled
		a.SetCLI(channel.NewCLI(os.Stdin, os.Stdout, config.On(cfg.CLI.ShowReasoning), config.On(cfg.CLI.ShowUsage)), func() {
			if onlyCLI {
				stop()
			}
		})
	}

	var ws *web.Server
	if cfg.Web.Listen != "off" {
		if ws, err = web.New(a, conf, personas, newClient, log); err != nil {
			log.Error("初始化网页管理界面失败", "err", err)
			os.Exit(1)
		}
		go func() {
			if err := ws.Serve(ctx, cfg.Web.Listen); err != nil {
				log.Error("网页管理界面启动失败", "listen", cfg.Web.Listen, "err", err)
			}
		}()
		url := ws.LoginURL(cfg.Web.Listen)
		log.Info("网页管理界面", "url", strings.Split(url, "#")[0])
		if cfg.CLI.Enabled {
			fmt.Printf("网页管理界面：%s\n", url)
		}
		os.WriteFile(filepath.Join(cfg.Session.DataDir, "web.url"), []byte(url+"\n"), 0o600)
	}

	// 网页里改了配置：能立即生效的立即生效，其余的等重启
	conf.OnChange(func(old, cur *config.Config) {
		a.SetConfig(cur)
		if !reflect.DeepEqual(old.LLMConfig(), cur.LLMConfig()) {
			if c, err := newClient(cur.LLMConfig()); err != nil {
				log.Warn("新的模型配置有误，继续用原来的", "err", err)
			} else {
				a.SetLLM(c)
				log.Info("模型配置已更新", "model", cur.LLM.Model)
			}
		}
		a.ReconcileChannels(cur)
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

	<-ctx.Done()
	log.Info("退出")
	if p, ok := strings.CutPrefix(cfg.Kernel.Listen, "unix:"); ok {
		os.Remove(p)
	}
}
