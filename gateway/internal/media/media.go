// Package media 存用户发来的图片：按内容哈希放到数据目录（data/uploads），
// 会话历史里只存引用（Attachment），发给模型时再读出来做成 data URL。
//
// 只支持图片（PNG、JPEG、GIF、WebP）；音视频以后再说。单张大小和每条消息的张数有上限。
package media

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

const (
	MaxImageBytes = 10 << 20 // 单张图片最多 10 MB
	MaxPerMessage = 4        // 一条消息最多几张图片
)

// Attachment 是一张图片的引用，存在会话历史里。ID 是内容哈希，也是文件名。
type Attachment struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`   // 原始文件名，只用来显示
	MIME   string `json:"mime"`             // 按内容嗅探出来的类型
	Size   int64  `json:"size"`             //
	Source string `json:"source,omitempty"` // web / telegram / feishu
}

var idRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsImage 判断是不是支持的图片类型。
func IsImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Store 是图片目录。并发安全（只按内容哈希写新文件，不修改已有的）。
type Store struct {
	dir string
}

// Open 打开（必要时创建）图片目录，权限只给自己。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir 是图片目录。
func (s *Store) Dir() string { return s.dir }

// Save 存一张图片（按内容去重），返回引用。空内容、太大、不是支持的图片都会报错。
func (s *Store) Save(data []byte, name, source string) (Attachment, error) {
	if len(data) == 0 {
		return Attachment{}, errors.New("图片是空的")
	}
	if len(data) > MaxImageBytes {
		return Attachment{}, fmt.Errorf("图片太大（%d 字节，最多 %d MB）", len(data), MaxImageBytes>>20)
	}
	mime := http.DetectContentType(data)
	if !IsImage(mime) {
		return Attachment{}, fmt.Errorf("不是支持的图片（%s），只支持 PNG、JPEG、GIF、WebP", mime)
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, id)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return Attachment{}, err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return Attachment{}, err
		}
	}
	return Attachment{ID: id, Name: name, MIME: mime, Size: int64(len(data)), Source: source}, nil
}

// Read 读一个附件。ID 不对或者文件没了会报错。
func (s *Store) Read(id string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, errors.New("附件 ID 不对")
	}
	return os.ReadFile(filepath.Join(s.dir, id))
}

// Info 按 ID 读回附件的类型和大小（网页发消息时客户端只传 ID，类型以文件内容为准）。
func (s *Store) Info(id string) (Attachment, error) {
	b, err := s.Read(id)
	if err != nil {
		return Attachment{}, err
	}
	mime := http.DetectContentType(b)
	if !IsImage(mime) {
		return Attachment{}, errors.New("不是图片")
	}
	return Attachment{ID: id, MIME: mime, Size: int64(len(b))}, nil
}

// DataURL 是发给模型用的 data: URL。
func (s *Store) DataURL(a Attachment) (string, error) {
	b, err := s.Read(a.ID)
	if err != nil {
		return "", err
	}
	mime := a.MIME
	if mime == "" {
		mime = http.DetectContentType(b)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}
