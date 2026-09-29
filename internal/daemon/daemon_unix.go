//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, sending signal 0 checks for process existence without actually sending a signal.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}

// DetachProcess configures the command so that it runs in a new process group,
// decoupled from the parent's terminal and session.
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
