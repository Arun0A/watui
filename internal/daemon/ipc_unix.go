//go:build !windows

package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// SocketPath returns the Unix domain socket path next to the database.
func SocketPath(dbPath string) string {
	if dbPath == "" {
		dbPath = "watui.db"
	}
	dir := filepath.Dir(dbPath)
	return filepath.Join(dir, "watui.sock")
}

// ListenIPC creates an IPC listener for the daemon.
func ListenIPC(dbPath string) (net.Listener, error) {
	sock := SocketPath(dbPath)
	_ = os.Remove(sock) // Remove any stale socket file
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return net.Listen("unix", sock)
}

// DialIPC connects to a running daemon's IPC socket.
func DialIPC(dbPath string) (net.Conn, error) {
	sock := SocketPath(dbPath)
	return net.Dial("unix", sock)
}

// CleanupIPC removes any leftover socket file.
func CleanupIPC(dbPath string) {
	sock := SocketPath(dbPath)
	_ = os.Remove(sock)
}
