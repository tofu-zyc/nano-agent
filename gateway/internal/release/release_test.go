package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func targz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(data))
	}
	zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFetchVerifiesAndCaches(t *testing.T) {
	pkg := targz(t, map[string]string{"nano-kernel-1.2.3-linux-x86_64/nano-kernel": "K", "nano-kernel-1.2.3-linux-x86_64/rg": "R"})
	win := zipped(t, map[string]string{"nano-kernel-1.2.3-windows-x86_64/nano-kernel.exe": "KW"})
	files := map[string][]byte{
		"/download/v1.2.3/nano-kernel-1.2.3-linux-x86_64.tar.gz": pkg,
		"/download/v1.2.3/nano-kernel-1.2.3-windows-x86_64.zip":  win,
		"/latest/download/VERSION":                               []byte("1.2.3\n"),
	}
	files["/download/v1.2.3/SHA256SUMS"] = []byte(sum(pkg) + "  nano-kernel-1.2.3-linux-x86_64.tar.gz\n" +
		sum(win) + "  nano-kernel-1.2.3-windows-x86_64.zip\n")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			hits++
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.Cache = t.TempDir()
	ctx := context.Background()

	if v, err := c.Latest(ctx); err != nil || v != "1.2.3" {
		t.Fatalf("Latest: %q %v", v, err)
	}
	p, err := c.Fetch(ctx, "1.2.3", KernelAsset("1.2.3", "linux", "x86_64"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unpack(p)
	if err != nil || len(got) != 2 {
		t.Fatalf("Unpack: %+v %v", got, err)
	}
	if _, err := c.Fetch(ctx, "1.2.3", KernelAsset("1.2.3", "linux", "amd64")); err != nil || hits != 1 {
		t.Fatalf("应该用缓存：hits=%d err=%v", hits, err)
	}
	p, err = c.Fetch(ctx, "1.2.3", KernelAsset("1.2.3", "windows", "x86_64"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Unpack(p); err != nil || len(got) != 1 || got[0].Name != "nano-kernel.exe" || string(got[0].Data) != "KW" {
		t.Fatalf("zip: %+v %v", got, err)
	}

	// 内容被改了：校验失败，不留文件
	files["/download/v1.2.3/nano-kernel-1.2.3-linux-x86_64.tar.gz"] = append(pkg, 'x')
	c.Cache = t.TempDir()
	if _, err := c.Fetch(ctx, "1.2.3", KernelAsset("1.2.3", "linux", "x86_64")); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("应该校验失败：%v", err)
	}
	if _, err := c.Fetch(ctx, "1.2.3", KernelAsset("1.2.3", "macos", "arm64")); err == nil {
		t.Fatal("没有这个包应该报错")
	}
	if _, err := c.Fetch(ctx, "dev", "x.tar.gz"); err == nil {
		t.Fatal("dev 不是发布版本")
	}
}

func TestVersions(t *testing.T) {
	for _, v := range []string{"0.1.0", "v1.2.3", "10.0.12"} {
		if !IsRelease(v) {
			t.Errorf("%s 应该是发布版本", v)
		}
	}
	for _, v := range []string{"dev", "test", "0.1", "0.1.0-3-gabc", "4f6d325-dirty", ""} {
		if IsRelease(v) {
			t.Errorf("%s 不是发布版本", v)
		}
	}
	if Compare("0.1.9", "0.1.10") != -1 || Compare("1.0.0", "0.9.9") != 1 || Compare("0.1.0", "v0.1.0") != 0 || Compare("dev", "0.0.1") != -1 {
		t.Fatal("Compare")
	}
}
