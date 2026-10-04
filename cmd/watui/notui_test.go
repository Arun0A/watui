package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"watui/internal/domain"
)

type mockCLIAdapter struct {
	domain.WhatsAppAdapter
	isLoggedIn    bool
	status        domain.ConnectionStatus
	sentChatID    string
	sentText      string
	sentFilePath  string
	sentCaption   string
	sendErr       error
	statusHandler domain.StatusHandler
}

func (m *mockCLIAdapter) Connect(ctx context.Context) error {
	m.status = domain.StatusConnected
	if m.statusHandler != nil {
		m.statusHandler(domain.StatusConnected)
	}
	return nil
}

func (m *mockCLIAdapter) Disconnect() {
	m.status = domain.StatusDisconnected
}

func (m *mockCLIAdapter) IsLoggedIn() bool {
	return m.isLoggedIn
}

func (m *mockCLIAdapter) OnMessage(handler domain.MessageHandler) {}

func (m *mockCLIAdapter) OnStatus(handler domain.StatusHandler) {
	m.statusHandler = handler
}

func (m *mockCLIAdapter) SendTextMessage(ctx context.Context, chatID string, text string, quotedMsg ...string) (domain.Message, error) {
	m.sentChatID = chatID
	m.sentText = text
	if m.sendErr != nil {
		return domain.Message{}, m.sendErr
	}
	return domain.Message{
		ID:        "test-msg-123",
		ChatID:    chatID,
		Body:      text,
		Type:      domain.MessageTypeText,
		Timestamp: time.Now(),
	}, nil
}

func (m *mockCLIAdapter) SendFileMessage(ctx context.Context, chatID string, filePath string, caption string, quotedMsg ...string) (domain.Message, error) {
	m.sentChatID = chatID
	m.sentFilePath = filePath
	m.sentCaption = caption
	if m.sendErr != nil {
		return domain.Message{}, m.sendErr
	}
	return domain.Message{
		ID:        "test-file-456",
		ChatID:    chatID,
		Body:      caption,
		Type:      domain.MessageTypeDocument,
		Timestamp: time.Now(),
	}, nil
}

func (m *mockCLIAdapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	return nil, nil
}

func (m *mockCLIAdapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
	return nil
}

func (m *mockCLIAdapter) OnContactsUpdated(handler func([]domain.Contact)) {}

func (m *mockCLIAdapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	return nil, nil
}

func (m *mockCLIAdapter) DismissUnread(ctx context.Context, chatID string) error { return nil }
func (m *mockCLIAdapter) Sync(ctx context.Context) error                         { return nil }
func (m *mockCLIAdapter) OnChatDismissed(handler func(string))                   {}
func (m *mockCLIAdapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	return "", nil
}
func (m *mockCLIAdapter) EnsureGroupNames(ctx context.Context, jids []string) {}
func (m *mockCLIAdapter) IsChatArchived(chatID string) bool                   { return false }
func (m *mockCLIAdapter) GetArchivedChats() map[string]bool                   { return nil }
func (m *mockCLIAdapter) SetChatArchived(ctx context.Context, chatID string, archived bool) error {
	return nil
}
func (m *mockCLIAdapter) DeleteMessage(ctx context.Context, chatID string, messageID string, deleteForEveryone bool, sender ...string) error {
	return nil
}
func (m *mockCLIAdapter) GetChatHistory(ctx context.Context, chatID string, limit int, beforeTimestamp time.Time) ([]domain.Message, error) {
	return nil, nil
}
func (m *mockCLIAdapter) GetGroupParticipants(ctx context.Context, groupJID string) ([]domain.Contact, error) {
	return nil, nil
}

func TestNormalizeJID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"123456789@s.whatsapp.net", "123456789@s.whatsapp.net"},
		{"120363430194759319@g.us", "120363430194759319@g.us"},
		{"+91 98765 43210", "919876543210@s.whatsapp.net"},
		{"919876543210", "919876543210@s.whatsapp.net"},
		{" 12345-6789 ", "123456789@s.whatsapp.net"},
	}

	for _, tt := range tests {
		got := normalizeJID(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeJID(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestReadMessageBody(t *testing.T) {
	// 1. Multiple args
	args := []string{"target_jid", "hello", "world", "from", "terminal"}
	body, err := readMessageBody(args, nil)
	if err != nil || body != "hello world from terminal" {
		t.Errorf("Expected 'hello world from terminal', got %q, err=%v", body, err)
	}

	// 2. Stdin reading with "-"
	stdinBuf := bytes.NewBufferString("piped input content\n")
	argsStdin := []string{"target_jid", "-"}
	bodyStdin, err := readMessageBody(argsStdin, stdinBuf)
	if err != nil || bodyStdin != "piped input content" {
		t.Errorf("Expected 'piped input content', got %q, err=%v", bodyStdin, err)
	}
}

func TestExecuteNoTuiSend_TextSuccess(t *testing.T) {
	adapter := &mockCLIAdapter{
		isLoggedIn: true,
	}
	var stdout, stderr bytes.Buffer

	err := executeNoTuiSend(
		context.Background(),
		adapter,
		false,
		"919876543210",
		"Hello from CLI API!",
		false,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("executeNoTuiSend failed: %v", err)
	}

	if adapter.sentChatID != "919876543210@s.whatsapp.net" {
		t.Errorf("Expected chat ID 919876543210@s.whatsapp.net, got %q", adapter.sentChatID)
	}
	if adapter.sentText != "Hello from CLI API!" {
		t.Errorf("Expected text 'Hello from CLI API!', got %q", adapter.sentText)
	}
	if !strings.Contains(stdout.String(), "Message sent successfully") {
		t.Errorf("Expected success message in stdout, got %q", stdout.String())
	}
}

func TestExecuteNoTuiSend_JSONOutput(t *testing.T) {
	adapter := &mockCLIAdapter{
		isLoggedIn: true,
	}
	var stdout, stderr bytes.Buffer

	err := executeNoTuiSend(
		context.Background(),
		adapter,
		true, // remote daemon
		"120363430194759319@g.us",
		"Build passing!",
		true,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("executeNoTuiSend failed: %v", err)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("Failed to parse JSON stdout: %v, raw: %s", err, stdout.String())
	}
	if res["status"] != "success" || res["id"] != "test-msg-123" {
		t.Errorf("Unexpected JSON output: %v", res)
	}
}

func TestExecuteNoTuiSend_SendError(t *testing.T) {
	adapter := &mockCLIAdapter{
		isLoggedIn: true,
		sendErr:    errors.New("network timeout"),
	}
	var stdout, stderr bytes.Buffer

	err := executeNoTuiSend(
		context.Background(),
		adapter,
		true,
		"12345@s.whatsapp.net",
		"Test message",
		false,
		&stdout,
		&stderr,
	)
	if err == nil {
		t.Fatalf("Expected error, got nil")
	}
	if !strings.Contains(stderr.String(), "network timeout") {
		t.Errorf("Expected error message in stderr, got %q", stderr.String())
	}
}

func TestExecuteNoTuiSend_FileAttachment(t *testing.T) {
	adapter := &mockCLIAdapter{
		isLoggedIn: true,
	}
	var stdout, stderr bytes.Buffer

	err := executeNoTuiSend(
		context.Background(),
		adapter,
		true,
		"12345@s.whatsapp.net",
		"file:///tmp/report.pdf Annual Report",
		false,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("executeNoTuiSend failed: %v", err)
	}

	if adapter.sentFilePath != "/tmp/report.pdf" {
		t.Errorf("Expected file path /tmp/report.pdf, got %q", adapter.sentFilePath)
	}
	if adapter.sentCaption != "Annual Report" {
		t.Errorf("Expected caption 'Annual Report', got %q", adapter.sentCaption)
	}
}
