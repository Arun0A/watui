package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"watui/internal/config"
	"watui/internal/daemon"
)

const (
	DefaultRepo      = "Arun0A/watui"
	UpdateCacheTTL   = 6 * time.Hour
	HTTPTimeoutShort = 1500 * time.Millisecond
	HTTPTimeoutLong  = 60 * time.Second
)

// ReleaseAsset represents an asset attached to a GitHub release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ReleaseInfo represents GitHub release metadata.
type ReleaseInfo struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []ReleaseAsset `json:"assets"`
}

// VersionCache caches update checks locally to avoid network delays.
type VersionCache struct {
	LatestTag string    `json:"latest_tag"`
	CheckedAt time.Time `json:"checked_at"`
}

// PackageManager represents a detected external package manager.
type PackageManager string

const (
	PkgManagerNone     PackageManager = ""
	PkgManagerNix      PackageManager = "Nix"
	PkgManagerHomebrew PackageManager = "Homebrew"
	PkgManagerSnap     PackageManager = "Snap"
	PkgManagerFlatpak  PackageManager = "Flatpak"
	PkgManagerScoop    PackageManager = "Scoop"
	PkgManagerChoco    PackageManager = "Chocolatey"
	PkgManagerPacman   PackageManager = "Pacman"
	PkgManagerAPT      PackageManager = "APT"
	PkgManagerReadOnly PackageManager = "ReadOnly"
)

// CleanVersion strips leading "v" and any extra whitespace.
func CleanVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// CompareVersions compares two semver strings (e.g., "0.3.2" and "0.4.0").
// Returns -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2.
func CompareVersions(v1, v2 string) int {
	c1 := CleanVersion(v1)
	c2 := CleanVersion(v2)

	// Strip commit hashes or prerelease tags (e.g. "0.3.2-alpha" -> "0.3.2")
	parseParts := func(v string) []int {
		base := strings.Split(v, "-")[0]
		base = strings.Split(base, "+")[0]
		parts := strings.Split(base, ".")
		res := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				n = 0
			}
			res = append(res, n)
		}
		for len(res) < 3 {
			res = append(res, 0)
		}
		return res
	}

	p1 := parseParts(c1)
	p2 := parseParts(c2)

	for i := 0; i < 3; i++ {
		if p1[i] < p2[i] {
			return -1
		}
		if p1[i] > p2[i] {
			return 1
		}
	}
	return 0
}

// GetUpdateCachePath returns the file path for the update check cache.
func GetUpdateCachePath() string {
	if runtime.GOOS == "windows" {
		exeDir := config.GetExeDir()
		if exeDir != "" && exeDir != "." {
			return filepath.Join(exeDir, ".watui_update_cache.json")
		}
	}

	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		cacheHome := os.Getenv("XDG_CACHE_HOME")
		if cacheHome == "" {
			cacheHome = filepath.Join(home, ".cache")
		}
		dir := filepath.Join(cacheHome, "watui")
		_ = os.MkdirAll(dir, 0755)
		return filepath.Join(dir, "update_cache.json")
	}
	return filepath.Join(os.TempDir(), "watui_update_cache.json")
}

// ReadCachedRelease reads the cached latest version tag if not expired.
func ReadCachedRelease() string {
	cachePath := GetUpdateCachePath()
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	var c VersionCache
	if err := json.Unmarshal(data, &c); err != nil {
		return ""
	}
	if time.Since(c.CheckedAt) > UpdateCacheTTL {
		return ""
	}
	return c.LatestTag
}

// WriteCachedRelease saves the latest release tag to disk cache.
func WriteCachedRelease(tag string) {
	cachePath := GetUpdateCachePath()
	c := VersionCache{
		LatestTag: tag,
		CheckedAt: time.Now(),
	}
	data, err := json.Marshal(c)
	if err == nil {
		_ = os.WriteFile(cachePath, data, 0644)
	}
}

// FetchLatestRelease queries the GitHub Releases API for the latest release.
func FetchLatestRelease(ctx context.Context, repo string) (*ReleaseInfo, error) {
	if repo == "" {
		repo = DefaultRepo
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "watui-updater")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release API returned HTTP %d", resp.StatusCode)
	}

	var info ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to parse release metadata: %w", err)
	}

	if info.TagName != "" {
		WriteCachedRelease(info.TagName)
	}

	return &info, nil
}

// DetectPackageManager checks if the running executable is managed by a package manager
// or located in a restricted/read-only system directory.
func DetectPackageManager(exePath string) (PackageManager, string) {
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return PkgManagerNone, ""
		}
	}

	realPath, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		realPath = exePath
	}

	normalized := strings.ToLower(strings.ReplaceAll(realPath, "\\", "/"))

	// 1. Nix store
	if strings.Contains(normalized, "/nix/store/") {
		return PkgManagerNix, "Installed via Nix. Please update using: nix profile upgrade watui (or update your flake input)."
	}

	// 2. Homebrew
	if strings.Contains(normalized, "/cellar/") || strings.Contains(normalized, "/opt/homebrew/") {
		return PkgManagerHomebrew, "Installed via Homebrew. Please update using: brew upgrade watui"
	}

	// 3. Snap
	if strings.Contains(normalized, "/snap/") {
		return PkgManagerSnap, "Installed via Snap. Please update using: snap refresh watui"
	}

	// 4. Flatpak
	if strings.Contains(normalized, "/flatpak/") {
		return PkgManagerFlatpak, "Installed via Flatpak. Please update using: flatpak update watui"
	}

	// 5. Windows Scoop
	if strings.Contains(normalized, "/scoop/apps/") {
		return PkgManagerScoop, "Installed via Scoop. Please update using: scoop update watui"
	}

	// 6. Windows Chocolatey
	if strings.Contains(normalized, "/chocolatey/lib/") {
		return PkgManagerChoco, "Installed via Chocolatey. Please update using: choco upgrade watui"
	}

	// 7. Linux System Packages (dpkg / pacman)
	if runtime.GOOS == "linux" && (realPath == "/usr/bin/watui" || realPath == "/bin/watui") {
		if _, err := os.Stat("/var/lib/dpkg/info/watui.list"); err == nil {
			return PkgManagerAPT, "Installed via APT/deb. Please update using your distribution package manager."
		}
		if _, err := os.Stat("/var/lib/pacman/local"); err == nil {
			// Check if owned by pacman
			if _, err := os.Stat("/usr/share/licenses/watui"); err == nil {
				return PkgManagerPacman, "Installed via Pacman/AUR. Please update using: pacman -Syu watui"
			}
		}
	}

	// 8. Permission check on current binary directory
	exeDir := filepath.Dir(realPath)
	testFile := filepath.Join(exeDir, fmt.Sprintf(".watui_write_test_%d", os.Getpid()))
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		if os.IsPermission(err) {
			if runtime.GOOS != "windows" {
				return PkgManagerReadOnly, fmt.Sprintf("Directory %s requires elevated privileges. Please rerun with sudo: sudo watui --update", exeDir)
			}
			return PkgManagerReadOnly, fmt.Sprintf("Directory %s requires administrator privileges. Please run terminal as Administrator.", exeDir)
		}
	} else {
		_ = os.Remove(testFile)
	}

	return PkgManagerNone, ""
}

// FindMatchingAsset locates the release asset corresponding to the current OS and architecture.
func FindMatchingAsset(assets []ReleaseAsset, goos, goarch string) (*ReleaseAsset, *ReleaseAsset) {
	var binaryAsset *ReleaseAsset
	var checksumAsset *ReleaseAsset

	targetKey := fmt.Sprintf("%s-%s", goos, goarch)

	for i := range assets {
		name := strings.ToLower(assets[i].Name)
		if strings.Contains(name, "checksum") {
			checksumAsset = &assets[i]
			continue
		}
		if strings.Contains(name, targetKey) {
			binaryAsset = &assets[i]
		}
	}

	return binaryAsset, checksumAsset
}

// DownloadFile downloads a remote URL into a local file path with progress callback.
func DownloadFile(ctx context.Context, url, destPath string, onProgress func(downloaded, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "watui-updater")

	client := &http.Client{Timeout: HTTPTimeoutLong}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d for %s", resp.StatusCode, url)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer out.Close()

	total := resp.ContentLength
	var downloaded int64

	buf := make([]byte, 32*1024)
	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				return wErr
			}
			downloaded += int64(n)
			if onProgress != nil {
				onProgress(downloaded, total)
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				break
			}
			return rErr
		}
	}

	return nil
}

// VerifyChecksum verifies that the file matches the expected SHA-256 hash in checksums.txt.
func VerifyChecksum(filePath, checksumsPath, assetName string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	cf, err := os.Open(checksumsPath)
	if err != nil {
		return fmt.Errorf("failed to open checksums file: %w", err)
	}
	defer cf.Close()

	scanner := bufio.NewScanner(cf)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			hash := parts[0]
			fname := filepath.Base(parts[1])
			if fname == assetName || strings.HasSuffix(fname, assetName) {
				if !strings.EqualFold(hash, actualHash) {
					return fmt.Errorf("checksum mismatch: expected %s, got %s", hash, actualHash)
				}
				return nil
			}
		}
	}

	return fmt.Errorf("no matching checksum found for %s", assetName)
}

// ExtractBinary extracts the executable file ("watui" or "watui.exe") from .tar.gz or .zip.
func ExtractBinary(archivePath, destBinaryPath string) error {
	lower := strings.ToLower(archivePath)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		return extractTarGz(archivePath, destBinaryPath)
	}
	if strings.HasSuffix(lower, ".zip") {
		return extractZip(archivePath, destBinaryPath)
	}
	return fmt.Errorf("unsupported archive format: %s", archivePath)
}

func extractTarGz(archivePath, destBinaryPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		base := filepath.Base(header.Name)
		if base == "watui" || base == "watui.exe" {
			out, err := os.OpenFile(destBinaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
			return nil
		}
	}
	return errors.New("binary not found in archive")
}

func extractZip(archivePath, destBinaryPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, file := range zr.File {
		base := filepath.Base(file.Name)
		if base == "watui" || base == "watui.exe" {
			rc, err := file.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err := os.OpenFile(destBinaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, rc); err != nil {
				out.Close()
				return err
			}
			out.Close()
			return nil
		}
	}
	return errors.New("binary not found in archive")
}

// ReplaceBinary atomically replaces currentExe with newBinary.
// On Windows, renames running .exe to .old, moves new to target, and attempts cleanup.
// On Unix, uses atomic os.Rename.
func ReplaceBinary(currentExe, newBinary string) error {
	realExe, err := filepath.EvalSymlinks(currentExe)
	if err != nil {
		realExe = currentExe
	}

	exeDir := filepath.Dir(realExe)

	// Clean up any old Windows backup files if present
	CleanupOldBinaries(exeDir)

	if runtime.GOOS == "windows" {
		oldExe := realExe + ".old"
		_ = os.Remove(oldExe) // Remove leftover from earlier runs if any

		if err := os.Rename(realExe, oldExe); err != nil {
			return fmt.Errorf("failed to move current executable to .old: %w", err)
		}

		if err := copyAndSyncFile(newBinary, realExe, 0755); err != nil {
			// Rollback on failure
			_ = os.Rename(oldExe, realExe)
			return fmt.Errorf("failed to write new executable: %w", err)
		}

		// Attempt removing .old; if locked, will be cleaned up on next execution
		_ = os.Remove(oldExe)
		return nil
	}

	// Unix atomic swap: staging file in the same directory (same mount point)
	stagingFile := filepath.Join(exeDir, fmt.Sprintf(".watui.update.%d", os.Getpid()))
	if err := copyAndSyncFile(newBinary, stagingFile, 0755); err != nil {
		_ = os.Remove(stagingFile)
		return fmt.Errorf("failed to stage new binary: %w", err)
	}

	if err := os.Rename(stagingFile, realExe); err != nil {
		_ = os.Remove(stagingFile)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}

func copyAndSyncFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// CleanupOldBinaries removes temporary .old or staging files left by previous updates.
func CleanupOldBinaries(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".old") || strings.HasPrefix(name, ".watui.update.") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// CheckUpdateNotice checks if a newer version exists (using cache first, then non-blocking network query).
// Returns a human-friendly string if an update is available, or empty string.
func CheckUpdateNotice(currentVersion string) string {
	cachedTag := ReadCachedRelease()
	if cachedTag != "" {
		if CompareVersions(currentVersion, cachedTag) < 0 {
			return fmt.Sprintf(" (New version %s is available! Run 'watui --update' to upgrade)", cachedTag)
		}
		return ""
	}

	// Stale or missing cache: perform quick query with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), HTTPTimeoutShort)
	defer cancel()

	info, err := FetchLatestRelease(ctx, DefaultRepo)
	if err == nil && info != nil && info.TagName != "" {
		if CompareVersions(currentVersion, info.TagName) < 0 {
			return fmt.Sprintf(" (New version %s is available! Run 'watui --update' to upgrade)", info.TagName)
		}
	}

	return ""
}

// PerformSelfUpdate coordinates checking, downloading, verifying, stopping/restarting daemon,
// and atomically updating the binary.
func PerformSelfUpdate(ctx context.Context, currentVersion, dbPath string) error {
	fmt.Printf("[*] Current version: %s\n", currentVersion)

	// 1. Package manager guard
	realExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("unable to determine current executable path: %w", err)
	}
	realExe, _ = filepath.EvalSymlinks(realExe)

	pm, advice := DetectPackageManager(realExe)
	if pm != PkgManagerNone {
		return fmt.Errorf("automatic update aborted:\n%s", advice)
	}

	// 2. Query GitHub for latest release
	fmt.Println("[*] Checking GitHub for latest release...")
	info, err := FetchLatestRelease(ctx, DefaultRepo)
	if err != nil {
		return fmt.Errorf("failed to check latest release: %w", err)
	}

	cmp := CompareVersions(currentVersion, info.TagName)
	if cmp >= 0 {
		fmt.Printf("watui is already up to date (%s).\n", info.TagName)
		return nil
	}

	fmt.Printf("[*] New version found: %s (current: %s)\n", info.TagName, currentVersion)

	// 3. Locate matching platform asset
	binaryAsset, checksumAsset := FindMatchingAsset(info.Assets, runtime.GOOS, runtime.GOARCH)
	if binaryAsset == nil {
		return fmt.Errorf("no release asset found for platform %s-%s", runtime.GOOS, runtime.GOARCH)
	}

	tempDir, err := os.MkdirTemp("", "watui-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, binaryAsset.Name)
	fmt.Printf("[*] Downloading %s (%.2f MB)...\n", binaryAsset.Name, float64(binaryAsset.Size)/(1024*1024))

	lastPercent := -1
	err = DownloadFile(ctx, binaryAsset.BrowserDownloadURL, archivePath, func(downloaded, total int64) {
		if total > 0 {
			pct := int((downloaded * 100) / total)
			if pct != lastPercent && pct%20 == 0 {
				lastPercent = pct
				fmt.Printf("    Downloading: %d%%\n", pct)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("failed to download release asset: %w", err)
	}

	// 4. Checksum verification
	if checksumAsset != nil {
		checksumPath := filepath.Join(tempDir, checksumAsset.Name)
		fmt.Println("[*] Verifying package integrity (SHA-256)...")
		if err := DownloadFile(ctx, checksumAsset.BrowserDownloadURL, checksumPath, nil); err == nil {
			if err := VerifyChecksum(archivePath, checksumPath, binaryAsset.Name); err != nil {
				return fmt.Errorf("security check failed: %w", err)
			}
			fmt.Println("Checksum verified successfully.")
		}
	}

	// 5. Unpack binary
	extractedBin := filepath.Join(tempDir, "watui_new_bin")
	if runtime.GOOS == "windows" {
		extractedBin += ".exe"
	}

	fmt.Println("[*] Extracting update...")
	if err := ExtractBinary(archivePath, extractedBin); err != nil {
		return fmt.Errorf("failed to extract binary: %w", err)
	}

	// 6. Daemon coordination
	pidFile := daemon.PIDFilePath(dbPath)
	daemonWasRunning, daemonPID := daemon.IsRunning(pidFile)
	if daemonWasRunning {
		fmt.Printf("[*] Stopping running background daemon (PID %d)...\n", daemonPID)
		daemon.Stop(pidFile)
		time.Sleep(300 * time.Millisecond)
	}

	// 7. Atomic self-replace
	fmt.Printf("[*] Updating executable at: %s\n", realExe)
	if err := ReplaceBinary(realExe, extractedBin); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}

	// 8. Restart daemon if it was running
	if daemonWasRunning {
		fmt.Println("[*] Restarting background daemon...")
		daemon.CleanupIPC(dbPath)
		cmd := exec.Command(realExe, "daemon", "start", "--db", dbPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("! Warning: could not restart daemon automatically: %v\n%s\n", err, string(out))
			fmt.Println("You can start it manually with: watui daemon start")
		} else {
			fmt.Print(string(out))
		}
	}

	fmt.Printf("Successfully updated watui to %s!\n", info.TagName)
	return nil
}
