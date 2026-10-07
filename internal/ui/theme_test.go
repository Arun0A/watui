package ui

import (
	"os"
	"path/filepath"
	"testing"

	"watui/internal/config"
)

func TestDefaultTheme(t *testing.T) {
	theme := DefaultTheme()
	if theme.Title != "#A6E3A1" {
		t.Errorf("Expected Title #A6E3A1, got %q", theme.Title)
	}
	if theme.ChatSelected != "#89B4FA" {
		t.Errorf("Expected ChatSelected #89B4FA, got %q", theme.ChatSelected)
	}
	if theme.BadgeUnreadBG != "#A6E3A1" {
		t.Errorf("Expected BadgeUnreadBG #A6E3A1, got %q", theme.BadgeUnreadBG)
	}
	if theme.BadgeUnreadFG != "#11111B" {
		t.Errorf("Expected BadgeUnreadFG #11111B, got %q", theme.BadgeUnreadFG)
	}
	if theme.Cursor != "#FAB387" {
		t.Errorf("Expected Cursor #FAB387, got %q", theme.Cursor)
	}

	styles := MakeStyles(theme)
	if styles.Title.GetForeground() == nil {
		t.Errorf("Expected Title style to have foreground color")
	}
}

func TestLoadTheme_MissingFile(t *testing.T) {
	// With an empty or non-existent path, should cleanly return default theme without error
	cfg := &config.Config{
		Theme: "/non/existent/theme.yaml",
	}
	theme, path := LoadTheme(cfg)
	if path != "" {
		t.Errorf("Expected empty path for non-existent file, got %q", path)
	}
	if theme.Title != "#A6E3A1" {
		t.Errorf("Expected default Title #A6E3A1, got %q", theme.Title)
	}
}

func TestLoadTheme_PartialYamlOverride(t *testing.T) {
	tmpDir := t.TempDir()
	themeFile := filepath.Join(tmpDir, "theme.yaml")
	content := `
title: "#FF0000"
chat_selected: "#00FF00"
cursor: "yellow"
`
	if err := os.WriteFile(themeFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test theme.yaml: %v", err)
	}

	cfg := &config.Config{
		Theme: themeFile,
	}
	theme, resolvedPath := LoadTheme(cfg)
	if resolvedPath != themeFile {
		t.Errorf("Expected resolved path %q, got %q", themeFile, resolvedPath)
	}

	// Overridden keys
	if theme.Title != "#FF0000" {
		t.Errorf("Expected overridden Title #FF0000, got %q", theme.Title)
	}
	if theme.ChatSelected != "#00FF00" {
		t.Errorf("Expected overridden ChatSelected #00FF00, got %q", theme.ChatSelected)
	}
	if theme.Cursor != "yellow" {
		t.Errorf("Expected overridden Cursor yellow, got %q", theme.Cursor)
	}

	// Omitted keys must retain their exact defaults
	if theme.Divider != "#313244" {
		t.Errorf("Expected default Divider #313244, got %q", theme.Divider)
	}
	if theme.BadgeUnreadBG != "#A6E3A1" {
		t.Errorf("Expected default BadgeUnreadBG #A6E3A1, got %q", theme.BadgeUnreadBG)
	}
	if theme.Select != "#A6E3A1" {
		t.Errorf("Expected default Select #A6E3A1, got %q", theme.Select)
	}
	if theme.Notice != "#89DCEB" {
		t.Errorf("Expected default Notice #89DCEB, got %q", theme.Notice)
	}
}

func TestLoadTheme_NextToActiveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.yaml")
	themeFile := filepath.Join(tmpDir, "theme.yaml")

	if err := os.WriteFile(cfgFile, []byte(""), 0644); err != nil {
		t.Fatalf("Failed to write test watui.yaml: %v", err)
	}
	if err := os.WriteFile(themeFile, []byte("title: \"#123456\"\n"), 0644); err != nil {
		t.Fatalf("Failed to write test theme.yaml: %v", err)
	}

	cfg := &config.Config{
		SourcePath: cfgFile,
	}

	theme, resolvedPath := LoadTheme(cfg)
	if resolvedPath != themeFile {
		t.Errorf("Expected auto-discovered theme path %q, got %q", themeFile, resolvedPath)
	}
	if theme.Title != "#123456" {
		t.Errorf("Expected Title #123456, got %q", theme.Title)
	}
}

func TestLoadTheme_JsonFormat(t *testing.T) {
	tmpDir := t.TempDir()
	themeFile := filepath.Join(tmpDir, "theme.json")
	content := `{"title": "#AABBCC", "error": "#CCBBAA"}`
	if err := os.WriteFile(themeFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test theme.json: %v", err)
	}

	cfg := &config.Config{
		Theme: themeFile,
	}
	theme, _ := LoadTheme(cfg)
	if theme.Title != "#AABBCC" {
		t.Errorf("Expected Title #AABBCC, got %q", theme.Title)
	}
	if theme.Error != "#CCBBAA" {
		t.Errorf("Expected Error #CCBBAA, got %q", theme.Error)
	}
	if theme.Status != "#6C7086" {
		t.Errorf("Expected default Status #6C7086, got %q", theme.Status)
	}
}
