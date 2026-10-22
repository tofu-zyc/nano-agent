// Package secret 加密 nano-agent 自己保存的密钥（模型 API key、SSH 密码、机器人 token、网页密码）。
//
// 密钥在 $NANO_HOME/secret.key（32 字节，0600）。存盘格式：enc:v1:<base64(nonce|密文)>，
// 用 AES-256-GCM。能防的是明文出现在文件里（AI 读文件、截图、备份、误传），
// 防不了有这个账号权限的人——密钥文件就在同一块盘上。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const prefix = "enc:v1:"

var (
	mu  sync.RWMutex
	box *Box
)

// Box 是一个已经加载的密钥。
type Box struct{ key []byte }

// Init 读取或生成 home/secret.key。密钥文件丢失或长度不对时报错，不会悄悄换成新密钥。
func Init(home string) error {
	path := filepath.Join(home, "secret.key")
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != 32 {
			return fmt.Errorf("%s 应该是 32 字节，现在是 %d 字节；不要删它，否则已加密的密钥都解不开", path, len(key))
		}
	case os.IsNotExist(err):
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return err
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return err
		}
	default:
		return fmt.Errorf("读取 %s：%w", path, err)
	}
	mu.Lock()
	box = &Box{key: key}
	mu.Unlock()
	return nil
}

// Reset 清掉已加载的密钥，测试用。
func Reset() {
	mu.Lock()
	box = nil
	mu.Unlock()
}

// Ready 表示已经加载了密钥；测试里没有加载时按明文处理。
func Ready() bool {
	mu.RLock()
	defer mu.RUnlock()
	return box != nil
}

// IsEncrypted 判断一个值是不是 enc:v1: 格式。
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

// Encrypt 把明文加密成 enc:v1:...。空字符串原样返回。
func Encrypt(plain string) (string, error) {
	if plain == "" || IsEncrypted(plain) {
		return plain, nil
	}
	mu.RLock()
	b := box
	mu.RUnlock()
	if b == nil {
		return plain, nil // 测试/嵌入式没有 secret.key：保持明文
	}
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt 解密 enc:v1:...；不是这个格式就原样返回。
func Decrypt(value string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	mu.RLock()
	b := box
	mu.RUnlock()
	if b == nil {
		return "", errors.New("这个值已加密，但没有加载 secret.key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("密钥格式坏了：%w", err)
	}
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密钥格式坏了：内容太短")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("解不开这个密钥：secret.key 不对或者文件损坏")
	}
	return string(plain), nil
}
