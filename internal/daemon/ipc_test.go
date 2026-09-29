package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"watui/internal/domain"
)

type mockAdapter struct {
	msgHandler      domain.MessageHandler
	statusHandler   domain.StatusHandler
	contactsHandler func([]domain.Contact)
	dismissHandler  func(string)
}

func (m *mockAdapter) Connect(ctx context.Context) error { return nil }
func (m *mockAdapter) Disconnect()                       {}
func (m *mockAdapter) IsLoggedIn() bool                  { return true }
func (m *mockAdapter) OnMessage(h domain.MessageHandler) { m.msgHandler = h }
func (m *mockAdapter) OnStatus(h domain.StatusHandler)   { m.statusHandler = h }
func (m *mockAdapter) OnContactsUpdated(h func([]domain.Contact)) {
	m.contactsHandler = h
}
func (m *mockAdapter) OnChatDismissed(h func(string)) { m.dismissHandler = h }
func (m *mockAdapter) SendTextMessage(ctx context.Context, chatID string, text string) (domain.Message, error) {
	return domain.Message{ID: "TEST1", ChatID: chatID, Body: text}, nil
}
func (m *mockAdapter) SendFileMessage(ctx context.Context, chatID string, filePath string, caption string) (domain.Message, error) {
	return domain.Message{ID: "FILE1", ChatID: chatID, Body: caption}, nil
}
func (m *mockAdapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	return []domain.Contact{{JID: "friend@s.whatsapp.net", Name: "My Friend"}}, nil
}
func (m *mockAdapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
	return nil
}
func (m *mockAdapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	return []domain.Message{{ID: "U1", ChatID: "friend@s.whatsapp.net", Body: "Hello"}}, nil
}
func (m *mockAdapter) DismissUnread(ctx context.Context, chatID string) error {
	return nil
}
func (m *mockAdapter) Sync(ctx context.Context) error { return nil }
func (m *mockAdapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	return "/tmp/downloaded.jpg", nil
}
func (m *mockAdapter) EnsureGroupNames(ctx context.Context, jids []string) {}
func (m *mockAdapter) IsChatArchived(chatID string) bool                   { return chatID == "archived@g.us" }
func (m *mockAdapter) GetArchivedChats() map[string]bool {
	return map[string]bool{"archived@g.us": true}
}
func (m *mockAdapter) SetChatArchived(ctx context.Context, chatID string, archived bool) error {
	return nil
}

func TestIPCEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "watui.db")

	mock := &mockAdapter{}
	server, err := NewServer(mock, dbPath)
	if err != nil {
		t.Fatalf("Failed to create daemon server: %v", err)
	}
	defer server.Close()

	if server.HasActiveClients() {
		t.Errorf("Expected 0 active clients initially")
	}

	// Connect client
	client, err := ConnectRemote(dbPath)
	if err != nil {
		t.Fatalf("Failed to connect client: %v", err)
	}
	defer client.Disconnect()

	// Wait briefly for connection registration
	time.Sleep(50 * time.Millisecond)
	if !server.HasActiveClients() {
		t.Errorf("Expected active client")
	}

	// Verify immediate connection status dispatch
	var receivedStatus domain.ConnectionStatus
	client.OnStatus(func(s domain.ConnectionStatus) {
		receivedStatus = s
	})
	if receivedStatus != domain.StatusConnected {
		t.Errorf("Expected initial status %q, got %q", domain.StatusConnected, receivedStatus)
	}

	// Verify contacts
	contacts, err := client.GetContacts(context.Background())
	if err != nil || len(contacts) != 1 || contacts[0].Name != "My Friend" {
		t.Errorf("Unexpected contacts: %v, err: %v", contacts, err)
	}

	// Verify unread messages
	unreads, err := client.GetUnreadMessages(context.Background())
	if err != nil || len(unreads) != 1 || unreads[0].Body != "Hello" {
		t.Errorf("Unexpected unreads: %v, err: %v", unreads, err)
	}

	// Verify sending message over RPC
	sent, err := client.SendTextMessage(context.Background(), "friend@s.whatsapp.net", "Hi there!")
	if err != nil || sent.Body != "Hi there!" {
		t.Errorf("Unexpected sent message: %v, err: %v", sent, err)
	}

	// Verify archived check
	if !client.IsChatArchived("archived@g.us") {
		t.Errorf("Expected archived true for archived@g.us")
	}
	if client.IsChatArchived("friend@s.whatsapp.net") {
		t.Errorf("Expected archived false for friend")
	}

	// Verify SetChatArchived RPC
	if err := client.SetChatArchived(context.Background(), "friend@s.whatsapp.net", true); err != nil {
		t.Errorf("SetChatArchived failed: %v", err)
	}
	if !client.IsChatArchived("friend@s.whatsapp.net") {
		t.Errorf("Expected friend to be archived after SetChatArchived")
	}

	// Verify live event broadcast
	var receivedMsg domain.Message
	msgReceived := make(chan struct{})
	client.OnMessage(func(msg domain.Message) {
		receivedMsg = msg
		close(msgReceived)
	})

	mock.msgHandler(domain.Message{
		ID:     "LIVE1",
		ChatID: "friend@s.whatsapp.net",
		Body:   "Live incoming message",
	})

	select {
	case <-msgReceived:
		if receivedMsg.Body != "Live incoming message" {
			t.Errorf("Unexpected message body: %s", receivedMsg.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for live message event")
	}

	// Disconnect client
	client.Disconnect()
	time.Sleep(50 * time.Millisecond)

	if server.HasActiveClients() {
		t.Errorf("Expected 0 active clients after disconnect")
	}
}
