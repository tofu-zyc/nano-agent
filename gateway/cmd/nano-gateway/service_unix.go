//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func reexec(args []string) error {
	self, err := currentExe()
	if err != nil {
		return err
	}
	return syscall.Exec(self, args, os.Environ())
}
