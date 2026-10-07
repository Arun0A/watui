package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
	"watui/internal/config"
)

// Theme holds customizable colors for all UI elements.
type Theme struct {
	// Header & App
	Title   string `json:"title" yaml:"title"`
	Divider string `json:"divider" yaml:"divider"`
	Status  string `json:"status" yaml:"status"`
	Notice  string `json:"notice" yaml:"notice"`
	Error   string `json:"error" yaml:"error"`
	Prompt  string `json:"prompt" yaml:"prompt"`

	// Chat List
	ChatSelected string `json:"chat_selected" yaml:"chat_selected"`
	ChatNormal   string `json:"chat_normal" yaml:"chat_normal"`
	Snippet      string `json:"snippet" yaml:"snippet"`
	Help         string `json:"help" yaml:"help"`

	// Badges
	BadgeUnreadBG  string `json:"badge_unread_bg" yaml:"badge_unread_bg"`
	BadgeUnreadFG  string `json:"badge_unread_fg" yaml:"badge_unread_fg"`
	BadgePinBG     string `json:"badge_pin_bg" yaml:"badge_pin_bg"`
	BadgePinFG     string `json:"badge_pin_fg" yaml:"badge_pin_fg"`
	BadgeMentionBG string `json:"badge_mention_bg" yaml:"badge_mention_bg"`
	BadgeMentionFG string `json:"badge_mention_fg" yaml:"badge_mention_fg"`
	BadgeMediaBG   string `json:"badge_media_bg" yaml:"badge_media_bg"`
	BadgeMediaFG   string `json:"badge_media_fg" yaml:"badge_media_fg"`
	BadgeEphemeral string `json:"badge_ephemeral" yaml:"badge_ephemeral"`

	// Messages
	Cursor      string `json:"cursor" yaml:"cursor"`
	Select      string `json:"select" yaml:"select"`
	MsgYou      string `json:"msg_you" yaml:"msg_you"`
	MsgThem     string `json:"msg_them" yaml:"msg_them"`
	MsgText     string `json:"msg_text" yaml:"msg_text"`
	TextMuted   string `json:"text_muted" yaml:"text_muted"`
	QuotePrefix string `json:"quote_prefix" yaml:"quote_prefix"`
	EditTag     string `json:"edit_tag" yaml:"edit_tag"`

	// Input Box & Mentions
	InputText        string `json:"input_text" yaml:"input_text"`
	InputPrompt      string `json:"input_prompt" yaml:"input_prompt"`
	InputPlaceholder string `json:"input_placeholder" yaml:"input_placeholder"`
	MentionYou       string `json:"mention_you" yaml:"mention_you"`
	MentionOther     string `json:"mention_other" yaml:"mention_other"`

	// Keybinds Help Screen (F1 / ?)
	KeybindGroup string `json:"keybind_group" yaml:"keybind_group"`
	KeybindKey   string `json:"keybind_key" yaml:"keybind_key"`
	KeybindDesc  string `json:"keybind_desc" yaml:"keybind_desc"`
}

// DefaultTheme returns the default built-in Catppuccin Mocha color scheme.
func DefaultTheme() Theme {
	return Theme{
		Title:   "#A6E3A1",
		Divider: "#313244",
		Status:  "#6C7086",
		Notice:  "#89DCEB",
		Error:   "#F38BA8",
		Prompt:  "#FAB387",

		ChatSelected: "#89B4FA",
		ChatNormal:   "#CDD6F4",
		Snippet:      "#6C7086",
		Help:         "#585B70",

		BadgeUnreadBG:  "#A6E3A1",
		BadgeUnreadFG:  "#11111B",
		BadgePinBG:     "#89B4FA",
		BadgePinFG:     "#11111B",
		BadgeMentionBG: "#A6E3A1",
		BadgeMentionFG: "#11111B",
		BadgeMediaBG:   "#FAB387",
		BadgeMediaFG:   "#11111B",
		BadgeEphemeral: "#FAB387",

		Cursor:      "#FAB387",
		Select:      "#A6E3A1",
		MsgYou:      "#A6E3A1",
		MsgThem:     "#89B4FA",
		MsgText:     "#CDD6F4",
		TextMuted:   "#A6ADC8",
		QuotePrefix: "#6C7086",
		EditTag:     "#FAB387",

		InputText:        "#CDD6F4",
		InputPrompt:      "#89DCEB",
		InputPlaceholder: "#6C7086",
		MentionYou:       "#A6E3A1",
		MentionOther:     "#89B4FA",

		KeybindGroup: "#FAB387",
		KeybindKey:   "#89DCEB",
		KeybindDesc:  "#CDD6F4",
	}
}

// Styles holds pre-compiled lipgloss.Style objects built from a Theme.
type Styles struct {
	Title               lipgloss.Style
	Status              lipgloss.Style
	Divider             lipgloss.Style
	Badge               lipgloss.Style
	PinBadge            lipgloss.Style
	MentionBadge        lipgloss.Style
	EphemeralBadge      lipgloss.Style
	SelectedTitle       lipgloss.Style
	NormalTitle         lipgloss.Style
	Snippet             lipgloss.Style
	Help                lipgloss.Style
	Jid                 lipgloss.Style
	MentionYou          lipgloss.Style
	MentionOther        lipgloss.Style
	MediaBadgeSelected  lipgloss.Style
	CursorPrefix        lipgloss.Style
	SelectPrefix        lipgloss.Style
	MsgYouHeader        lipgloss.Style
	MsgThemHeader       lipgloss.Style
	BodyPrefixCursor    lipgloss.Style
	BodyPrefixSelect    lipgloss.Style
	EditTag             lipgloss.Style
	QuoteLinePrefix     lipgloss.Style
	QuoteLineClean      lipgloss.Style
	ReplyBarPrompt      lipgloss.Style
	ReplyBarSender      lipgloss.Style
	ReplyBarSnippet     lipgloss.Style
	ReplyBarEsc         lipgloss.Style
	EditBarPrompt       lipgloss.Style
	EditBarSnippet      lipgloss.Style
	EditBarEsc          lipgloss.Style
	MentionHeaderPrompt lipgloss.Style
	MentionHeaderEsc    lipgloss.Style
	MentionItemSelPref  lipgloss.Style
	MentionItemSelName  lipgloss.Style
	MentionItemPhone    lipgloss.Style
	MentionItemNormName lipgloss.Style
	MentionFooter       lipgloss.Style
	DateDividerText     lipgloss.Style
	PromptText          lipgloss.Style
	PromptEsc           lipgloss.Style
	NoticeText          lipgloss.Style
	ErrorText           lipgloss.Style
	KeybindGroupTitle   lipgloss.Style
	KeybindKey          lipgloss.Style
	KeybindDesc         lipgloss.Style
}

// MakeStyles compiles a Theme into lipgloss styles.
func MakeStyles(t Theme) Styles {
	return Styles{
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Title)),

		Status: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		Divider: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Divider)),

		Badge: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.BadgeUnreadFG)).
			Background(lipgloss.Color(t.BadgeUnreadBG)).
			Padding(0, 1),

		PinBadge: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.BadgePinFG)).
			Background(lipgloss.Color(t.BadgePinBG)).
			Padding(0, 1),

		MentionBadge: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.BadgeMentionFG)).
			Background(lipgloss.Color(t.BadgeMentionBG)).
			Padding(0, 1),

		EphemeralBadge: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.BadgeEphemeral)),

		SelectedTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.ChatSelected)),

		NormalTitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.ChatNormal)),

		Snippet: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Snippet)),

		Help: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Help)),

		Jid: lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color(t.Help)),

		MentionYou: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.MentionYou)),

		MentionOther: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.MentionOther)),

		MediaBadgeSelected: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.BadgeMediaFG)).
			Background(lipgloss.Color(t.BadgeMediaBG)),

		CursorPrefix: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Cursor)),

		SelectPrefix: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Select)),

		MsgYouHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.MsgYou)),

		MsgThemHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.MsgThem)),

		BodyPrefixCursor: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Cursor)),

		BodyPrefixSelect: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Select)),

		EditTag: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.EditTag)),

		QuoteLinePrefix: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.QuotePrefix)),

		QuoteLineClean: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.QuotePrefix)).
			Italic(true),

		ReplyBarPrompt: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Prompt)),

		ReplyBarSender: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.MsgText)),

		ReplyBarSnippet: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.TextMuted)),

		ReplyBarEsc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		EditBarPrompt: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Prompt)),

		EditBarSnippet: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.TextMuted)),

		EditBarEsc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		MentionHeaderPrompt: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.InputPrompt)),

		MentionHeaderEsc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		MentionItemSelPref: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Select)),

		MentionItemSelName: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Select)),

		MentionItemPhone: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		MentionItemNormName: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.MsgText)),

		MentionFooter: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		DateDividerText: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.TextMuted)),

		PromptText: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Prompt)),

		PromptEsc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Status)),

		NoticeText: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.Notice)),

		ErrorText: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.Error)),

		KeybindGroupTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.KeybindGroup)),

		KeybindKey: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(t.KeybindKey)),

		KeybindDesc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(t.KeybindDesc)),
	}
}

// FindThemePath locates theme.yaml in standard locations or next to the active config file.
func FindThemePath(cfg *config.Config) string {
	if cfg != nil {
		if t := strings.TrimSpace(cfg.Theme); t != "" {
			expanded := config.ExpandHome(t)
			if filepath.IsAbs(expanded) {
				if _, err := os.Stat(expanded); err == nil {
					return expanded
				}
			}
			if cfg.SourcePath != "" {
				cand := filepath.Join(filepath.Dir(cfg.SourcePath), expanded)
				if _, err := os.Stat(cand); err == nil {
					return cand
				}
			}
			if _, err := os.Stat(expanded); err == nil {
				return expanded
			}
		}

		// Next to active watui.yaml
		if cfg.SourcePath != "" {
			cfgDir := filepath.Dir(cfg.SourcePath)
			for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
				cand := filepath.Join(cfgDir, name)
				if _, err := os.Stat(cand); err == nil {
					return cand
				}
			}
		}

		// In configured config_dir
		if cfg.ConfigDir != "" {
			dir := config.ExpandHome(cfg.ConfigDir)
			for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
				cand := filepath.Join(dir, name)
				if _, err := os.Stat(cand); err == nil {
					return cand
				}
			}
		}
	}

	// Current directory
	for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}

	// Executable directory (for portable Windows / portable Linux)
	if exeDir := config.GetExeDir(); exeDir != "" && exeDir != "." {
		for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
			cand := filepath.Join(exeDir, name)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}

	// OS User config dir (Linux, macOS, Windows)
	if runtime.GOOS != "windows" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			xdgConfig := os.Getenv("XDG_CONFIG_HOME")
			if xdgConfig == "" {
				xdgConfig = filepath.Join(home, ".config")
			}
			for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
				cand := filepath.Join(xdgConfig, "watui", name)
				if _, err := os.Stat(cand); err == nil {
					return cand
				}
			}
		}
	} else {
		appData := os.Getenv("APPDATA")
		if appData != "" {
			for _, name := range []string{"theme.yaml", "theme.yml", "theme.json"} {
				cand := filepath.Join(appData, "watui", name)
				if _, err := os.Stat(cand); err == nil {
					return cand
				}
			}
		}
	}

	return ""
}

// LoadTheme loads a Theme, falling back to DefaultTheme for any missing keys or if file does not exist.
func LoadTheme(cfg *config.Config) (Theme, string) {
	theme := DefaultTheme()
	themePath := FindThemePath(cfg)
	if themePath == "" {
		return theme, ""
	}

	data, err := os.ReadFile(themePath)
	if err != nil {
		return theme, ""
	}

	var raw map[string]interface{}
	ext := strings.ToLower(filepath.Ext(themePath))
	if ext == ".json" {
		_ = json.Unmarshal(data, &raw)
	} else {
		_ = yaml.Unmarshal(data, &raw)
	}

	if raw == nil {
		return theme, themePath
	}

	assign := func(target *string, key string) {
		if v, ok := raw[key]; ok && v != nil {
			s := strings.TrimSpace(fmt.Sprintf("%v", v))
			if s != "" {
				*target = s
			}
		}
	}

	// Header & App
	assign(&theme.Title, "title")
	assign(&theme.Divider, "divider")
	assign(&theme.Status, "status")
	assign(&theme.Notice, "notice")
	assign(&theme.Error, "error")
	assign(&theme.Prompt, "prompt")

	// Chat List
	assign(&theme.ChatSelected, "chat_selected")
	assign(&theme.ChatNormal, "chat_normal")
	assign(&theme.Snippet, "snippet")
	assign(&theme.Help, "help")

	// Badges
	assign(&theme.BadgeUnreadBG, "badge_unread_bg")
	assign(&theme.BadgeUnreadFG, "badge_unread_fg")
	assign(&theme.BadgePinBG, "badge_pin_bg")
	assign(&theme.BadgePinFG, "badge_pin_fg")
	assign(&theme.BadgeMentionBG, "badge_mention_bg")
	assign(&theme.BadgeMentionFG, "badge_mention_fg")
	assign(&theme.BadgeMediaBG, "badge_media_bg")
	assign(&theme.BadgeMediaFG, "badge_media_fg")
	assign(&theme.BadgeEphemeral, "badge_ephemeral")

	// Messages
	assign(&theme.Cursor, "cursor")
	assign(&theme.Select, "select")
	assign(&theme.MsgYou, "msg_you")
	assign(&theme.MsgThem, "msg_them")
	assign(&theme.MsgText, "msg_text")
	assign(&theme.TextMuted, "text_muted")
	assign(&theme.QuotePrefix, "quote_prefix")
	assign(&theme.EditTag, "edit_tag")

	// Input Box & Mentions
	assign(&theme.InputText, "input_text")
	assign(&theme.InputPrompt, "input_prompt")
	assign(&theme.InputPlaceholder, "input_placeholder")
	assign(&theme.MentionYou, "mention_you")
	assign(&theme.MentionOther, "mention_other")

	// Help Screen
	assign(&theme.KeybindGroup, "keybind_group")
	assign(&theme.KeybindKey, "keybind_key")
	assign(&theme.KeybindDesc, "keybind_desc")

	return theme, themePath
}
