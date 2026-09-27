package security

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mutecomm/go-sqlcipher/v4"
	"golang.org/x/crypto/hkdf"
)

const (
	// SQLiteHeaderPrefix is the 15-byte magic string of unencrypted SQLite databases.
	SQLiteHeaderPrefix = "SQLite format 3"
	// KeyFileName is the local salt/key stored with 0600 permissions.
	KeyFileName = ".key"
	// HKDFInfoContext is domain-separated info for key derivation.
	HKDFInfoContext = "watui-database-v1"
)

// GetMachineID attempts to read the machine ID from Linux systemd or D-Bus locations.
// Falls back to hostname + architecture if unavailable.
func GetMachineID() string {
	paths := []string{
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if len(trimmed) > 0 {
				return trimmed
			}
		}
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "watui-host"
	}
	return "fallback-" + hostname
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

	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate random salt: %w", err)
	}

	if err := os.WriteFile(keyFilePath, salt, 0600); err != nil {
		return nil, fmt.Errorf("failed to write key file %s: %w", keyFilePath, err)
	}

	return salt, nil
}

// DeriveDatabaseKey computes a 32-byte machine-bound AES key using HKDF-SHA256.
// It combines the host machine-id, user UID, and the local secret salt.
// Returns a 64-character hex string suitable for SQLCipher.
func DeriveDatabaseKey(keyDir string) (string, error) {
	salt, err := GetOrCreateLocalSalt(keyDir)
	if err != nil {
		return "", err
	}

	machineID := GetMachineID()
	uid := os.Getuid()
	ikm := []byte(fmt.Sprintf("%s:%d", machineID, uid))

	hkdfReader := hkdf.New(sha256.New, ikm, salt, []byte(HKDFInfoContext))
	keyBytes := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, keyBytes); err != nil {
		return "", fmt.Errorf("failed to derive database key: %w", err)
	}

	return hex.EncodeToString(keyBytes), nil
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
	return fmt.Sprintf("file:%s?_pragma_key=%s&_foreign_keys=on", dbPath, hexKey)
}
