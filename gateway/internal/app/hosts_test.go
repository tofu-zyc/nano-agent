package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// fakeHost 是网关能直接执行工具的一台机器的替身：记下执行请求，Ls 列真实的目录
type fakeHost struct {
	mu     sync.Mutex
	reqs   []map[string]any
	put    map[string][]host.File
	result host.Result
	block  bool          // Exec 一直等到 ctx 结束
	gate   chan struct{} // 不为空时 Exec 等它关上再返回
}

func (f *fakeHost) Exec(ctx context.Context, req []byte) host.Result {
	var m map[string]any
	json.Unmarshal(req, &m)
	f.mu.Lock()
	f.reqs = append(f.reqs, m)
	block, res, gate := f.block, f.result, f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return host.Result{Content: "已停止"}
		}
	}
	if block {
		<-ctx.Done()
		return host.Result{Content: "已停止"}
	}
	return res
}

func (f *fakeHost) lastReq(t *testing.T) map[string]any {
	t.Helper()
	for i := 0; i < 300; i++ {
		f.mu.Lock()
		n := len(f.reqs)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("机器上没有执行请求")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeHost) Ls(_ context.Context, path string) (*host.Listing, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = home + strings.TrimPrefix(path, "~")
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	l := &host.Listing{OK: true, Path: path, Home: "/home/x"}
	for _, e := range ents {
		if e.IsDir() {
			l.Entries = append(l.Entries, proto.DirEntry{Name: e.Name()})
		}
	}
	return l, nil
}

func (f *fakeHost) PutFiles(_ context.Context, dir string, files []host.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.put == nil {
		f.put = map[string][]host.File{}
	}
	f.put[dir] = files
	return nil
}

type fakeHosts struct {
	mu    sync.Mutex
	hosts map[string]*fakeHost
	infos map[string]host.Info
}

func newFakeHosts() *fakeHosts {
	f := &fakeHosts{hosts: map[string]*fakeHost{}, infos: map[string]host.Info{}}
	f.set(host.LocalName, "local", true, 7, "")
	return f
}

// set 添加或者修改一台机器的状态
func (f *fakeHosts) set(name, kind string, ready bool, landlock int, detail string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts[name] == nil {
		f.hosts[name] = &fakeHost{result: host.Result{OK: true, Content: "执行结果"}}
	}
	f.infos[name] = host.Info{Name: name, Kind: kind, Ready: ready, Landlock: landlock, Detail: detail}
	return f.hosts[name]
}

func (f *fakeHosts) Host(name string) (host.Host, host.Info, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.infos[name]
	if !ok {
		return nil, host.Info{}, false
	}
	if !info.Ready {
		return nil, info, true
	}
	return f.hosts[name], info, true
}

func (f *fakeHosts) Infos() []host.Info {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []host.Info
	for _, i := range f.infos {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind == "local" || out[i].Name < out[j].Name })
	return out
}

// useDir 添加本机上的一个工作目录（不存在就建），设为新会话的默认，返回它的路径
func (e *env) useDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(e.dir, name)
	os.MkdirAll(dir, 0o755)
	if _, err := e.a.AddWorkspace(context.Background(), host.LocalName, dir, true); err != nil {
		t.Fatal(err)
	}
	return dir
}
