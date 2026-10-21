package channel

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestEncodeAddons(t *testing.T) {
	s, err := encodeAddons(regAddons)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	var v map[string]any
	if json.Unmarshal(b, &v) != nil || !strings.Contains(string(b), "im.message.receive_v1") || !strings.Contains(string(b), `"tenant"`) {
		t.Fatalf("addons: %s", b)
	}
}

func TestQRCodeSVG(t *testing.T) {
	svg, err := QRCodeSVG("https://open.feishu.cn/page/app-registration?code=abc")
	if err != nil || !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "M4 4h1v1h-1z") {
		t.Fatalf("svg: %v %.120s", err, svg)
	}
}
