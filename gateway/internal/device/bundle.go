// Package device 管理电脑启动包和由网关建立的 SSH 内核连接。
package device

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed assets/*
var assets embed.FS

type Bundle struct{ Dir string }

// FindBundle 同时支持源码目录和解压后的安装目录。
func FindBundle(configDir string) Bundle {
	exe, _ := os.Executable()
	for _, d := range []string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "../kernel"), filepath.Join(configDir, "kernel")} {
		if st, err := os.Stat(filepath.Join(d, "nano-kernel")); err == nil && !st.IsDir() {
			return Bundle{Dir: d}
		}
	}
	return Bundle{}
}

type bundleFile struct {
	name string
	data []byte
	mode int64
}

func (b Bundle) files(conf string) ([]bundleFile, error) {
	source, err := assets.ReadFile("assets/kernel-src.tar.gz")
	if err != nil {
		return nil, err
	}
	skills, err := assets.ReadFile("assets/skills.tar.gz")
	if err != nil {
		return nil, err
	}
	files := []bundleFile{{"kernel-src.tar.gz", source, 0600}, {"skills.tar.gz", skills, 0600}, {"kernel.conf", []byte(conf), 0600}}
	for _, name := range []string{"nano-kernel", "nano-exec", "nano-exec-static", "nano-exec-src.tar.gz"} {
		if b.Dir == "" {
			break
		}
		data, err := os.ReadFile(filepath.Join(b.Dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, bundleFile{"bin/" + name, data, 0700})
	}
	for _, name := range []string{"start.sh", "start.ps1", "start.cmd", "ensure-engine.sh"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return nil, err
		}
		if name == "start.ps1" {
			data = append([]byte{0xef, 0xbb, 0xbf}, data...)
		} // Windows PowerShell 5 的 UTF-8 识别
		files = append(files, bundleFile{name, data, 0700})
	}
	return files, nil
}

func writeTar(files []bundleFile) ([]byte, error) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Package 的口令来自登录后下载的网关配置，启动时仍可修改地址、口令和节点名。
func (b Bundle) Package(platform, gateway, token string) ([]byte, string, error) {
	conf := fmt.Sprintf("gateway = %s\ntoken = %s\n", gateway, token)
	files, err := b.files(conf)
	if err != nil {
		return nil, "", err
	}
	if platform == "linux" {
		data, err := writeTar(files)
		return data, "nano-agent-linux.tar.gz", err
	}
	if platform != "windows" {
		return nil, "", fmt.Errorf("支持 windows 或 linux")
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, "", err
		}
		if _, err = w.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "nano-agent-windows.zip", nil
}

func (b Bundle) remoteArchive(name, token string) ([]byte, error) {
	files, err := b.files(fmt.Sprintf("gateway = stdio\nnode = %s\ntoken = %s\nexec = bin/nano-exec\n", name, token))
	if err != nil {
		return nil, err
	}
	return writeTar(files)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// 只在 SSH 中执行；协议连接另开一个 SSH session，避免安装包与协议争用 stdin。
func bootstrap(id string) string {
	return `umask 077; d="$HOME/.local/share/nano-agent/managed/` + id + `"; mkdir -p "$d" && tar xzf - -C "$d" && sh "$d/ensure-engine.sh" "$d"`
}

func remoteCommand(id string) string {
	return `exec "$HOME/.local/share/nano-agent/managed/` + id + `/bin/nano-kernel" -c "$HOME/.local/share/nano-agent/managed/` + id + `/kernel.conf"`
}
