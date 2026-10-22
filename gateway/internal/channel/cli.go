package channel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/paths"
)

// CLI 是终端聊天渠道。网关在数据目录里开一个 unix socket（只有自己能连），
// 另开一个终端运行 `nano-gateway chat` 连上来聊天，随时进出，网关照常运行。
// 身份和聊天窗口都是 cli:local，默认接在主会话上；同一时间只接一个终端，新连上来的会顶掉旧的。
//
// 回复边生成边显示，思考过程和工具调用用灰色；同一个会话里别的窗口发来的消息和回复也同步显示。
// 终端那边只负责收发文字，怎么显示都在这里决定。
type CLI struct {
	statusBox
	sock string
	log  *slog.Logger

	mu            sync.Mutex
	showReasoning bool
	showUsage     bool
	conn          net.Conn  // 当前连着的终端，nil 表示没有（输出直接丢掉）
	out           io.Writer // 写给终端的输出；测试里可以换成别的
	color         bool
	midLine       bool // 光标不在行首
	inGray        bool
	onConnect     func()
}

// ChatHello 是 `nano-gateway chat` 连上来后发的第一行，后面跟着 color 表示终端支持颜色。
const ChatHello = "nano-chat 1"

// ChatStop 是终端里按 Ctrl-C 时客户端发的一行：停下正在进行的任务。用户打不出这一行。
const ChatStop = "\x1b[nano:stop]"

const (
	gray  = "\x1b[90m"
	reset = "\x1b[0m"
)

// CLISocket 是终端聊天用的 unix socket 路径：$NANO_HOME/run/cli.sock；路径太长时
// 退回 $XDG_RUNTIME_DIR 或 /tmp 下的私有目录（见 paths.Socket）。网关和 chat 按同样的规则算。
func CLISocket(dataDir string) (string, error) {
	p := paths.Socket("cli.sock", dataDir)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := privateDir(dir); err != nil {
		return "", err
	}
	if len(p) > 100 {
		return "", fmt.Errorf("socket 路径太长：%s", p)
	}
	return p, nil
}

// NewCLI 创建终端渠道，sock 是 unix socket 的路径（用 CLISocket 算）。
func NewCLI(sock string, showReasoning, showUsage bool, log *slog.Logger) *CLI {
	c := &CLI{sock: sock, log: log, showReasoning: showReasoning, showUsage: showUsage, out: io.Discard}
	c.st = Status{State: StateIdle, Identity: "make chat 接入", Since: time.Now().UnixMilli()}
	return c
}

func (c *CLI) Name() string { return "cli" }

// Socket 是终端连接用的 unix socket 路径。
func (c *CLI) Socket() string { return c.sock }

// OnConnect 设置终端连上来之后的回调（调度层用它打招呼、补上最近的对话）。
func (c *CLI) OnConnect(f func()) {
	c.mu.Lock()
	c.onConnect = f
	c.mu.Unlock()
}

// SetDisplay 修改显示选项（网页里改了配置时调用）。
func (c *CLI) SetDisplay(showReasoning, showUsage bool) {
	c.mu.Lock()
	c.showReasoning, c.showUsage = showReasoning, showUsage
	c.mu.Unlock()
}

func (c *CLI) Run(ctx context.Context, h Handler) error {
	ln, err := listenUnix(c.sock)
	if err != nil {
		c.setState(StateError, err.Error())
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer os.Remove(c.sock)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				c.drop(nil)
				return nil
			}
			return err
		}
		go c.serve(conn, h)
	}
}

// listenUnix 监听 socket，权限只给自己。已经有网关在用这个 socket 就报错，否则是上次没清理掉的，删掉重建。
func listenUnix(path string) (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("%s 已经有一个网关在用了", path)
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (c *CLI) serve(conn net.Conn, h Handler) {
	defer conn.Close()
	if err := checkPeer(conn); err != nil {
		c.log.Warn("拒绝终端连接", "err", err)
		return
	}
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	hello, err := br.ReadString('\n')
	conn.SetReadDeadline(time.Time{})
	f := strings.Fields(hello)
	if err != nil || len(f) < 2 || strings.Join(f[:2], " ") != ChatHello {
		fmt.Fprintln(conn, "连接方式不对，请用 nano-gateway chat 连接")
		return
	}

	c.mu.Lock()
	if c.conn != nil {
		c.write("\n另一个终端接进来了，这里断开。\n")
		c.conn.Close()
	}
	c.conn, c.out, c.color = conn, &deadlineWriter{conn}, slices.Contains(f[2:], "color")
	c.midLine, c.inGray = false, false
	onConnect := c.onConnect
	c.mu.Unlock()
	c.update(func(s *Status) { s.State, s.Identity = StateOnline, "终端已连接" })
	c.log.Info("终端已连接")
	if onConnect != nil {
		onConnect()
	}

	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m := Message{ChatID: "local", SenderID: "local", SenderName: "你", ChatName: "命令行", Text: line, Mentioned: true}
		if line == ChatStop {
			m.Text, m.Action = "", ActionStop
		}
		h(c, m)
	}
	c.drop(conn)
}

// drop 断开终端 conn（nil 表示不管是哪个）。新终端顶掉旧终端时，旧的那个已经不是当前连接了，不用管。
func (c *CLI) drop(conn net.Conn) {
	c.mu.Lock()
	if c.conn == nil || (conn != nil && c.conn != conn) {
		c.mu.Unlock()
		return
	}
	c.conn.Close()
	c.conn, c.out = nil, io.Discard
	c.mu.Unlock()
	c.update(func(s *Status) { s.State, s.Identity = StateIdle, "make chat 接入" })
	c.log.Info("终端已断开")
}

// deadlineWriter 每次写都带超时：终端卡住（比如被 Ctrl-Z 挂起）时不能把网关也卡住
type deadlineWriter struct{ conn net.Conn }

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	n, err := w.conn.Write(p)
	if err != nil {
		w.conn.Close() // 读的那边会跟着结束，然后 drop
	}
	return n, err
}

func (c *CLI) Send(_ context.Context, _, text, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineStart()
	c.write("bot> " + strings.ReplaceAll(text, "\n", "\n     ") + "\n")
	return nil
}

// Notice 用灰色显示一段系统说明（不是机器人说的话），比如补上的聊天记录。
func (c *CLI) Notice(_, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineStart()
	c.setGray(true)
	c.write(strings.TrimRight(text, "\n"))
	c.setGray(false)
	c.write("\n")
}

// Echo 显示同一个会话里别的窗口发来的消息。
func (c *CLI) Echo(_, via, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineStart()
	c.setGray(true)
	c.write("[" + via + "] ")
	c.setGray(false)
	c.write(strings.ReplaceAll(text, "\n", "\n     ") + "\n")
}

// 以下方法都要持有 c.mu

func (c *CLI) write(s string) {
	if _, err := io.WriteString(c.out, s); err != nil && !errors.Is(err, net.ErrClosed) {
		c.log.Debug("写终端失败", "err", err)
	}
}

func (c *CLI) setGray(on bool) {
	if !c.color || c.inGray == on {
		return
	}
	if on {
		c.write(gray)
	} else {
		c.write(reset)
	}
	c.inGray = on
}

// lineStart 结束当前行（如果有），恢复颜色
func (c *CLI) lineStart() {
	c.setGray(false)
	if c.midLine {
		c.write("\n")
		c.midLine = false
	}
}

func (c *CLI) dimLine(text string) {
	c.lineStart()
	c.setGray(true)
	c.write(text)
	c.setGray(false)
	c.write("\n")
}

// ---- 流式显示 ----

func (c *CLI) OpenRun(_, _ string) RunView { return &cliRun{c: c} }

func (c *CLI) OpenMirror(chat, via, text string) RunView {
	c.Echo(chat, via, text)
	return c.OpenRun(chat, "")
}

type cliRun struct {
	c       *CLI
	state   int // 0 没在输出，1 正在输出思考，2 正在输出正文
	pending int // 还没输出的换行：等后面有内容再输出，消息末尾的换行就不会多出空行
	closed  bool
}

var cliPrefix = map[int]struct{ head, indent string }{
	1: {"思考> ", "      "},
	2: {"bot> ", "     "},
}

func (r *cliRun) write(state int, text string) {
	c := r.c
	if r.state != state {
		r.end()
		c.lineStart()
		c.setGray(state == 1)
		c.write(cliPrefix[state].head)
		c.midLine = true
		r.state = state
	}
	for i, part := range strings.Split(text, "\n") {
		if i > 0 {
			r.pending++
		}
		if part == "" {
			continue
		}
		if r.pending > 0 {
			c.write(strings.Repeat("\n", r.pending) + cliPrefix[state].indent)
			r.pending = 0
		}
		c.write(part)
	}
}

func (r *cliRun) end() {
	if r.state == 0 {
		return
	}
	r.c.lineStart()
	r.state, r.pending = 0, 0
}

func (r *cliRun) Reasoning(text string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if !r.closed && r.c.showReasoning {
		r.write(1, text)
	}
}

func (r *cliRun) Content(text string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if !r.closed {
		r.write(2, text)
	}
}

func (r *cliRun) EndMessage() {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.end()
}

func (r *cliRun) Tool(name, detail string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if !r.closed {
		r.end()
		r.c.dimLine("  ⚙ " + name + "  " + detail)
	}
}

func (r *cliRun) ToolResult(ok bool, detail string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.closed {
		return
	}
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	r.end()
	r.c.dimLine("    " + mark + " " + detail)
}

func (r *cliRun) Note(text string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if !r.closed && r.c.showUsage {
		r.end()
		r.c.dimLine("  " + text)
	}
}

func (r *cliRun) Close() {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.end()
	r.closed = true
}
