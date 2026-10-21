// Package kernel 管理内核的连接：监听、校验 hello、收发 NDJSON 消息。
//
// 同一时间只认一个内核；新内核连上来会顶掉旧的（旧连接上进行中的 run 全部作废）。
package kernel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/proto"
)

const (
	helloTimeout = 10 * time.Second
	readTimeout  = 120 * time.Second // 内核每 30 秒 ping 一次
	writeTimeout = 30 * time.Second
	maxLine      = 64 << 20
)

// Handler 接收内核的事件。KernelMsg 在连接的读协程里串行调用，不要阻塞太久。
type Handler interface {
	KernelUp(node string, tools []proto.ToolSpec)
	KernelMsg(m *proto.Msg)
	KernelDown()
}

type Hub struct {
	token string
	h     Handler
	log   *slog.Logger

	mu  sync.Mutex
	cur *conn
}

type conn struct {
	nc   net.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

func NewHub(token string, h Handler, log *slog.Logger) *Hub {
	return &Hub{token: token, h: h, log: log}
}

// Listen 打开监听地址：unix:/path、tcp:host:port 或 host:port。
func Listen(addr string) (net.Listener, error) {
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
			if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
				c.Close()
				return nil, errors.New("这个地址上已经有网关在运行")
			}
			os.Remove(p) // 上次没清理掉的 socket 文件
		}
		ln, err := net.Listen("unix", p)
		if err == nil {
			os.Chmod(p, 0o600)
		}
		return ln, err
	}
	return net.Listen("tcp", strings.TrimPrefix(addr, "tcp:"))
}

// Serve 在 ln 上接受内核连接，直到 ctx 结束。
func (h *Hub) Serve(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		ln.Close()
		h.mu.Lock()
		if h.cur != nil {
			h.cur.close()
		}
		h.mu.Unlock()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.log.Warn("接受内核连接失败", "err", err)
			time.Sleep(time.Second)
			continue
		}
		go h.handle(nc)
	}
}

// Connected 报告当前是否有内核在线。
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur != nil
}

// Send 把消息发给当前内核；没有内核在线时返回 false。
func (h *Hub) Send(m proto.Msg) bool {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c == nil {
		return false
	}
	b, err := json.Marshal(m)
	if err != nil {
		h.log.Error("序列化消息失败", "err", err)
		return false
	}
	select {
	case c.out <- append(b, '\n'):
		return true
	case <-c.done:
		return false
	}
}

func (h *Hub) handle(nc net.Conn) {
	c := &conn{nc: nc, out: make(chan []byte, 256), done: make(chan struct{})}
	defer c.close()
	remote := nc.RemoteAddr().String()

	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	nc.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := readMsg(sc)
	if err == nil && hello.T != proto.THello {
		err = fmt.Errorf("第一条消息应为 hello，收到 %q", hello.T)
	}
	if err == nil && subtle.ConstantTimeCompare([]byte(hello.Token), []byte(h.token)) != 1 {
		err = errors.New("token 不对")
	}
	if err == nil && hello.V != proto.Version {
		err = fmt.Errorf("协议版本不兼容：内核 %d，网关 %d", hello.V, proto.Version)
	}
	if err != nil {
		h.log.Warn("拒绝内核连接", "remote", remote, "err", err)
		b, _ := json.Marshal(proto.Msg{T: proto.TError, Detail: err.Error()})
		nc.SetWriteDeadline(time.Now().Add(time.Second))
		nc.Write(append(b, '\n'))
		return
	}

	h.mu.Lock()
	old := h.cur
	h.cur = c
	h.mu.Unlock()
	if old != nil {
		h.log.Warn("新的内核连接顶替了旧连接", "node", hello.Node)
		old.close()
		h.h.KernelDown()
	}

	go func() {
		for {
			select {
			case b := <-c.out:
				nc.SetWriteDeadline(time.Now().Add(writeTimeout))
				if _, err := nc.Write(b); err != nil {
					h.log.Warn("写内核连接失败", "err", err)
					c.close()
					return
				}
			case <-c.done:
				return
			}
		}
	}()

	h.Send(proto.Msg{T: proto.TWelcome, V: proto.Version})
	h.log.Info("内核已连接", "node", hello.Node, "tools", len(hello.Tools), "remote", remote)
	h.h.KernelUp(hello.Node, hello.Tools)

	for {
		nc.SetReadDeadline(time.Now().Add(readTimeout))
		m, err := readMsg(sc)
		if err != nil {
			select {
			case <-c.done:
			default:
				h.log.Warn("内核连接断开", "node", hello.Node, "err", err)
			}
			break
		}
		if m.T == proto.TPing {
			h.Send(proto.Msg{T: proto.TPong})
			continue
		}
		h.h.KernelMsg(m)
	}

	h.mu.Lock()
	still := h.cur == c
	if still {
		h.cur = nil
	}
	h.mu.Unlock()
	if still {
		h.h.KernelDown()
	}
}

func readMsg(sc *bufio.Scanner) (*proto.Msg, error) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m proto.Msg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("消息不是合法 JSON：%w", err)
		}
		return &m, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("连接已关闭")
}
