package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	v := strings.TrimSpace(Version)
	if v == "" {
		t.Fatalf("Version string should not be empty")
	}
	trimmed := strings.TrimPrefix(v, "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		t.Errorf("Expected semantic version format, got %q", Version)
	}
}
