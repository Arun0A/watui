package notify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"watui/internal/config"
	"watui/internal/domain"
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
	// Muted chats never trigger notifications
	if cfg.IsMuted(msg.ChatID, msg.ChatName, msg.SenderName) {
		return
	}

	title, body := FormatNotification(msg)

	if notifCfg.Banner {
		SendBanner(title, body)
	}

	if notifCfg.Sound {
		soundPath := cfg.ResolveSoundPath()
		if soundPath != "" {
			PlaySound(soundPath)
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
		safeTitle := strings.ReplaceAll(title, "'", "''")
		safeBody := strings.ReplaceAll(body, "'", "''")
		psCmd := fmt.Sprintf(
			`[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; `+
				`$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02); `+
				`$x = $t.GetElementsByTagName('text'); `+
				`$x.Item(0).AppendChild($t.CreateTextNode('%s')) > $null; `+
				`$x.Item(1).AppendChild($t.CreateTextNode('%s')) > $null; `+
				`$n = [Windows.UI.Notifications.ToastNotification]::new($t); `+
				`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('watui').Show($n)`,
			safeTitle, safeBody,
		)
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psCmd)
		_ = cmd.Start()
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
		absPath, err := filepath.Abs(soundPath)
		if err != nil {
			absPath = soundPath
		}
		if strings.HasSuffix(strings.ToLower(absPath), ".wav") {
			safePath := strings.ReplaceAll(absPath, "'", "''")
			psCmd := fmt.Sprintf(`(New-Object Media.SoundPlayer '%s').PlaySync()`, safePath)
			cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psCmd)
			_ = cmd.Start()
			return
		}

		safePath := strings.ReplaceAll(absPath, "'", "''")
		psCmd := fmt.Sprintf(
			`Add-Type -AssemblyName presentationCore; `+
				`$p = New-Object System.Windows.Media.MediaPlayer; `+
				`$p.Open([System.Uri]'%s'); `+
				`$p.Play(); `+
				`Start-Sleep -Seconds 2`,
			safePath,
		)
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psCmd)
		_ = cmd.Start()
		return
	}
}
