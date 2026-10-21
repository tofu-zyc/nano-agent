// nano-gateway：nano-agent 的网关。持有对话上下文，对接模型和聊天平台，
// 等内核连上来驱动循环。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

func main() {
	cfgPath := flag.String("c", "gateway.toml", "配置文件路径")
	verbose := flag.Bool("v", false, "输出调试日志")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, warnings, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("配置有误", "err", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		log.Warn(w)
	}

	var client llm.Client = &llm.Mock{}
	if cfg.LLM.Provider == "openai" {
		if client, err = llm.NewOpenAI(cfg.LLM); err != nil {
			log.Error("初始化模型客户端失败", "err", err)
			os.Exit(1)
		}
	}
	store, err := session.New(cfg.Session.DataDir, cfg.LLM.MaxHistoryTokens*2)
	if err != nil {
		log.Error("打开数据目录失败", "dir", cfg.Session.DataDir, "err", err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(cfg.Persona.Dir, "default.md")); err != nil {
		log.Warn("没有找到默认人格文件，将不带系统提示词", "file", filepath.Join(cfg.Persona.Dir, "default.md"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := app.New(ctx, cfg, client, store, persona.New(cfg.Persona.Dir, log), log)
	hub := kernel.NewHub(cfg.Kernel.Token, a, log)
	a.SetHub(hub)
	ln, err := kernel.Listen(cfg.Kernel.Listen)
	if err != nil {
		log.Error("监听失败", "listen", cfg.Kernel.Listen, "err", err)
		os.Exit(1)
	}
	go hub.Serve(ctx, ln)

	var chans []channel.Channel
	if cfg.Telegram.Enabled {
		tg, err := channel.NewTelegram(cfg.Telegram, log)
		if err != nil {
			log.Error("初始化 Telegram 失败", "err", err)
			os.Exit(1)
		}
		chans = append(chans, tg)
	}
	if cfg.Feishu.Enabled {
		fs, err := channel.NewFeishu(cfg.Feishu, log)
		if err != nil {
			log.Error("初始化飞书失败", "err", err)
			os.Exit(1)
		}
		chans = append(chans, fs)
	}
	if cfg.CLI.Enabled {
		chans = append(chans, channel.NewCLI(os.Stdin, os.Stdout))
	}

	names := make([]string, len(chans))
	for i, ch := range chans {
		names[i] = ch.Name()
		a.AddChannel(ch)
		go func(ch channel.Channel) {
			err := ch.Run(ctx, a.OnMessage)
			switch {
			case err != nil:
				log.Error("聊天通道退出", "channel", ch.Name(), "err", err)
			case ch.Name() == "cli" && ctx.Err() == nil && len(chans) == 1:
				log.Info("标准输入已关闭，退出") // 只开了命令行时，Ctrl-D 就是退出
				stop()
			case ch.Name() == "cli" && ctx.Err() == nil:
				log.Info("标准输入已关闭，命令行通道停止")
			}
		}(ch)
	}
	log.Info("网关已启动", "listen", cfg.Kernel.Listen, "model", cfg.LLM.Model, "provider", cfg.LLM.Provider,
		"channels", strings.Join(names, ","), "persona", cfg.Persona.Dir, "data", cfg.Session.DataDir)

	<-ctx.Done()
	log.Info("退出")
	if p, ok := strings.CutPrefix(cfg.Kernel.Listen, "unix:"); ok {
		os.Remove(p)
	}
}
