//go:build windows

package notify

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	sndSync      = 0x00000000
	sndAsync     = 0x00000001
	sndNodefault = 0x00000002
	sndFilename  = 0x00020000

	createNoWindow = 0x08000000
)

var (
	winmm              = syscall.NewLazyDLL("winmm.dll")
	procPlaySoundW     = winmm.NewProc("PlaySoundW")
	procMciSendStringW = winmm.NewProc("mciSendStringW")
)

func playSoundWindows(soundPath string) {
	absPath, err := filepath.Abs(soundPath)
	if err != nil {
		absPath = soundPath
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	if ext == ".wav" {
		p, err := syscall.UTF16PtrFromString(absPath)
		if err == nil {
			procPlaySoundW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(sndFilename|sndAsync|sndNodefault))
			return
		}
	}

	// For .mp3, .wma, or other media files, play using Win32 MCI (Media Control Interface)
	// without spawning any external process or console window.
	closeCmd, _ := syscall.UTF16PtrFromString("close watui_snd")
	procMciSendStringW.Call(uintptr(unsafe.Pointer(closeCmd)), 0, 0, 0)

	openStr := fmt.Sprintf(`open "%s" type mpegvideo alias watui_snd`, absPath)
	openCmd, err := syscall.UTF16PtrFromString(openStr)
	if err == nil {
		r1, _, _ := procMciSendStringW.Call(uintptr(unsafe.Pointer(openCmd)), 0, 0, 0)
		if r1 == 0 {
			playCmd, _ := syscall.UTF16PtrFromString("play watui_snd from 0")
			procMciSendStringW.Call(uintptr(unsafe.Pointer(playCmd)), 0, 0, 0)
			return
		}
	}

	// Fallback: PowerShell with CREATE_NO_WINDOW so it NEVER blinks a console window
	safePath := strings.ReplaceAll(absPath, "'", "''")
	psCmd := fmt.Sprintf(
		`Add-Type -AssemblyName presentationCore; `+
			`$p = New-Object System.Windows.Media.MediaPlayer; `+
			`$p.Open([System.Uri]'%s'); `+
			`$p.Play(); `+
			`Start-Sleep -Seconds 2`,
		safePath,
	)
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	_ = cmd.Start()
}

func sendBannerWindows(title, body string) {
	safeTitle := strings.ReplaceAll(title, "'", "''")
	safeTitle = strings.ReplaceAll(safeTitle, "\r", "")
	safeTitle = strings.ReplaceAll(safeTitle, "\n", " ")

	safeBody := strings.ReplaceAll(body, "'", "''")
	safeBody = strings.ReplaceAll(safeBody, "\r", "")
	safeBody = strings.ReplaceAll(safeBody, "\n", " ")

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

	cmd := exec.Command(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", psCmd,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	_ = cmd.Start()
}
