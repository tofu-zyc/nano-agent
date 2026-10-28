//go:build !windows

package session

func alternateNames(path, base string) ([]string, error) { return nil, nil }
