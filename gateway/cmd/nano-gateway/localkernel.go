package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
)

// 网关所在的机器上也跑一个内核：网关自己启动、看着它（退出了就重启），不用另外写 kernel.conf、另外启动。
// 它用一个每次启动随机生成的口令连回网关，配置文件放在数据目录里（只有自己能读）。

// 默认不用主机名：同一台机器上别人装的内核（比如 WSL 里装的电脑内核）多半就叫主机名，同名会互相顶掉
func localKernelName(cfg *config.Config) string {
	name := strings.TrimSpace(cfg.Kernel.LocalName)
	if name == "" {
		name = "gateway"
	}
	name = strings.Map(func(r rune) rune {
		if r == ' ' || r == ':' || r == '/' || r < 0x20 {
			return '-'
		}
		return r
	}, name)
	if name == "" || name == host.LocalName || strings.EqualFold(name, "local") {
		name = "gateway"
	}
	return name
}

// startLocalKernel 启动本机内核，返回的 done 在它最终退出（ctx 结束后）时关闭。不用启动时返回 nil。
func startLocalKernel(ctx context.Context, cfg *config.Config, exe host.Executor, hub *kernel.Hub, a *app.App, log *slog.Logger) <-chan struct{} {
	if !config.On(cfg.Kernel.Local) {
		return nil
	}
	if exe.Path == "" {
		log.Warn("找不到执行器，本机内核不启动")
		return nil
	}
	bin := filepath.Join(filepath.Dir(exe.Path), "nano-kernel")
	if _, err := os.Stat(bin); err != nil {
		log.Warn("找不到 nano-kernel，本机内核不启动（本机的工作目录由网关直接执行）", "path", bin)
		return nil
	}
	addr := cfg.Kernel.Listen
	if !strings.HasPrefix(addr, "unix:") {
		addr = "tcp:127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:] // 监听的是 tcp：从本机回环地址连
	}
	var b [24]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	hub.AllowToken(token)
	name := localKernelName(cfg)
	conf := filepath.Join(cfg.Session.DataDir, "local-kernel.conf")
	text := fmt.Sprintf("# 网关自动生成的本机内核配置，每次启动网关都会重写，不用改\ngateway = %s\ntoken = %s\nnode = %s\nexec = %s\n",
		addr, token, name, exe.Path)
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		log.Warn("写不了本机内核的配置，本机内核不启动", "err", err)
		return nil
	}
	a.SetManagedKernel(name)
	logPath := filepath.Join(cfg.Session.DataDir, "kernel.log")
	done := make(chan struct{})
	go func() {
		defer close(done)
		backoff := time.Second
		for ctx.Err() == nil {
			lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				log.Warn("打不开内核日志", "err", err)
				lf = nil
			} else if err := lf.Chmod(0o600); err != nil {
				log.Warn("收紧内核日志权限失败", "err", err)
			}
			cmd := exec.Command(bin, "-c", conf)
			if lf != nil {
				cmd.Stderr = lf
			}
			start := time.Now()
			if err := cmd.Start(); err != nil {
				log.Error("本机内核启动失败", "err", err)
			} else {
				log.Info("本机内核已启动", "node", name, "pid", cmd.Process.Pid, "log", logPath)
				exited := make(chan error, 1)
				go func() { exited <- cmd.Wait() }()
				select {
				case err := <-exited:
					log.Warn("本机内核退出了，稍后重启", "err", err, "log", logPath)
				case <-ctx.Done():
					// 让内核先清理它正在执行的命令
					cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-exited:
					case <-time.After(4 * time.Second):
						cmd.Process.Kill()
						<-exited
					}
				}
			}
			if lf != nil {
				lf.Close()
			}
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
		os.Remove(conf)
	}()
	return done
}
