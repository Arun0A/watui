package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGetMediaCacheDir(t *testing.T) {
	dir, err := GetMediaCacheDir()
	if err != nil {
		t.Fatalf("GetMediaCacheDir returned error: %v", err)
	}
	if dir == "" {
		t.Fatalf("GetMediaCacheDir returned empty directory")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("Expected %s to exist as a directory", dir)
	}
}

func TestCleanExpiredMedia(t *testing.T) {
	cacheDir, err := GetMediaCacheDir()
	if err != nil {
		t.Fatalf("Failed to get media cache dir: %v", err)
	}

	// 1. Create a fresh file (1 hour old)
	freshFile := filepath.Join(cacheDir, "test_fresh_media.jpg")
	if err := os.WriteFile(freshFile, []byte("fresh media content"), 0644); err != nil {
		t.Fatalf("Failed to write fresh file: %v", err)
	}
	defer os.Remove(freshFile)

	// 2. Create an old expired file (80 hours old)
	expiredFile := filepath.Join(cacheDir, "test_expired_media.mp4")
	if err := os.WriteFile(expiredFile, []byte("old expired content"), 0644); err != nil {
		t.Fatalf("Failed to write expired file: %v", err)
	}
	defer os.Remove(expiredFile)

	oldTime := time.Now().Add(-80 * time.Hour)
	if err := os.Chtimes(expiredFile, oldTime, oldTime); err != nil {
		t.Fatalf("Failed to set old times on expired file: %v", err)
	}

	// 3. Clean with expireHours=0 (disabled) -> nothing should be removed
	removed, err := CleanExpiredMedia(0)
	if err != nil {
		t.Fatalf("CleanExpiredMedia(0) returned error: %v", err)
	}
	if removed != 0 {
		t.Errorf("Expected 0 removed with expireHours=0, got %d", removed)
	}
	if _, err := os.Stat(expiredFile); os.IsNotExist(err) {
		t.Errorf("Expired file should NOT be removed when expireHours=0")
	}

	// 4. Clean with expireHours=72 -> expiredFile should be removed, freshFile preserved
	removed, err = CleanExpiredMedia(72)
	if err != nil {
		t.Fatalf("CleanExpiredMedia(72) returned error: %v", err)
	}
	if removed < 1 {
		t.Errorf("Expected at least 1 removed with expireHours=72, got %d", removed)
	}
	if _, err := os.Stat(expiredFile); !os.IsNotExist(err) {
		t.Errorf("Expected expired file %s to be deleted", expiredFile)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("Expected fresh file %s to be preserved, got err=%v", freshFile, err)
	}

	// 5. Test TouchMediaFile prevents expiration
	reusedFile := filepath.Join(cacheDir, "test_reused_media.jpg")
	if err := os.WriteFile(reusedFile, []byte("reused content"), 0644); err != nil {
		t.Fatalf("Failed to write reused file: %v", err)
	}
	defer os.Remove(reusedFile)

	// Set to 80 hours old
	_ = os.Chtimes(reusedFile, oldTime, oldTime)
	// Touch it to now
	if err := TouchMediaFile(reusedFile); err != nil {
		t.Fatalf("TouchMediaFile failed: %v", err)
	}

	_, err = CleanExpiredMedia(72)
	if err != nil {
		t.Fatalf("CleanExpiredMedia(72) returned error: %v", err)
	}
	if _, err := os.Stat(reusedFile); err != nil {
		t.Errorf("Expected touched file %s to be preserved after TouchMediaFile, got err=%v", reusedFile, err)
	}
}

func TestClearMediaCache(t *testing.T) {
	cacheDir, err := GetMediaCacheDir()
	if err != nil {
		t.Fatalf("Failed to get media cache dir: %v", err)
	}

	testFile1 := filepath.Join(cacheDir, "test_clear_media_1.jpg")
	testFile2 := filepath.Join(cacheDir, "test_clear_media_2.png")

	if err := os.WriteFile(testFile1, []byte("content 1"), 0644); err != nil {
		t.Fatalf("Failed to write test file 1: %v", err)
	}
	if err := os.WriteFile(testFile2, []byte("content 2"), 0444); err != nil {
		t.Fatalf("Failed to write test file 2: %v", err)
	}

	if err := ClearMediaCache(); err != nil {
		t.Fatalf("ClearMediaCache returned error: %v", err)
	}

	if _, err := os.Stat(testFile1); !os.IsNotExist(err) {
		t.Errorf("Expected testFile1 to be deleted, got err=%v", err)
	}
	if _, err := os.Stat(testFile2); !os.IsNotExist(err) {
		t.Errorf("Expected testFile2 (readonly) to be deleted, got err=%v", err)
	}
}
