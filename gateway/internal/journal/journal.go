// Package journal 是只追加的可靠日志：先落盘再生效的状态都写在这里。
//
// 一行一条记录，带连续的序号和数据的 SHA-256。重放只截掉没有换行结尾的最后半条；
// 完整记录损坏、版本不支持、序号不连续时拒绝打开，文件保持原样。
// 一次写入或同步失败后日志冻结，之后所有追加都失败，重启时重放已经提交的部分。
// 不加锁：由使用方的一个写协程独占。
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrRecordTooLarge = errors.New("日志记录超过单条上限")
	ErrFileFull       = errors.New("日志文件超过上限")
	ErrFrozen         = errors.New("日志写入失败后已冻结")
)

// Limits 是单条记录（data 的字节数）和整个文件的字节上限。
type Limits struct{ Record, File int64 }

type record struct {
	Version int             `json:"version"`
	Seq     uint64          `json:"seq"`
	Data    json.RawMessage `json:"data"`
	Hash    string          `json:"sha256"`
}

// lineSlack 是一行里 data 之外的字节上限（版本、序号、校验和键名）。
const lineSlack = 4096

type Journal struct {
	path    string
	version int
	limits  Limits
	file    *os.File
	seq     uint64
	size    int64
	frozen  error
}

// replaceFile 把 from 原子替换到 to；测试里可以换成会失败的实现。
var replaceFile = replace

// Open 打开（不存在就创建）日志并按序重放每条记录。replay 返回错误时拒绝打开。
func Open(path string, version int, limits Limits, replay func(seq uint64, data json.RawMessage) error) (*Journal, error) {
	if version <= 0 || limits.Record <= 0 || limits.File <= 0 {
		return nil, errors.New("日志的版本和上限要大于 0")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// 中断的改写留下的 .next 从来不是权威，直接清掉。
	if err := os.Remove(path + ".next"); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	j := &Journal{path: path, version: version, limits: limits, file: file}
	if err := j.replay(replay); err != nil {
		file.Close()
		return nil, err
	}
	if created {
		if err := syncParent(path); err != nil {
			file.Close()
			return nil, err
		}
	}
	return j, nil
}

func (j *Journal) replay(fn func(uint64, json.RawMessage) error) error {
	info, err := j.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > j.limits.File {
		return fmt.Errorf("%w: %d 字节", ErrFileFull, info.Size())
	}
	reader := bufio.NewReaderSize(j.file, int(j.limits.Record)+lineSlack)
	var offset int64
	var torn bool
	for {
		line, readErr := reader.ReadSlice('\n')
		if readErr != nil {
			if readErr == io.EOF {
				torn = len(line) > 0
				break
			}
			return fmt.Errorf("偏移 %d 的记录超过上限或读取失败: %w", offset, readErr)
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("偏移 %d 的记录损坏: %w", offset, err)
		}
		sum := sha256.Sum256(rec.Data)
		switch {
		case rec.Version != j.version:
			return fmt.Errorf("偏移 %d 的日志版本 %d 不受支持", offset, rec.Version)
		case rec.Seq != j.seq+1:
			return fmt.Errorf("偏移 %d 的序号 %d 不连续，应为 %d", offset, rec.Seq, j.seq+1)
		case rec.Hash != hex.EncodeToString(sum[:]):
			return fmt.Errorf("偏移 %d 的记录校验失败", offset)
		case int64(len(rec.Data)) > j.limits.Record:
			return fmt.Errorf("偏移 %d 的记录: %w", offset, ErrRecordTooLarge)
		}
		if fn != nil {
			if err := fn(rec.Seq, rec.Data); err != nil {
				return fmt.Errorf("重放序号 %d: %w", rec.Seq, err)
			}
		}
		j.seq = rec.Seq
		offset += int64(len(line))
	}
	// 只有没换行结尾的最后半条可以丢。放在全部重放成功之后，拒绝打开时文件不变。
	if torn {
		if err := j.file.Truncate(offset); err != nil {
			return err
		}
		if err := j.file.Sync(); err != nil {
			return err
		}
	}
	j.size = offset
	_, err = j.file.Seek(offset, io.SeekStart)
	return err
}

func (j *Journal) encode(seq uint64, data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > j.limits.Record {
		return nil, fmt.Errorf("%w: %d 字节", ErrRecordTooLarge, len(raw))
	}
	sum := sha256.Sum256(raw)
	line, err := json.Marshal(record{j.version, seq, raw, hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// Append 写入一条记录并 fsync，返回时这条已经提交。超过上限只返回错误；
// 写入或同步失败会冻结日志。
func (j *Journal) Append(data any) error {
	if j.frozen != nil {
		return fmt.Errorf("%w: %v", ErrFrozen, j.frozen)
	}
	line, err := j.encode(j.seq+1, data)
	if err != nil {
		return err
	}
	if j.size+int64(len(line)) > j.limits.File {
		return fmt.Errorf("%w: 已有 %d 字节", ErrFileFull, j.size)
	}
	if _, err := j.file.Write(line); err != nil {
		j.frozen = err
		return err
	}
	if err := j.file.Sync(); err != nil {
		j.frozen = err
		return err
	}
	j.seq++
	j.size += int64(len(line))
	return nil
}

// Rewrite 用 records 替换整个日志（序号从 1 重排）。先写 <path>.next 并 fsync，再原子替换；
// 替换之前出错，原文件不变、日志仍可用；替换开始之后出错则冻结，重启读到旧文件或新文件，不会读到半个。
func (j *Journal) Rewrite(records []any) error {
	if j.frozen != nil {
		return fmt.Errorf("%w: %v", ErrFrozen, j.frozen)
	}
	next := j.path + ".next"
	lines := make([][]byte, len(records))
	var total int64
	for i, r := range records {
		line, err := j.encode(uint64(i+1), r)
		if err != nil {
			return err
		}
		if total += int64(len(line)); total > j.limits.File {
			return fmt.Errorf("%w: 改写后 %d 字节", ErrFileFull, total)
		}
		lines[i] = line
	}
	if err := writeSynced(next, bytes.Join(lines, nil)); err != nil {
		os.Remove(next)
		return err
	}
	// Windows 替换前要先关掉旧句柄。从这里开始任何失败都冻结。
	if err := j.file.Close(); err != nil {
		j.frozen = err
		return err
	}
	if err := replaceFile(next, j.path); err != nil {
		j.frozen = err
		return err
	}
	file, err := os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err == nil {
		_, err = file.Seek(total, io.SeekStart)
	}
	if err != nil {
		j.frozen = err
		return err
	}
	j.file, j.seq, j.size = file, uint64(len(records)), total
	return nil
}

func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Size 是已经提交的字节数。
func (j *Journal) Size() int64 { return j.size }

// Close 关闭文件。冻结之后关闭也不改文件内容。
func (j *Journal) Close() error {
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
