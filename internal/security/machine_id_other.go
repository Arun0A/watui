//go:build !linux && !windows && !darwin

package security

import "errors"

func getPlatformMachineID() (string, error) {
	return "", errors.New("unsupported platform")
}
