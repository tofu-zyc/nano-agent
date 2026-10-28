package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LegacyFiles includes named NTFS data streams, which ReadDir alone cannot see.
// Returned names are rooted in dir; data is copied later with a fixed buffer.
func LegacyFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
		streams, err := alternateNames(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			return names, err
		}
		names = append(names, streams...)
		if len(names) > 4096 {
			return names[:4096], fmt.Errorf("旧布局盘点超过 4096 项，请分批迁移并保留原文件")
		}
	}
	sort.Strings(names)
	return names, nil
}
