package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1       string
		v2       string
		expected int
	}{
		{"0.3.2", "0.3.2", 0},
		{"v0.3.2", "0.3.2", 0},
		{"0.3.2", "v0.3.3", -1},
		{"v0.4.0", "v0.3.9", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.3.2-alpha", "0.3.2", 0},
		{"0.3.1", "0.3.2-beta", -1},
		{"0.3.3", "0.3.2+build123", 1},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.v1, tt.v2)
		if got != tt.expected {
			t.Errorf("CompareVersions(%q, %q) = %d; want %d", tt.v1, tt.v2, got, tt.expected)
		}
	}
}

func TestDetectPackageManager(t *testing.T) {
	tests := []struct {
		path        string
		expectedPM  PackageManager
		expectEmpty bool
	}{
		{"/nix/store/abc123-watui/bin/watui", PkgManagerNix, false},
		{"/usr/local/Cellar/watui/0.3.2/bin/watui", PkgManagerHomebrew, false},
		{"/opt/homebrew/bin/watui", PkgManagerHomebrew, false},
		{"/snap/watui/current/bin/watui", PkgManagerSnap, false},
		{"/var/lib/flatpak/app/watui/bin/watui", PkgManagerFlatpak, false},
		{"C:\\Users\\user\\scoop\\apps\\watui\\current\\watui.exe", PkgManagerScoop, false},
		{"C:\\ProgramData\\chocolatey\\lib\\watui\\watui.exe", PkgManagerChoco, false},
	}

	for _, tt := range tests {
		pm, advice := DetectPackageManager(tt.path)
		if pm != tt.expectedPM {
			t.Errorf("DetectPackageManager(%q) = %v; want %v", tt.path, pm, tt.expectedPM)
		}
		if (advice == "") != tt.expectEmpty {
			t.Errorf("DetectPackageManager(%q) advice empty = %v; want %v", tt.path, advice == "", tt.expectEmpty)
		}
	}
}

func TestFindMatchingAsset(t *testing.T) {
	assets := []ReleaseAsset{
		{Name: "watui-v0.3.3-linux-amd64.tar.gz", Size: 1000, BrowserDownloadURL: "http://example.com/linux-amd64"},
		{Name: "watui-v0.3.3-linux-arm64.tar.gz", Size: 1000, BrowserDownloadURL: "http://example.com/linux-arm64"},
		{Name: "watui-v0.3.3-darwin-arm64.tar.gz", Size: 1000, BrowserDownloadURL: "http://example.com/darwin-arm64"},
		{Name: "watui-v0.3.3-windows-amd64-portable.zip", Size: 1000, BrowserDownloadURL: "http://example.com/windows-amd64"},
		{Name: "checksums.txt", Size: 100, BrowserDownloadURL: "http://example.com/checksums"},
	}

	bin, check := FindMatchingAsset(assets, "linux", "arm64")
	if bin == nil || bin.Name != "watui-v0.3.3-linux-arm64.tar.gz" {
		t.Fatalf("Expected linux-arm64 asset, got: %v", bin)
	}
	if check == nil || check.Name != "checksums.txt" {
		t.Fatalf("Expected checksums.txt asset, got: %v", check)
	}

	winBin, _ := FindMatchingAsset(assets, "windows", "amd64")
	if winBin == nil || winBin.Name != "watui-v0.3.3-windows-amd64-portable.zip" {
		t.Fatalf("Expected windows-amd64 asset, got: %v", winBin)
	}

	missingBin, _ := FindMatchingAsset(assets, "freebsd", "riscv64")
	if missingBin != nil {
		t.Fatalf("Expected nil for unsupported platform, got: %v", missingBin)
	}
}

func TestChecksumVerification(t *testing.T) {
	tmpDir := t.TempDir()
	content := []byte("fake-watui-binary-content-12345")
	archivePath := filepath.Join(tmpDir, "watui-test.tar.gz")
	if err := os.WriteFile(archivePath, content, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(h[:])

	checksumPath := filepath.Join(tmpDir, "checksums.txt")
	checksumContent := fmt.Sprintf("%s  watui-test.tar.gz\n%s  other.zip\n", expectedHash, "abcdef123456")
	if err := os.WriteFile(checksumPath, []byte(checksumContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Valid checksum
	if err := VerifyChecksum(archivePath, checksumPath, "watui-test.tar.gz"); err != nil {
		t.Errorf("Expected checksum verification to succeed, got: %v", err)
	}

	// 2. Mismatched checksum
	badChecksumPath := filepath.Join(tmpDir, "bad_checksums.txt")
	_ = os.WriteFile(badChecksumPath, []byte("0000000000000000000000000000000000000000000000000000000000000000  watui-test.tar.gz\n"), 0644)
	if err := VerifyChecksum(archivePath, badChecksumPath, "watui-test.tar.gz"); err == nil {
		t.Errorf("Expected checksum mismatch error, got nil")
	}

	// 3. Missing asset in checksums
	missingChecksumPath := filepath.Join(tmpDir, "missing_checksums.txt")
	_ = os.WriteFile(missingChecksumPath, []byte("abcdef  other-file.tar.gz\n"), 0644)
	if err := VerifyChecksum(archivePath, missingChecksumPath, "watui-test.tar.gz"); err == nil {
		t.Errorf("Expected missing checksum entry error, got nil")
	}
}

func TestExtractBinaryTarGz(t *testing.T) {
	tmpDir := t.TempDir()
	tarGzPath := filepath.Join(tmpDir, "watui.tar.gz")

	// Create test .tar.gz containing watui binary
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	binContent := []byte("#!/bin/sh\necho watui-v0.4.0\n")
	hdr := &tar.Header{
		Name: "watui-folder/watui",
		Mode: 0755,
		Size: int64(len(binContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binContent); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	if err := os.WriteFile(tarGzPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(tmpDir, "extracted-watui")
	if err := ExtractBinary(tarGzPath, destPath); err != nil {
		t.Fatalf("Failed to extract binary from tar.gz: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binContent) {
		t.Errorf("Extracted content mismatch: got %q, want %q", got, binContent)
	}
}

func TestExtractBinaryZip(t *testing.T) {
	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "watui.zip")

	// Create test .zip containing watui.exe
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	binContent := []byte("MZ-executable-binary-mock")
	w, err := zw.Create("watui-portable/watui.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binContent); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	if err := os.WriteFile(zipPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(tmpDir, "extracted-watui.exe")
	if err := ExtractBinary(zipPath, destPath); err != nil {
		t.Fatalf("Failed to extract binary from zip: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binContent) {
		t.Errorf("Extracted content mismatch: got %q, want %q", got, binContent)
	}
}

func TestReplaceBinary(t *testing.T) {
	tmpDir := t.TempDir()
	currentExe := filepath.Join(tmpDir, "watui")
	_ = os.WriteFile(currentExe, []byte("old-binary-v0.1.0"), 0755)

	newExe := filepath.Join(tmpDir, "watui-new")
	_ = os.WriteFile(newExe, []byte("new-binary-v0.2.0"), 0755)

	if err := ReplaceBinary(currentExe, newExe); err != nil {
		t.Fatalf("ReplaceBinary failed: %v", err)
	}

	updatedContent, err := os.ReadFile(currentExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedContent) != "new-binary-v0.2.0" {
		t.Errorf("Expected updated content 'new-binary-v0.2.0', got %q", string(updatedContent))
	}
}

func TestFetchLatestReleaseMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/test/watui/releases/latest" {
			http.NotFound(w, r)
			return
		}
		info := ReleaseInfo{
			TagName: "v0.4.0",
			Name:    "v0.4.0 Release",
			Assets: []ReleaseAsset{
				{Name: "watui-v0.4.0-linux-amd64.tar.gz", Size: 12345},
			},
		}
		_ = json.NewEncoder(w).Encode(info)
	}))
	defer server.Close()

	// Direct query with custom client pointing to mock server
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/repos/test/watui/releases/latest", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var info ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.TagName != "v0.4.0" {
		t.Errorf("Expected tag v0.4.0, got %s", info.TagName)
	}
}

func TestCheckUpdateNoticeWithCache(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("XDG_CACHE_HOME")
	os.Setenv("XDG_CACHE_HOME", tmpDir)
	defer os.Setenv("XDG_CACHE_HOME", origHome)

	// Write cache with newer version v0.9.0
	WriteCachedRelease("v0.9.0")

	notice := CheckUpdateNotice("0.3.2")
	if notice == "" {
		t.Errorf("Expected update notice for v0.9.0, got empty string")
	}

	// When current version is equal or newer, notice should be empty
	noNotice := CheckUpdateNotice("0.9.0")
	if noNotice != "" {
		t.Errorf("Expected no update notice for current version, got: %s", noNotice)
	}
}
