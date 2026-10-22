//go:build windows

package main

import (
	"errors"
	"os/exec"
)

func detach(cmd *exec.Cmd) {}

func reexec(args []string) error {
	return errors.New("Windows 上暂不支持网关自重启")
}
