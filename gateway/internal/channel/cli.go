package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// CLI 是命令行聊天通道，主要用来本地调试。身份固定为 cli:local，会话为 cli:local。
// 回复边生成边打印，思考过程和工具调用用灰色显示。
type CLI struct {
	in            io.Reader
	out           io.Writer
	showReasoning bool
	showUsage     bool
	color         bool

	mu      sync.Mutex
	midLine bool // 光标不在行首
	inGray  bool
}

const (
	gray  = "\x1b[90m"
	reset = "\x1b[0m"
)

func NewCLI(in io.Reader, out io.Writer, showReasoning, showUsage bool) *CLI {
	c := &CLI{in: in, out: out, showReasoning: showReasoning, showUsage: showUsage}
	if f, ok := out.(*os.File); ok && os.Getenv("NO_COLOR") == "" {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			c.color = true
		}
	}
	return c
}

func (c *CLI) Name() string { return "cli" }

func (c *CLI) Run(ctx context.Context, h Handler) error {
	sc := bufio.NewScanner(c.in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		h(c, Message{ChatID: "local", SenderID: "local", SenderName: "你", ChatName: "命令行", Text: line, Mentioned: true})
	}
	return sc.Err()
}

func (c *CLI) Status() Status        { return Status{State: StateOnline, Identity: "本机终端"} }
func (c *CLI) OnStatus(func(Status)) {}

// SetDisplay 修改显示选项（网页里改了配置时调用）。
func (c *CLI) SetDisplay(showReasoning, showUsage bool) {
	c.mu.Lock()
	c.showReasoning, c.showUsage = showReasoning, showUsage
	c.mu.Unlock()
}

func (c *CLI) Send(_ context.Context, _, text, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineStart()
	_, err := fmt.Fprintf(c.out, "bot> %s\n", strings.ReplaceAll(text, "\n", "\n     "))
	return err
}

// 以下方法都要持有 c.mu

func (c *CLI) setGray(on bool) {
	if !c.color || c.inGray == on {
		return
	}
	if on {
		io.WriteString(c.out, gray)
	} else {
		io.WriteString(c.out, reset)
	}
	c.inGray = on
}

// lineStart 结束当前行（如果有），恢复颜色
func (c *CLI) lineStart() {
	c.setGray(false)
	if c.midLine {
		io.WriteString(c.out, "\n")
		c.midLine = false
	}
}

func (c *CLI) dimLine(text string) {
	c.lineStart()
	c.setGray(true)
	io.WriteString(c.out, text)
	c.setGray(false)
	io.WriteString(c.out, "\n")
}

// ---- 流式显示 ----

func (c *CLI) OpenRun(_, _ string) RunView { return &cliRun{c: c} }

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
		io.WriteString(c.out, cliPrefix[state].head)
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
			io.WriteString(c.out, strings.Repeat("\n", r.pending)+cliPrefix[state].indent)
			r.pending = 0
		}
		io.WriteString(c.out, part)
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
