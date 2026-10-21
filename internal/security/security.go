package security

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mutecomm/go-sqlcipher/v4"
	"golang.org/x/crypto/hkdf"
)

const (
	// SQLiteHeaderPrefix is the 15-byte magic string of unencrypted SQLite databases.
	SQLiteHeaderPrefix = "SQLite format 3"
	// KeyFileName is the local salt/key stored with 0600 permissions.
	KeyFileName = ".key"
	// MachineIDFileName is the local persistent machine ID token stored with 0600 permissions.
	MachineIDFileName = ".machine-id"
	// HKDFInfoContext is domain-separated info for key derivation.
	HKDFInfoContext = "watui-database-v1"
)

// GetMachineID attempts to read the unique hardware/OS machine ID across platforms:
// - Linux: /etc/machine-id or /var/lib/dbus/machine-id
// - Windows: HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Cryptography\MachineGuid
// - macOS: IOPlatformUUID
// - Fallback: hostname
func GetMachineID() string {
	if id, err := getPlatformMachineID(); err == nil && len(strings.TrimSpace(id)) > 0 {
		return strings.TrimSpace(id)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "watui-host"
	}
	return "fallback-" + hostname
}

// GetUserIdentifier returns the system user identifier:
// - Linux / macOS: UID (e.g. "1000")
// - Windows: User SID (e.g. "S-1-5-21-...") or username
func GetUserIdentifier() string {
	if u, err := user.Current(); err == nil && u.Uid != "" {
		return u.Uid
	}
	return strconv.Itoa(os.Getuid())
}

// GetOrCreateLocalSalt returns or generates a 32-byte cryptographically secure salt in keyDir with 0600 permissions.
func GetOrCreateLocalSalt(keyDir string) ([]byte, error) {
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", keyDir, err)
	}

	keyFilePath := filepath.Join(keyDir, KeyFileName)
	if data, err := os.ReadFile(keyFilePath); err == nil && len(data) >= 32 {
		return data[:32], nil
	}

	// Check fallback locations for existing salt before generating a new one
	// (e.g. when app is invoked through a symlink or moved between directories)
	var fallbackDirs []string
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		fallbackDirs = append(fallbackDirs, filepath.Join(xdgData, "watui"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		fallbackDirs = append(fallbackDirs, filepath.Join(home, ".local", "share", "watui"))
	}
	if exe, err := os.Executable(); err == nil {
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			fallbackDirs = append(fallbackDirs, filepath.Dir(realExe))
		}
		fallbackDirs = append(fallbackDirs, filepath.Dir(exe))
	}

	for _, fallbackDir := range fallbackDirs {
		if fallbackDir == "" || fallbackDir == keyDir {
			continue
		}
		fbPath := filepath.Join(fallbackDir, KeyFileName)
		if data, err := os.ReadFile(fbPath); err == nil && len(data) >= 32 {
			salt := data[:32]
			_ = os.WriteFile(keyFilePath, salt, 0600)
			return salt, nil
		}
	}

	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate random salt: %w", err)
	}

	if err := os.WriteFile(keyFilePath, salt, 0600); err != nil {
		return nil, fmt.Errorf("failed to write key file %s: %w", keyFilePath, err)
	}

	return salt, nil
}

// DeriveKeyWithMachineID derives a 32-byte AES key given salt, machineID, and userIdentifier.
func DeriveKeyWithMachineID(salt []byte, machineID, userIdentifier string) (string, error) {
	ikm := fmt.Appendf(nil, "%s:%s", machineID, userIdentifier)
	hkdfReader := hkdf.New(sha256.New, ikm, salt, []byte(HKDFInfoContext))
	keyBytes := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, keyBytes); err != nil {
		return "", fmt.Errorf("failed to derive database key: %w", err)
	}
	return hex.EncodeToString(keyBytes), nil
}

// VerifyDatabaseKey checks if hexKey can successfully open and read sqlite_master from dbPath.
func VerifyDatabaseKey(dbPath, hexKey string) bool {
	dsn := fmt.Sprintf("file:%s?_pragma_key=%s&mode=ro&_busy_timeout=3000", dbPath, hexKey)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()

	var count int
	err = db.QueryRow("SELECT count(*) FROM sqlite_master;").Scan(&count)
	return err == nil
}

func saveLocalMachineID(keyDir, machineID string) {
	if keyDir == "" || machineID == "" {
		return
	}
	_ = os.MkdirAll(keyDir, 0700)
	midFilePath := filepath.Join(keyDir, MachineIDFileName)
	_ = os.WriteFile(midFilePath, []byte(machineID), 0600)
}

func getMachineIDCandidates(keyDir, currentMID string) []string {
	seen := make(map[string]bool)
	var list []string
	add := func(id string) {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			list = append(list, trimmed)
		}
	}

	// 1. Current GetMachineID()
	add(currentMID)
	add(GetMachineID())

	// 2. Saved .machine-id file
	if data, err := os.ReadFile(filepath.Join(keyDir, MachineIDFileName)); err == nil {
		add(string(data))
	}

	// 3. Platform machine IDs from common files on Linux
	linuxPaths := []string{
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
		"/run/machine-id",
		"/run/systemd/machine-id",
		"/sys/class/dmi/id/product_uuid",
		"/sys/devices/virtual/dmi/id/product_uuid",
	}
	for _, p := range linuxPaths {
		if data, err := os.ReadFile(p); err == nil {
			val := strings.TrimSpace(string(data))
			if val != "uninitialized" && val != "00000000-0000-0000-0000-000000000000" {
				add(val)
				add(strings.ToLower(val))
			}
		}
	}

	// 4. Hostname variations
	if h, err := os.Hostname(); err == nil && h != "" {
		add("fallback-" + h)
		add(h)
		if idx := strings.Index(h, "."); idx > 0 {
			short := h[:idx]
			add("fallback-" + short)
			add(short)
		}
	}
	add("fallback-watui-host")
	add("watui-host")
	add("fallback-localhost")
	add("localhost")

	return list
}

// ResolveDatabaseKey computes or recovers the 32-byte AES key for the database.
// If dbPath exists, it verifies the key against the database, automatically attempting
// candidate recovery if machine-id or hostname changed across reboots or network changes.
func ResolveDatabaseKey(dbPath, keyDir string) (string, error) {
	// 1. Explicit direct database hex key override
	if envKey := strings.TrimSpace(os.Getenv("WATUI_DB_KEY")); len(envKey) == 64 {
		if _, err := hex.DecodeString(envKey); err == nil {
			return envKey, nil
		}
	}

	salt, err := GetOrCreateLocalSalt(keyDir)
	if err != nil {
		return "", err
	}

	userIdentifier := GetUserIdentifier()

	// Determine base machine ID
	var machineID string
	if envMID := strings.TrimSpace(os.Getenv("WATUI_MACHINE_ID")); envMID != "" {
		machineID = envMID
	} else {
		// Check for locally persisted machine ID first
		midFilePath := filepath.Join(keyDir, MachineIDFileName)
		if data, err := os.ReadFile(midFilePath); err == nil {
			saved := strings.TrimSpace(string(data))
			if len(saved) > 0 {
				machineID = saved
			}
		}
		if machineID == "" {
			machineID = GetMachineID()
		}
	}

	primaryKey, err := DeriveKeyWithMachineID(salt, machineID, userIdentifier)
	if err != nil {
		return "", err
	}

	// If no dbPath provided or file doesn't exist yet, save machineID and return primaryKey
	if dbPath == "" {
		saveLocalMachineID(keyDir, machineID)
		return primaryKey, nil
	}

	fi, err := os.Stat(dbPath)
	if err != nil || fi.IsDir() || fi.Size() == 0 || IsPlaintextSQLite(dbPath) {
		// New or plaintext database
		saveLocalMachineID(keyDir, machineID)
		return primaryKey, nil
	}

	// Verify primary key against the existing database
	if VerifyDatabaseKey(dbPath, primaryKey) {
		saveLocalMachineID(keyDir, machineID)
		return primaryKey, nil
	}

	// Primary key failed! Generate candidate machine IDs to recover the database.
	candidates := getMachineIDCandidates(keyDir, machineID)
	for _, candMID := range candidates {
		// Test with userIdentifier
		if candKey, err := DeriveKeyWithMachineID(salt, candMID, userIdentifier); err == nil {
			if VerifyDatabaseKey(dbPath, candKey) {
				saveLocalMachineID(keyDir, candMID)
				return candKey, nil
			}
		}
		// Test with root/0 (in case previously run with sudo)
		if userIdentifier != "0" {
			if candKey, err := DeriveKeyWithMachineID(salt, candMID, "0"); err == nil {
				if VerifyDatabaseKey(dbPath, candKey) {
					saveLocalMachineID(keyDir, candMID)
					return candKey, nil
				}
			}
		}
		// Test without userIdentifier
		if candKey, err := DeriveKeyWithMachineID(salt, candMID, ""); err == nil {
			if VerifyDatabaseKey(dbPath, candKey) {
				saveLocalMachineID(keyDir, candMID)
				return candKey, nil
			}
		}
	}

	// Fallback to primaryKey so caller receives standard SQLCipher error if none matched
	return primaryKey, nil
}

// DeriveDatabaseKey computes a 32-byte machine-bound AES key using HKDF-SHA256.
// It combines the host machine-id, user identifier, and the local secret salt.
// Returns a 64-character hex string suitable for SQLCipher.
func DeriveDatabaseKey(keyDir string) (string, error) {
	return ResolveDatabaseKey("", keyDir)
}

// IsPlaintextSQLite checks if a database file exists and begins with the unencrypted SQLite header.
func IsPlaintextSQLite(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 16)
	n, err := f.Read(buf)
	if err != nil || n < 15 {
		return false
	}

	return string(buf[:15]) == SQLiteHeaderPrefix
}

// EnsureSecurePermissions enforces 0700 on the parent folder and 0600 on the database file.
func EnsureSecurePermissions(dbPath string) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		_ = os.Chmod(dir, 0700)
	}
	if info, err := os.Stat(dbPath); err == nil && !info.IsDir() {
		_ = os.Chmod(dbPath, 0600)
	}
	// Also ensure any WAL and SHM files have 0600 permissions
	if info, err := os.Stat(dbPath + "-wal"); err == nil && !info.IsDir() {
		_ = os.Chmod(dbPath+"-wal", 0600)
	}
	if info, err := os.Stat(dbPath + "-shm"); err == nil && !info.IsDir() {
		_ = os.Chmod(dbPath+"-shm", 0600)
	}
}

// MigratePlaintextDatabase transparently migrates an unencrypted SQLite database to an encrypted SQLCipher database.
// The original unencrypted file is preserved as a backup with 0600 permissions.
func MigratePlaintextDatabase(dbPath, hexKey string) error {
	if !IsPlaintextSQLite(dbPath) {
		return nil
	}

	backupPath := dbPath + ".unencrypted.bak"
	if err := os.Rename(dbPath, backupPath); err != nil {
		return fmt.Errorf("failed to rename plaintext database for migration: %w", err)
	}
	_ = os.Chmod(backupPath, 0600)

	srcDB, err := sql.Open("sqlite3", backupPath)
	if err != nil {
		_ = os.Rename(backupPath, dbPath)
		return fmt.Errorf("failed to open plaintext database: %w", err)
	}
	defer func() { _ = srcDB.Close() }()

	attachQuery := fmt.Sprintf("ATTACH DATABASE '%s' AS encrypted KEY '%s';", dbPath, hexKey)
	if _, err := srcDB.Exec(attachQuery); err != nil {
		_ = os.Rename(backupPath, dbPath)
		return fmt.Errorf("failed to attach encrypted target database: %w", err)
	}

	if _, err := srcDB.Exec("SELECT sqlcipher_export('encrypted');"); err != nil {
		_ = os.Rename(backupPath, dbPath)
		return fmt.Errorf("failed to export data to encrypted database: %w", err)
	}

	if _, err := srcDB.Exec("DETACH DATABASE encrypted;"); err != nil {
		_ = os.Rename(backupPath, dbPath)
		return fmt.Errorf("failed to detach encrypted database: %w", err)
	}

	EnsureSecurePermissions(dbPath)
	return nil
}

// BuildEncryptedDSN formats an SQLite connection string with SQLCipher key and WAL/foreign key options.
func BuildEncryptedDSN(dbPath, hexKey string) string {
	return fmt.Sprintf("file:%s?_pragma_key=%s&_foreign_keys=on&_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_txlock=immediate", dbPath, hexKey)
}
