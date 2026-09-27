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
	Sticker  string `json:"sticker" yaml:"sticker"`
	Document string `json:"document" yaml:"document"`
}

// Config represents declarative user configuration for watui.
type Config struct {
	// Preview defines commands to preview media attachments on demand.
	Preview PreviewConfig `json:"preview" yaml:"preview"`

	// FilePicker specifies an optional custom command to open a file picker.
	FilePicker string `json:"file_picker" yaml:"file_picker"`

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

func defaultDocumentCommand() string {
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
	switch strings.ToLower(msgType) {
	case "video":
		return "mpv"
	case "document":
		return defaultDocumentCommand()
	default:
		return "mpv --loop=inf"
	}
}

// GetFilePickerCommand returns any user-configured file picker command, or empty string.
func (c *Config) GetFilePickerCommand() string {
	if c != nil {
		return strings.TrimSpace(c.FilePicker)
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
	return &cfg, nil
}

func defaultCandidatePaths() []string {
	paths := []string{
		"watui.yaml",
		"watui.yml",
		"watui.json",
	}

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

	return paths
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
