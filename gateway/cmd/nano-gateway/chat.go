package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
)

// runChat 是 `nano-gateway chat`：在终端里和正在运行的网关聊天。只负责收发文字，
// 怎么显示（颜色、思考过程、工具调用）由网关决定。Ctrl-C 停下正在进行的任务，连按两次退出；
// Ctrl-D 退出。退出后网关照常运行。
func runChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cfgPath := fs.String("c", "gateway.toml", "配置文件路径")
	fs.Parse(args)

	conf, _, err := config.Open(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	cfg := conf.Current()
	if !config.On(cfg.CLI.Enabled) {
		fmt.Fprintln(os.Stderr, "配置里关掉了终端聊天（[cli] enabled = false）")
		return 1
	}
	sock, err := channel.CLISocket(cfg.Session.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连不上网关（%s）。网关没在运行的话，先在另一个终端里 make run。\n", sock)
		return 1
	}
	conn := c.(*net.UnixConn)
	hello := channel.ChatHello
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
		hello += " color"
	}
	fmt.Fprintln(conn, hello)

	closed := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, conn)
		close(closed)
	}()
	inputDone := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if _, err := fmt.Fprintln(conn, sc.Text()); err != nil {
				break
			}
		}
		conn.CloseWrite() // 告诉网关不会再说话了，网关处理完会断开
		close(inputDone)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var last time.Time
	for {
		select {
		case <-closed:
			select {
			case <-inputDone:
				return 0
			default:
				fmt.Fprintln(os.Stderr, "\n和网关的连接断了（网关停了，或者另一个终端接了进来）。")
				return 1
			}
		case s := <-sig:
			if s != os.Interrupt || time.Since(last) < 2*time.Second {
				fmt.Println()
				return 0
			}
			last = time.Now()
			// 第一次 Ctrl-C：让网关停下这个会话正在进行的任务（没有任务时什么也不做）
			fmt.Fprintln(conn, channel.ChatStop)
			fmt.Fprintln(os.Stderr, "\n（再按一次 Ctrl-C 退出）")
		}
	}
}
