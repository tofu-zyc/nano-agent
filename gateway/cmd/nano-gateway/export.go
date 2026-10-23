package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/control"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/paths"
)

// 导出包里的顶层目录
const exportRoot = "nano-agent-export"

// data/ 里不带走的：日志、登录链接、本机内核配置、重启标记和临时文件（新机器上会重新生成）
func skipDataFile(rel string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(base, "gateway.log"), strings.HasPrefix(base, "kernel.log"):
		return true
	case base == "web.url", base == "local-kernel.conf", base == "restart.json", base == "control.sock",
		strings.HasSuffix(base, ".pid"), strings.HasSuffix(base, ".status.json"), strings.HasSuffix(base, ".stop"): // 本机内核的运行状态
		return true
	case strings.HasSuffix(base, ".tmp"), strings.HasSuffix(base, ".sock"):
		return true
	}
	return false
}

// runExport 把配置、数据、密钥文件、人格和技能打成一个包，搬到另一台机器上用 import 导入。
func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	cfgPath := fs.String("c", "", "配置文件路径")
	out := fs.String("o", "", "导出到哪个文件")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway export [-c 配置文件] [-o 文件]")
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = paths.Config()
	}
	if *out == "" {
		*out = "nano-agent-export-" + time.Now().Format("20060102-150405") + ".tar.gz"
	}
	n, err := exportTo(*cfgPath, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "导出失败：", err)
		os.Remove(*out)
		return 1
	}
	fmt.Printf("已导出到 %s（%d 个文件）\n", *out, n)
	fmt.Println("注意：这个文件里有 secret.key，能解开所有 API key 和密码，像密码一样保管，用完删掉。")
	fmt.Println("在新机器上：nano-gateway import " + filepath.Base(*out) + "（或者一行安装时加 --import）")
	return 0
}

func exportTo(cfgPath, out string) (int, error) {
	conf, err := openConfig(cfgPath)
	if err != nil {
		return 0, fmt.Errorf("配置有误：%w", err)
	}
	cfg := conf.Current()
	keyPath := filepath.Join(paths.Home(), "secret.key")
	if _, err := os.Stat(keyPath); err != nil {
		return 0, fmt.Errorf("找不到密钥文件 %s：%w", keyPath, err)
	}
	toml, err := os.ReadFile(cfgPath)
	if err != nil {
		return 0, err
	}

	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	count := 0
	addFile := func(name string, data []byte) error {
		count++
		hdr := &tar.Header{Name: exportRoot + "/" + name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addTree := func(dir, prefix string, skip func(string) bool) error {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() || !d.Type().IsRegular() || (skip != nil && skip(rel)) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return addFile(prefix+"/"+rel, b)
		})
	}

	if err := addFile("gateway.toml", []byte(portableConfig(string(toml), cfg.Kernel.Listen))); err != nil {
		return 0, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return 0, err
	}
	if err := addFile("secret.key", key); err != nil {
		return 0, err
	}
	if err := addTree(cfg.Session.DataDir, "data", skipDataFile); err != nil {
		return 0, err
	}
	if err := addTree(cfg.Persona.Dir, "personas", nil); err != nil {
		return 0, err
	}
	if err := addTree(cfg.Skills.Dir, "skills", nil); err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return count, f.Close()
}

// portableConfig 把配置里和这台机器有关的路径换成相对配置文件的默认位置：
// 数据、人格、技能目录，以及 unix socket（新机器上放在 run/ 下）。
func portableConfig(text, listen string) string {
	section := ""
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			section = strings.Trim(trim, "[] ")
		}
		key := ""
		if k, _, ok := strings.Cut(trim, "="); ok && !strings.HasPrefix(trim, "#") {
			key = strings.TrimSpace(k)
		}
		switch {
		case section == "session" && key == "data_dir":
			line = `data_dir = "data"`
		case section == "persona" && key == "dir":
			line = `dir = "personas"`
		case section == "skills" && key == "dir":
			line = `dir = "skills"`
		case section == "kernel" && key == "listen" && strings.HasPrefix(listen, "unix:"):
			line = `listen = "unix:run/gateway.sock"`
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// runImport 把 export 的包导入到 $NANO_HOME（默认 ~/.nano-agent）。
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	force := fs.Bool("force", false, "已有配置和数据时先备份再覆盖")
	oldLocal := fs.String("old-local-as", "", "旧网关“本机”上的工作目录改记到这个机器名下")
	// 允许文件写在选项前面：nano-gateway import 包.tar.gz --old-local-as wsl
	var file string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		file, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway import <文件> [--force] [--old-local-as 机器名]")
		return 2
	}
	if file == "" && fs.NArg() == 1 {
		file = fs.Arg(0)
	} else if fs.NArg() > 0 || file == "" {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway import <文件> [--force] [--old-local-as 机器名]")
		return 2
	}
	if *oldLocal != "" && (!kernel.ValidNode(*oldLocal) || *oldLocal == host.LocalName) {
		fmt.Fprintf(os.Stderr, "机器名 %q 不能用：不能为空、不能有空白和冒号\n", *oldLocal)
		return 2
	}
	home := paths.Home()
	if _, err := control.Query(filepath.Join(home, "data")); err == nil {
		fmt.Fprintln(os.Stderr, "网关正在运行，先 nano-gateway stop 再导入")
		return 1
	}
	report, err := importFrom(file, home, *force, *oldLocal)
	if err != nil {
		fmt.Fprintln(os.Stderr, "导入失败：", err)
		return 1
	}
	fmt.Print(report)
	return 0
}

func importFrom(file, home string, force bool, oldLocal string) (string, error) {
	var b strings.Builder
	existing := []string{}
	for _, name := range []string{"gateway.toml", "secret.key", "data", "personas", "skills"} {
		p := filepath.Join(home, name)
		if st, err := os.Stat(p); err == nil && (!st.IsDir() || !dirEmpty(p)) {
			existing = append(existing, name)
		}
	}
	if len(existing) > 0 {
		if !force {
			return "", fmt.Errorf("%s 里已经有 %s；确定要用导入的覆盖就加 --force（会先备份）", home, strings.Join(existing, "、"))
		}
		backup := filepath.Join(home, "backup", "import-"+time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(backup, 0o700); err != nil {
			return "", err
		}
		for _, name := range existing {
			if err := os.Rename(filepath.Join(home, name), filepath.Join(backup, name)); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&b, "原来的 %s 已备份到 %s\n", strings.Join(existing, "、"), backup)
	}

	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("不是导出包：%w", err)
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("导出包坏了：%w", err)
		}
		rel, ok := strings.CutPrefix(path.Clean(hdr.Name), exportRoot+"/")
		if !ok || !fs.ValidPath(rel) || hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return "", err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		n++
	}
	if _, err := os.Stat(filepath.Join(home, "gateway.toml")); err != nil {
		return "", errors.New("导出包里没有 gateway.toml")
	}
	if _, err := os.Stat(filepath.Join(home, "secret.key")); err != nil {
		return "", errors.New("导出包里没有 secret.key，加密保存的 API key 和密码解不开")
	}
	fmt.Fprintf(&b, "已导入 %d 个文件到 %s\n", n, home)

	if oldLocal != "" {
		moved, err := moveLocalWorkspaces(filepath.Join(home, "data", "workspaces.json"), oldLocal)
		if err != nil {
			return "", err
		}
		if moved > 0 {
			fmt.Fprintf(&b, "旧网关“本机”上的 %d 个工作目录改记到 %s 名下：在旧机器上装内核、名字用 %s 连过来就能接着用\n", moved, oldLocal, oldLocal)
		}
	}
	b.WriteString(importChecks(home))
	return b.String(), nil
}

func dirEmpty(p string) bool {
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) == 0
}

// moveLocalWorkspaces 把 workspaces.json 里机器是“本机”的改成 name。
func moveLocalWorkspaces(file, name string) (int, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var list []map[string]any
	if err := json.Unmarshal(b, &list); err != nil {
		return 0, fmt.Errorf("workspaces.json 看不懂：%w", err)
	}
	n := 0
	for _, w := range list {
		if w["host"] == host.LocalName {
			w["host"] = name
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	out, _ := json.MarshalIndent(list, "", "  ")
	return n, os.WriteFile(file, out, 0o600)
}

// importChecks 列出导入后要注意的地方。
func importChecks(home string) string {
	var b strings.Builder
	if b2, err := os.ReadFile(filepath.Join(home, "data", "workspaces.json")); err == nil {
		var list []struct{ Host, Path string }
		if json.Unmarshal(b2, &list) == nil {
			for _, w := range list {
				if w.Host != host.LocalName {
					continue
				}
				p := w.Path
				if strings.HasPrefix(p, "~/") {
					if h, err := os.UserHomeDir(); err == nil {
						p = filepath.Join(h, p[2:])
					}
				}
				if _, err := os.Stat(p); err != nil {
					fmt.Fprintf(&b, "注意：工作目录 本机:%s 在这台机器上不存在（可以在网页上删掉，或者导入时用 --old-local-as）\n", w.Path)
				}
			}
		}
	}
	if b2, err := os.ReadFile(filepath.Join(home, "data", "paired.json")); err == nil {
		var list []struct{ Node string }
		if json.Unmarshal(b2, &list) == nil && len(list) > 0 {
			names := make([]string, 0, len(list))
			for _, p := range list {
				names = append(names, p.Node)
			}
			fmt.Fprintf(&b, "注意：配对过的电脑（%s）要改连这台机器的地址：在那台电脑上重新运行安装脚本，--gateway 写这台机器的地址（配对口令保留）\n", strings.Join(names, "、"))
		}
	}
	if t, err := os.ReadFile(filepath.Join(home, "gateway.toml")); err == nil {
		if strings.Contains(string(t), `listen = "unix:`) {
			b.WriteString("提示：内核监听的是 unix socket，别的电脑连不上；要接别的电脑，把 [kernel] listen 改成 tcp:0.0.0.0:7700\n")
		}
	}
	return b.String()
}
