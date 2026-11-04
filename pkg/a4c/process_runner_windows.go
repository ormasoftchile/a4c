//go:build windows

package a4c

import (
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Process grouping on Windows handled by process tree termination
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	_ = cmd.Process.Kill()
}
