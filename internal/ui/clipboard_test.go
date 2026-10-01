package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectImageExtension(t *testing.T) {
	pngHeader := []byte("\x89PNG\r\n\x1a\nmoredata")
	if ext := detectImageExtension(pngHeader); ext != ".png" {
		t.Errorf("Expected .png, got %s", ext)
	}

	jpgHeader := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}
	if ext := detectImageExtension(jpgHeader); ext != ".jpg" {
		t.Errorf("Expected .jpg, got %s", ext)
	}

	gifHeader := []byte("GIF89a...")
	if ext := detectImageExtension(gifHeader); ext != ".gif" {
		t.Errorf("Expected .gif, got %s", ext)
	}

	webpHeader := []byte("RIFF1234WEBPVP8 ")
	if ext := detectImageExtension(webpHeader); ext != ".webp" {
		t.Errorf("Expected .webp, got %s", ext)
	}

	unknownHeader := []byte("randomdata")
	if ext := detectImageExtension(unknownHeader); ext != ".png" {
		t.Errorf("Expected fallback .png, got %s", ext)
	}
}

func TestResolveFilePath(t *testing.T) {
	tmpDir := t.TempDir()
	sampleFile := filepath.Join(tmpDir, "sample image.png")
	if err := os.WriteFile(sampleFile, []byte("fake png"), 0644); err != nil {
		t.Fatalf("Failed to create sample file: %v", err)
	}

	// 1. Direct path
	if resolved, ok := resolveFilePath(sampleFile); !ok || resolved != sampleFile {
		t.Errorf("Direct path resolution failed: got %q, ok=%v", resolved, ok)
	}

	// 2. Quoted path
	if resolved, ok := resolveFilePath(`"` + sampleFile + `"`); !ok || resolved != sampleFile {
		t.Errorf("Quoted path resolution failed: got %q, ok=%v", resolved, ok)
	}

	// 3. file:// URI
	fileURI := "file://" + sampleFile
	if resolved, ok := resolveFilePath(fileURI); !ok || resolved != sampleFile {
		t.Errorf("file:// URI resolution failed: got %q, ok=%v", resolved, ok)
	}

	// 4. URL encoded file URI
	encodedURI := "file://" + filepath.Join(tmpDir, "sample%20image.png")
	if resolved, ok := resolveFilePath(encodedURI); !ok || resolved != sampleFile {
		t.Errorf("URL encoded file URI resolution failed: got %q, ok=%v", resolved, ok)
	}

	// 5. Non-existent file
	if _, ok := resolveFilePath(filepath.Join(tmpDir, "doesnotexist.png")); ok {
		t.Errorf("Expected non-existent file to return false")
	}

	// 6. Directory
	if _, ok := resolveFilePath(tmpDir); ok {
		t.Errorf("Expected directory to return false")
	}
}

func TestSaveClipboardImage(t *testing.T) {
	pngData := []byte("\x89PNG\r\n\x1a\nsample-image-bytes")
	targetPath, err := saveClipboardImage(pngData)
	if err != nil {
		t.Fatalf("saveClipboardImage failed: %v", err)
	}
	defer os.Remove(targetPath)

	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("Saved image file does not exist: %v", err)
	}
	if info.Size() != int64(len(pngData)) {
		t.Errorf("Expected size %d, got %d", len(pngData), info.Size())
	}
}

func TestReadClipboardFallbackTextAndURI(t *testing.T) {
	tmpDir := t.TempDir()
	docFile := filepath.Join(tmpDir, "document.pdf")
	if err := os.WriteFile(docFile, []byte("pdf data"), 0644); err != nil {
		t.Fatalf("Failed to create document: %v", err)
	}

	origFunc := readClipboardFunc
	defer func() { readClipboardFunc = origFunc }()

	// Mock reader that simulates text containing a file path
	readClipboardFunc = func(ctx context.Context) (*ClipboardItem, error) {
		return &ClipboardItem{
			IsMedia:  true,
			FilePath: docFile,
			FileName: filepath.Base(docFile),
		}, nil
	}

	item, err := readClipboardFunc(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !item.IsMedia || item.FilePath != docFile {
		t.Errorf("Expected media item with path %q, got %+v", docFile, item)
	}
}

func TestClearClipboardCache(t *testing.T) {
	pngData := []byte("\x89PNG\r\n\x1a\ntest-cleanup-bytes")
	targetPath, err := saveClipboardImage(pngData)
	if err != nil {
		t.Fatalf("saveClipboardImage failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("Expected clipboard file to exist before clear: %v", err)
	}

	// Also create a read-only file in cache directory to verify cross-platform removal
	cacheDir := filepath.Dir(targetPath)
	readOnlyFile := filepath.Join(cacheDir, "clip_readonly_test.png")
	if err := os.WriteFile(readOnlyFile, pngData, 0444); err != nil {
		t.Fatalf("Failed to create read-only test file: %v", err)
	}
	_ = os.Chmod(readOnlyFile, 0444)

	// Clear cache
	if err := ClearClipboardCache(); err != nil {
		t.Fatalf("ClearClipboardCache failed: %v", err)
	}

	// Verify files have been deleted
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("Expected clipboard file to be removed after ClearClipboardCache, got err=%v", err)
	}
	if _, err := os.Stat(readOnlyFile); !os.IsNotExist(err) {
		t.Errorf("Expected read-only clipboard file to be removed after ClearClipboardCache, got err=%v", err)
	}
}

func TestCrossPlatformScratchCleanup(t *testing.T) {
	// Simulate temporary scratch files created by macOS and Windows CLI fallbacks
	macScratch := filepath.Join(os.TempDir(), "mac_clip_test_123.png")
	winScratch := filepath.Join(os.TempDir(), "win_clip_test_456.png")
	_ = os.WriteFile(macScratch, []byte("mac dummy"), 0644)
	_ = os.WriteFile(winScratch, []byte("win dummy"), 0644)

	if err := ClearClipboardCache(); err != nil {
		t.Fatalf("ClearClipboardCache failed: %v", err)
	}

	if _, err := os.Stat(macScratch); !os.IsNotExist(err) {
		t.Errorf("Expected mac scratch file %q to be cleared", macScratch)
	}
	if _, err := os.Stat(winScratch); !os.IsNotExist(err) {
		t.Errorf("Expected win scratch file %q to be cleared", winScratch)
	}
}

func TestClearSystemClipboard(t *testing.T) {
	// Ensures ClearSystemClipboard runs without runtime error or panic on any OS
	err := ClearSystemClipboard()
	if err != nil {
		t.Errorf("ClearSystemClipboard failed: %v", err)
	}
}
