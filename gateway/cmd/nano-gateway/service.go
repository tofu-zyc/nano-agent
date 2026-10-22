package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/secret"
)

const systemdUnit = "nano-gateway.service"

func unitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", systemdUnit)
}

func hasSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

func useSystemd() bool {
	_, err := os.Stat(unitPath())
	return err == nil && hasSystemd()
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

func runtimeMode() string {
	if v := os.Getenv("NANO_GATEWAY_MODE"); v != "" {
		return v
	}
	if os.Getenv("NANO_GATEWAY_SUPERVISED") == "1" {
		return "background"
	}
	if useSystemd() {
		return "systemd"
	}
	return "foreground"
}

func pidFile(name string) string { return filepath.Join(paths.RunDir(), name) }

func readAlivePID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return 0, false
	}
	return pid, true
}

func writePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// isGatewayProcess 确认 pid 对应的是自己的 nano-gateway（pid 可能被系统复用）。
func isGatewayProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	if b, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.Contains(filepath.Base(b), "nano-gateway")
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err == nil {
		return strings.Contains(filepath.Base(strings.TrimSpace(string(out))), "nano-gateway")
	}
	return true
}

func currentExe() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	return self, nil
}

func startBackground(cfgPath, logPath string, quiet bool) error {
	if pid, ok := readAlivePID(pidFile("gateway.supervisor.pid")); ok && isGatewayProcess(pid) {
		if !quiet {
			fmt.Printf("网关已经在运行了（pid %d）。\n", pid)
		}
		return nil
	}
	if pid, ok := readAlivePID(pidFile("gateway.pid")); ok && isGatewayProcess(pid) {
		if !quiet {
			fmt.Printf("网关已经在运行了（pid %d）。\n", pid)
		}
		return nil
	}
	self, err := currentExe()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(self, "supervise", "-c", cfgPath)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = f, f, nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		if pid, ok := readAlivePID(pidFile("gateway.supervisor.pid")); ok && isGatewayProcess(pid) {
			if !quiet {
				fmt.Printf("网关已在后台启动（supervisor pid %d）。日志：%s\n", pid, logPath)
			}
			return nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !quiet {
		fmt.Printf("网关已在后台启动，日志：%s\n", logPath)
	}
	return nil
}

func stopBackground(quiet bool) error {
	sup, supOK := readAlivePID(pidFile("gateway.supervisor.pid"))
	child, childOK := readAlivePID(pidFile("gateway.pid"))
	if supOK && isGatewayProcess(sup) {
		_ = syscall.Kill(sup, syscall.SIGTERM)
		for i := 0; i < 50; i++ {
			if !isGatewayProcess(sup) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if isGatewayProcess(sup) {
			_ = syscall.Kill(sup, syscall.SIGKILL)
		}
	}
	if childOK && isGatewayProcess(child) {
		_ = syscall.Kill(child, syscall.SIGTERM)
		for i := 0; i < 30; i++ {
			if !isGatewayProcess(child) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	_ = os.Remove(pidFile("gateway.supervisor.pid"))
	_ = os.Remove(pidFile("gateway.pid"))
	if !quiet {
		fmt.Println("网关已停止。")
	}
	return nil
}

// runSupervisor 是后台模式的看护进程：网关异常退出时隔几秒拉起；stop 时一起停。
func runSupervisor(args []string) int {
	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	cfgPath := fs.String("c", "", "配置文件路径")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	if err := secret.Init(paths.Home()); err != nil {
		fmt.Fprintln(os.Stderr, "读取密钥文件失败：", err)
		return 1
	}
	conf, _, err := config.Open(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	logPath := filepath.Join(conf.Current().Session.DataDir, "gateway.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer logf.Close()
	log := slog.New(slog.NewTextHandler(logf, nil))
	self, err := currentExe()
	if err != nil {
		return 1
	}
	if err := writePID(pidFile("gateway.supervisor.pid"), os.Getpid()); err != nil {
		return 1
	}
	defer os.Remove(pidFile("gateway.supervisor.pid"))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	env := append(os.Environ(), "NANO_GATEWAY_SUPERVISED=1")
	for {
		cmd := exec.Command(self, "-c", *cfgPath)
		cmd.Env, cmd.Stdout, cmd.Stderr, cmd.Stdin = env, logf, logf, nil
		if err := cmd.Start(); err != nil {
			log.Error("看护进程启动网关失败", "err", err)
			return 1
		}
		if err := writePID(pidFile("gateway.pid"), cmd.Process.Pid); err != nil {
			log.Warn("写 pid 文件失败", "err", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case s := <-sig:
			log.Info("看护进程收到停止信号", "signal", s)
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
			}
			os.Remove(pidFile("gateway.pid"))
			return 0
		case err := <-done:
			os.Remove(pidFile("gateway.pid"))
			if err != nil {
				log.Error("网关退出，看护进程准备重启", "err", err)
			} else {
				log.Warn("网关退出，看护进程准备重启")
			}
			select {
			case <-sig:
				return 0
			case <-time.After(3 * time.Second):
			}
		}
	}
}
