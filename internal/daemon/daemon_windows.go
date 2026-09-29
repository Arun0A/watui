//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess = kernel32.NewProc("OpenProcess")
	procGetExitCode = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle = kernel32.NewProc("CloseHandle")
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	detachedProcess                = 0x00000008
)

func isProcessAlive(pid int) bool {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)

	var exitCode uint32
	ret, _, _ := procGetExitCode.Call(h, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 {
		return false
	}
	return exitCode == stillActive
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
}

// DetachProcess configures the command to run as a detached process on Windows.
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | 0x08000000,
		HideWindow:    true,
	}
}
