package notify

import (
	"strings"
	"testing"

	"watui/internal/config"
	"watui/internal/domain"
)

func TestFormatNotification(t *testing.T) {
	// 1. Group message
	msgGroup := domain.Message{
		ChatID:     "123@g.us",
		ChatName:   "Work Team",
		Sender:     "456@s.whatsapp.net",
		SenderName: "Alice",
		Body:       "Meeting at 3 PM\nPlease be on time!",
	}
	title, body := FormatNotification(msgGroup)
	if title != "Alice (Work Team)" {
		t.Errorf("Expected title 'Alice (Work Team)', got %q", title)
	}
	if strings.Contains(body, "\n") {
		t.Errorf("Expected body newlines to be flattened, got %q", body)
	}
	if body != "Meeting at 3 PM Please be on time!" {
		t.Errorf("Unexpected body format: %q", body)
	}

	// 2. Direct message
	msgDM := domain.Message{
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Bob",
		Body:       "Hello there",
	}
	title2, body2 := FormatNotification(msgDM)
	if title2 != "Bob" {
		t.Errorf("Expected title 'Bob', got %q", title2)
	}
	if body2 != "Hello there" {
		t.Errorf("Expected body 'Hello there', got %q", body2)
	}

	// 3. Media with empty body
	msgMedia := domain.Message{
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Charlie",
		Type:       domain.MessageTypeImage,
		Body:       "",
	}
	_, body3 := FormatNotification(msgMedia)
	if body3 != "[image]" {
		t.Errorf("Expected body '[image]', got %q", body3)
	}

	// 4. Very long text truncation
	longText := strings.Repeat("A", 200)
	msgLong := domain.Message{
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "David",
		Body:       longText,
	}
	_, body4 := FormatNotification(msgLong)
	if len([]rune(body4)) > 150 {
		t.Errorf("Expected truncated body <= 150 runes, got %d", len([]rune(body4)))
	}
	if !strings.HasSuffix(body4, "...") {
		t.Errorf("Expected truncated body to end with '...', got %q", body4)
	}

	// 5. Edited message -> should prefix [EDIT]
	msgEdit := domain.Message{
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Alice",
		Body:       "Corrected text",
		IsEdit:     true,
	}
	_, bodyEdit := FormatNotification(msgEdit)
	if !strings.HasPrefix(bodyEdit, "[EDIT] ") {
		t.Errorf("Expected body to start with '[EDIT] ', got %q", bodyEdit)
	}
}

func TestDispatchRules(t *testing.T) {
	cfg := &config.Config{
		Notifications: config.NotificationConfig{
			Enabled: false, // disabled by default
			Banner:  true,
			Sound:   true,
		},
		Hidden: []string{"Hidden Channel", "hidden@newsletter"},
		Muted:  []string{"Spam Group", "annoying@s.whatsapp.net"},
	}

	// When disabled, Dispatch returns immediately
	Dispatch(cfg, domain.Message{
		ChatID:     "123@s.whatsapp.net",
		SenderName: "Friend",
		Body:       "Hi",
	})

	// Enable notifications
	cfg.Notifications.Enabled = true

	// Self-sent message should be ignored
	Dispatch(cfg, domain.Message{
		ChatID:     "123@s.whatsapp.net",
		SenderName: "Me",
		IsFromMe:   true,
		Body:       "My own message",
	})

	// Hidden chat should be ignored
	Dispatch(cfg, domain.Message{
		ChatID:     "hidden@newsletter",
		ChatName:   "Hidden Channel",
		SenderName: "Admin",
		Body:       "Newsletter update",
	})

	// Muted chat should be ignored
	Dispatch(cfg, domain.Message{
		ChatID:     "annoying@s.whatsapp.net",
		ChatName:   "Spam Group",
		SenderName: "Spammer",
		Body:       "Buy crypto",
	})
}

func TestOnlyOnMentionNotifications(t *testing.T) {
	origBanner := sendBannerFunc
	origSound := playSoundFunc
	defer func() {
		sendBannerFunc = origBanner
		playSoundFunc = origSound
	}()

	var bannerDispatched bool
	sendBannerFunc = func(title, body string) {
		bannerDispatched = true
	}
	playSoundFunc = func(path string) {}

	cfg := &config.Config{
		Notifications: config.NotificationConfig{
			Enabled:       true,
			Banner:        true,
			Sound:         false,
			OnlyOnMention: true,
		},
	}

	// 1. Group message without mention -> should NOT trigger notification
	bannerDispatched = false
	Dispatch(cfg, domain.Message{
		ChatID:     "120363000000000000@g.us",
		ChatName:   "Tech Group",
		SenderName: "Bob",
		Body:       "Regular discussion without tags",
	})
	if bannerDispatched {
		t.Errorf("Expected NO notification for group message without mention when only_on_mention: true")
	}

	// 2. Group message with @You mention -> SHOULD trigger notification
	bannerDispatched = false
	Dispatch(cfg, domain.Message{
		ChatID:     "120363000000000000@g.us",
		ChatName:   "Tech Group",
		SenderName: "Alice",
		Body:       "Hey @You please take a look at this PR",
	})
	if !bannerDispatched {
		t.Errorf("Expected notification for group message with @You mention")
	}

	// 3. Group message with IsMentioned flag -> SHOULD trigger notification
	bannerDispatched = false
	Dispatch(cfg, domain.Message{
		ChatID:      "120363000000000000@g.us",
		ChatName:    "Tech Group",
		SenderName:  "Charlie",
		Body:        "Tagged in message",
		IsMentioned: true,
	})
	if !bannerDispatched {
		t.Errorf("Expected notification for group message with IsMentioned: true")
	}

	// 4. Direct message (1-on-1) without mention -> SHOULD trigger notification (only_on_mention only filters group chats)
	bannerDispatched = false
	Dispatch(cfg, domain.Message{
		ChatID:     "123456789@s.whatsapp.net",
		ChatName:   "Alice",
		SenderName: "Alice",
		Body:       "Direct private message",
	})
	if !bannerDispatched {
		t.Errorf("Expected notification for direct 1-on-1 message when only_on_mention is true")
	}

	// 5. When only_on_mention is false -> group messages without mention notify normally
	cfg.Notifications.OnlyOnMention = false
	bannerDispatched = false
	Dispatch(cfg, domain.Message{
		ChatID:     "120363000000000000@g.us",
		ChatName:   "Tech Group",
		SenderName: "Bob",
		Body:       "General group announcement",
	})
	if !bannerDispatched {
		t.Errorf("Expected notification for general group message when only_on_mention: false")
	}
}
