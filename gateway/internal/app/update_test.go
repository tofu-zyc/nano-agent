package app

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/release"
)

type fakeUpdater struct {
	mu      sync.Mutex
	info    UpdateInfo
	applied []string
	req     RestartRequest
	kernels []string
}

func (f *fakeUpdater) Check(context.Context) (UpdateInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, nil
}

func (f *fakeUpdater) Apply(_ context.Context, v string, req RestartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, v)
	f.req = req
	return nil
}

func (f *fakeUpdater) KernelFiles(_ context.Context, v, osName, arch string) ([]release.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kernels = append(f.kernels, v+"/"+osName+"/"+arch)
	return []release.File{{Name: "nano-kernel", Data: []byte("K")}, {Name: "nano-exec", Data: []byte("X")}}, nil
}

// waitSaid 等频道上收到包含 want 的消息
func (e *env) waitSaid(t *testing.T, want string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-e.ch.sent:
			if strings.Contains(m, want) {
				return m
			}
		case <-deadline:
			t.Fatalf("没等到包含 %q 的消息", want)
		}
	}
}

func TestUpdateCommand(t *testing.T) {
	e := setup(t)
	e.a.SetRuntime("0.1.0", "background", "")
	u := &fakeUpdater{info: UpdateInfo{Updatable: true, Latest: "0.1.0"}}
	e.a.SetUpdater(u)

	e.say("/update")
	e.waitSaid(t, "已经是最新版本 0.1.0")

	u.mu.Lock()
	u.info = UpdateInfo{Updatable: true, Latest: "0.2.0", Available: true}
	u.mu.Unlock()
	e.say("/update")
	e.waitSaid(t, "新版本 0.2.0 已经下载")
	u.mu.Lock()
	if len(u.applied) != 1 || u.applied[0] != "0.2.0" || u.req.SID == "" || u.req.Channel == "" {
		t.Fatalf("更新请求不对：%v %+v", u.applied, u.req)
	}
	u.mu.Unlock()
	if st := e.a.Status(); st.Update == nil || st.Update.Updating != "0.2.0" || !strings.Contains(formatStatusBrief(st), "有新版本 0.2.0") {
		t.Fatalf("status 里没有更新信息：%+v", st.Update)
	}

	// 正在更新时不重复更新
	e.say("/update")
	e.waitSaid(t, "已经在更新到 0.2.0")

	// 不是安装脚本装的：不能自动更新
	e2 := setup(t)
	e2.a.SetRuntime("0.1.0", "foreground", "")
	e2.a.SetUpdater(&fakeUpdater{info: UpdateInfo{Latest: "0.2.0", Available: true, Why: "源码目录"}})
	e2.say("/update")
	e2.waitSaid(t, "不能自动更新：源码目录")
}

// helloVersion 连一个别的电脑上的内核，hello 里带版本
func (e *env) helloVersion(t *testing.T, name string, m proto.Msg) *fakeKernel {
	t.Helper()
	hubSide, kernelSide := net.Pipe()
	e.a.hub.Attach(e.a.ctx, hubSide, false)
	t.Cleanup(func() { kernelSide.Close() })
	k := &fakeKernel{t: t, conn: kernelSide, sc: bufio.NewScanner(kernelSide)}
	m.T, m.V, m.Node, m.Tools = proto.THello, proto.Version, name, echoTool
	go k.send(m)
	k.expect(proto.TWelcome)
	waitNode(e.a, name, true)
	return k
}

func TestKernelAutoUpdate(t *testing.T) {
	e := setup(t)
	e.a.SetRuntime("0.2.0", "background", "")
	u := &fakeUpdater{}
	e.a.SetUpdater(u)

	old := e.helloVersion(t, "oldpc", proto.Msg{Version: "0.1.0", OS: "windows", Arch: "x86_64"})
	m := old.expect(proto.TUpdate)
	if m.Version != "0.2.0" || len(m.Files) != 2 || m.Files[0].Path != "nano-kernel" || m.Files[0].Data != "Sw==" {
		t.Fatalf("推送的更新不对：%+v", m)
	}
	u.mu.Lock()
	if len(u.kernels) != 1 || u.kernels[0] != "0.2.0/windows/x86_64" {
		t.Fatalf("取内核包：%v", u.kernels)
	}
	u.mu.Unlock()

	// 已经是同一个版本、上次自动更新失败过、同一次运行里推过的：都不推
	e.helloVersion(t, "samepc", proto.Msg{Version: "0.2.0", OS: "linux", Arch: "x86_64"}).none()
	e.helloVersion(t, "failedpc", proto.Msg{Version: "0.1.0", UpdateFailed: "0.2.0", OS: "linux", Arch: "x86_64"}).none()
	e.helloVersion(t, "oldpc", proto.Msg{Version: "0.1.0", OS: "windows", Arch: "x86_64"}).none()

	// 网关是源码编译的（不是发布版本）：不推
	e2 := setup(t)
	e2.a.SetRuntime("4f6d325-dirty", "foreground", "")
	e2.a.SetUpdater(&fakeUpdater{})
	e2.helloVersion(t, "pc", proto.Msg{Version: "0.1.0", OS: "linux", Arch: "x86_64"}).none()
}
