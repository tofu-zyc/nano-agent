package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// copyBufferSize 是流式复制 / 校验的固定缓冲区。复制和恢复都不能按文件大小分配内存：
// 历史文件可能很大，整份 ReadFile 在内存紧张时会让恢复自己失败。
const copyBufferSize = 64 << 10

func hashReader(r io.Reader) (int64, string, error) {
	h := sha256.New()
	buf := make([]byte, copyBufferSize)
	n, err := io.CopyBuffer(h, r, buf)
	if err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile 流式计算整个文件的内容版本。
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	return hashReader(f)
}

// hashPrefix 流式计算文件前 n 字节的摘要（历史允许在发布内容之后继续追加）。
func hashPrefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(h, io.LimitReader(f, n), buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileVersion 把 src 流式复制到 dst，同时得到内容版本。目标先写临时文件再改名，
// 失败不会留下半份正文；成功返回时 dst 已经是完整内容。
func copyFileVersion(src, dst string) (renameFile, error) {
	in, err := os.Open(src)
	if err != nil {
		return renameFile{}, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return renameFile{}, err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return renameFile{}, err
	}
	h := sha256.New()
	buf := make([]byte, copyBufferSize)
	n, copyErr := io.CopyBuffer(io.MultiWriter(out, h), in, buf)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return renameFile{}, copyErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return renameFile{}, err
	}
	return renameFile{Len: n, Sum: hex.EncodeToString(h.Sum(nil))}, nil
}

// versionMatches 检查文件当前内容是否正好等于记录里的版本。
func versionMatches(path string, meta renameFile) (bool, error) {
	n, sum, err := hashFile(path)
	if err != nil {
		return false, err
	}
	return n == meta.Len && sum == meta.Sum, nil
}

// versionIsPrefix 检查文件是否以记录里的内容开头（长度相同也算），
// 用于恢复时确认目标确实承接了我们发布过的那一段，而后面可能已经追加了新消息。
func versionIsPrefix(path string, meta renameFile) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if st.Size() < meta.Len {
		return false, nil
	}
	if st.Size() == meta.Len {
		return versionMatches(path, meta)
	}
	sum, err := hashPrefix(path, meta.Len)
	if err != nil {
		return false, err
	}
	return sum == meta.Sum, nil
}

// sameFileContent 流式比较两个文件是否完全一致（升级旧会话时判重）。
func sameFileContent(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	na, ha, err := hashFile(a)
	if err != nil {
		return false, err
	}
	nb, hb, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return na == nb && ha == hb, nil
}

// verifyFileVersion 校验一个文件仍与记录版本一致，并给出可读的错误。
func verifyFileVersion(path string, meta renameFile, what string) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s不见了，保留现场等待人工核对", what)
		}
		return err
	}
	if st.Size() != meta.Len {
		return fmt.Errorf("%s与记录不一致，保留现场等待人工核对", what)
	}
	ok, err := versionMatches(path, meta)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s与记录不一致，保留现场等待人工核对", what)
	}
	return nil
}
