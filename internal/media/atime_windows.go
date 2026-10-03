//go:build windows

package media

import (
	"os"
	"syscall"
	"time"
)

func getPlatformAccessTime(fi os.FileInfo) time.Time {
	if stat, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, stat.LastAccessTime.Nanoseconds())
	}
	return fi.ModTime()
}
