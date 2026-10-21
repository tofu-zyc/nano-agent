package kernel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
)

// kconn 是测试里的一个内核连接
type kconn struct {
	c  net.Conn
	sc *bufio.Scanner
}

func hello(t *testing.T, sock string, m proto.Msg) *kconn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	m.T, m.V = proto.THello, proto.Version
	b, _ := json.Marshal(m)
	c.Write(append(b, '\n'))
	return &kconn{c, bufio.NewScanner(c)}
}

func (k *kconn) next(t *testing.T) proto.Msg {
	t.Helper()
	k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if !k.sc.Scan() {
		t.Fatal("没有收到消息")
	}
	var m proto.Msg
	json.Unmarshal(k.sc.Bytes(), &m)
	return m
}

func (k *kconn) closed(t *testing.T) {
	t.Helper()
	k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if k.sc.Scan() {
		t.Fatalf("连接应该断开，又收到：%s", k.sc.Text())
	}
}

func waitPending(t *testing.T, h *Hub, n int) []PairRequest {
	t.Helper()
	for i := 0; i < 300; i++ {
		if reqs := h.PairRequests(); len(reqs) == n {
			return reqs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("配对请求不是 %d 个：%v", n, h.PairRequests())
	return nil
}

func TestPairing(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "k.sock")
	ln, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recHandler{}
	h := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	store := filepath.Join(dir, "paired.json")
	if err := h.EnablePairing(store); err != nil {
		t.Fatal(err)
	}
	changes := make(chan struct{}, 100)
	h.OnPairChange(func() { changes <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Serve(ctx, ln)

	// 没有口令：请求配对，网页上看到请求，内核拿到确认码
	k := hello(t, sock, proto.Msg{Pair: true, Node: "laptop"})
	m := k.next(t)
	if m.T != proto.TPairing || len(m.Code) != 6 {
		t.Fatalf("pairing: %+v", m)
	}
	reqs := waitPending(t, h, 1)
	if reqs[0].Node != "laptop" || reqs[0].Code != m.Code {
		t.Fatalf("request: %+v", reqs[0])
	}
	if h.Connected("laptop") {
		t.Fatal("还没确认就上线了")
	}
	// 等待期间 ping 照常有回应
	k.c.Write([]byte(`{"t":"ping"}` + "\n"))
	if m := k.next(t); m.T != proto.TPong {
		t.Fatalf("ping: %+v", m)
	}
	// 同名的第二个请求被拒绝
	if m := hello(t, sock, proto.Msg{Pair: true, Node: "laptop"}).next(t); m.T != proto.TError {
		t.Fatalf("同名请求：%+v", m)
	}

	if err := h.DecidePair(reqs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	m = k.next(t)
	if m.T != proto.TPaired || len(m.Token) != 64 {
		t.Fatalf("paired: %+v", m)
	}
	token := m.Token
	k.c.Close()
	waitPending(t, h, 0)
	if b, _ := os.ReadFile(store); strings.Contains(string(b), token) || !strings.Contains(string(b), "laptop") {
		t.Fatalf("配对记录应当只存口令的摘要：%s", b)
	}
	if st, _ := os.Stat(store); st.Mode().Perm() != 0o600 {
		t.Fatalf("配对记录的权限：%v", st.Mode())
	}

	// 用配对的口令连上来；换个名字就不认
	k = hello(t, sock, proto.Msg{Token: token, Node: "laptop"})
	if m := k.next(t); m.T != proto.TWelcome {
		t.Fatalf("配对的口令连不上：%+v", m)
	}
	if m := hello(t, sock, proto.Msg{Token: token, Node: "pc"}).next(t); m.T != proto.TError || m.Reason != proto.ErrAuth {
		t.Fatalf("配对的口令冒充了别的节点：%+v", m)
	}
	// 已经配对过的名字不能再申请
	if m := hello(t, sock, proto.Msg{Pair: true, Node: "laptop"}).next(t); m.T != proto.TError {
		t.Fatalf("重名配对：%+v", m)
	}
	// 重启网关：配对记录还在
	h2 := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h2.EnablePairing(store); err != nil || len(h2.PairedDevices()) != 1 || !h2.pairedToken("laptop", token) {
		t.Fatalf("重启后配对记录丢了：%v %v", err, h2.PairedDevices())
	}

	// 移除：在线的连接断开，口令作废
	if err := h.Unpair("laptop"); err != nil {
		t.Fatal(err)
	}
	k.closed(t)
	if m := hello(t, sock, proto.Msg{Token: token, Node: "laptop"}).next(t); m.T != proto.TError || m.Reason != proto.ErrAuth {
		t.Fatalf("移除后口令还能用：%+v", m)
	}

	// 拒绝：内核收到 denied 后退出
	k = hello(t, sock, proto.Msg{Pair: true, Node: "phone"})
	k.next(t)
	reqs = waitPending(t, h, 1)
	h.DecidePair(reqs[0].ID, false)
	if m := k.next(t); m.T != proto.TError || m.Reason != proto.ErrDenied {
		t.Fatalf("deny: %+v", m)
	}

	// 内核断开：请求撤销
	k = hello(t, sock, proto.Msg{Pair: true, Node: "tablet"})
	k.next(t)
	waitPending(t, h, 1)
	k.c.Close()
	waitPending(t, h, 0)
	if len(changes) == 0 {
		t.Fatal("配对变化没有通知网页")
	}

	// 共用口令照常能用，名字随意
	if m := hello(t, sock, proto.Msg{Token: "tok", Node: "anything"}).next(t); m.T != proto.TWelcome {
		t.Fatalf("共用口令：%+v", m)
	}
}

// 网关自己建立的通道（SSH）不查口令
func TestAttachTrusted(t *testing.T) {
	rec := &recHandler{}
	h := NewHub("tok", rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hubSide, kernelSide := net.Pipe()
	h.Attach(ctx, hubSide, false)
	b, _ := json.Marshal(proto.Msg{T: proto.THello, V: proto.Version, Node: "server"})
	go kernelSide.Write(append(b, '\n'))
	sc := bufio.NewScanner(kernelSide)
	kernelSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	if !sc.Scan() || !strings.Contains(sc.Text(), proto.TWelcome) {
		t.Fatalf("SSH 通道上的内核没有被接受：%s", sc.Text())
	}
	if ev := rec.wait(t, 1); ev[0] != "up server" {
		t.Fatalf("events: %v", ev)
	}
	kernelSide.Close()
}
