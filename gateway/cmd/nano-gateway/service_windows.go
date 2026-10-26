//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func detach(cmd *exec.Cmd) {}

func reexec(args []string) error {
	return errors.New("Windows 上暂不支持网关自重启")
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

func processIsGateway(pid int) bool { return processNamed(pid, "nano-gateway") }
func processIsKernel(pid int) bool  { return processNamed(pid, "nano-kernel") }
func processNamed(pid int, name string) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return true
	}
	return strings.Contains(strings.ToLower(string(out)), name)
}

func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
func killProcess(pid int) error { return terminateProcess(pid) }
func terminateCmd(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
func notifyStop(c chan os.Signal) { signal.Notify(c, os.Interrupt) }
