package media

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// 1x1 的 PNG
var pngData = func() []byte {
	b, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return b
}()

func TestSaveDedupeAndRead(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Save(pngData, "a.png", "web")
	if err != nil {
		t.Fatal(err)
	}
	if a.MIME != "image/png" || a.Size != int64(len(pngData)) || a.Source != "web" || a.Name != "a.png" {
		t.Fatalf("附件：%+v", a)
	}
	if len(a.ID) != 64 {
		t.Fatalf("ID：%q", a.ID)
	}
	// 同样的内容只存一份，ID 一样
	b, err := st.Save(pngData, "b.png", "telegram")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID {
		t.Fatalf("没有按内容去重：%s / %s", a.ID, b.ID)
	}
	got, err := st.Read(a.ID)
	if err != nil || !bytes.Equal(got, pngData) {
		t.Fatalf("读回来：%v %d", err, len(got))
	}
	info, err := st.Info(a.ID)
	if err != nil || info.MIME != "image/png" || info.Size != int64(len(pngData)) {
		t.Fatalf("Info：%+v %v", info, err)
	}
	url, err := st.DataURL(a)
	if err != nil || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("DataURL：%q %v", url, err)
	}
}

func TestRejects(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save([]byte("not an image"), "x.txt", "web"); err == nil {
		t.Fatal("不是图片应该报错")
	}
	if _, err := st.Save(nil, "", "web"); err == nil {
		t.Fatal("空内容应该报错")
	}
	big := append(append([]byte{}, pngData...), make([]byte, MaxImageBytes)...)
	if _, err := st.Save(big, "big.png", "web"); err == nil {
		t.Fatal("太大的图片应该报错")
	}
	if _, err := st.Read("../../etc/passwd"); err == nil {
		t.Fatal("乱写的 ID 应该报错")
	}
	if _, err := st.Read(strings.Repeat("z", 64)); err == nil {
		t.Fatal("不存在的图片应该报错")
	}
}
