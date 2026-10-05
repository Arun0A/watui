package notify

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"watui/internal/config"
	"watui/internal/domain"
)

var (
	sendBannerFunc = SendBanner
	playSoundFunc  = PlaySound
)

// Dispatch evaluates config and triggers banner and/or sound notifications for an incoming message.
func Dispatch(cfg *config.Config, msg domain.Message) {
	if cfg == nil {
		return
	}
	notifCfg := cfg.GetNotificationConfig()
	if !notifCfg.Enabled {
		return
	}
	// Never notify on self-sent messages
	if msg.IsFromMe {
		return
	}
	// Never notify on reaction messages
	if msg.Type == domain.MessageTypeReaction {
		return
	}
	// Hidden and muted chats never trigger notifications
	if cfg.IsHidden(msg.ChatID, msg.ChatName, msg.SenderName) || cfg.IsMuted(msg.ChatID, msg.ChatName, msg.SenderName) {
		return
	}

	// For group chats, if only_on_mention is enabled, notify only when user is tagged/mentioned
	isGroup := strings.HasSuffix(msg.ChatID, "@g.us") || strings.Contains(msg.ChatID, "@g.us")
	if isGroup && notifCfg.OnlyOnMention && !msg.MentionsMe() {
		return
	}

	title, body := FormatNotification(msg)

	if notifCfg.Banner {
		sendBannerFunc(title, body)
	}

	if notifCfg.Sound {
		soundPath := cfg.ResolveSoundPath()
		if soundPath != "" {
			playSoundFunc(soundPath)
		}
	}
}

// FormatNotification formats clean title and body text for desktop notifications.
func FormatNotification(msg domain.Message) (string, string) {
	title := msg.SenderName
	if msg.ChatName != "" && msg.ChatName != msg.SenderName {
		if title != "" {
			title = fmt.Sprintf("%s (%s)", title, msg.ChatName)
		} else {
			title = msg.ChatName
		}
	}
	if title == "" {
		title = msg.Sender
		if idx := strings.Index(title, "@"); idx != -1 {
			title = title[:idx]
		}
	}

	body := strings.TrimSpace(msg.Body)
	if body == "" && msg.IsMedia() {
		body = fmt.Sprintf("[%s]", msg.Type)
	}
	if msg.IsEdit {
		body = "[EDIT] " + body
	}
	// Clean newlines to space for single-line banner preview
	body = strings.ReplaceAll(body, "\r", "")
	body = strings.ReplaceAll(body, "\n", " ")
	r := []rune(body)
	if len(r) > 150 {
		body = string(r[:147]) + "..."
	}
	if body == "" {
		body = "New message"
	}

	return title, body
}

// SendBanner dispatches a desktop notification banner across Linux, macOS, and Windows.
func SendBanner(title, body string) {
	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("notify-send"); err == nil {
			cmd := exec.Command(path, "-a", "watui", title, body)
			_ = cmd.Start()
		}
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q subtitle "watui"`, body, title)
		cmd := exec.Command("osascript", "-e", script)
		_ = cmd.Start()
	case "windows":
		sendBannerWindows(title, body)
	}
}

// PlaySound plays an audio file in the background asynchronously across Linux, macOS, and Windows.
func PlaySound(soundPath string) {
	if soundPath == "" {
		return
	}
	if _, err := os.Stat(soundPath); err != nil {
		return
	}

	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("pw-play"); err == nil {
			cmd := exec.Command(path, soundPath)
			_ = cmd.Start()
			return
		}
		if path, err := exec.LookPath("paplay"); err == nil {
			cmd := exec.Command(path, soundPath)
			_ = cmd.Start()
			return
		}
		if path, err := exec.LookPath("mpv"); err == nil {
			cmd := exec.Command(path, "--no-video", "--really-quiet", soundPath)
			_ = cmd.Start()
			return
		}
		if path, err := exec.LookPath("ffplay"); err == nil {
			cmd := exec.Command(path, "-nodisp", "-autoexit", "-loglevel", "quiet", soundPath)
			_ = cmd.Start()
			return
		}
		if path, err := exec.LookPath("play"); err == nil {
			cmd := exec.Command(path, "-q", soundPath)
			_ = cmd.Start()
			return
		}
		if strings.HasSuffix(strings.ToLower(soundPath), ".wav") {
			if path, err := exec.LookPath("aplay"); err == nil {
				cmd := exec.Command(path, "-q", soundPath)
				_ = cmd.Start()
				return
			}
		}

	case "darwin":
		if path, err := exec.LookPath("afplay"); err == nil {
			cmd := exec.Command(path, soundPath)
			_ = cmd.Start()
			return
		}

	case "windows":
		playSoundWindows(soundPath)
	}
}
