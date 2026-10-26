//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func processIsGateway(pid int) bool { return processNamed(pid, "nano-gateway") }
func processIsKernel(pid int) bool  { return processNamed(pid, "nano-kernel") }
func processNamed(pid int, name string) bool {
	if b, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.Contains(filepath.Base(b), name)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err == nil {
		return strings.Contains(filepath.Base(strings.TrimSpace(string(out))), name)
	}
	return true
}

func terminateProcess(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
func killProcess(pid int) error      { return syscall.Kill(pid, syscall.SIGKILL) }
func terminateCmd(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}
func notifyStop(c chan os.Signal) { signal.Notify(c, syscall.SIGTERM, syscall.SIGINT) }
