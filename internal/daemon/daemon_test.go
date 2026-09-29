package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonPIDLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "watui.pid")

	// Initially not running
	running, pid := IsRunning(pidFile)
	if running || pid != 0 {
		t.Fatalf("expected running=false, pid=0; got running=%v, pid=%d", running, pid)
	}

	// Write self PID
	myPID := os.Getpid()
	if err := WritePID(pidFile, myPID); err != nil {
		t.Fatalf("failed to write PID: %v", err)
	}

	// Verify reading PID
	readPid, err := ReadPID(pidFile)
	if err != nil || readPid != myPID {
		t.Fatalf("expected PID=%d; got %d (err: %v)", myPID, readPid, err)
	}

	// Should be recognized as running
	running, pid = IsRunning(pidFile)
	if !running || pid != myPID {
		t.Fatalf("expected running=true, pid=%d; got running=%v, pid=%d", myPID, running, pid)
	}

	// Write an invalid/stale PID (non-existent process PID)
	// PID 999999 is almost certainly non-existent
	stalePID := 99999999
	if err := WritePID(pidFile, stalePID); err != nil {
		t.Fatalf("failed to write stale PID: %v", err)
	}

	// IsRunning should detect it's dead, clean up the PID file, and return false
	running, pid = IsRunning(pidFile)
	if running || pid != 0 {
		t.Fatalf("expected stale PID to return running=false, pid=0; got %v, %d", running, pid)
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed")
	}
}

func TestPIDFilePath(t *testing.T) {
	p := PIDFilePath("/tmp/watui/watui.db")
	if p != "/tmp/watui/watui.pid" {
		t.Fatalf("unexpected pid path: %s", p)
	}

	pDefault := PIDFilePath("")
	if filepath.Base(pDefault) != "watui.pid" {
		t.Fatalf("unexpected default pid path: %s", pDefault)
	}
}
