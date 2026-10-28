//go:build !windows

package dispatch

import (
	"os"
	"path/filepath"
)

func replaceCheckpoint(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	// Persist both the checkpoint directory entry and a newly created runtime
	// directory before authorizing a process to depend on this ownership record.
	dir := filepath.Dir(to)
	for _, path := range []string{dir, filepath.Dir(dir)} {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
