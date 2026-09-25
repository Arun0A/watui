package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoadYAML(t *testing.T) {
	yamlContent := `
mute:
  - "Crypto Announcements"
  - "120363311130744191@g.us"
  - "+1-800-555-0199"

pin:
  - "Alice Smith"
  - "Weekend Hikers"
  - "919876543210@s.whatsapp.net"
`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.yaml")
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.GetMuted()) != 3 {
		t.Errorf("Expected 3 muted items, got %d", len(cfg.GetMuted()))
	}
	if len(cfg.GetPinned()) != 3 {
		t.Errorf("Expected 3 pinned items, got %d", len(cfg.GetPinned()))
	}

	// Test Mute matching with JID (both with and without @g.us)
	if !cfg.IsMuted("120363311130744191@g.us", "Some Group") {
		t.Errorf("Expected JID with server to be muted")
	}
	if !cfg.IsMuted("120363311130744191", "Some Group") {
		t.Errorf("Expected JID without server to match rule with server")
	}
	if !cfg.IsMuted("random@s.whatsapp.net", "VIP Crypto Announcements Official") {
		t.Errorf("Expected chat name substring match to be muted")
	}
	if !cfg.IsMuted("18005550199@s.whatsapp.net", "Spam Caller") {
		t.Errorf("Expected phone number to be muted")
	}
	if cfg.IsMuted("999@s.whatsapp.net", "Family Member") {
		t.Errorf("Unexpected mute on regular contact")
	}

	// Test Pin matching with JID
	if !cfg.IsPinned("111@s.whatsapp.net", "Alice Smith") {
		t.Errorf("Expected Alice Smith to be pinned")
	}
	if !cfg.IsPinned("1203639999@g.us", "Weekend Hikers") {
		t.Errorf("Expected Weekend Hikers to be pinned")
	}
	if !cfg.IsPinned("919876543210@s.whatsapp.net") {
		t.Errorf("Expected JID pin to match exact JID")
	}
	if !cfg.IsPinned("919876543210:2@s.whatsapp.net") {
		t.Errorf("Expected JID pin to match device JID")
	}
	if !cfg.IsPinned("+91 98765 43210") {
		t.Errorf("Expected JID pin to match formatted phone number")
	}
	if cfg.IsPinned("222@s.whatsapp.net", "Bob Jones") {
		t.Errorf("Unexpected pin on Bob Jones")
	}
}

func TestConfigLoadJSON(t *testing.T) {
	jsonContent := `{
		"muted": ["Spam Bot", "9998887777"],
		"pinned": ["Work Group"]
	}`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.json")
	if err := os.WriteFile(cfgFile, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("Failed to write temp json config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load json config: %v", err)
	}

	if !cfg.IsMuted("9998887777@s.whatsapp.net") {
		t.Errorf("Expected phone number to be muted from JSON")
	}
	if !cfg.IsPinned("some_jid@g.us", "Work Group") {
		t.Errorf("Expected Work Group to be pinned from JSON")
	}
}

func TestConfigLoadMissingDefault(t *testing.T) {
	// Loading with empty string when no default file exists should return empty config without error
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Expected nil error for empty default, got: %v", err)
	}
	if cfg == nil {
		t.Fatalf("Expected non-nil config")
	}
	if len(cfg.GetMuted()) != 0 || len(cfg.GetPinned()) != 0 {
		t.Errorf("Expected empty rules")
	}
}
