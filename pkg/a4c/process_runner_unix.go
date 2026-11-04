//go:build !windows

package a4c

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	// Negative PID signals the entire process group
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
