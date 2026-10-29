package eventlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The first rule-bearing transaction is a format upgrade. Preserve the last
// v1 journal with bounded streaming I/O; a partial backup is never authoritative.
func (s *Store) backupV1() error {
	destination := s.path + ".v1.bak"
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".events-v1-backup-")
	if err != nil {
		return err
	}
	pending := file.Name()
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(pending)
		}
	}()
	if _, err = io.CopyBuffer(file, io.NewSectionReader(s.file, 0, s.bytes), make([]byte, 32<<10)); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("保留 v1 可靠事件备份失败: %w", err)
	}
	if err = os.Rename(pending, destination); err != nil {
		return err
	}
	if err = syncParent(destination); err != nil {
		return err
	}
	complete = true
	return nil
}
