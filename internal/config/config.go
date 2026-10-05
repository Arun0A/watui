package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// PreviewConfig specifies commands used to preview media attachments on demand.
type PreviewConfig struct {
	Image      string            `json:"image" yaml:"image"`
	Video      string            `json:"video" yaml:"video"`
	Audio      string            `json:"audio" yaml:"audio"`
	Sticker    string            `json:"sticker" yaml:"sticker"`
	Document   string            `json:"document" yaml:"document"`
	Extensions map[string]string `json:"extensions" yaml:"extensions"`
}

// UnmarshalYAML decodes PreviewConfig and also populates Extensions from direct keys (e.g. pdf: zathura).
func (p *PreviewConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawPreview PreviewConfig
	var raw rawPreview
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*p = PreviewConfig(raw)
	if p.Extensions == nil {
		p.Extensions = make(map[string]string)
	}

	var m map[string]interface{}
	if err := value.Decode(&m); err == nil {
		for k, v := range m {
			kLower := strings.ToLower(strings.TrimSpace(k))
			if kLower == "image" || kLower == "video" || kLower == "audio" || kLower == "sticker" || kLower == "document" || kLower == "extensions" {
				continue
			}
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				ext := strings.TrimPrefix(kLower, ".")
				p.Extensions[ext] = strings.TrimSpace(s)
			}
		}
	}
	return nil
}

// NotificationConfig controls desktop notifications and sounds for incoming messages.
type NotificationConfig struct {
	Enabled       bool   `json:"enabled" yaml:"enabled"`
	Banner        bool   `json:"banner" yaml:"banner"`
	Sound         bool   `json:"sound" yaml:"sound"`
	SoundPath     string `json:"sound_path" yaml:"sound_path"`
	OnlyOnMention bool   `json:"only_on_mention" yaml:"only_on_mention"`
}

// HistoryConfig controls local message history caching and cyclic retention.
type HistoryConfig struct {
	PersistChatHistory   *bool `json:"persist_chat_history" yaml:"persist_chat_history"`
	CycleMsgCountPerChat int   `json:"cycle_msg_count_per_chat" yaml:"cycle_msg_count_per_chat"`
	AlwaysLoadHistory    *bool `json:"always_load_history" yaml:"always_load_history"`

	// Nested aliases
	Persist    *bool `json:"persist" yaml:"persist"`
	CycleCount int   `json:"cycle_count" yaml:"cycle_count"`
	AlwaysLoad *bool `json:"always_load" yaml:"always_load"`
}

// DaemonConfig controls background notification daemon options and logging.
type DaemonConfig struct {
	Log     *bool  `json:"log" yaml:"log"`
	Logging *bool  `json:"logging" yaml:"logging"`
	LogPath string `json:"log_path" yaml:"log_path"`
	LogFile string `json:"log_file" yaml:"log_file"`
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

	// Hidden lists chat JIDs, phone numbers, or group/contact names to hide from unread and contacts.
	Hidden []string `json:"hidden" yaml:"hidden"`

	// Pinned lists chat JIDs, phone numbers, or group/contact names to keep pinned at top.
	Pinned []string `json:"pinned" yaml:"pinned"`

	// Muted lists chat JIDs, phone numbers, or group/contact names to mute notifications only (still shows in unread).
	Muted []string `json:"muted" yaml:"muted"`

	// Aliases for user convenience (hide / pin / mute)
	Hide []string `json:"hide" yaml:"hide"`
	Pin  []string `json:"pin" yaml:"pin"`
	Mute []string `json:"mute" yaml:"mute"`

	// History controls local chat history persistence and cyclic retention.
	PersistChatHistory   *bool         `json:"persist_chat_history" yaml:"persist_chat_history"`
	CycleMsgCountPerChat int           `json:"cycle_msg_count_per_chat" yaml:"cycle_msg_count_per_chat"`
	AlwaysLoadHistory    *bool         `json:"always_load_history" yaml:"always_load_history"`
	History              HistoryConfig `json:"history" yaml:"history"`

	// Notifications controls desktop notification banners and sounds for incoming messages.
	Notifications NotificationConfig `json:"notifications" yaml:"notifications"`
	Notification  NotificationConfig `json:"notification" yaml:"notification"`

	// Daemon controls background notification daemon behavior and logging.
	Daemon DaemonConfig `json:"daemon" yaml:"daemon"`

	// DisableReactions controls whether reaction messages are displayed in chat (default: false).
	DisableReactions *bool `json:"disable_reactions" yaml:"disable_reactions"`
	HideReactions    *bool `json:"hide_reactions" yaml:"hide_reactions"`
	ShowReactions    *bool `json:"show_reactions" yaml:"show_reactions"`

	// ClipboardPaste controls whether pasting images/files/text from clipboard is enabled (default: true).
	ClipboardPaste        *bool `json:"clipboard_paste" yaml:"clipboard_paste"`
	EnableClipboardPaste  *bool `json:"enable_clipboard_paste" yaml:"enable_clipboard_paste"`
	DisableClipboardPaste *bool `json:"disable_clipboard_paste" yaml:"disable_clipboard_paste"`

	// ExpireMedia controls after how many hours of last access cached media files expire (default: 72 hours / 3 days).
	// Set to 0 to disable automatic expiration.
	ExpireMedia *int `json:"expire_media" yaml:"expire_media"`

	// ClearOnExit controls whether the entire media cache is cleared whenever the TUI session is closed (default: false).
	ClearOnExit      *bool `json:"clear_on_exit" yaml:"clear_on_exit"`
	ClearMediaOnExit *bool `json:"clear_media_on_exit" yaml:"clear_media_on_exit"`

	// SourcePath stores the path of the file this config was loaded from (if any).
	SourcePath string `json:"-" yaml:"-"`
}

// IsClearOnExitEnabled returns true if all cached media files should be cleared when the TUI exits.
// Defaults to false.
func (c *Config) IsClearOnExitEnabled() bool {
	if c == nil {
		return false
	}
	if c.ClearOnExit != nil {
		return *c.ClearOnExit
	}
	if c.ClearMediaOnExit != nil {
		return *c.ClearMediaOnExit
	}
	return false
}

// GetExpireMediaHours returns the media expiration threshold in hours.
// Defaults to 72 hours (3 days). If configured to <= 0, returns 0 (expiration disabled).
func (c *Config) GetExpireMediaHours() int {
	if c == nil || c.ExpireMedia == nil {
		return 72
	}
	if *c.ExpireMedia <= 0 {
		return 0
	}
	return *c.ExpireMedia
}

// IsDaemonLogEnabled reports whether daemon file logging is enabled in config.
// Defaults to false (logging disabled unless enabled in config or via flag).
func (c *Config) IsDaemonLogEnabled() bool {
	if c == nil {
		return false
	}
	if c.Daemon.Log != nil {
		return *c.Daemon.Log
	}
	if c.Daemon.Logging != nil {
		return *c.Daemon.Logging
	}
	return false
}

// ResolveDaemonLogPath returns the resolved file path for daemon logs.
func (c *Config) ResolveDaemonLogPath(dbPath string) string {
	if c != nil {
		path := c.Daemon.LogPath
		if path == "" {
			path = c.Daemon.LogFile
		}
		if path != "" {
			return ResolveDBPath(path)
		}
	}
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}
	dir := filepath.Dir(ResolveDBPath(dbPath))
	return filepath.Join(dir, "watui-daemon.log")
}

// GetNotificationConfig returns the merged notification configuration.
func (c *Config) GetNotificationConfig() NotificationConfig {
	if c == nil {
		return NotificationConfig{}
	}
	n := c.Notifications
	if !n.Enabled && c.Notification.Enabled {
		n.Enabled = true
	}
	if !n.Banner && c.Notification.Banner {
		n.Banner = true
	}
	if !n.Sound && c.Notification.Sound {
		n.Sound = true
	}
	if n.SoundPath == "" && c.Notification.SoundPath != "" {
		n.SoundPath = c.Notification.SoundPath
	}
	if !n.OnlyOnMention && c.Notification.OnlyOnMention {
		n.OnlyOnMention = true
	}
	return n
}

// IsOnlyOnMention returns true if group chat notifications should only be triggered on mentions.
func (c *Config) IsOnlyOnMention() bool {
	if c == nil {
		return false
	}
	return c.GetNotificationConfig().OnlyOnMention
}

// ResolveSoundPath resolves the audio file path to play for notifications.
// Returns an existing path, or empty string if not found.
func (c *Config) ResolveSoundPath() string {
	if c == nil {
		return ""
	}
	cfg := c.GetNotificationConfig()
	soundPath := strings.TrimSpace(cfg.SoundPath)
	if soundPath != "" {
		if filepath.IsAbs(soundPath) {
			if _, err := os.Stat(soundPath); err == nil {
				return soundPath
			}
		}
		if c.SourcePath != "" {
			cfgDir := filepath.Dir(c.SourcePath)
			cand := filepath.Join(cfgDir, soundPath)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
		exeDir := GetExeDir()
		if exeDir != "" {
			cand := filepath.Join(exeDir, soundPath)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
		if _, err := os.Stat(soundPath); err == nil {
			return soundPath
		}
	}

	// Default fallback: look for default.mp3 in assets or next to config/exe
	candidates := []string{
		"assets/default.mp3",
		"default.mp3",
		"assets/whatsapp_notification.mp3",
		"whatsapp_notification.mp3",
	}
	if c.SourcePath != "" {
		cfgDir := filepath.Dir(c.SourcePath)
		candidates = append(candidates,
			filepath.Join(cfgDir, "assets", "default.mp3"),
			filepath.Join(cfgDir, "default.mp3"),
			filepath.Join(cfgDir, "assets", "whatsapp_notification.mp3"),
			filepath.Join(cfgDir, "whatsapp_notification.mp3"),
		)
	}
	exeDir := GetExeDir()
	if exeDir != "" {
		candidates = append(candidates,
			filepath.Join(exeDir, "assets", "default.mp3"),
			filepath.Join(exeDir, "default.mp3"),
			filepath.Join(exeDir, "assets", "whatsapp_notification.mp3"),
			filepath.Join(exeDir, "whatsapp_notification.mp3"),
		)
	}

	for _, cand := range candidates {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}

	return ""
}

// IsHistoryPersistEnabled reports whether local message history persistence is enabled.
// Defaults to false.
func (c *Config) IsHistoryPersistEnabled() bool {
	if c == nil {
		return false
	}
	if c.PersistChatHistory != nil {
		return *c.PersistChatHistory
	}
	if c.History.PersistChatHistory != nil {
		return *c.History.PersistChatHistory
	}
	if c.History.Persist != nil {
		return *c.History.Persist
	}
	return false
}

// GetCycleMsgCountPerChat returns the maximum number of messages to cyclically retain per chat.
// Defaults to 30.
func (c *Config) GetCycleMsgCountPerChat() int {
	if c == nil {
		return 30
	}
	if c.CycleMsgCountPerChat > 0 {
		return c.CycleMsgCountPerChat
	}
	if c.History.CycleMsgCountPerChat > 0 {
		return c.History.CycleMsgCountPerChat
	}
	if c.History.CycleCount > 0 {
		return c.History.CycleCount
	}
	return 30
}

// IsAlwaysLoadHistoryEnabled reports whether history should be automatically pre-loaded when entering a chat.
// Defaults to false.
func (c *Config) IsAlwaysLoadHistoryEnabled() bool {
	if c == nil {
		return false
	}
	if c.AlwaysLoadHistory != nil {
		return *c.AlwaysLoadHistory
	}
	if c.History.AlwaysLoadHistory != nil {
		return *c.History.AlwaysLoadHistory
	}
	if c.History.AlwaysLoad != nil {
		return *c.History.AlwaysLoad
	}
	return false
}

// IsReactionsDisabled reports whether reactions should be completely disabled from chat display.
// Defaults to false.
func (c *Config) IsReactionsDisabled() bool {
	if c == nil {
		return false
	}
	if c.DisableReactions != nil {
		return *c.DisableReactions
	}
	if c.HideReactions != nil {
		return *c.HideReactions
	}
	if c.ShowReactions != nil {
		return !*c.ShowReactions
	}
	return false
}

// IsClipboardPasteEnabled reports whether clipboard paste support is enabled (default: true).
func (c *Config) IsClipboardPasteEnabled() bool {
	if c == nil {
		return true
	}
	if c.DisableClipboardPaste != nil && *c.DisableClipboardPaste {
		return false
	}
	if c.EnableClipboardPaste != nil {
		return *c.EnableClipboardPaste
	}
	if c.ClipboardPaste != nil {
		return *c.ClipboardPaste
	}
	return true
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

// GetPreviewCommandForFile returns the preview command for a given file and message type.
// If the file has an extension matching an entry in preview.extensions (e.g. pdf, txt, log),
// that extension command is used. Otherwise it falls back to GetPreviewCommand(msgType).
func (c *Config) GetPreviewCommandForFile(filePath string, msgType string) string {
	if c != nil && len(c.Preview.Extensions) > 0 && filePath != "" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filePath), "."))
		if ext != "" {
			if cmd, ok := c.Preview.Extensions[ext]; ok && strings.TrimSpace(cmd) != "" {
				return strings.TrimSpace(cmd)
			}
			if cmd, ok := c.Preview.Extensions["."+ext]; ok && strings.TrimSpace(cmd) != "" {
				return strings.TrimSpace(cmd)
			}
		}
	}
	return c.GetPreviewCommand(msgType)
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

	// Also parse direct raw map keys for kebab-case aliases
	var rawMap map[string]interface{}
	if err := yaml.Unmarshal(data, &rawMap); err == nil {
		for k, v := range rawMap {
			normalized := strings.ToLower(strings.ReplaceAll(k, "_", "-"))
			switch normalized {
			case "persist-chat-history":
				if b, ok := v.(bool); ok {
					cfg.PersistChatHistory = &b
				}
			case "cycle-msg-count-per-chat":
				if n, ok := v.(int); ok {
					cfg.CycleMsgCountPerChat = n
				}
			case "always-load-history":
				if b, ok := v.(bool); ok {
					cfg.AlwaysLoadHistory = &b
				}
			case "disable-reactions":
				if b, ok := v.(bool); ok {
					cfg.DisableReactions = &b
				}
			case "hide-reactions":
				if b, ok := v.(bool); ok {
					cfg.HideReactions = &b
				}
			case "show-reactions":
				if b, ok := v.(bool); ok {
					cfg.ShowReactions = &b
				}
			case "clipboard-paste":
				if b, ok := v.(bool); ok {
					cfg.ClipboardPaste = &b
				}
			case "enable-clipboard-paste":
				if b, ok := v.(bool); ok {
					cfg.EnableClipboardPaste = &b
				}
			case "disable-clipboard-paste":
				if b, ok := v.(bool); ok {
					cfg.DisableClipboardPaste = &b
				}
			}
		}
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
	if len(overlay.Preview.Extensions) > 0 {
		if base.Preview.Extensions == nil {
			base.Preview.Extensions = make(map[string]string)
		}
		for k, v := range overlay.Preview.Extensions {
			base.Preview.Extensions[k] = v
		}
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
	if len(overlay.Hidden) > 0 {
		base.Hidden = overlay.Hidden
	}
	if len(overlay.Hide) > 0 {
		base.Hide = overlay.Hide
	}
	if len(overlay.Muted) > 0 {
		base.Muted = overlay.Muted
	}
	if len(overlay.Mute) > 0 {
		base.Mute = overlay.Mute
	}
	if overlay.PersistChatHistory != nil {
		base.PersistChatHistory = overlay.PersistChatHistory
	}
	if overlay.CycleMsgCountPerChat > 0 {
		base.CycleMsgCountPerChat = overlay.CycleMsgCountPerChat
	}
	if overlay.AlwaysLoadHistory != nil {
		base.AlwaysLoadHistory = overlay.AlwaysLoadHistory
	}
	if overlay.History.PersistChatHistory != nil {
		base.History.PersistChatHistory = overlay.History.PersistChatHistory
	}
	if overlay.History.CycleMsgCountPerChat > 0 {
		base.History.CycleMsgCountPerChat = overlay.History.CycleMsgCountPerChat
	}
	if overlay.History.AlwaysLoadHistory != nil {
		base.History.AlwaysLoadHistory = overlay.History.AlwaysLoadHistory
	}
	if overlay.History.Persist != nil {
		base.History.Persist = overlay.History.Persist
	}
	if overlay.History.CycleCount > 0 {
		base.History.CycleCount = overlay.History.CycleCount
	}
	if overlay.History.AlwaysLoad != nil {
		base.History.AlwaysLoad = overlay.History.AlwaysLoad
	}
	if overlay.Daemon.Log != nil {
		base.Daemon.Log = overlay.Daemon.Log
	}
	if overlay.Daemon.Logging != nil {
		base.Daemon.Logging = overlay.Daemon.Logging
	}
	if overlay.Daemon.LogPath != "" {
		base.Daemon.LogPath = overlay.Daemon.LogPath
	}
	if overlay.Daemon.LogFile != "" {
		base.Daemon.LogFile = overlay.Daemon.LogFile
	}
	if overlay.DisableReactions != nil {
		base.DisableReactions = overlay.DisableReactions
	}
	if overlay.HideReactions != nil {
		base.HideReactions = overlay.HideReactions
	}
	if overlay.ShowReactions != nil {
		base.ShowReactions = overlay.ShowReactions
	}
	if overlay.ClipboardPaste != nil {
		base.ClipboardPaste = overlay.ClipboardPaste
	}
	if overlay.EnableClipboardPaste != nil {
		base.EnableClipboardPaste = overlay.EnableClipboardPaste
	}
	if overlay.DisableClipboardPaste != nil {
		base.DisableClipboardPaste = overlay.DisableClipboardPaste
	}
	if overlay.ExpireMedia != nil {
		base.ExpireMedia = overlay.ExpireMedia
	}
	if overlay.ClearOnExit != nil {
		base.ClearOnExit = overlay.ClearOnExit
	}
	if overlay.ClearMediaOnExit != nil {
		base.ClearMediaOnExit = overlay.ClearMediaOnExit
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

// GetHidden returns all unique, trimmed hide rules.
func (c *Config) GetHidden() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]bool)
	var list []string
	for _, item := range append(append([]string(nil), c.Hidden...), c.Hide...) {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			list = append(list, trimmed)
		}
	}
	return list
}

// IsHidden returns true if the chat matches any configured hide rule.
func (c *Config) IsHidden(chatID string, chatNames ...string) bool {
	if c == nil {
		return false
	}
	for _, rule := range c.GetHidden() {
		if MatchTarget(rule, chatID, chatNames...) {
			return true
		}
	}
	return false
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

var (
	targetRegexMu  sync.RWMutex
	targetRegexMap = make(map[string]*regexp.Regexp)
)

func getCompiledRegex(pattern string) *regexp.Regexp {
	targetRegexMu.RLock()
	re, exists := targetRegexMap[pattern]
	targetRegexMu.RUnlock()
	if exists {
		return re
	}

	targetRegexMu.Lock()
	defer targetRegexMu.Unlock()
	if re, exists := targetRegexMap[pattern]; exists {
		return re
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		targetRegexMap[pattern] = nil
		return nil
	}
	targetRegexMap[pattern] = compiled
	return compiled
}

// MatchTarget checks if a chat (identified by chatID and possible names) matches a rule.
// A rule can be:
// - A wildcard / glob pattern (e.g. "*@newsletter", "status@*", "*@broadcast")
// - A regular expression (e.g. ".*@newsletter", "^status@broadcast$", "/(?i)newsletter/")
// - A direct suffix starting with @ (e.g. "@newsletter", "@broadcast")
// - A phone number (e.g. "+1 (234) 567-8900", "12345678900")
// - A full JID or JID user (e.g. "120363311130744191@g.us", "status@broadcast")
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

	// 2. Direct @suffix match (e.g. "@newsletter" or "@broadcast")
	if strings.HasPrefix(ruleLower, "@") {
		if strings.HasSuffix(chatLower, ruleLower) {
			return true
		}
	}

	// 3. Glob / Wildcard matching (*, ?)
	if strings.ContainsAny(ruleLower, "*?") {
		if matched, _ := path.Match(ruleLower, chatLower); matched {
			return true
		}
		for _, name := range chatNames {
			nameLower := strings.ToLower(strings.TrimSpace(name))
			if matched, _ := path.Match(ruleLower, nameLower); matched {
				return true
			}
		}
	}

	// 4. Regex matching (slashed e.g. /pattern/ or implicit with .*, ^, $, |, \)
	isSlashRegex := strings.HasPrefix(rule, "/") && strings.HasSuffix(rule, "/") && len(rule) > 2
	isImplicitRegex := strings.Contains(rule, ".*") || strings.HasPrefix(rule, "^") || strings.HasSuffix(rule, "$") || strings.Contains(rule, "|") || strings.Contains(rule, `\`)

	if isSlashRegex || isImplicitRegex {
		pattern := rule
		if isSlashRegex {
			pattern = rule[1 : len(rule)-1]
		}
		if !strings.HasPrefix(pattern, "(?i)") {
			pattern = "(?i)" + pattern
		}
		re := getCompiledRegex(pattern)
		if re != nil {
			if re.MatchString(chatID) || re.MatchString(chatLower) {
				return true
			}
			for _, name := range chatNames {
				if re.MatchString(name) || re.MatchString(strings.ToLower(name)) {
					return true
				}
			}
		}
	}

	// 5. Normalize both rule and chatID to base user & server
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
