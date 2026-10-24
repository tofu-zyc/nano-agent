package main

// 网关自己的更新：下载新版本的包、校验、备份、换程序（bin/、lib/、自带的人格和技能），
// 用新程序检查配置，再重启。新版本启动 3 次都没能正常运行，就自动退回旧版本。

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/control"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/release"
	"nano-agent/gateway/internal/version"
)

const (
	updateMaxTries = 3                // 新版本最多启动几次
	updateHealthy  = 20 * time.Second // 新版本正常运行这么久算更新成功
	shippedFile    = ".shipped.sha256"
)

// updateState 记在 $NANO_HOME/update.json：新进程据此判断要不要退回、给谁回话。
type updateState struct {
	From    string             `json:"from"`
	To      string             `json:"to"`
	Backup  string             `json:"backup"`
	Config  string             `json:"config"`
	DataDir string             `json:"data_dir"`
	Tries   int                `json:"tries"`
	Status  string             `json:"status"` // pending | rolled_back
	Reason  string             `json:"reason,omitempty"`
	Req     app.RestartRequest `json:"req"`
}

func updateStatePath() string { return filepath.Join(paths.Home(), "update.json") }

func readUpdateState() *updateState {
	b, err := os.ReadFile(updateStatePath())
	if err != nil {
		return nil
	}
	var st updateState
	if json.Unmarshal(b, &st) != nil {
		os.Remove(updateStatePath())
		return nil
	}
	return &st
}

func writeUpdateState(st *updateState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(updateStatePath(), b, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b, st.Mode().Perm())
}

// installedExe 确认这个网关是安装脚本装的（$NANO_HOME/bin/nano-gateway）：自动更新只换那里的程序。
func installedExe() (string, error) {
	self, err := currentExe()
	if err != nil {
		return "", err
	}
	want := filepath.Join(paths.Home(), "bin", "nano-gateway")
	if r, err := filepath.EvalSymlinks(want); err == nil {
		want = r
	}
	if self != want {
		return "", fmt.Errorf("这个网关（%s）不是用安装脚本装在 %s 的；源码目录里用 git pull && make install 更新", self, paths.Home())
	}
	return self, nil
}

// gatewayUpdater 实现 app.Updater。
type gatewayUpdater struct {
	cfgPath string
	conf    *config.Store
	restart func(app.RestartRequest) error // 换好程序后让网关重启（不写重启标记，回话由 update.json 负责）
	log     *slog.Logger
}

func (u *gatewayUpdater) client() *release.Client {
	cfg := u.conf.Current()
	return release.New(cfg.Update.URL, cfg.Network.Proxy)
}

func (u *gatewayUpdater) Check(ctx context.Context) (app.UpdateInfo, error) {
	info := app.UpdateInfo{Current: version.Version, Updatable: true}
	if _, err := installedExe(); err != nil {
		info.Updatable, info.Why = false, err.Error()
	}
	latest, err := u.client().Latest(ctx)
	if err != nil {
		return info, err
	}
	info.Latest = latest
	info.Available = release.Compare(latest, version.Version) > 0
	return info, nil
}

func (u *gatewayUpdater) Apply(ctx context.Context, ver string, req app.RestartRequest) error {
	notes, err := installGatewayVersion(ctx, u.client(), u.cfgPath, u.conf.Current().Session.DataDir, ver, req)
	if err != nil {
		return err
	}
	for _, n := range notes {
		u.log.Info("更新", "note", n)
	}
	return u.restart(req)
}

func (u *gatewayUpdater) KernelFiles(ctx context.Context, ver, osName, arch string) ([]release.File, error) {
	return kernelFiles(ctx, u.client(), ver, osName, arch)
}

// kernelFiles 取某个系统和架构的内核、执行器和 rg。
func kernelFiles(ctx context.Context, c *release.Client, ver, osName, arch string) ([]release.File, error) {
	p, err := c.Fetch(ctx, ver, release.KernelAsset(ver, osName, arch))
	if err != nil {
		return nil, err
	}
	files, err := release.Unpack(p)
	if err != nil {
		return nil, err
	}
	ext := ""
	if osName == "windows" {
		ext = ".exe"
	}
	want := map[string]bool{"nano-kernel" + ext: true, "nano-exec" + ext: true, "rg" + ext: true}
	var out []release.File
	for _, f := range files {
		if want[f.Name] {
			out = append(out, f)
			delete(want, f.Name)
		}
	}
	if want["nano-kernel"+ext] || want["nano-exec"+ext] {
		return nil, fmt.Errorf("%s 包里没有内核或执行器", release.KernelAsset(ver, osName, arch))
	}
	return out, nil
}

// installGatewayVersion 下载、校验、预检新版本，备份后换上，写 update.json。不重启。
// 返回给人看的提示（比如改过的人格文件，新版本放成了 .new）。
func installGatewayVersion(ctx context.Context, c *release.Client, cfgPath, dataDir, ver string, req app.RestartRequest) ([]string, error) {
	home := paths.Home()
	if _, err := installedExe(); err != nil {
		return nil, err
	}
	pkg, err := c.Fetch(ctx, ver, release.GatewayAsset(ver))
	if err != nil {
		return nil, err
	}
	files, err := release.Unpack(pkg)
	if err != nil {
		return nil, fmt.Errorf("解开 %s 失败：%w", filepath.Base(pkg), err)
	}
	byName := map[string]release.File{}
	for _, f := range files {
		byName[f.Name] = f
	}
	for _, need := range []string{"nano-gateway", "lib/nano-kernel", "lib/nano-exec"} {
		if _, ok := byName[need]; !ok {
			return nil, fmt.Errorf("%s 包里没有 %s", filepath.Base(pkg), need)
		}
	}

	// 先用新程序自己检查：能运行、版本对、能读懂现在的配置
	staging := filepath.Join(home, "run", "update-"+ver)
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	newBin := filepath.Join(staging, "nano-gateway")
	if err := writeFileAtomic(newBin, byName["nano-gateway"].Data, 0o755); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, newBin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != ver {
		return nil, fmt.Errorf("新版本的程序运行不了或者版本不对（%q）：%v", strings.TrimSpace(string(out)), err)
	}
	chk := exec.CommandContext(ctx, newBin, "check-config", "-c", cfgPath)
	chk.Env = append(os.Environ(), "NANO_HOME="+home)
	if msg, err := chk.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("新版本读不了现在的配置，没有更新：%s", strings.TrimSpace(string(msg)))
	}

	// 备份：程序、配置、data 下的 JSON（会话记录只追加，不用备份）
	backup := filepath.Join(home, "backup", version.Version+"-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return nil, err
	}
	bak := func(src, rel string) error {
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return copyFile(src, filepath.Join(backup, rel))
	}
	if err := bak(filepath.Join(home, "bin", "nano-gateway"), "bin/nano-gateway"); err != nil {
		return nil, fmt.Errorf("备份失败：%w", err)
	}
	if ents, err := os.ReadDir(filepath.Join(home, "lib")); err == nil {
		for _, e := range ents {
			if e.Type().IsRegular() {
				if err := bak(filepath.Join(home, "lib", e.Name()), "lib/"+e.Name()); err != nil {
					return nil, fmt.Errorf("备份失败：%w", err)
				}
			}
		}
	}
	if err := bak(cfgPath, "gateway.toml"); err != nil {
		return nil, fmt.Errorf("备份失败：%w", err)
	}
	if ents, err := os.ReadDir(dataDir); err == nil {
		for _, e := range ents {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
				if err := bak(filepath.Join(dataDir, e.Name()), "data/"+e.Name()); err != nil {
					return nil, fmt.Errorf("备份失败：%w", err)
				}
			}
		}
	}

	// 换程序。bin/nano-gateway 直接改名覆盖：正在运行的旧程序不受影响，重启时执行的就是新的
	for _, f := range files {
		var dst string
		switch {
		case f.Name == "nano-gateway":
			dst = filepath.Join(home, "bin", "nano-gateway")
		case strings.HasPrefix(f.Name, "lib/") && !strings.Contains(f.Name[4:], "/"):
			dst = filepath.Join(home, filepath.FromSlash(f.Name))
		case f.Name == "VERSION":
			dst = filepath.Join(home, "lib", "VERSION")
		default:
			continue
		}
		mode := os.FileMode(0o755)
		if f.Name == "VERSION" {
			mode = 0o644
		}
		if err := writeFileAtomic(dst, f.Data, mode); err != nil {
			rollbackFiles(backup, home, cfgPath, dataDir)
			return nil, fmt.Errorf("换程序失败（已换回旧的）：%w", err)
		}
	}
	notes := installShipped(home, files)

	st := &updateState{From: version.Version, To: ver, Backup: backup, Config: cfgPath, DataDir: dataDir,
		Status: "pending", Req: req}
	if err := writeUpdateState(st); err != nil {
		rollbackFiles(backup, home, cfgPath, dataDir)
		return nil, err
	}
	pruneBackups(filepath.Join(home, "backup"), 3)
	return notes, nil
}

// installShipped 更新包里自带的人格（agents/、personas/）和技能：用户没改过的（和上次装的一样）才覆盖，
// 改过的保留，新版本写成 .new；用户删掉的不再放回来。清单记在 .shipped.sha256。
func installShipped(home string, files []release.File) []string {
	old := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(home, shippedFile)); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			if h, p, ok := strings.Cut(sc.Text(), "  "); ok {
				old[p] = h
			}
		}
	}
	cur := map[string]string{}
	for p, h := range old {
		cur[p] = h
	}
	var notes []string
	for _, f := range files {
		// 包里自带的人格（agents/ 下，某个 agent 的 persona.md）、会话人格和技能
		if !strings.HasPrefix(f.Name, "agents/") && !strings.HasPrefix(f.Name, "personas/") && !strings.HasPrefix(f.Name, "skills/") {
			continue
		}
		sum := sha256.Sum256(f.Data)
		h := hex.EncodeToString(sum[:])
		dst := filepath.Join(home, filepath.FromSlash(f.Name))
		mode := f.Mode.Perm()
		if mode == 0 {
			mode = 0o600
		}
		mine, err := os.ReadFile(dst)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if _, shipped := old[f.Name]; shipped {
				continue // 以前装过、用户删掉了
			}
			writeFileAtomic(dst, f.Data, mode)
		case err != nil:
			continue
		default:
			ms := sha256.Sum256(mine)
			mh := hex.EncodeToString(ms[:])
			switch {
			case mh == h:
			case old[f.Name] == mh:
				writeFileAtomic(dst, f.Data, mode)
			default:
				writeFileAtomic(dst+".new", f.Data, mode)
				notes = append(notes, fmt.Sprintf("%s 你改过，新版本放在 %s.new", f.Name, f.Name))
			}
		}
		cur[f.Name] = h
	}
	keys := make([]string, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s  %s\n", cur[k], k)
	}
	writeFileAtomic(filepath.Join(home, shippedFile), []byte(b.String()), 0o600)
	return notes
}

// rollbackFiles 把备份里的程序、配置和 data 下的 JSON 放回去。
func rollbackFiles(backup, home, cfgPath, dataDir string) error {
	var firstErr error
	put := func(rel, dst string) {
		src := filepath.Join(backup, rel)
		if _, err := os.Stat(src); err != nil {
			return
		}
		if err := copyFile(src, dst); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	put("bin/nano-gateway", filepath.Join(home, "bin", "nano-gateway"))
	if ents, err := os.ReadDir(filepath.Join(backup, "lib")); err == nil {
		for _, e := range ents {
			put("lib/"+e.Name(), filepath.Join(home, "lib", e.Name()))
		}
	}
	put("gateway.toml", cfgPath)
	if ents, err := os.ReadDir(filepath.Join(backup, "data")); err == nil {
		for _, e := range ents {
			put("data/"+e.Name(), filepath.Join(dataDir, e.Name()))
		}
	}
	return firstErr
}

// pruneBackups 只留最近几份更新前的备份。
func pruneBackups(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "import-") {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, _ := os.Stat(filepath.Join(dir, names[i]))
		b, _ := os.Stat(filepath.Join(dir, names[j]))
		return a != nil && b != nil && a.ModTime().After(b.ModTime())
	})
	for i, n := range names {
		if i >= keep {
			os.RemoveAll(filepath.Join(dir, n))
		}
	}
}

// updateGuard 在网关启动最开始调用（读配置之前，配置读不了也算一次失败）。
// 返回要在启动后处理的更新状态：pending 的等正常运行一会儿后算成功，rolled_back 的告诉发起的人。
func updateGuard(log *slog.Logger) *updateState {
	st := readUpdateState()
	if st == nil {
		return nil
	}
	switch st.Status {
	case "rolled_back":
		return st
	case "pending":
	default:
		os.Remove(updateStatePath())
		return nil
	}
	if st.To != version.Version {
		// 跑的不是要更新到的版本（比如手动换回去了）
		st.Status, st.Reason = "rolled_back", "新版本没有运行"
		writeUpdateState(st)
		return st
	}
	st.Tries++
	if st.Tries > updateMaxTries {
		log.Error("新版本启动了好几次都没能正常运行，退回旧版本", "to", st.To, "from", st.From)
		if err := rollbackFiles(st.Backup, paths.Home(), st.Config, st.DataDir); err != nil {
			log.Error("退回旧版本失败", "err", err, "backup", st.Backup)
			os.Remove(updateStatePath())
			return nil
		}
		st.Status, st.Reason = "rolled_back", fmt.Sprintf("新版本启动了 %d 次都没能正常运行", updateMaxTries)
		writeUpdateState(st)
		if err := reexec(os.Args); err != nil {
			log.Error("启动旧版本失败", "err", err)
			os.Exit(1)
		}
	}
	writeUpdateState(st)
	return st
}

// finishUpdate 在网关启动完成后调用：更新成功（正常运行一会儿后）或者已经退回，都告诉发起的人。
func finishUpdate(ctx context.Context, a *app.App, st *updateState, log *slog.Logger) {
	if st == nil {
		return
	}
	if st.Status == "rolled_back" {
		os.Remove(updateStatePath())
		log.Warn("更新失败，已退回", "to", st.To, "from", st.From, "reason", st.Reason)
		a.Notify(st.Req, fmt.Sprintf("更新到 %s 失败，已退回 %s：%s。日志：nano-gateway logs", st.To, st.From, st.Reason))
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(updateHealthy):
		}
		os.Remove(updateStatePath())
		log.Info("更新成功", "from", st.From, "to", st.To)
		a.Notify(st.Req, fmt.Sprintf("网关已更新到 %s（原来是 %s）", st.To, st.From))
	}()
}

// runCheckConfig 是更新前新程序自检用的：能不能读懂现在的配置。
func runCheckConfig(args []string) int {
	cfgPath, rest, err := pathFlag("check-config", args)
	if err != nil || len(rest) > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway check-config [-c 配置文件]")
		return 2
	}
	if err := initSecret(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := config.Check(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// runUpdate：nano-gateway update [--check] [--version x.y.z] [--yes]
func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("c", "", "配置文件路径")
	checkOnly := fs.Bool("check", false, "只查有没有新版本")
	want := fs.String("version", "", "更新到指定版本（也可以用来退回旧版本）")
	yes := fs.Bool("yes", false, "不提问")
	fs.BoolVar(yes, "y", false, "不提问")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway update [--check] [--version x.y.z] [--yes] [-c 配置文件]")
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	conf, err := openConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置有误：", err)
		return 1
	}
	cfg := conf.Current()
	c := release.New(cfg.Update.URL, cfg.Network.Proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	target := strings.TrimPrefix(*want, "v")
	if target == "" {
		if target, err = c.Latest(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "查最新版本失败：", err)
			fmt.Fprintln(os.Stderr, "访问不了 GitHub 的话：在网页拓扑图的网关节点里填「默认代理」，或者把「下载地址」换成镜像（也可以设 NANO_DOWNLOAD_URL）")
			return 1
		}
	} else if !release.IsRelease(target) {
		fmt.Fprintln(os.Stderr, "版本号要写成 x.y.z")
		return 2
	}
	cur := version.Version
	fmt.Printf("现在的版本：%s；", cur)
	if *want == "" {
		fmt.Printf("最新版本：%s\n", target)
	} else {
		fmt.Printf("要换成：%s\n", target)
	}
	if *want == "" && release.Compare(target, cur) <= 0 {
		fmt.Println("已经是最新版本。")
		return 0
	}
	if *checkOnly {
		fmt.Println("可以更新：nano-gateway update")
		return 0
	}
	if target == cur {
		fmt.Println("已经是这个版本。")
		return 0
	}
	if _, err := installedExe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !*yes && !confirmTTY(fmt.Sprintf("更新网关 %s → %s？内核和执行器一起更新，配置和数据不动", cur, target)) {
		fmt.Println("已取消。")
		return 0
	}
	fmt.Println("下载并校验…")
	notes, err := installGatewayVersion(ctx, c, *cfgPath, cfg.Session.DataDir, target, app.RestartRequest{By: "cli", At: time.Now().UnixMilli()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "更新失败：", err)
		return 1
	}
	for _, n := range notes {
		fmt.Println("提示：" + n)
	}
	if _, err := control.Query(cfg.Session.DataDir); err != nil {
		fmt.Printf("已换成 %s。网关没有在运行，nano-gateway start 启动。\n", target)
		return 0
	}
	fmt.Println("已换好，重启网关…")
	if code := runRestart([]string{"-c", *cfgPath}); code != 0 {
		return code
	}
	// 等新版本正常运行一会儿（或者起不来自动退回）
	deadline := time.Now().Add(updateHealthy + 40*time.Second)
	for time.Now().Before(deadline) {
		st := readUpdateState()
		if st != nil && st.Status == "rolled_back" {
			fmt.Fprintf(os.Stderr, "新版本没能正常运行，已退回 %s：%s。日志：nano-gateway logs\n", st.From, st.Reason)
			return 1
		}
		if st == nil {
			// 新版本确认成功、或者退回后旧版本回过话，都会删掉 update.json：看现在跑的是哪个版本
			if raw, err := control.Query(cfg.Session.DataDir); err == nil {
				var now struct {
					Version string `json:"version"`
				}
				json.Unmarshal(raw, &now)
				switch {
				case now.Version == target:
					fmt.Printf("网关已更新到 %s。\n", target)
					return 0
				case now.Version != "":
					fmt.Fprintf(os.Stderr, "新版本没能正常运行，已退回 %s。日志：nano-gateway logs\n", now.Version)
					return 1
				}
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Println("新版本已经启动，还在确认；过一会儿用 nano-gateway status 看看。")
	return 0
}

// confirmTTY 在终端上问一句（默认是）；没有终端时当作否。
func confirmTTY(q string) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	fmt.Printf("%s [Y/n]：", q)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	switch strings.TrimSpace(line) {
	case "", "y", "Y", "yes", "是":
		return true
	}
	return false
}
