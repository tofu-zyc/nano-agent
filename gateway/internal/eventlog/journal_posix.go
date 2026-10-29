//go:build !windows

package eventlog

import (
	"os"
	"path/filepath"
)

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
func replaceJournal(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncParent(to)
}
