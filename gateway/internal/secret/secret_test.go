package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	home := t.TempDir()
	if err := Init(home); err != nil {
		t.Fatal(err)
	}
	enc, err := Encrypt("sk-1234567890abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(enc) || strings.Contains(enc, "1234567890") {
		t.Fatalf("没有加密：%q", enc)
	}
	got, err := Decrypt(enc)
	if err != nil || got != "sk-1234567890abcdef" {
		t.Fatalf("%q %v", got, err)
	}
	if again, err := Encrypt("sk-1234567890abcdef"); err != nil || again == enc {
		t.Fatal("每次应该用新的 nonce")
	}
}

func TestInitCreates0600AndRejectsBadFile(t *testing.T) {
	home := t.TempDir()
	if err := Init(home); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(home, "secret.key"))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("%v %v", st, err)
	}
	os.WriteFile(filepath.Join(home, "secret.key"), []byte("short"), 0o600)
	if err := Init(home); err == nil {
		t.Fatal("长度不对应该报错")
	}
}

func TestDecryptWithoutKey(t *testing.T) {
	mu.Lock()
	old := box
	box = nil
	mu.Unlock()
	defer func() { mu.Lock(); box = old; mu.Unlock() }()
	if _, err := Decrypt("enc:v1:AAAA"); err == nil {
		t.Fatal("没有密钥应该报错")
	}
}
