package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// CLI 是命令行聊天通道，主要用来本地调试。身份固定为 cli:local，会话为 cli:local。
type CLI struct {
	in  io.Reader
	out io.Writer
	mu  sync.Mutex
}

func NewCLI(in io.Reader, out io.Writer) *CLI { return &CLI{in: in, out: out} }

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
		h(c, Message{ChatID: "local", SenderID: "local", SenderName: "你", Text: line, Mentioned: true})
	}
	return sc.Err()
}

func (c *CLI) Send(_ context.Context, _, text, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := fmt.Fprintf(c.out, "bot> %s\n", strings.ReplaceAll(text, "\n", "\n     "))
	return err
}
