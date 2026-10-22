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
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/secret"
)

// runChat 是 `nano-gateway chat`：在终端里和正在运行的网关聊天。只负责收发文字，
// 怎么显示（颜色、思考过程、工具调用）由网关决定。Ctrl-C 停下正在进行的任务，连按两次退出；
// Ctrl-D 退出。网关重启时自动重连，重连后会收到“网关已重启”。
func runChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cfgPath := fs.String("c", "", "配置文件路径（默认 $NANO_HOME/gateway.toml）")
	fs.Parse(args)
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

	// 标准输入只读一次；网关重启时把还没发出去的行留到新连接。
	input := make(chan string, 64)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			input <- sc.Text()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var last time.Time
	hadConn := false
	var conn net.Conn
	var closed chan struct{}

	writeLine := func(line string) bool {
		if conn == nil {
			return false
		}
		_, err := fmt.Fprintln(conn, line)
		return err == nil
	}
	connect := func() bool {
		c, err := net.Dial("unix", sock)
		if err != nil {
			if !hadConn {
				fmt.Fprintf(os.Stderr, "连不上网关（%s）。网关没在运行的话，先运行 nano-gateway start。\n", sock)
			}
			return false
		}
		conn = c
		hello := channel.ChatHello
		if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
			hello += " color"
		}
		if _, err := fmt.Fprintln(conn, hello); err != nil {
			conn.Close()
			conn = nil
			return false
		}
		closed = make(chan struct{})
		go func() {
			io.Copy(os.Stdout, c)
			close(closed)
		}()
		hadConn = true
		return true
	}

	if !connect() {
		return 1
	}
	for {
		select {
		case line, ok := <-input:
			if !ok {
				if conn != nil {
					if uc, ok := conn.(*net.UnixConn); ok {
						uc.CloseWrite()
					}
				}
				// 等网关把剩下的回复发完
				if closed != nil {
					<-closed
				}
				return 0
			}
			if !writeLine(line) {
				// 连接刚好断了，等重连后重发这一行
				input <- line
			}
		case <-closed:
			conn.Close()
			conn = nil
			select {
			case <-inputDone:
				return 0
			default:
			}
			fmt.Fprintln(os.Stderr, "\n和网关的连接断了（网关重启或停了），正在重连…")
			ok := false
			for i := 0; i < 60; i++ {
				time.Sleep(500 * time.Millisecond)
				if connect() {
					ok = true
					break
				}
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "重连不上网关。重新运行 nano-gateway chat 试试。")
				return 1
			}
			fmt.Fprintln(os.Stderr, "已重新连上网关。")
		case s := <-sig:
			if s != os.Interrupt || time.Since(last) < 2*time.Second {
				fmt.Println()
				return 0
			}
			last = time.Now()
			// 第一次 Ctrl-C：让网关停下这个会话正在进行的任务（没有任务时什么也不做）
			writeLine(channel.ChatStop)
			fmt.Fprintln(os.Stderr, "\n（再按一次 Ctrl-C 退出）")
		}
	}
}
