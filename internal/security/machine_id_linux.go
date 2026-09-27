//go:build linux

package security

import (
	"os"
	"strings"
)

func getPlatformMachineID() (string, error) {
	paths := []string{
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if len(trimmed) > 0 {
				return trimmed, nil
			}
		}
	}
	return "", os.ErrNotExist
}
