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
		"/run/machine-id",
		"/run/systemd/machine-id",
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if len(trimmed) > 0 && trimmed != "uninitialized" {
				return trimmed, nil
			}
		}
	}

	dmiPaths := []string{
		"/sys/class/dmi/id/product_uuid",
		"/sys/devices/virtual/dmi/id/product_uuid",
	}
	for _, p := range dmiPaths {
		if data, err := os.ReadFile(p); err == nil {
			trimmed := strings.ToLower(strings.TrimSpace(string(data)))
			if len(trimmed) > 0 && trimmed != "00000000-0000-0000-0000-000000000000" && !strings.Contains(trimmed, "none") {
				return trimmed, nil
			}
		}
	}

	return "", os.ErrNotExist
}
