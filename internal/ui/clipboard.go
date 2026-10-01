package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	atotto_clipboard "github.com/atotto/clipboard"
	"golang.design/x/clipboard"
)

var (
	clipInitOnce sync.Once
	clipInitErr  error
)

func initClipboard() error {
	clipInitOnce.Do(func() {
		clipInitErr = clipboard.Init()
	})
	return clipInitErr
}

// ClipboardItem represents data extracted from the system clipboard.
type ClipboardItem struct {
	IsMedia  bool
	FilePath string
	FileName string
	Text     string
}

// readClipboardFunc can be replaced in unit tests.
var readClipboardFunc = ReadClipboard

func getClipboardCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "watui-clipboard")
	} else {
		cacheDir = filepath.Join(cacheDir, "watui", "clipboard")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	return cacheDir, nil
}

func getClipboardCacheDirs() []string {
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
		addDir(filepath.Join(uCache, "watui", "clipboard"))
	}

	// 2. User home dir based fallbacks
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		addDir(filepath.Join(home, ".cache", "watui", "clipboard"))
		addDir(filepath.Join(home, "Library", "Caches", "watui", "clipboard"))
		addDir(filepath.Join(home, "AppData", "Local", "watui", "clipboard"))
	}

	// 3. Environment variables
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		addDir(filepath.Join(xdg, "watui", "clipboard"))
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		addDir(filepath.Join(localAppData, "watui", "clipboard"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		addDir(filepath.Join(appData, "watui", "clipboard"))
	}

	// 4. Temporary directories across platforms
	addDir(filepath.Join(os.TempDir(), "watui-clipboard"))
	addDir(filepath.Join("/tmp", "watui-clipboard"))
	if winTemp := os.Getenv("TEMP"); winTemp != "" {
		addDir(filepath.Join(winTemp, "watui-clipboard"))
	}
	if winTmp := os.Getenv("TMP"); winTmp != "" {
		addDir(filepath.Join(winTmp, "watui-clipboard"))
	}

	// 5. Portable executable directory (if running standalone or portable)
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		addDir(filepath.Join(exeDir, "cache", "clipboard"))
		addDir(filepath.Join(exeDir, "clipboard"))
	}

	return dirs
}

func removeFileCrossPlatform(path string) error {
	// On Windows, read-only permissions cause os.Remove to return Access is Denied.
	// Setting writable permissions ensures clean removal.
	_ = os.Chmod(path, 0666)

	err := os.Remove(path)
	if err != nil && runtime.GOOS == "windows" {
		// On Windows, if a file handle is briefly held by an indexer or antivirus, retry once.
		time.Sleep(20 * time.Millisecond)
		err = os.Remove(path)
	}
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// ClearClipboardCache removes all temporary files stored in watui clipboard cache directories across Linux, macOS, and Windows.
func ClearClipboardCache() error {
	dirs := getClipboardCacheDirs()

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

	// Also clean up any lingering CLI clipboard scratch files in temporary directories
	tempDirs := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		tempDirs = append(tempDirs, "/tmp")
	}
	if t := os.Getenv("TEMP"); t != "" {
		tempDirs = append(tempDirs, t)
	}
	if t := os.Getenv("TMP"); t != "" {
		tempDirs = append(tempDirs, t)
	}

	seenTemp := make(map[string]bool)
	for _, td := range tempDirs {
		cleanTD := filepath.Clean(td)
		if seenTemp[cleanTD] {
			continue
		}
		seenTemp[cleanTD] = true

		entries, err := os.ReadDir(cleanTD)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, "mac_clip_") || strings.HasPrefix(name, "win_clip_") || strings.HasPrefix(name, "watui_clip_") {
				_ = removeFileCrossPlatform(filepath.Join(cleanTD, name))
			}
		}
	}

	return nil
}

// ClearSystemClipboard clears the system clipboard across Linux (X11 & Wayland), macOS, and Windows.
func ClearSystemClipboard() error {
	// 1. golang.design/x/clipboard
	if err := initClipboard(); err == nil {
		_, _ = clipboard.Write(context.Background(), clipboard.FmtText, []byte{})
	}

	// 2. atotto clipboard
	_ = atotto_clipboard.WriteAll("")

	// 3. Platform-specific CLI tools
	switch runtime.GOOS {
	case "darwin":
		if p, err := exec.LookPath("pbcopy"); err == nil {
			cmd := exec.Command(p)
			cmd.Stdin = strings.NewReader("")
			_ = cmd.Run()
		}
	case "windows":
		if p, err := exec.LookPath("clip"); err == nil {
			cmd := exec.Command(p)
			cmd.Stdin = strings.NewReader("")
			_ = cmd.Run()
		}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if p, err := exec.LookPath("wl-copy"); err == nil {
				_ = exec.Command(p, "--clear").Run()
			}
		}
		if os.Getenv("DISPLAY") != "" {
			if p, err := exec.LookPath("xclip"); err == nil {
				cmd := exec.Command(p, "-selection", "clipboard")
				cmd.Stdin = strings.NewReader("")
				_ = cmd.Run()
			}
		}
	}
	return nil
}

func detectImageExtension(data []byte) string {
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		return ".png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return ".jpg"
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return ".gif"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return ".webp"
	}
	return ".png"
}

func saveClipboardImage(data []byte) (string, error) {
	cacheDir, err := getClipboardCacheDir()
	if err != nil {
		return "", err
	}
	ext := detectImageExtension(data)
	filename := fmt.Sprintf("clip_%s%s", time.Now().Format("20060102_150405_000"), ext)
	targetPath := filepath.Join(cacheDir, filename)
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		return "", err
	}
	return targetPath, nil
}

// resolveFilePath checks if raw string represents a file:// URI or existing file path.
func resolveFilePath(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}

	// 1. Check for file:// URI
	if strings.HasPrefix(s, "file://") {
		trimmed := strings.TrimPrefix(s, "file://")
		if unescaped, err := url.PathUnescape(trimmed); err == nil {
			trimmed = unescaped
		}
		expanded := expandPath(trimmed)
		if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
			return expanded, true
		}
	}

	// 2. Strip quotes if present: "path" or 'path'
	if (strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2) ||
		(strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2) {
		s = s[1 : len(s)-1]
	}

	// 3. Check if path starts with root, home, or relative specifier
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") || (len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')) {
		expanded := expandPath(s)
		if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
			return expanded, true
		}
	}

	return "", false
}

// readImageFromCLI executes OS-level clipboard CLI tools as fallback.
func readImageFromCLI(ctx context.Context) ([]byte, error) {
	// Wayland wl-paste
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if path, err := exec.LookPath("wl-paste"); err == nil {
			cmd := exec.CommandContext(ctx, path, "--type", "image/png")
			if out, err := cmd.Output(); err == nil && len(out) > 0 {
				return out, nil
			}
		}
	}

	// X11 xclip
	if os.Getenv("DISPLAY") != "" {
		if path, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.CommandContext(ctx, path, "-selection", "clipboard", "-target", "image/png", "-o")
			if out, err := cmd.Output(); err == nil && len(out) > 0 {
				return out, nil
			}
		}
	}

	// macOS osascript
	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("osascript"); err == nil {
			targetDir, err := getClipboardCacheDir()
			if err != nil {
				targetDir = os.TempDir()
			}
			tmpPath := filepath.Join(targetDir, fmt.Sprintf("mac_clip_%d.png", time.Now().UnixNano()))
			script := fmt.Sprintf(`
				try
					set png_data to the clipboard as «class PNGf»
					set fp to open for access POSIX file "%s" with write permission
					write png_data to fp
					close access fp
					return "ok"
				on error
					try
						close access POSIX file "%s"
					end try
					return "err"
				end try`, tmpPath, tmpPath)
			cmd := exec.CommandContext(ctx, path, "-e", script)
			if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "ok" {
				defer removeFileCrossPlatform(tmpPath)
				if bytes, err := os.ReadFile(tmpPath); err == nil && len(bytes) > 0 {
					return bytes, nil
				}
			}
		}
	}

	// Windows PowerShell
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("powershell"); err == nil {
			targetDir, err := getClipboardCacheDir()
			if err != nil {
				targetDir = os.TempDir()
			}
			tmpPath := filepath.Join(targetDir, fmt.Sprintf("win_clip_%d.png", time.Now().UnixNano()))
			escapedPath := filepath.ToSlash(tmpPath)
			psScript := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; $img = [System.Windows.Forms.Clipboard]::GetImage(); if ($img) { $img.Save('%s', [System.Drawing.Imaging.ImageFormat]::Png); Write-Output "ok" }`, escapedPath)
			cmd := exec.CommandContext(ctx, path, "-NoProfile", "-Command", psScript)
			if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "ok" {
				defer removeFileCrossPlatform(tmpPath)
				if bytes, err := os.ReadFile(tmpPath); err == nil && len(bytes) > 0 {
					return bytes, nil
				}
			}
		}
	}

	return nil, errors.New("no image on clipboard")
}

// ReadClipboard inspects the system clipboard for files, images, or text.
func ReadClipboard(ctx context.Context) (*ClipboardItem, error) {
	// 1. Check for files (e.g. copied from file managers like Nautilus, Dolphin, Explorer, Finder)
	if err := initClipboard(); err == nil {
		if files, err := clipboard.ReadFiles(ctx); err == nil && len(files) > 0 {
			for _, f := range files {
				if resolved, ok := resolveFilePath(f); ok {
					return &ClipboardItem{
						IsMedia:  true,
						FilePath: resolved,
						FileName: filepath.Base(resolved),
					}, nil
				}
			}
		}

		// 2. Check for image data from clipboard
		if imgBytes, err := clipboard.Read(ctx, clipboard.FmtImage); err == nil && len(imgBytes) > 0 {
			if path, err := saveClipboardImage(imgBytes); err == nil {
				return &ClipboardItem{
					IsMedia:  true,
					FilePath: path,
					FileName: filepath.Base(path),
				}, nil
			}
		}
	}

	// 3. Fallback: try CLI image tools
	if imgBytes, err := readImageFromCLI(ctx); err == nil && len(imgBytes) > 0 {
		if path, err := saveClipboardImage(imgBytes); err == nil {
			return &ClipboardItem{
				IsMedia:  true,
				FilePath: path,
				FileName: filepath.Base(path),
			}, nil
		}
	}

	// 4. Check for text on clipboard
	var text string
	if err := initClipboard(); err == nil {
		if textBytes, err := clipboard.Read(ctx, clipboard.FmtText); err == nil && len(textBytes) > 0 {
			text = string(textBytes)
		}
	}
	if text == "" {
		if t, err := atotto_clipboard.ReadAll(); err == nil {
			text = t
		}
	}

	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errors.New("clipboard is empty")
	}

	// 5. Check if the text contains a file URI or file path
	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if resolved, ok := resolveFilePath(line); ok {
			return &ClipboardItem{
				IsMedia:  true,
				FilePath: resolved,
				FileName: filepath.Base(resolved),
			}, nil
		}
	}

	if resolved, ok := resolveFilePath(trimmed); ok {
		return &ClipboardItem{
			IsMedia:  true,
			FilePath: resolved,
			FileName: filepath.Base(resolved),
		}, nil
	}

	// 6. Otherwise return standard text
	return &ClipboardItem{
		IsMedia: false,
		Text:    text,
	}, nil
}
