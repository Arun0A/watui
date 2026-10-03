//go:build !linux && !darwin && !windows

package media

import (
	"os"
	"time"
)

func getPlatformAccessTime(fi os.FileInfo) time.Time {
	return fi.ModTime()
}
