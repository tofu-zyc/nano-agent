package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/control"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/secret"
	"nano-agent/gateway/internal/version"
)

func initSecret() error {
	return secret.Init(paths.Home())
}

func openConfig(cfgPath string) (*config.Store, error) {
	if err := initSecret(); err != nil {
		return nil, fmt.Errorf("读取密钥文件失败：%w", err)
	}
	conf, _, err := config.Open(cfgPath)
	return conf, err
}

func pathFlag(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("c", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	return *cfgPath, fs.Args(), nil
}

func runVersion() int {
	fmt.Println(version.Version)
	return 0
}

func runStatus(args []string) int {
	cfgPath, rest, err := pathFlag("status", args)
	if err != nil || len(rest) > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway status [-c 配置文件]")
		return 2
	}
	conf, err := openConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		fmt.Fprintln(os.Stderr, "网关可能在运行，但没法读取数据目录。")
		return 1
	}
	cfg := conf.Current()
	raw, err := control.Query(cfg.Session.DataDir)
	if err != nil {
		fmt.Println("网关没有在运行。")
		if useSystemd() {
			fmt.Println("运行方式：systemd（systemctl --user status " + systemdUnit + "）")
		} else {
			fmt.Println("运行方式：后台进程（nano-gateway start）")
		}
		printLogTail(filepath.Join(cfg.Session.DataDir, "gateway.log"), 5)
		return 1
	}
	var st app.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(os.Stderr, "网关返回的状态看不懂：", err)
		return 1
	}
	printStatus(st)
	return 0
}

func printStatus(st app.Status) {
	fmt.Printf("网关运行中（pid %d，版本 %s，运行方式 %s）\n", st.PID, st.Version, st.Mode)
	fmt.Printf("已运行：%s\n", shortDuration(st.UptimeMS))
	if st.WebURL != "" {
		fmt.Printf("网页：%s\n", st.WebURL)
	}
	fmt.Printf("内核监听：%s\n", st.KernelListen)
	if len(st.Kernels) == 0 {
		fmt.Println("在线内核：没有")
	} else {
		parts := make([]string, 0, len(st.Kernels))
		for _, k := range st.Kernels {
			parts = append(parts, fmt.Sprintf("%s（%s）", k.Node, k.OS))
		}
		fmt.Println("在线内核：" + strings.Join(parts, "、"))
	}
	for _, c := range st.Channels {
		if c.Enabled {
			fmt.Printf("渠道：%s（%s）\n", c.Name, c.Status.State)
		}
	}
	if st.DefaultModel != "" {
		fmt.Printf("默认模型：%s\n", st.DefaultModel)
	}
	if st.PendingRestart > 0 {
		fmt.Printf("待重启设置：%d 项\n", st.PendingRestart)
	}
	if len(st.RecentErrors) > 0 {
		fmt.Println("最近的错误：")
		for _, e := range st.RecentErrors {
			fmt.Println("  " + e)
		}
	}
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

func runStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("c", "", "配置文件路径")
	quiet := fs.Bool("quiet", false, "不打印提示")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway start [-c 配置文件] [--quiet]")
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	if err := initSecret(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := config.Check(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "配置有误，不能启动：", err)
		return 1
	}
	conf, err := openConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	cfg := conf.Current()
	if useSystemd() {
		if err := systemctl("start", systemdUnit); err != nil {
			fmt.Fprintln(os.Stderr, "启动失败：", err)
			return 1
		}
		if !*quiet {
			fmt.Println("网关服务已启动。")
		}
		return 0
	}
	if err := startBackground(*cfgPath, filepath.Join(cfg.Session.DataDir, "gateway.log"), *quiet); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	return 0
}

func runStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_ = fs.String("c", "", "配置文件路径（只为和其他命令用法一致，停止不需要）")
	quiet := fs.Bool("quiet", false, "不打印提示")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway stop [-c 配置文件] [--quiet]")
		return 2
	}
	if useSystemd() {
		if err := systemctl("stop", systemdUnit); err != nil {
			fmt.Fprintln(os.Stderr, "停止失败：", err)
			return 1
		}
		if !*quiet {
			fmt.Println("网关服务已停止。")
		}
		return 0
	}
	return boolCode(stopBackground(*quiet))
}

func boolCode(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runRestart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("c", "", "配置文件路径")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway restart [-c 配置文件]")
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	if err := initSecret(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := config.Check(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "配置有误，没有重启（网关保持原样）：", err)
		return 1
	}
	conf, err := openConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	cfg := conf.Current()
	if useSystemd() {
		if err := systemctl("restart", systemdUnit); err != nil {
			fmt.Fprintln(os.Stderr, "重启失败：", err)
			return 1
		}
		fmt.Println("网关服务已重启。")
		return 0
	}
	if err := stopBackground(true); err != nil {
		fmt.Fprintln(os.Stderr, "停止旧进程失败：", err)
		return 1
	}
	if err := startBackground(*cfgPath, filepath.Join(cfg.Session.DataDir, "gateway.log"), false); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	return 0
}

func runLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("c", "", "配置文件路径")
	follow := fs.Bool("f", false, "持续输出")
	n := fs.Int("n", 100, "显示最后几行")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway logs [-c 配置文件] [-f] [-n 100]")
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	conf, err := openConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	path := filepath.Join(conf.Current().Session.DataDir, "gateway.log")
	if *follow {
		cmd := exec.Command("tail", "-n", strconv.Itoa(*n), "-F", path)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if err := printLogTail(path, *n); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func printLogTail(path string, n int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

func runURL(args []string) int {
	cfgPath, rest, err := pathFlag("url", args)
	if err != nil || len(rest) > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway url [-c 配置文件]")
		return 2
	}
	conf, err := openConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	path := filepath.Join(conf.Current().Session.DataDir, "web.url")
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "还没有网页登录链接（网页可能没开，或者网关还没启动）：", err)
		return 1
	}
	fmt.Print(string(b))
	if !strings.HasSuffix(string(b), "\n") {
		fmt.Println()
	}
	return 0
}
