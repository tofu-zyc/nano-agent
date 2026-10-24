package app

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex 是短的随机后缀：中文名字取不出拉丁字母时，agent 的 ID 用它。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "noid"
	}
	return hex.EncodeToString(b)
}
