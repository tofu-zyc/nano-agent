package kernel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
)

func TestListenUnix(t *testing.T) {
	addr := "unix:" + filepath.Join(t.TempDir(), "k.sock")
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(addr); err == nil {
		t.Fatal("第二个网关不应该抢走正在使用的 socket")
	}

	// 留下一个没人监听的 socket 文件，模拟上次异常退出
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln, err = Listen(addr)
	if err != nil {
		t.Fatalf("残留的 socket 文件应该被清理掉：%v", err)
	}
	ln.Close()
}

type recHandler struct {
	mu     sync.Mutex
	events []string
}

func (r *recHandler) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}
func (r *recHandler) KernelUp(node string, _ *proto.Msg, local bool) {
	if local {
		r.add("up " + node + " local")
	} else {
		r.add("up " + node)
	}
}
func (r *recHandler) KernelMsg(node string, m *proto.Msg) { r.add("msg " + node + " " + m.T) }
func (r *recHandler) KernelDown(node string)              { r.add("down " + node) }

func (r *recHandler) wait(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < 300; i++ {
		r.mu.Lock()
		if len(r.events) >= n {
			out := append([]string(nil), r.events...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("事件不够 %d 个：%v", n, r.events)
	return nil
}

func dial(t *testing.T, sock, node string) (net.Conn, *proto.Msg) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(proto.Msg{T: proto.THello, V: proto.Version, Token: "tok", Node: node})
	c.Write(append(b, '\n'))
	sc := bufio.NewScanner(c)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !sc.Scan() {
		t.Fatalf("%s：没有回复", node)
	}
	var m proto.Msg
	json.Unmarshal(sc.Bytes(), &m)
	return c, &m
}

// 从 127.0.0.1 TCP 连上来的远程内核不算本机（WSL 网关 + Windows 内核就是这种情况）；
// 只有网关自己启动的本机内核（AllowToken）走 TCP 也算本机。
func TestLoopbackTCPIsNotLocal(t *testing.T) {
	ln, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recHandler{}
	h := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Serve(ctx, ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := json.Marshal(proto.Msg{T: proto.THello, V: proto.Version, Token: "tok", Node: "winpc"})
	c.Write(append(b, '\n'))
	sc := bufio.NewScanner(c)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !sc.Scan() {
		t.Fatal("没有回复")
	}
	var m proto.Msg
	json.Unmarshal(sc.Bytes(), &m)
	if m.T != proto.TWelcome {
		t.Fatalf("welcome: %+v", m)
	}
	ev := rec.wait(t, 1)
	if ev[0] != "up winpc" {
		t.Fatalf("TCP 回环连接不应该算本机：%v", ev)
	}

	// 网关自己启动的内核用额外口令，哪怕走 TCP 也算本机
	ln2, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec2 := &recHandler{}
	h2 := NewHub("tok", rec2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h2.AllowToken("managed")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go h2.Serve(ctx2, ln2)
	c2, err := net.Dial("tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	b2, _ := json.Marshal(proto.Msg{T: proto.THello, V: proto.Version, Token: "managed", Node: "gateway"})
	c2.Write(append(b2, '\n'))
	sc2 := bufio.NewScanner(c2)
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !sc2.Scan() {
		t.Fatal("没有回复")
	}
	ev2 := rec2.wait(t, 1)
	if ev2[0] != "up gateway local" {
		t.Fatalf("AllowToken 的 TCP 连接应该算本机：%v", ev2)
	}
}

// 同时连着好几个内核，按名字区分；同名的新连接顶掉旧的；名字不合规的拒绝
func TestHubNodes(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "k.sock")
	ln, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recHandler{}
	h := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Serve(ctx, ln)

	a, m := dial(t, sock, "pc")
	if m.T != proto.TWelcome {
		t.Fatalf("pc: %+v", m)
	}
	b, _ := dial(t, sock, "phone")
	rec.wait(t, 2)
	if !h.Connected("pc") || !h.Connected("phone") || h.Connected("nas") {
		t.Fatal("两个内核都应该在线")
	}
	if !h.Send("phone", proto.Msg{T: proto.TCancel, SID: "s"}) || h.Send("nas", proto.Msg{T: proto.TCancel}) {
		t.Fatal("send")
	}
	b.Write([]byte(`{"t":"notice","sid":"s","text":"x"}` + "\n"))
	rec.wait(t, 3)

	// pc 换了网络重连：新连接顶掉旧的
	a2, _ := dial(t, sock, "pc")
	defer a2.Close()
	ev := rec.wait(t, 5)
	if ev[0] != "up pc local" || ev[1] != "up phone local" || ev[2] != "msg phone notice" || ev[3] != "down pc" || ev[4] != "up pc local" {
		t.Fatalf("events: %v", ev)
	}
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err == nil {
		t.Fatal("旧连接应该被关掉")
	}

	if _, m := dial(t, sock, "bad name"); m.T != proto.TError {
		t.Fatalf("名字里有空格应该拒绝：%+v", m)
	}
	b.Close()
	if ev := rec.wait(t, 6); ev[5] != "down phone" {
		t.Fatalf("events: %v", ev)
	}
}

// dialV 用指定的协议版本握手。
func dialV(t *testing.T, addr, node string, v int) (net.Conn, *proto.Msg) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(proto.Msg{T: proto.THello, V: v, Token: "tok", Node: node})
	c.Write(append(b, '\n'))
	sc := bufio.NewScanner(c)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !sc.Scan() {
		t.Fatalf("%s：没有回复", node)
	}
	var m proto.Msg
	json.Unmarshal(sc.Bytes(), &m)
	return c, &m
}

// 旧协议的内核要先让连上（不然自动更新永远推不到它），太老的才拒绝。
func TestOldKernelCanConnect(t *testing.T) {
	ln, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub("tok", &recHandler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Serve(ctx, ln)
	addr := ln.Addr().String()

	c, m := dialV(t, addr, "old", proto.Version-1)
	if m.T != proto.TWelcome {
		t.Fatalf("旧一版的内核应该先放进来（好推新版本）：%+v", m)
	}
	c.Close()

	c2, m2 := dialV(t, addr, "ancient", proto.MinVersion-1)
	if m2.T != proto.TError {
		t.Fatalf("太老的内核应该拒绝：%+v", m2)
	}
	c2.Close()
}

type leasedHandler struct{ recHandler }

func (*leasedHandler) LeaseProtocol() int { return 1 }
func TestMachineLeaseNegotiationRequiresValidBootIdentity(t *testing.T) {
	for _, epoch := range []string{"", "other/boot", "abc", "00112233445566778899aabbccddeeff"} {
		t.Run(epoch, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &leasedHandler{}
			h := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
			server, client := net.Pipe()
			defer client.Close()
			h.Attach(ctx, server, true)
			hello := proto.Msg{T: proto.THello, V: proto.Version, Node: "node", NodeEpoch: epoch,
				Features: map[string]json.RawMessage{"machine_leases": json.RawMessage("1")}}
			if err := json.NewEncoder(client).Encode(hello); err != nil {
				t.Fatal(err)
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			var reply proto.Msg
			if err := json.NewDecoder(client).Decode(&reply); err != nil {
				t.Fatal(err)
			}
			if len(epoch) == 32 {
				if reply.T != proto.TWelcome || reply.Feature("machine_leases") != 1 {
					t.Fatalf("capability not negotiated: %+v", reply)
				}
			} else if reply.T != proto.TError {
				t.Fatalf("invalid boot identity admitted: %+v", reply)
			}
		})
	}
}
