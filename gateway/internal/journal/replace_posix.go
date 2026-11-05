//go:build !windows

package journal

import (
	"os"
	"path/filepath"
)

// syncParent 同步文件所在目录和它的上一级，新建的文件和目录项才算落盘。
func syncParent(path string) error {
	dir := filepath.Dir(path)
	for _, name := range []string{dir, filepath.Dir(dir)} {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func replace(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncParent(to)
}
