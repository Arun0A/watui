package security

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestGetMachineID(t *testing.T) {
	id := GetMachineID()
	if id == "" {
		t.Fatalf("expected non-empty machine ID")
	}
}

func TestDeriveDatabaseKey(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "watui-sec-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	key1, err := DeriveDatabaseKey(tmpDir)
	if err != nil {
		t.Fatalf("failed to derive key: %v", err)
	}
	if len(key1) != 64 {
		t.Fatalf("expected 64 hex characters, got %d", len(key1))
	}

	// Should be deterministic with the same key file
	key2, err := DeriveDatabaseKey(tmpDir)
	if err != nil {
		t.Fatalf("failed to derive key second time: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("expected deterministic key, got %s != %s", key1, key2)
	}
}

func TestEncryptedDatabaseAndMigration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "watui-sec-db-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	key, err := DeriveDatabaseKey(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	plainDBPath := filepath.Join(tmpDir, "plain.db")
	plainDB, err := sql.Open("sqlite3", plainDBPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plainDB.Exec("CREATE TABLE users (id INT, name TEXT); INSERT INTO users VALUES (1, 'Alice');")
	if err != nil {
		t.Fatal(err)
	}
	_ = plainDB.Close()

	if !IsPlaintextSQLite(plainDBPath) {
		t.Fatalf("expected plain.db to be recognized as plaintext SQLite")
	}

	// Run migration to encrypted
	if err := MigratePlaintextDatabase(plainDBPath, key); err != nil {
		t.Fatalf("failed to migrate database: %v", err)
	}

	// Verify plainDBPath is no longer plaintext
	if IsPlaintextSQLite(plainDBPath) {
		t.Fatalf("expected plain.db to NOT be plaintext after migration")
	}

	// Open with encrypted DSN
	dsn := BuildEncryptedDSN(plainDBPath, key)
	encDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("failed to open encrypted database: %v", err)
	}
	defer func() { _ = encDB.Close() }()

	var name string
	err = encDB.QueryRow("SELECT name FROM users WHERE id = 1;").Scan(&name)
	if err != nil {
		t.Fatalf("failed to query migrated encrypted database: %v", err)
	}
	if name != "Alice" {
		t.Fatalf("expected 'Alice', got '%s'", name)
	}
}
