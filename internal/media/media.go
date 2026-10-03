package media

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GetMediaCacheDir returns the primary directory used to store downloaded media attachments.
func GetMediaCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "watui-media")
	} else {
		cacheDir = filepath.Join(cacheDir, "watui", "media")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create media cache dir: %w", err)
	}
	return cacheDir, nil
}

// GetMediaCacheDirs returns all possible locations where media files may be cached across platforms.
func GetMediaCacheDirs() []string {
	seen := make(map[string]bool)
	dirs := []string{}

	addDir := func(d string) {
		if d == "" {
			return
		}
		clean := filepath.Clean(d)
		if !seen[clean] {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}

	// 1. Standard user cache dir (Linux: ~/.cache, macOS: ~/Library/Caches, Windows: %LocalAppData%)
	if uCache, err := os.UserCacheDir(); err == nil && uCache != "" {
		addDir(filepath.Join(uCache, "watui", "media"))
	}

	// 2. User home dir based fallbacks
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		addDir(filepath.Join(home, ".cache", "watui", "media"))
		addDir(filepath.Join(home, "Library", "Caches", "watui", "media"))
		addDir(filepath.Join(home, "AppData", "Local", "watui", "media"))
	}

	// 3. Environment variables
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		addDir(filepath.Join(xdg, "watui", "media"))
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		addDir(filepath.Join(localAppData, "watui", "media"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		addDir(filepath.Join(appData, "watui", "media"))
	}

	// 4. Temporary directories across platforms
	addDir(filepath.Join(os.TempDir(), "watui-media"))
	if t := os.Getenv("TEMP"); t != "" {
		addDir(filepath.Join(t, "watui-media"))
	}
	if t := os.Getenv("TMP"); t != "" {
		addDir(filepath.Join(t, "watui-media"))
	}

	return dirs
}

// TouchMediaFile updates the last access and modification times of a media file to the current time.
func TouchMediaFile(filePath string) error {
	now := time.Now()
	return os.Chtimes(filePath, now, now)
}

// GetFileAccessTime returns the latest known timestamp for the file between its access time and modification time.
func GetFileAccessTime(fi os.FileInfo) time.Time {
	atime := getPlatformAccessTime(fi)
	if !atime.IsZero() && atime.After(fi.ModTime()) {
		return atime
	}
	return fi.ModTime()
}

// CleanExpiredMedia scans all media cache directories and deletes files whose last access/modification
// time is older than expireHours. If expireHours <= 0, no files are removed (expiration is disabled).
func CleanExpiredMedia(expireHours int) (int, error) {
	if expireHours <= 0 {
		return 0, nil
	}

	maxAge := time.Duration(expireHours) * time.Hour
	now := time.Now()
	removedCount := 0

	dirs := GetMediaCacheDirs()
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			filePath := filepath.Join(d, entry.Name())
			fi, err := entry.Info()
			if err != nil {
				fi, err = os.Stat(filePath)
				if err != nil {
					continue
				}
			}

			lastAccess := GetFileAccessTime(fi)
			if now.Sub(lastAccess) > maxAge {
				if err := removeFileCrossPlatform(filePath); err == nil {
					removedCount++
				}
			}
		}
	}
	return removedCount, nil
}

// ClearMediaCache removes all files stored in watui media cache directories across platforms.
func ClearMediaCache() error {
	dirs := GetMediaCacheDirs()
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				_ = removeFileCrossPlatform(filepath.Join(d, entry.Name()))
			}
		}
	}
	return nil
}

func removeFileCrossPlatform(path string) error {
	_ = os.Chmod(path, 0666) // Clear any read-only bits on Windows
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
