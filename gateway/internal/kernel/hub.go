// Package kernel 管理内核的连接：监听、校验 hello、收发 NDJSON 消息。
//
// 可以同时连着好几个内核（节点），按 hello 里的名字区分。同名的新连接会顶掉旧的
// （比如网络切换后重连），旧连接上进行中的 run 全部作废。
package kernel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"nano-agent/gateway/internal/proto"
)

const (
	helloTimeout = 10 * time.Second
	readTimeout  = 120 * time.Second // 内核每 30 秒 ping 一次
	writeTimeout = 30 * time.Second
	maxLine      = 64 << 20
)

// Handler 接收内核的事件。同一个节点的 KernelMsg 在连接的读协程里串行调用，不要阻塞太久。
// local 表示内核和网关在同一台机器上（通过 unix socket 或本机回环地址连上来）。
type Handler interface {
	KernelUp(node string, hello *proto.Msg, local bool)
	KernelMsg(node string, m *proto.Msg)
	KernelDown(node string)
}

type Hub struct {
	token string
	extra []string // 另外接受的口令（网关自己启动的本机内核用的，每次启动随机生成）
	h     Handler
	log   *slog.Logger

	mu         sync.Mutex
	conns      map[string]*conn // 节点名 → 连接
	pairPath   string           // 配对记录（pair.go），空表示没打开配对
	paired     map[string]PairedDevice
	pending    map[string]*PairRequest
	pairNotify func()
}

type conn struct {
	nc     net.Conn
	out    chan []byte
	done   chan struct{}
	once   sync.Once
	paired bool // 用配对得到的口令连上来的
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

// AllowToken 另外接受一个口令，任何名字都能用（网关自己启动的本机内核用）。
func (h *Hub) AllowToken(token string) {
	h.mu.Lock()
	h.extra = append(h.extra, token)
	h.mu.Unlock()
}

func (h *Hub) extraToken(t string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range h.extra {
		if subtle.ConstantTimeCompare([]byte(t), []byte(x)) == 1 {
			return true
		}
	}
	return false
}

func NewHub(token string, h Handler, log *slog.Logger) *Hub {
	return &Hub{token: token, h: h, log: log, conns: map[string]*conn{}}
}

// validNode：节点名会出现在会话设置、日志和网页上，不能有空白、控制字符和冒号，最长 64 个字
func validNode(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == ':' || r == '/' {
			return false
		}
	}
	return true
}

// isLocal 判断连接是不是来自本机：只认 unix socket。
// 从 127.0.0.1 用 TCP 连上来的可能是 WSL 转发的另一台机器（Windows 内核），
// 不能按回环地址判断；网关自己启动的本机内核在 serve 里用 AllowToken 认。
func isLocal(nc net.Conn) bool {
	_, ok := nc.RemoteAddr().(*net.UnixAddr)
	return ok
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
		for _, c := range h.conns {
			c.close()
		}
		h.mu.Unlock()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			h.log.Warn("接受内核连接失败", "err", err)
			time.Sleep(time.Second)
			continue
		}
		go h.serve(nc, isLocal(nc), false)
	}
}

// Connected 报告节点是否在线。
func (h *Hub) Connected(node string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[node] != nil
}

// Send 把消息发给节点 node；它不在线时返回 false。
func (h *Hub) Send(node string, m proto.Msg) bool {
	h.mu.Lock()
	c := h.conns[node]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	return h.send(c, m)
}

func (h *Hub) send(c *conn, m proto.Msg) bool {
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

// Attach 接入一条网关自己建立的连接，不查口令（连接不是从监听的端口进来的）。
// local 表示把它当成本机的内核。目前只有测试用它模拟别的电脑上的内核。
func (h *Hub) Attach(ctx context.Context, nc net.Conn, local bool) {
	go func() {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				nc.Close()
			case <-done:
			}
		}()
		h.serve(nc, local, true)
		close(done)
	}()
}

func ValidNode(name string) bool { return validNode(name) }

// serve 处理一个内核连接。trusted 表示通道是网关自己建立的，不查口令。
func (h *Hub) serve(nc net.Conn, local, trusted bool) {
	c := &conn{nc: nc, out: make(chan []byte, 256), done: make(chan struct{})}
	defer c.close()
	remote := nc.RemoteAddr().String()

	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	nc.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := readMsg(sc)
	reason := ""
	if err == nil && hello.T != proto.THello {
		err = fmt.Errorf("第一条消息应为 hello，收到 %q", hello.T)
	}
	if err == nil && (hello.V < proto.MinVersion || hello.V > proto.Version) {
		fix := "网关太旧了，先升级网关"
		if hello.V < proto.MinVersion {
			fix = "内核太旧了，重新运行安装脚本（或者在源码目录 make）升级"
		}
		err = fmt.Errorf("协议版本不兼容：内核 %d，网关 %d。%s", hello.V, proto.Version, fix)
	} else if err == nil && hello.V < proto.Version {
		// 旧内核先让它连上，随后 maybeUpdateKernel 会把新版本推过去；直接拒绝的话永远推不到
		h.log.Warn("内核的协议版本比较旧，先连上再自动更新", "node", hello.Node, "kernel_proto", hello.V, "gateway_proto", proto.Version)
	}
	if err == nil && !validNode(hello.Node) {
		err = fmt.Errorf("内核的名字 %q 不能用：不能为空、不能有空白和冒号，最长 64 个字（在 kernel.conf 里设 node）", hello.Node)
	}
	if err == nil && hello.Feature("machine_leases") > 0 {
		decoded, decodeErr := hex.DecodeString(hello.NodeEpoch)
		if decodeErr != nil || len(decoded) != 16 {
			err = errors.New("machine_leases 需要 128 位 node_epoch 启动身份")
		}
	}
	if err == nil && !trusted {
		switch {
		case hello.Token == "" && hello.Pair:
			h.pair(nc, sc, hello)
			return
		case hello.Token != "" && subtle.ConstantTimeCompare([]byte(hello.Token), []byte(h.token)) == 1:
		case hello.Token != "" && h.extraToken(hello.Token):
			local = true // 网关自己启动的本机内核，哪怕走的是 127.0.0.1 TCP
		case hello.Token != "" && h.pairedToken(hello.Node, hello.Token):
			c.paired = true
		default:
			err, reason = errors.New("口令不对"), proto.ErrAuth
		}
	}
	if err != nil {
		h.log.Warn("拒绝内核连接", "remote", remote, "err", err)
		b, _ := json.Marshal(proto.Msg{T: proto.TError, Reason: reason, Detail: err.Error()})
		nc.SetWriteDeadline(time.Now().Add(time.Second))
		nc.Write(append(b, '\n'))
		return
	}

	node := hello.Node
	h.mu.Lock()
	old := h.conns[node]
	h.conns[node] = c
	h.mu.Unlock()
	if old != nil {
		h.log.Warn("同名内核的新连接顶替了旧连接（两台机器用了同一个名字的话，会互相顶来顶去）", "node", node)
		old.close()
		h.h.KernelDown(node)
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

	welcome := proto.Msg{T: proto.TWelcome, V: proto.Version}
	if handler, ok := h.h.(interface{ LeaseProtocol() int }); ok && hello.Feature("machine_leases") > 0 {
		welcome.Features = map[string]json.RawMessage{"machine_leases": json.RawMessage(fmt.Sprint(handler.LeaseProtocol()))}
	}
	h.send(c, welcome)
	h.log.Info("内核已连接", "node", node, "tools", len(hello.Tools), "remote", remote)
	h.h.KernelUp(node, hello, local)

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
			h.send(c, proto.Msg{T: proto.TPong})
			continue
		}
		h.h.KernelMsg(node, m)
	}

	h.mu.Lock()
	still := h.conns[node] == c
	if still {
		delete(h.conns, node)
	}
	h.mu.Unlock()
	if still {
		h.h.KernelDown(node)
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
