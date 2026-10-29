package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"watui/internal/config"
	"watui/internal/daemon"
	"watui/internal/media"
	"watui/internal/security"
	"watui/internal/ui"
	"watui/internal/whatsapp"
)

// runLogout unlinks the device session from WhatsApp servers, stops the background daemon,
// and deletes all database files, encryption keys, daemon runtime files, and cached media/clipboard data.
// It strictly preserves all user configuration files (e.g. watui.yaml, config.yaml, theme.yaml).
func runLogout(ctx context.Context, dbPath string, appCfg *config.Config) error {
	fmt.Println("Logging out of watui...")

	// 1. Stop background daemon if running so database locks are completely released
	pidFile := daemon.PIDFilePath(dbPath)
	if running, pid := daemon.IsRunning(pidFile); running {
		fmt.Printf("Stopping running background daemon (PID %d)...\n", pid)
		if _, _, err := daemon.Stop(pidFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop daemon: %v\n", err)
		}
	}
	daemon.CleanupIPC(dbPath)

	// 2. Unlink device session from WhatsApp servers
	if _, err := os.Stat(dbPath); err == nil {
		fmt.Println("Unlinking session from WhatsApp servers...")
		logoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		adapter, err := whatsapp.NewAdapter(logoutCtx, whatsapp.Config{
			DBPath:   dbPath,
			LogLevel: "ERROR",
		})
		if err == nil {
			if adapter.IsLoggedIn() {
				if logoutErr := adapter.Logout(logoutCtx); logoutErr != nil {
					fmt.Printf("Note: Server unlinking returned: %v (clearing local session data)\n", logoutErr)
				} else {
					fmt.Println("Session successfully unlinked from WhatsApp.")
				}
			} else {
				adapter.Disconnect()
			}
		} else {
			fmt.Printf("Note: Could not open database for unlinking: %v\n", err)
		}
		cancel()
	} else {
		fmt.Println("No existing session database found.")
	}

	// 3. Remove all database files and SQLite journal/wal/shm temporary files
	dbFiles := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		dbPath + "-journal",
	}
	for _, f := range dbFiles {
		if err := os.Remove(f); err == nil {
			fmt.Printf("Removed database file: %s\n", f)
		}
	}

	// 4. Remove encryption key and machine ID token in the database directory
	dbDir := filepath.Dir(dbPath)
	keyFile := filepath.Join(dbDir, security.KeyFileName)
	if err := os.Remove(keyFile); err == nil {
		fmt.Printf("Removed encryption key: %s\n", keyFile)
	}
	machineIDFile := filepath.Join(dbDir, security.MachineIDFileName)
	if err := os.Remove(machineIDFile); err == nil {
		fmt.Printf("Removed machine ID: %s\n", machineIDFile)
	}

	// 5. Remove daemon runtime files
	daemon.RemovePID(pidFile)
	daemon.CleanupIPC(dbPath)
	daemonLog := daemon.LogFilePath(dbPath)
	if err := os.Remove(daemonLog); err == nil {
		fmt.Printf("Removed daemon log: %s\n", daemonLog)
	}

	// 6. Clear cached media attachments and clipboard scratch files
	fmt.Println("Clearing media cache...")
	if err := media.ClearMediaCache(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error clearing media cache: %v\n", err)
	}
	fmt.Println("Clearing clipboard cache...")
	if err := ui.ClearClipboardCache(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: error clearing clipboard cache: %v\n", err)
	}

	// 7. Remove database directory if empty (only if not current dir, root, or config dir)
	if dbDir != "" && dbDir != "." && dbDir != "/" && filepath.Clean(dbDir) != filepath.Clean(config.GetExeDir()) {
		if appCfg == nil || appCfg.SourcePath == "" || filepath.Clean(dbDir) != filepath.Clean(filepath.Dir(appCfg.SourcePath)) {
			if entries, err := os.ReadDir(dbDir); err == nil && len(entries) == 0 {
				_ = os.Remove(dbDir)
			}
		}
	}

	// 8. Confirmation output
	fmt.Println("Logout complete. All database records, sessions, and cached data have been cleaned up.")
	if appCfg != nil && appCfg.SourcePath != "" {
		fmt.Printf("Configuration file preserved: %s\n", appCfg.SourcePath)
	}

	return nil
}
