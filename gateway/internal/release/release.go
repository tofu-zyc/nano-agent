// Package release 从 GitHub Release（或镜像）查最新版本、下载发布包并按 SHA256SUMS 校验。
// 网关自己更新、给内核推新版本都用它。下载的包缓存在 $NANO_HOME/cache/releases/<版本>/。
package release

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/httpx"
	"nano-agent/gateway/internal/paths"
)

// DefaultBase 是发布包的下载地址；NANO_DOWNLOAD_URL 或配置里的 [update] url 可以换成镜像。
const DefaultBase = "https://github.com/tofu-zyc/nano-agent/releases"

// Client 下载和校验发布包。
type Client struct {
	Base  string // 不带结尾的 /
	Cache string // 缓存目录
	HTTP  *http.Client
	Via   string // 走的什么代理，下载失败时写进错误里
}

// New 用 base（空表示 NANO_DOWNLOAD_URL 或默认地址）建一个客户端。proxy 是网关的默认代理
// （空：跟随环境变量；none：直连；或者代理地址）。
func New(base, proxy string) *Client {
	if base == "" {
		base = os.Getenv("NANO_DOWNLOAD_URL")
	}
	if base == "" {
		base = DefaultBase
	}
	hc, err := httpx.NewClient(proxy, 10*time.Minute)
	if err != nil { // 配置检查过，不会走到这里；万一走到了就跟随环境变量
		hc, _ = httpx.NewClient("", 10*time.Minute)
		proxy = ""
	}
	return &Client{
		Base:  strings.TrimRight(base, "/"),
		Cache: filepath.Join(paths.Home(), "cache", "releases"),
		HTTP:  hc,
		Via:   httpx.Describe(proxy),
	}
}

// IsRelease 判断是不是发布版本号（x.y.z）；源码编译的 dev、git describe 之类都不是。
func IsRelease(v string) bool {
	_, ok := parse(v)
	return ok
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Compare 比较两个发布版本：a < b 返回 -1，相等 0，a > b 返回 1。不是发布版本的当成最旧。
func Compare(a, b string) int {
	x, okA := parse(a)
	y, okB := parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Arch 把 CPU 架构名统一成发布包里用的（x86_64 / aarch64）。
func Arch(a string) string {
	switch a {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "aarch64"
	}
	return a
}

// KernelAsset 是某个系统和架构的内核包名（内核在 hello 里报的 os / arch）。
func KernelAsset(version, osName, arch string) string {
	ext := ".tar.gz"
	if osName == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("nano-kernel-%s-%s-%s%s", version, osName, Arch(arch), ext)
}

// GatewayAsset 是这台机器上网关的包名。
func GatewayAsset(version string) string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	return fmt.Sprintf("nano-gateway-%s-%s-%s.tar.gz", version, osName, Arch(runtime.GOARCH))
}

func (c *Client) get(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if c.Via != "" {
			return fmt.Errorf("下载 %s 失败（%s）：%w", url, c.Via, err)
		}
		return fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败：%s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Latest 查最新的发布版本号。
func (c *Client) Latest(ctx context.Context) (string, error) {
	var b strings.Builder
	if err := c.get(ctx, c.Base+"/latest/download/VERSION", &limitWriter{w: &b, n: 64}); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(b.String()), "v")
	if !IsRelease(v) {
		return "", fmt.Errorf("最新版本号看不懂：%q", v)
	}
	return v, nil
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("内容太长")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// sums 下载并解析某个版本的 SHA256SUMS。
func (c *Client) sums(ctx context.Context, version string) (map[string]string, error) {
	var b strings.Builder
	if err := c.get(ctx, c.url(version, "SHA256SUMS"), &limitWriter{w: &b, n: 64 << 10}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out, nil
}

func (c *Client) url(version, asset string) string {
	return c.Base + "/download/v" + strings.TrimPrefix(version, "v") + "/" + asset
}

// Fetch 下载某个版本的一个包（已经缓存且校验通过就直接用），返回本地路径。
func (c *Client) Fetch(ctx context.Context, version, asset string) (string, error) {
	if !IsRelease(version) {
		return "", fmt.Errorf("%q 不是发布版本，没有发布包", version)
	}
	if asset != path.Base(asset) || strings.HasPrefix(asset, ".") {
		return "", fmt.Errorf("包名不对：%q", asset)
	}
	sums, err := c.sums(ctx, version)
	if err != nil {
		return "", err
	}
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("版本 %s 没有 %s（这个系统或架构可能还没有发布包）", version, asset)
	}
	dir := filepath.Join(c.Cache, version)
	dst := filepath.Join(dir, asset)
	if got, err := fileSHA256(dst); err == nil && got == want {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, asset+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	err = c.get(ctx, c.url(version, asset), io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("%s 校验失败（SHA256 不对），可能下载坏了或者被篡改", asset)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// File 是包里的一个文件，Name 是去掉顶层目录后的路径（用 /）。
type File struct {
	Name string
	Mode os.FileMode
	Data []byte
}

// Unpack 读出包（.tar.gz 或 .zip）里的全部普通文件，去掉顶层目录。单个包最多 200 MB。
func Unpack(archive string) ([]File, error) {
	const limit = 200 << 20
	var out []File
	total := 0
	add := func(name string, mode os.FileMode, r io.Reader) error {
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		} else {
			return nil // 顶层只有目录
		}
		if name == "" || strings.HasPrefix(name, "../") {
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		if err != nil {
			return err
		}
		total += len(b)
		if total > limit {
			return errors.New("包太大")
		}
		out = append(out, File{Name: name, Mode: mode, Data: b})
		return nil
	}
	if strings.HasSuffix(archive, ".zip") {
		z, err := zip.OpenReader(archive)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		for _, f := range z.File {
			if f.FileInfo().IsDir() {
				continue
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = add(f.Name, f.Mode(), r)
			r.Close()
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := add(hdr.Name, os.FileMode(hdr.Mode).Perm(), tr); err != nil {
			return nil, err
		}
	}
}
