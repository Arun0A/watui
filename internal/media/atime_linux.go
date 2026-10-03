//go:build linux

package media

import (
	"os"
	"syscall"
	"time"
)

func getPlatformAccessTime(fi os.FileInfo) time.Time {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	}
	return fi.ModTime()
}
