package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// PreviewConfig specifies commands used to preview media attachments on demand.
type PreviewConfig struct {
	Image    string `json:"image" yaml:"image"`
	Video    string `json:"video" yaml:"video"`
	Audio    string `json:"audio" yaml:"audio"`
	Sticker  string `json:"sticker" yaml:"sticker"`
	Document string `json:"document" yaml:"document"`
}

// Config represents declarative user configuration for watui.
type Config struct {
	// Preview defines commands to preview media attachments on demand.
	Preview PreviewConfig `json:"preview" yaml:"preview"`

	// FilePicker specifies an optional custom command to open a file picker.
	FilePicker string `json:"file_picker" yaml:"file_picker"`

	// DeviceName specifies the companion client name registered in WhatsApp Linked Devices (default: "WA-TUI").
	DeviceName string `json:"device_name" yaml:"device_name"`

	// DB / DBPath / DBDir / DBDirectory optionally specifies the SQLite database location.
	// Can be a directory (containing watui.db) or an explicit database file path.
	DB          string `json:"db" yaml:"db"`
	DBPath      string `json:"db_path" yaml:"db_path"`
	DBDir       string `json:"db_dir" yaml:"db_dir"`
	DBDirectory string `json:"db_directory" yaml:"db_directory"`

	// Config / ConfigFile / ConfigDir optionally specifies another config file or directory to load/include.
	Config     string `json:"config" yaml:"config"`
	ConfigFile string `json:"config_file" yaml:"config_file"`
	ConfigDir  string `json:"config_dir" yaml:"config_dir"`

	// Muted lists chat JIDs, phone numbers, or group/contact names to hide from unread.
	Muted []string `json:"muted" yaml:"muted"`

	// Pinned lists chat JIDs, phone numbers, or group/contact names to keep pinned at top.
	Pinned []string `json:"pinned" yaml:"pinned"`

	// Aliases for user convenience (mute / pin)
	Mute []string `json:"mute" yaml:"mute"`
	Pin  []string `json:"pin" yaml:"pin"`

	// SourcePath stores the path of the file this config was loaded from (if any).
	SourcePath string `json:"-" yaml:"-"`
}

// DefaultDBPath returns the appropriate default path for the database file.
// Priority:
// 1. ./watui.db in current working directory (if it already exists)
// 2. Windows default: <exeDir>/watui.db (portable by default)
// 3. watui.db next to the resolved executable on Unix (only if it already exists, e.g. portable setup)
// 4. Unix default: $XDG_DATA_HOME/watui/watui.db or ~/.local/share/watui/watui.db
func DefaultDBPath() string {
	// 1. Current working directory watui.db (if it already exists)
	if _, err := os.Stat("watui.db"); err == nil {
		return "watui.db"
	}

	exeDir := GetExeDir()

	// 2. Windows default: self-contained next to the executable
	if runtime.GOOS == "windows" {
		if exeDir != "" && exeDir != "." {
			return filepath.Join(exeDir, "watui.db")
		}
		return "watui.db"
	}

	// 3. Next to the resolved executable on Unix only if watui.db already exists there
	if exeDir != "" && exeDir != "." {
		candidate := filepath.Join(exeDir, "watui.db")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 4. macOS: check if existing database is in Application Support
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			macSupport := filepath.Join(home, "Library", "Application Support", "watui", "watui.db")
			if _, err := os.Stat(macSupport); err == nil {
				return macSupport
			}
		}
	}

	// 5. Standard OS data directory on Unix (Linux & macOS)
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "watui", "watui.db")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "watui", "watui.db")
	}
	return "watui.db"
}

// DefaultOpenCommand returns the OS-native command to open files with their default application:
// - Windows: "rundll32 url.dll,FileProtocolHandler"
// - Darwin (macOS): "open"
// - Linux / others: "xdg-open"
func DefaultOpenCommand() string {
	switch runtime.GOOS {
	case "windows":
		return "rundll32 url.dll,FileProtocolHandler"
	case "darwin":
		return "open"
	default:
		return "xdg-open"
	}
}

// GetPreviewCommand returns the command string for previewing the given message type.
// If not customized in configuration, it defaults to the OS-native default opener.
func (c *Config) GetPreviewCommand(msgType string) string {
	if c != nil {
		switch strings.ToLower(msgType) {
		case "image":
			if c.Preview.Image != "" {
				return c.Preview.Image
			}
		case "video":
			if c.Preview.Video != "" {
				return c.Preview.Video
			}
		case "audio":
			if c.Preview.Audio != "" {
				return c.Preview.Audio
			}
		case "sticker":
			if c.Preview.Sticker != "" {
				return c.Preview.Sticker
			}
		case "document":
			if c.Preview.Document != "" {
				return c.Preview.Document
			}
		}
	}
	return DefaultOpenCommand()
}

// GetFilePickerCommand returns any user-configured file picker command, or empty string.
func (c *Config) GetFilePickerCommand() string {
	if c != nil {
		return strings.TrimSpace(c.FilePicker)
	}
	return ""
}

// GetDeviceName returns the configured companion device name, or "WA-TUI" by default.
func (c *Config) GetDeviceName() string {
	if c != nil && strings.TrimSpace(c.DeviceName) != "" {
		return strings.TrimSpace(c.DeviceName)
	}
	return "WA-TUI"
}

// ExpandHome replaces a leading ~/ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// ResolveDBPath normalizes and expands a database path or directory into a full database file path.
func ResolveDBPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	expanded := ExpandHome(trimmed)
	if strings.HasSuffix(trimmed, "/") || strings.HasSuffix(trimmed, "\\") {
		return filepath.Join(expanded, "watui.db")
	}
	if fi, err := os.Stat(expanded); err == nil && fi.IsDir() {
		return filepath.Join(expanded, "watui.db")
	}
	return filepath.Clean(expanded)
}

// GetDBPath returns the configured database path if specified via db, db_path, db_dir, or db_directory.
// If a directory was provided, it joins the directory with "watui.db".
// If none was specified, it returns an empty string.
func (c *Config) GetDBPath() string {
	if c == nil {
		return ""
	}

	// 1. Explicit directory options
	dir := strings.TrimSpace(c.DBDir)
	if dir == "" {
		dir = strings.TrimSpace(c.DBDirectory)
	}
	if dir != "" {
		return filepath.Join(ExpandHome(dir), "watui.db")
	}

	// 2. Path or directory options
	raw := strings.TrimSpace(c.DB)
	if raw == "" {
		raw = strings.TrimSpace(c.DBPath)
	}
	if raw == "" {
		return ""
	}

	expanded := ExpandHome(raw)
	if strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, "\\") {
		return filepath.Join(expanded, "watui.db")
	}
	if fi, err := os.Stat(expanded); err == nil && fi.IsDir() {
		return filepath.Join(expanded, "watui.db")
	}

	return filepath.Clean(expanded)
}

// GetConfigFile returns any specified included/target config file or directory, expanded.
func (c *Config) GetConfigFile() string {
	if c == nil {
		return ""
	}
	file := strings.TrimSpace(c.ConfigFile)
	if file == "" {
		file = strings.TrimSpace(c.Config)
	}
	if file != "" {
		return ExpandHome(file)
	}

	dir := strings.TrimSpace(c.ConfigDir)
	if dir != "" {
		expandedDir := ExpandHome(dir)
		for _, name := range []string{"watui.yaml", "watui.yml", "watui.json", "config.yaml", "config.yml", "config.json"} {
			candidate := filepath.Join(expandedDir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		return filepath.Join(expandedDir, "watui.yaml")
	}

	return ""
}

// Load loads configuration from an explicit path or checks default paths.
// If explicitPath is empty, it tries default paths:
// 1. ./watui.yaml, ./watui.yml, ./watui.json
// 2. ~/.config/watui/config.yaml, ~/.config/watui/config.yml, ~/.config/watui/config.json
// If no default config is found, returns an empty Config without error.
func Load(explicitPath string) (*Config, error) {
	if explicitPath != "" {
		return loadFile(explicitPath)
	}

	for _, p := range defaultCandidatePaths() {
		if _, err := os.Stat(p); err == nil {
			return loadFile(p)
		}
	}

	return &Config{}, nil
}

func loadFile(path string) (*Config, error) {
	return loadFileWithDepth(path, 0)
}

func loadFileWithDepth(path string, depth int) (*Config, error) {
	if depth > 5 {
		return nil, fmt.Errorf("config include cycle detected at %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
	}

	var cfg Config
	// yaml.v3 handles both YAML and JSON syntax natively
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %q: %w", path, err)
	}

	cfg.SourcePath = path

	includedPath := cfg.GetConfigFile()
	if includedPath != "" && filepath.Clean(includedPath) != filepath.Clean(path) {
		baseCfg, err := loadFileWithDepth(includedPath, depth+1)
		if err == nil && baseCfg != nil {
			mergeConfig(baseCfg, &cfg)
			baseCfg.SourcePath = path
			return baseCfg, nil
		}
	}

	return &cfg, nil
}

func mergeConfig(base, overlay *Config) {
	if overlay.Preview.Image != "" {
		base.Preview.Image = overlay.Preview.Image
	}
	if overlay.Preview.Video != "" {
		base.Preview.Video = overlay.Preview.Video
	}
	if overlay.Preview.Audio != "" {
		base.Preview.Audio = overlay.Preview.Audio
	}
	if overlay.Preview.Sticker != "" {
		base.Preview.Sticker = overlay.Preview.Sticker
	}
	if overlay.Preview.Document != "" {
		base.Preview.Document = overlay.Preview.Document
	}
	if overlay.FilePicker != "" {
		base.FilePicker = overlay.FilePicker
	}
	if overlay.DeviceName != "" {
		base.DeviceName = overlay.DeviceName
	}
	if overlay.DB != "" {
		base.DB = overlay.DB
	}
	if overlay.DBPath != "" {
		base.DBPath = overlay.DBPath
	}
	if overlay.DBDir != "" {
		base.DBDir = overlay.DBDir
	}
	if overlay.DBDirectory != "" {
		base.DBDirectory = overlay.DBDirectory
	}
	if len(overlay.Pinned) > 0 {
		base.Pinned = overlay.Pinned
	}
	if len(overlay.Pin) > 0 {
		base.Pin = overlay.Pin
	}
	if len(overlay.Muted) > 0 {
		base.Muted = overlay.Muted
	}
	if len(overlay.Mute) > 0 {
		base.Mute = overlay.Mute
	}
}

func defaultCandidatePaths() []string {
	paths := []string{
		"watui.yaml",
		"watui.yml",
		"watui.json",
	}

	if exeDir := GetExeDir(); exeDir != "" && exeDir != "." {
		paths = append(paths,
			filepath.Join(exeDir, "watui.yaml"),
			filepath.Join(exeDir, "watui.yml"),
			filepath.Join(exeDir, "watui.json"),
		)
	}

	if runtime.GOOS != "windows" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			xdgConfig := os.Getenv("XDG_CONFIG_HOME")
			if xdgConfig == "" {
				xdgConfig = filepath.Join(home, ".config")
			}
			paths = append(paths,
				filepath.Join(xdgConfig, "watui", "config.yaml"),
				filepath.Join(xdgConfig, "watui", "config.yml"),
				filepath.Join(xdgConfig, "watui", "config.json"),
			)
		}
	}

	return paths
}

// GetExeDir returns the directory containing the running binary, resolving any symlinks.
func GetExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	realExe, err := filepath.EvalSymlinks(exe)
	if err == nil {
		return filepath.Dir(realExe)
	}
	return filepath.Dir(exe)
}

// GetMuted returns all unique, trimmed mute rules.
func (c *Config) GetMuted() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]bool)
	var list []string
	for _, item := range append(append([]string(nil), c.Muted...), c.Mute...) {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			list = append(list, trimmed)
		}
	}
	return list
}

// GetPinned returns all unique, trimmed pin rules.
func (c *Config) GetPinned() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]bool)
	var list []string
	for _, item := range append(append([]string(nil), c.Pinned...), c.Pin...) {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			list = append(list, trimmed)
		}
	}
	return list
}

// IsMuted returns true if the chat matches any configured mute rule.
func (c *Config) IsMuted(chatID string, chatNames ...string) bool {
	if c == nil {
		return false
	}
	for _, rule := range c.GetMuted() {
		if MatchTarget(rule, chatID, chatNames...) {
			return true
		}
	}
	return false
}

// IsPinned returns true if the chat matches any configured pin rule.
func (c *Config) IsPinned(chatID string, chatNames ...string) bool {
	if c == nil {
		return false
	}
	for _, rule := range c.GetPinned() {
		if MatchTarget(rule, chatID, chatNames...) {
			return true
		}
	}
	return false
}

// MatchTarget checks if a chat (identified by chatID and possible names) matches a rule.
// A rule can be:
// - A phone number (e.g. "+1 (234) 567-8900", "12345678900")
// - A full JID or JID user (e.g. "120363311130744191@g.us", "120363311130744191")
// - A contact name or group name (case-insensitive exact or substring)
func normalizeJID(jid string) (user string, server string) {
	jid = strings.ToLower(strings.TrimSpace(jid))
	if idx := strings.Index(jid, ":"); idx != -1 {
		if atIdx := strings.Index(jid, "@"); atIdx != -1 && atIdx > idx {
			jid = jid[:idx] + jid[atIdx:]
		} else {
			jid = jid[:idx]
		}
	}
	parts := strings.SplitN(jid, "@", 2)
	user = parts[0]
	if len(parts) > 1 {
		server = parts[1]
	}
	return user, server
}

func isUserServer(server string) bool {
	return server == "s.whatsapp.net" || server == "c.us" || server == "lid"
}

// MatchTarget checks if a chat (identified by chatID and possible names) matches a rule.
// A rule can be:
// - A phone number (e.g. "+1 (234) 567-8900", "12345678900")
// - A full JID or JID user (e.g. "120363311130744191@g.us", "120363311130744191")
// - A contact name or group name (case-insensitive exact or substring)
func MatchTarget(rule string, chatID string, chatNames ...string) bool {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return false
	}

	ruleLower := strings.ToLower(rule)
	chatLower := strings.ToLower(strings.TrimSpace(chatID))

	// 1. Exact match
	if chatLower != "" && ruleLower == chatLower {
		return true
	}

	// 2. Normalize both rule and chatID to base user & server
	ruleUser, ruleServer := normalizeJID(ruleLower)
	chatUser, chatServer := normalizeJID(chatLower)

	if ruleUser != "" && chatUser != "" {
		if ruleUser == chatUser {
			if ruleServer == "" || chatServer == "" || ruleServer == chatServer || (isUserServer(ruleServer) && isUserServer(chatServer)) {
				return true
			}
		}

		// Numeric phone/group ID comparison
		ruleDigits := digitsOnly(ruleUser)
		chatDigits := digitsOnly(chatUser)
		if len(ruleDigits) >= 6 && len(chatDigits) >= 6 {
			if ruleDigits == chatDigits || strings.HasSuffix(chatDigits, ruleDigits) || strings.HasSuffix(ruleDigits, chatDigits) {
				if ruleServer == "" || chatServer == "" || ruleServer == chatServer || (isUserServer(ruleServer) && isUserServer(chatServer)) {
					return true
				}
			}
		}
	}

	// 3. Name matching
	for _, name := range chatNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		nameLower := strings.ToLower(name)

		if ruleLower == nameLower {
			return true
		}

		nameUser, nameServer := normalizeJID(nameLower)
		if nameUser != "" && ruleUser != "" {
			if nameUser == ruleUser {
				if ruleServer == "" || nameServer == "" || ruleServer == nameServer || (isUserServer(ruleServer) && isUserServer(nameServer)) {
					return true
				}
			}
			nameDigits := digitsOnly(nameUser)
			ruleDigits := digitsOnly(ruleUser)
			if len(nameDigits) >= 6 && len(ruleDigits) >= 6 {
				if nameDigits == ruleDigits || strings.HasSuffix(nameDigits, ruleDigits) || strings.HasSuffix(ruleDigits, nameDigits) {
					return true
				}
			}
		}

		if len(ruleLower) >= 3 && digitsOnly(ruleLower) != ruleLower && strings.Contains(nameLower, ruleLower) {
			return true
		}
	}

	return false
}

// DigitsOnly extracts only numerical digits from a string.
func DigitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func digitsOnly(s string) string {
	return DigitsOnly(s)
}
