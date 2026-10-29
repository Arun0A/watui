package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"watui/internal/config"
	"watui/internal/daemon"
	"watui/internal/security"
)

func TestRunLogoutCleanup(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "watui.db")

	// Create fake database files
	dbFiles := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		dbPath + "-journal",
	}
	for _, f := range dbFiles {
		if err := os.WriteFile(f, []byte("fake db data"), 0600); err != nil {
			t.Fatalf("failed to create db file %s: %v", f, err)
		}
	}

	// Create encryption key & machine ID
	keyFile := filepath.Join(tempDir, security.KeyFileName)
	if err := os.WriteFile(keyFile, []byte("secret-key"), 0600); err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	machineIDFile := filepath.Join(tempDir, security.MachineIDFileName)
	if err := os.WriteFile(machineIDFile, []byte("machine-uuid"), 0600); err != nil {
		t.Fatalf("failed to create machine-id file: %v", err)
	}

	// Create daemon runtime files
	pidFile := daemon.PIDFilePath(dbPath)
	if err := os.WriteFile(pidFile, []byte("99999999"), 0600); err != nil {
		t.Fatalf("failed to create pid file: %v", err)
	}
	daemonLog := daemon.LogFilePath(dbPath)
	if err := os.WriteFile(daemonLog, []byte("daemon log line"), 0600); err != nil {
		t.Fatalf("failed to create daemon log: %v", err)
	}

	// Create user configuration file (which MUST NOT be touched)
	configFile := filepath.Join(tempDir, "watui.yaml")
	configContent := "theme: custom\nlog: WARN\n"
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}

	// Create a theme file (which MUST NOT be touched)
	themeFile := filepath.Join(tempDir, "theme.yaml")
	themeContent := "colors:\n  primary: \"#123456\"\n"
	if err := os.WriteFile(themeFile, []byte(themeContent), 0644); err != nil {
		t.Fatalf("failed to create theme file: %v", err)
	}

	appCfg := &config.Config{
		SourcePath: configFile,
	}

	// Execute runLogout
	ctx := context.Background()
	err := runLogout(ctx, dbPath, appCfg)
	if err != nil {
		t.Fatalf("runLogout returned unexpected error: %v", err)
	}

	// Assert database files are removed
	for _, f := range dbFiles {
		if _, statErr := os.Stat(f); !os.IsNotExist(statErr) {
			t.Errorf("expected database file %s to be removed, but it still exists", f)
		}
	}

	// Assert key and machine ID are removed
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Errorf("expected key file %s to be removed, but it still exists", keyFile)
	}
	if _, statErr := os.Stat(machineIDFile); !os.IsNotExist(statErr) {
		t.Errorf("expected machine ID file %s to be removed, but it still exists", machineIDFile)
	}

	// Assert daemon pid and log are removed
	if _, statErr := os.Stat(pidFile); !os.IsNotExist(statErr) {
		t.Errorf("expected pid file %s to be removed, but it still exists", pidFile)
	}
	if _, statErr := os.Stat(daemonLog); !os.IsNotExist(statErr) {
		t.Errorf("expected daemon log %s to be removed, but it still exists", daemonLog)
	}

	// Assert config files are strictly preserved
	readConfig, err := os.ReadFile(configFile)
	if err != nil {
		t.Errorf("config file was removed or inaccessible: %v", err)
	} else if string(readConfig) != configContent {
		t.Errorf("config file content modified: got %q, want %q", string(readConfig), configContent)
	}

	readTheme, err := os.ReadFile(themeFile)
	if err != nil {
		t.Errorf("theme file was removed or inaccessible: %v", err)
	} else if string(readTheme) != themeContent {
		t.Errorf("theme file content modified: got %q, want %q", string(readTheme), themeContent)
	}
}

func TestRunLogoutWithNoExistingDB(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "nonexistent.db")

	ctx := context.Background()
	err := runLogout(ctx, dbPath, &config.Config{})
	if err != nil {
		t.Fatalf("runLogout with non-existent db failed: %v", err)
	}
}
