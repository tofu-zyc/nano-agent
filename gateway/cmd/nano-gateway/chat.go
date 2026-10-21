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

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
)

// runChat 是 `nano-gateway chat`：在终端里和正在运行的网关聊天。只负责收发文字，
// 怎么显示（颜色、思考过程、工具调用）由网关决定。Ctrl-D 或 Ctrl-C 退出，网关照常运行。
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
	select {
	case <-closed:
		select {
		case <-inputDone:
		default:
			fmt.Fprintln(os.Stderr, "\n和网关的连接断了（网关停了，或者另一个终端接了进来）。")
			return 1
		}
	case <-sig:
		fmt.Println()
	}
	return 0
}
