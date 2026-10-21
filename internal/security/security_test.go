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

func TestSaltFallbackLocation(t *testing.T) {
	origDir := t.TempDir()
	origSalt, err := GetOrCreateLocalSalt(origDir)
	if err != nil {
		t.Fatalf("failed to create salt in origDir: %v", err)
	}

	// Set XDG_DATA_HOME to point to origDir's parent so fallback finds it
	origXDG := os.Getenv("XDG_DATA_HOME")
	defer func() { _ = os.Setenv("XDG_DATA_HOME", origXDG) }()
	_ = os.Setenv("XDG_DATA_HOME", origDir)

	// Create a new folder (e.g. symlink or moved folder) that has .watui_key inside XDG_DATA_HOME/watui
	targetXDGDir := filepath.Join(origDir, "watui")
	_ = os.MkdirAll(targetXDGDir, 0700)
	_ = os.WriteFile(filepath.Join(targetXDGDir, KeyFileName), origSalt, 0600)

	newDir := t.TempDir()
	retrievedSalt, err := GetOrCreateLocalSalt(newDir)
	if err != nil {
		t.Fatalf("failed to get salt in newDir: %v", err)
	}

	if string(retrievedSalt) != string(origSalt) {
		t.Fatalf("expected salt to be reused from fallback location")
	}

	// Verify it was copied into newDir
	copied, err := os.ReadFile(filepath.Join(newDir, KeyFileName))
	if err != nil || string(copied) != string(origSalt) {
		t.Fatalf("expected salt to be persisted locally in newDir")
	}
}

func TestResolveDatabaseKeyPersistentMachineID(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Initial key derivation should save .machine-id
	key1, err := ResolveDatabaseKey("", tmpDir)
	if err != nil {
		t.Fatalf("failed to derive initial key: %v", err)
	}

	midPath := filepath.Join(tmpDir, MachineIDFileName)
	savedMID, err := os.ReadFile(midPath)
	if err != nil || len(savedMID) == 0 {
		t.Fatalf("expected %s to be created and non-empty", MachineIDFileName)
	}

	// 2. Simulating subsequent runs: the saved .machine-id ensures stable key derivation
	key2, err := ResolveDatabaseKey("", tmpDir)
	if err != nil {
		t.Fatalf("failed to derive key second time: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("expected key to remain identical across runs, got %s != %s", key1, key2)
	}
}

func TestResolveDatabaseKeyAutoRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "encrypted.db")

	// 1. Create an encrypted database using a known candidate ID
	salt, err := GetOrCreateLocalSalt(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	candMID := "fallback-watui-host"
	keyCand, err := DeriveKeyWithMachineID(salt, candMID, GetUserIdentifier())
	if err != nil {
		t.Fatal(err)
	}

	dsn := BuildEncryptedDSN(dbPath, keyCand)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE test_recovery (id INT, val TEXT); INSERT INTO test_recovery VALUES (42, 'saved');")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// 2. Point .machine-id to an invalid/stale ID to simulate a reboot with a changed/mismatched ID
	_ = os.WriteFile(filepath.Join(tmpDir, MachineIDFileName), []byte("stale-mismatched-id"), 0600)

	// ResolveDatabaseKey should automatically test candidate IDs, discover keyCand,
	// recover the database, and update .machine-id to the working ID!
	recoveredKey, err := ResolveDatabaseKey(dbPath, tmpDir)
	if err != nil {
		t.Fatalf("ResolveDatabaseKey failed: %v", err)
	}
	if recoveredKey != keyCand {
		t.Fatalf("expected auto-recovered key %s, got %s", keyCand, recoveredKey)
	}

	// Verify .machine-id was updated to candMID
	newMID, err := os.ReadFile(filepath.Join(tmpDir, MachineIDFileName))
	if err != nil || string(newMID) != candMID {
		t.Fatalf("expected .machine-id to be updated to %s, got %s", candMID, string(newMID))
	}
}

func TestResolveDatabaseKeyEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()

	customMID := "my-custom-persistent-machine-id-1234"
	_ = os.Setenv("WATUI_MACHINE_ID", customMID)
	defer func() { _ = os.Unsetenv("WATUI_MACHINE_ID") }()

	key, err := ResolveDatabaseKey("", tmpDir)
	if err != nil {
		t.Fatalf("failed to derive key with custom machine ID: %v", err)
	}

	salt, _ := GetOrCreateLocalSalt(tmpDir)
	expectedKey, _ := DeriveKeyWithMachineID(salt, customMID, GetUserIdentifier())
	if key != expectedKey {
		t.Fatalf("expected key %s matching WATUI_MACHINE_ID, got %s", expectedKey, key)
	}
}
