package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PIDFilePath returns the absolute or resolved path to watui.pid next to the database.
func PIDFilePath(dbPath string) string {
	if dbPath == "" {
		dbPath = "watui.db"
	}
	dir := filepath.Dir(dbPath)
	return filepath.Join(dir, "watui.pid")
}

// LogFilePath returns the path for the daemon background log file.
func LogFilePath(dbPath string) string {
	if dbPath == "" {
		dbPath = "watui.db"
	}
	dir := filepath.Dir(dbPath)
	return filepath.Join(dir, "watui-daemon.log")
}

// ReadPID reads and parses the process ID from a pid file.
// Returns 0 if the file does not exist or is invalid.
func ReadPID(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid PID in %s: %w", pidFile, err)
	}
	return pid, nil
}

// WritePID writes the current or specified process ID into the pid file.
func WritePID(pidFile string, pid int) error {
	dir := filepath.Dir(pidFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory for PID file: %w", err)
	}
	return os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0644)
}

// RemovePID deletes the pid file.
func RemovePID(pidFile string) {
	_ = os.Remove(pidFile)
}

// IsRunning checks whether the daemon recorded in pidFile is currently running.
// If the process is dead, the stale pid file is automatically cleaned up.
func IsRunning(pidFile string) (bool, int) {
	pid, err := ReadPID(pidFile)
	if err != nil || pid <= 0 {
		return false, 0
	}

	if isProcessAlive(pid) {
		return true, pid
	}

	// Clean up stale PID file
	RemovePID(pidFile)
	return false, 0
}

// Stop stops any running daemon recorded in pidFile.
// Returns nil if stopped successfully or if no daemon was running.
func Stop(pidFile string) (bool, int, error) {
	running, pid := IsRunning(pidFile)
	if !running {
		return false, 0, nil
	}

	if err := killProcess(pid); err != nil {
		return true, pid, fmt.Errorf("failed to stop daemon (PID %d): %w", pid, err)
	}

	// Poll up to 3 seconds for the process to exit
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessAlive(pid) {
			break
		}
	}

	RemovePID(pidFile)
	return true, pid, nil
}
