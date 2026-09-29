//go:build windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func portFilePath(dbPath string) string {
	if dbPath == "" {
		dbPath = "watui.db"
	}
	dir := filepath.Dir(dbPath)
	return filepath.Join(dir, "watui.ipc")
}

// ListenIPC creates a local loopback TCP listener on Windows and writes its port to a file.
func ListenIPC(dbPath string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	portFile := portFilePath(dbPath)
	dir := filepath.Dir(portFile)
	_ = os.MkdirAll(dir, 0755)
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(port)), 0644); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// DialIPC connects to the daemon using the loopback TCP port recorded in watui.ipc.
func DialIPC(dbPath string) (net.Conn, error) {
	portFile := portFilePath(dbPath)
	data, err := os.ReadFile(portFile)
	if err != nil {
		return nil, err
	}
	portStr := strings.TrimSpace(string(data))
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("invalid port in %s", portFile)
	}
	return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// CleanupIPC removes the port file.
func CleanupIPC(dbPath string) {
	portFile := portFilePath(dbPath)
	_ = os.Remove(portFile)
}
