//go:build darwin

package security

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
)

func getPlatformMachineID() (string, error) {
	cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if bytes.Contains(line, []byte("IOPlatformUUID")) {
			parts := bytes.Split(line, []byte("="))
			if len(parts) >= 2 {
				val := strings.Trim(strings.TrimSpace(string(parts[1])), "\"")
				return val, nil
			}
		}
	}
	return "", os.ErrNotExist
}
