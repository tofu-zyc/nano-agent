package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 协议 v10：把 MCP 服务的 stdio 透传到别的电脑上 —— 网关不自己起进程，而是让那台机器的
// 内核起（pipe_open），网关这边每写一行就是 pipe_write，内核把 stdout 的每一行用 pipe_data
// 送回来，进程退出是 pipe_closed。这样 computer use 这类服务可以挂在装了桌面环境的机器上。

// Link 是把管道消息送到某台机器上的通道（网关用内核连接实现）。
type Link interface {
	Open(node, pipe string, argv []string, env map[string]string, cwd string) error
	Write(node, pipe string, line []byte) error
	Close(node, pipe string) error
}

// Pipes 管着所有走透传的管道，地址是「机器/编号」。
type Pipes struct {
	link  Link
	mu    sync.Mutex
	conns map[string]*pipeConn
}

// NewPipes 建一个管道表。
func NewPipes(link Link) *Pipes {
	return &Pipes{link: link, conns: map[string]*pipeConn{}}
}

func key(node, id string) string { return node + "/" + id }

func (p *Pipes) add(c *pipeConn) {
	p.mu.Lock()
	p.conns[key(c.node, c.id)] = c
	p.mu.Unlock()
}

func (p *Pipes) remove(node, id string) {
	p.mu.Lock()
	delete(p.conns, key(node, id))
	p.mu.Unlock()
}

// Data 是内核送来的一行 stdout。
func (p *Pipes) Data(node, id, line string) {
	p.mu.Lock()
	c := p.conns[key(node, id)]
	p.mu.Unlock()
	if c != nil {
		c.feed([]byte(line))
	}
}

// Closed 是内核说进程退出了（code 是退出码）。
func (p *Pipes) Closed(node, id string, code int, msg string) {
	p.mu.Lock()
	c := p.conns[key(node, id)]
	p.mu.Unlock()
	if c != nil {
		c.finish(fmt.Errorf("MCP 进程退出（%d）：%s", code, msg))
		p.remove(node, id) // 进程已经没了：表里也清掉（之后的 Close 不用再通知内核）
	}
}

// newPipeID 是管道编号（每次连接一个，同一台机器上可以同时挂好几个服务）。
func newPipeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("p%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// PipeTransport 是在某台机器上跑 MCP 进程的传输。
type PipeTransport struct {
	Pipes *Pipes
	Node  string
	Argv  []string
	Env   map[string]string
	Cwd   string
	ID    string
}

// Connect 让那台机器起进程，然后把管道的两端接成 JSON-RPC。
func (t *PipeTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	if t.Pipes == nil || t.Pipes.link == nil {
		return nil, errors.New("MCP 透传还没接上")
	}
	id := t.ID
	if id == "" {
		id = newPipeID()
	}
	c := &pipeConn{node: t.Node, id: id, pipes: t.Pipes, in: make(chan []byte, 128), done: make(chan struct{})}
	t.Pipes.add(c)
	if err := t.Pipes.link.Open(t.Node, id, t.Argv, t.Env, t.Cwd); err != nil {
		t.Pipes.remove(t.Node, id)
		return nil, fmt.Errorf("在 %s 上起 MCP 进程失败：%w", t.Node, err)
	}
	io := &sdkmcp.IOTransport{Reader: c, Writer: c, MaxLineLength: 32 << 20}
	return io.Connect(ctx)
}

// pipeConn 是一条管道的本地一端：Read 等内核送来的行，Write 交给内核写进进程的 stdin。
type pipeConn struct {
	node  string
	id    string
	pipes *Pipes
	in    chan []byte
	rest  []byte
	done  chan struct{}
	once  sync.Once
	err   error
}

func (c *pipeConn) feed(line []byte) {
	select {
	case c.in <- line:
	case <-c.done:
	}
}

func (c *pipeConn) finish(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.done)
	})
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if len(c.rest) == 0 {
		select {
		case line := <-c.in:
			c.rest = append(line, '\n')
		case <-c.done:
			// 进程退出前最后写的几行可能还在队列里：先读完再报结束
			select {
			case line := <-c.in:
				c.rest = append(line, '\n')
			default:
				if c.err != nil {
					return 0, c.err
				}
				return 0, io.EOF
			}
		}
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

// Write 收到的是 JSON-RPC 的一整条消息（可能带换行），按行交给内核。
func (c *pipeConn) Write(p []byte) (int, error) {
	start := 0
	for i, b := range p {
		if b != '\n' {
			continue
		}
		if err := c.pipes.link.Write(c.node, c.id, p[start:i]); err != nil {
			return start, err
		}
		start = i + 1
	}
	if start < len(p) {
		if err := c.pipes.link.Write(c.node, c.id, p[start:]); err != nil {
			return start, err
		}
	}
	return len(p), nil
}

// Close 关掉管道（先关 stdin，内核那边不退就杀）。
func (c *pipeConn) Close() error {
	select {
	case <-c.done:
		// 进程已经退出了，或者那台机器断开了：内核那边没有这条管道了，不用再通知
		c.pipes.remove(c.node, c.id)
	default:
		c.finish(nil)
		c.pipes.remove(c.node, c.id)
		if c.pipes.link != nil {
			return c.pipes.link.Close(c.node, c.id)
		}
	}
	return nil
}

// DropNode 是某台机器的内核断开了：它上面起的 MCP 进程都没了，等它重连再重新起。
func (p *Pipes) DropNode(node string) {
	p.mu.Lock()
	var hits []*pipeConn
	for k, c := range p.conns {
		if c.node == node {
			hits = append(hits, c)
			delete(p.conns, k)
		}
	}
	p.mu.Unlock()
	for _, c := range hits {
		c.finish(fmt.Errorf("%s 的内核断开了，MCP 进程跟着没了", node))
	}
}
