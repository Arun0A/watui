//go:build !windows

package notify

func sendBannerWindows(title, body string) {}
func playSoundWindows(soundPath string)    {}
