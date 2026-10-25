package mcp

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockTransport 的 Connect 一直等到 release 才返回：模拟起得慢的服务（npx 第一次要下载）、
// 或者管道透传那边内核还没回话。
type blockTransport struct {
	started chan struct{}
	release chan struct{}
	next    sdkmcp.Transport // 放行以后真正去连的（nil 就报连不上）
}

func (b *blockTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if b.next == nil {
		return nil, errors.New("连不上")
	}
	return b.next.Connect(ctx)
}

func oneServer(t *testing.T) *Store {
	t.Helper()
	t.Setenv("NANO_HOME", t.TempDir())
	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]Server{{Name: "demo", Command: "demo", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	return st
}

// 正在连的时候，读缓存的工具列表和连接状态不能等它：网关是在总锁里读的，
// 而连接（管道透传）要等总锁 —— 以前连接时拿着 conn 的锁，两边互相等，网关就死锁了。
func TestCachedToolsDoNotWaitForDial(t *testing.T) {
	m := NewManager(oneServer(t), nil)
	defer m.Close()
	c := m.conn("demo")
	c.tools = []*sdkmcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	bt := &blockTransport{started: make(chan struct{}), release: make(chan struct{})}
	m.dial = func(Server) (sdkmcp.Transport, error) { return bt, nil }
	m.Warm(10 * time.Second)
	<-bt.started
	done := make(chan []ToolInfo, 1)
	go func() {
		m.Status("demo")
		done <- m.CachedTools("default")
	}()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].Exposed != "demo__echo" {
			t.Fatalf("连接中也该给出上次的工具：%+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("读缓存的工具在等正在进行的连接（会让网关卡住甚至死锁）")
	}
	close(bt.release)
}

// 断开（调用出错、改了配置、空闲）只丢连接，不丢工具列表：服务暂时连不上的时候，
// 工具定义照样有它，调用时再报“连不上”。
func TestDropKeepsCachedTools(t *testing.T) {
	m := NewManager(oneServer(t), nil)
	defer m.Close()
	c := m.conn("demo")
	c.tools = []*sdkmcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	m.Reload()
	if got := m.CachedTools("default"); len(got) != 1 {
		t.Fatalf("重载以后工具列表不该清空：%+v", got)
	}
}

// 连到一半配置改了：这条按旧配置连上的连接不要，免得新配置不生效。
func TestReloadDiscardsDialInFlight(t *testing.T) {
	ctx := context.Background()
	cTransport, stop := fakeServer(t, ctx)
	defer stop()
	m := NewManager(oneServer(t), nil)
	defer m.Close()
	bt := &blockTransport{started: make(chan struct{}), release: make(chan struct{}), next: cTransport}
	m.dial = func(Server) (sdkmcp.Transport, error) { return bt, nil }
	errc := make(chan error, 1)
	go func() {
		_, err := m.Tools(ctx, "demo")
		errc <- err
	}()
	<-bt.started
	m.Reload() // 用户在网页上保存了配置
	close(bt.release)
	if err := <-errc; err == nil {
		t.Fatal("配置改过以后，旧的那次连接不该算数")
	}
	if online, _ := m.Status("demo"); online {
		t.Fatal("旧配置的连接不该留下")
	}
}

// 进程自己退出（pipe_closed）以后：表里清掉；退出前最后写的几行还能读到。
func TestPipeClosedDrainsAndForgets(t *testing.T) {
	link := &nopLink{}
	p := NewPipes(link)
	c := &pipeConn{node: "pc", id: "p1", pipes: p, in: make(chan []byte, 8), done: make(chan struct{})}
	p.add(c)
	p.Data("pc", "p1", `{"last":1}`)
	p.Closed("pc", "p1", 0, "")
	b, err := io.ReadAll(c)
	if string(b) != "{\"last\":1}\n" {
		t.Fatalf("最后一行丢了：%q %v", b, err)
	}
	p.mu.Lock()
	n := len(p.conns)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("退出的管道还留在表里：%d", n)
	}
	_ = c.Close()
	if link.closes != 0 {
		t.Fatal("进程已经退出了，不用再让内核关")
	}
}

type nopLink struct{ closes int }

func (l *nopLink) Open(string, string, []string, map[string]string, string) error { return nil }
func (l *nopLink) Write(string, string, []byte) error                             { return nil }
func (l *nopLink) Close(string, string) error                                     { l.closes++; return nil }

// 连上过的服务，工具列表记到磁盘：网关重启以后还没连上时，工具定义照样有它（工具快照不抖、缓存不丢）。
func TestToolCacheSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cTransport, stop := fakeServer(t, ctx)
	defer stop()
	st := oneServer(t)
	m := NewManager(st, nil)
	m.dial = func(Server) (sdkmcp.Transport, error) { return cTransport, nil }
	if _, err := m.Tools(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	before := m.CachedTools("default")
	m.Close()

	m2 := NewManager(st, nil) // 网关重启
	defer m2.Close()
	m2.dial = func(Server) (sdkmcp.Transport, error) {
		t.Fatal("读缓存不该去连")
		return nil, nil
	}
	got := m2.CachedTools("default")
	if len(got) != 2 || got[0].Exposed != "demo__echo" || got[1].Exposed != "demo__hidden" {
		t.Fatalf("重启后应该先用上次的工具列表：%+v", got)
	}
	for i := range got {
		// 一个字节都不能变：工具定义是请求前缀的一部分
		if string(got[i].Schema) != string(before[i].Schema) || got[i].Description != before[i].Description {
			t.Fatalf("重启前后工具定义不一样：%s vs %s", got[i].Schema, before[i].Schema)
		}
	}
}
