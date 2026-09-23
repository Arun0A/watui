package ui

import (
	"context"
	"testing"
	"time"

	"watui/internal/domain"
)

type mockAdapter struct {
	msgHandler    domain.MessageHandler
	statusHandler domain.StatusHandler
}

func (m *mockAdapter) Connect(ctx context.Context) error                          { return nil }
func (m *mockAdapter) Disconnect()                                                {}
func (m *mockAdapter) IsLoggedIn() bool                                           { return true }
func (m *mockAdapter) OnMessage(h domain.MessageHandler)                          { m.msgHandler = h }
func (m *mockAdapter) OnStatus(h domain.StatusHandler)                            { m.statusHandler = h }
func (m *mockAdapter) SendTextMessage(ctx context.Context, c, t string) (domain.Message, error) {
	return domain.Message{ID: "SENT1", ChatID: c, Body: t}, nil
}
func (m *mockAdapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	return []domain.Contact{
		{JID: "111@s.whatsapp.net", Name: "Alice Smith"},
		{JID: "222@s.whatsapp.net", Name: "Bob Jones"},
		{JID: "333@s.whatsapp.net", Name: "Charlie Brown"},
	}, nil
}
func (m *mockAdapter) MarkRead(ctx context.Context, c, s string, ids []string) error { return nil }

func TestUnreadModelLifecycle(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// 1. Initial state: Inbox zero
	if len(model.chatOrder) != 0 {
		t.Fatalf("Expected 0 unread chats initially, got %d", len(model.chatOrder))
	}

	// 2. Incoming message arrives -> added to unread queue
	msg1 := domain.Message{
		ID:         "MSG001",
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Alice",
		Timestamp:  time.Now(),
		Body:       "Hey there!",
	}
	model.handleIncomingMessage(msg1)

	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected 1 unread chat after message, got %d", len(model.chatOrder))
	}
	if model.unreadChats["123@s.whatsapp.net"].Messages[0].Body != "Hey there!" {
		t.Errorf("Unexpected body in unread chat")
	}

	// 3. Second message to same chat -> updates unread count
	msg2 := domain.Message{
		ID:         "MSG002",
		ChatID:     "123@s.whatsapp.net",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Alice",
		Timestamp:  time.Now(),
		Body:       "Are you online?",
	}
	model.handleIncomingMessage(msg2)

	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected still 1 chat in unread list, got %d", len(model.chatOrder))
	}
	if len(model.unreadChats["123@s.whatsapp.net"].Messages) != 2 {
		t.Errorf("Expected 2 unread messages for chat, got %d", len(model.unreadChats["123@s.whatsapp.net"].Messages))
	}

	// 4. Dismiss unread chat -> returns to inbox zero
	model.dismissUnread("123@s.whatsapp.net")
	if len(model.chatOrder) != 0 {
		t.Errorf("Expected 0 unread chats after dismiss, got %d", len(model.chatOrder))
	}
}

func TestContactFiltering(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	model.contacts = []domain.Contact{
		{JID: "111@s.whatsapp.net", Name: "Alice Smith"},
		{JID: "222@s.whatsapp.net", Name: "Bob Jones"},
		{JID: "333@s.whatsapp.net", Name: "Charlie Brown"},
	}

	// Empty query returns all
	model.filterContacts("")
	if len(model.filteredList) != 3 {
		t.Errorf("Expected 3 contacts for empty query, got %d", len(model.filteredList))
	}

	// Query 'bob' returns 1
	model.filterContacts("bob")
	if len(model.filteredList) != 1 || model.filteredList[0].Name != "Bob Jones" {
		t.Errorf("Expected Bob Jones, got %v", model.filteredList)
	}

	// Query '111' returns Alice by JID/phone
	model.filterContacts("111")
	if len(model.filteredList) != 1 || model.filteredList[0].Name != "Alice Smith" {
		t.Errorf("Expected Alice Smith, got %v", model.filteredList)
	}
}
