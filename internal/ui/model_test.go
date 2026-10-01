package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watui/internal/config"
	"watui/internal/domain"
)

type mockAdapter struct {
	msgHandler      domain.MessageHandler
	statusHandler   domain.StatusHandler
	archivedChats   map[string]bool
	lastSentText    string
	lastQuotedID    string
	dismissedChats  []string
	historyMessages map[string][]domain.Message
	participants    []domain.Contact
}

func (m *mockAdapter) Connect(ctx context.Context) error { return nil }
func (m *mockAdapter) Disconnect()                       {}
func (m *mockAdapter) IsLoggedIn() bool                  { return true }
func (m *mockAdapter) OnMessage(h domain.MessageHandler) { m.msgHandler = h }
func (m *mockAdapter) OnStatus(h domain.StatusHandler)   { m.statusHandler = h }
func (m *mockAdapter) SendTextMessage(ctx context.Context, c, t string, q ...string) (domain.Message, error) {
	m.lastSentText = t
	if len(q) > 0 {
		m.lastQuotedID = q[0]
	} else {
		m.lastQuotedID = ""
	}
	return domain.Message{ID: "SENT1", ChatID: c, Body: t, QuotedID: m.lastQuotedID}, nil
}
func (m *mockAdapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	return []domain.Contact{
		{JID: "111@s.whatsapp.net", Name: "Alice Smith"},
		{JID: "222@s.whatsapp.net", Name: "Bob Jones"},
		{JID: "333@s.whatsapp.net", Name: "Charlie Brown"},
	}, nil
}
func (m *mockAdapter) MarkRead(ctx context.Context, c, s string, ids []string) error { return nil }
func (m *mockAdapter) OnContactsUpdated(h func([]domain.Contact))                    {}
func (m *mockAdapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	return nil, nil
}
func (m *mockAdapter) DismissUnread(ctx context.Context, chatID string) error {
	m.dismissedChats = append(m.dismissedChats, chatID)
	return nil
}
func (m *mockAdapter) Sync(ctx context.Context) error                      { return nil }
func (m *mockAdapter) OnChatDismissed(h func(chatID string))               {}
func (m *mockAdapter) EnsureGroupNames(ctx context.Context, jids []string) {}
func (m *mockAdapter) IsChatArchived(chatID string) bool {
	if m.archivedChats != nil {
		return m.archivedChats[chatID]
	}
	return false
}
func (m *mockAdapter) GetArchivedChats() map[string]bool {
	res := make(map[string]bool)
	for k, v := range m.archivedChats {
		res[k] = v
	}
	return res
}
func (m *mockAdapter) SetChatArchived(ctx context.Context, chatID string, archived bool) error {
	if m.archivedChats == nil {
		m.archivedChats = make(map[string]bool)
	}
	if archived {
		m.archivedChats[chatID] = true
	} else {
		delete(m.archivedChats, chatID)
	}
	return nil
}
func (m *mockAdapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	return "/tmp/mock_media.jpg", nil
}
func (m *mockAdapter) SendFileMessage(ctx context.Context, c, filePath, caption string) (domain.Message, error) {
	return domain.Message{
		ID:       "FILESENT1",
		ChatID:   c,
		IsFromMe: true,
		Type:     domain.MessageTypeDocument,
		Body:     fmt.Sprintf("[Document: %s] %s", filepath.Base(filePath), caption),
	}, nil
}
func (m *mockAdapter) GetChatHistory(ctx context.Context, chatID string, limit int, beforeTimestamp time.Time) ([]domain.Message, error) {
	if m.historyMessages == nil {
		return nil, nil
	}
	all := m.historyMessages[chatID]
	var filtered []domain.Message
	for _, msg := range all {
		if beforeTimestamp.IsZero() || msg.Timestamp.Before(beforeTimestamp) {
			filtered = append(filtered, msg)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

func (m *mockAdapter) GetGroupParticipants(ctx context.Context, groupJID string) ([]domain.Contact, error) {
	if m.participants != nil {
		return m.participants, nil
	}
	return nil, nil
}

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

func TestRebuildUnreadChats(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Add an unread chat
	model.handleIncomingMessage(domain.Message{
		ID:         "M1",
		ChatID:     "alice@s.whatsapp.net",
		Sender:     "alice@s.whatsapp.net",
		SenderName: "Alice",
		Timestamp:  time.Now(),
		Body:       "Hello",
	})
	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected 1 unread chat, got %d", len(model.chatOrder))
	}

	// Now simulate refresh after marking read on WhatsApp Web: unread list is empty
	model.rebuildUnreadChats([]domain.Message{})
	if len(model.chatOrder) != 0 || len(model.unreadChats) != 0 {
		t.Errorf("Expected 0 unread chats after rebuild with empty messages, got %d", len(model.chatOrder))
	}
}

func TestCenteredResponsiveView(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Window resize to 120x40
	m, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	updated := m.(*Model)

	if updated.width != 120 || updated.height != 40 {
		t.Errorf("Expected 120x40 dimensions, got %dx%d", updated.width, updated.height)
	}

	viewStr := updated.View()
	if viewStr == "" {
		t.Fatalf("View output should not be empty")
	}

	// Should contain watui header
	if !strings.Contains(viewStr, "watui") {
		t.Errorf("Expected view to contain 'watui'")
	}

	// Transition to contact picker with 'n'
	cmd := updated.updateUnreadList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatalf("Expected cmd on 'n' transition")
	}
	if updated.view != ViewContactPicker {
		t.Errorf("Expected view to be ViewContactPicker, got %v", updated.view)
	}

	// Transition back with 'esc'
	escCmd := updated.updateContactPicker(tea.KeyMsg{Type: tea.KeyEsc})
	if escCmd == nil {
		t.Fatalf("Expected cmd on 'esc' transition")
	}
	if updated.view != ViewUnreadList {
		t.Errorf("Expected view to be ViewUnreadList, got %v", updated.view)
	}
}

func TestContactFilteringGroupsAndNonExistent(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	model.contacts = []domain.Contact{
		{JID: "111@s.whatsapp.net", Name: "Alice Smith"},
		{JID: "120363116925086343@g.us", Name: "Weekend Hikers", IsGroup: true},
		{JID: "120363999999999999@g.us", Name: "Book Club", IsGroup: true},
	}

	// 1. Searching for a non-existent contact name: should be completely empty
	model.filterContacts("nonexistent")
	if len(model.filteredList) != 0 {
		t.Fatalf("Expected 0 contacts for 'nonexistent', got %d: %v", len(model.filteredList), model.filteredList)
	}

	// 2. Searching for "g.us" should NOT match groups by their JID
	model.filterContacts("g.us")
	if len(model.filteredList) != 0 {
		t.Fatalf("Expected 0 contacts matching 'g.us', got %d: %v", len(model.filteredList), model.filteredList)
	}

	// 3. Searching for numeric group JID prefix "120363" should NOT match groups by JID
	model.filterContacts("120363")
	for _, c := range model.filteredList {
		if c.IsGroup {
			t.Errorf("Group should not have been matched by JID digits: %v", c)
		}
	}

	// 4. Searching for group name "hikers" should match the group
	model.filterContacts("hikers")
	if len(model.filteredList) != 1 || model.filteredList[0].Name != "Weekend Hikers" {
		t.Errorf("Expected 'Weekend Hikers', got %v", model.filteredList)
	}
}

func TestContactPickerWindowingAndPadding(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 80
	model.height = 20

	// Add 20 contacts
	for i := 1; i <= 20; i++ {
		model.contacts = append(model.contacts, domain.Contact{
			JID:  strings.Repeat("1", 8) + "@s.whatsapp.net",
			Name: "Contact Number " + strings.Repeat("A", i),
		})
	}

	model.filterContacts("")
	maxItems := model.maxVisibleContacts()
	if maxItems <= 0 {
		t.Fatalf("Expected positive maxVisibleContacts, got %d", maxItems)
	}

	// Navigate down past maxItems
	for i := 0; i < maxItems+3; i++ {
		model.updateContactPicker(tea.KeyMsg{Type: tea.KeyDown})
	}

	if model.contactCursor != maxItems+3 {
		t.Errorf("Expected contactCursor %d, got %d", maxItems+3, model.contactCursor)
	}
	if model.contactOffset <= 0 {
		t.Errorf("Expected contactOffset to scroll forward, got %d", model.contactOffset)
	}

	// Render view and check output
	viewLines := model.renderContactPickerView()
	viewStr := strings.Join(viewLines, "\n")
	if !strings.Contains(viewStr, ">") {
		t.Errorf("Expected selection cursor '>' in view")
	}

	// Now filter for non-existent contact
	model.filterContacts("zyxwvu")
	emptyLines := model.renderContactPickerView()
	emptyStr := strings.Join(emptyLines, "\n")
	if !strings.Contains(emptyStr, "No contacts found matching") {
		t.Errorf("Expected 'No contacts found matching' in empty view")
	}
	if strings.Contains(emptyStr, "Weekend Hikers") || strings.Contains(emptyStr, "Contact Number") {
		t.Errorf("Empty view must not contain leftover contact names")
	}
}

func TestChatViewportAndScrolling(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 80
	model.height = 24
	model.view = ViewChat
	model.activeChatID = "friend@s.whatsapp.net"
	model.activeName = "Best Friend"

	// Add several messages, including a long message
	for i := 1; i <= 15; i++ {
		model.activeMsgs = append(model.activeMsgs, domain.Message{
			ID:         "msg-" + strings.Repeat("x", i),
			ChatID:     model.activeChatID,
			Sender:     model.activeChatID,
			SenderName: "Best Friend",
			Timestamp:  time.Now(),
			Body:       "Message line " + strings.Repeat("Long text that wraps multiple lines in the terminal chat viewport. ", 3),
		})
	}

	// Scroll up with pgup
	model.updateChat(tea.KeyMsg{Type: tea.KeyPgUp})
	if model.chatScrollOffset <= 0 {
		t.Errorf("Expected chatScrollOffset > 0 after pgup")
	}

	// Scroll down with pgdown
	model.updateChat(tea.KeyMsg{Type: tea.KeyPgDown})

	// Scroll up with ctrl+y
	offsetBefore := model.chatScrollOffset
	model.updateChat(tea.KeyMsg{Type: tea.KeyCtrlY})
	if model.chatScrollOffset <= offsetBefore {
		t.Errorf("Expected chatScrollOffset to increase after ctrl+y, got %d <= %d", model.chatScrollOffset, offsetBefore)
	}

	// Scroll down with ctrl+e
	offsetBefore = model.chatScrollOffset
	model.updateChat(tea.KeyMsg{Type: tea.KeyCtrlE})
	if model.chatScrollOffset >= offsetBefore {
		t.Errorf("Expected chatScrollOffset to decrease after ctrl+e, got %d >= %d", model.chatScrollOffset, offsetBefore)
	}

	// Render chat view and verify it doesn't panic or exceed height
	chatLines := model.renderChatView()
	if len(chatLines) > model.height {
		t.Errorf("Chat view lines (%d) exceeded model height (%d)", len(chatLines), model.height)
	}

	// Verify that the line directly preceding the message input box is empty padding
	inputIdx := len(chatLines) - 2
	if chatLines[inputIdx-1] != "" {
		t.Errorf("Expected blank padding line directly above message input box, got: %q", chatLines[inputIdx-1])
	}
}

func TestViewLineCountNeverExceedsHeight(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Add 8 unread chats with newlines and long text like user screenshot
	for i := 1; i <= 8; i++ {
		model.handleIncomingMessage(domain.Message{
			ID:         fmt.Sprintf("msg-%d", i),
			ChatID:     fmt.Sprintf("chat-%d@s.whatsapp.net", i),
			Sender:     fmt.Sprintf("sender-%d", i),
			SenderName: fmt.Sprintf("Contact %d", i),
			Timestamp:  time.Now(),
			Body:       "Line 1 of message\nLine 2 of message https://example.com/very/long/url",
		})
	}

	for _, h := range []int{15, 20, 24, 30, 40} {
		model.width = 80
		model.height = h
		view := model.View()
		lines := strings.Split(view, "\n")
		t.Logf("Height %d: rendered %d lines", h, len(lines))
		if len(lines) > h {
			t.Errorf("For terminal height %d, View() produced %d lines (exceeded by %d lines!)",
				h, len(lines), len(lines)-h)
		}
	}
}

func TestHeaderDividerAlignment(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 120
	model.height = 30

	// Check unread view
	view := model.View()
	lines := strings.Split(view, "\n")
	var headerLine, dividerLine string
	for _, l := range lines {
		if strings.Contains(l, "watui") {
			headerLine = l
		}
		if strings.Contains(l, "───") {
			dividerLine = l
		}
	}

	if headerLine == "" || dividerLine == "" {
		t.Fatalf("Could not find header or divider in view")
	}

	// Verify both lines start with the same left padding
	headerLeadingSpaces := len(headerLine) - len(strings.TrimLeft(headerLine, " "))
	dividerLeadingSpaces := len(dividerLine) - len(strings.TrimLeft(dividerLine, " "))

	if dividerLeadingSpaces == 0 {
		t.Errorf("Divider line starts at column 0 without prefix! Expected %d spaces", headerLeadingSpaces)
	}
	if headerLeadingSpaces != dividerLeadingSpaces {
		t.Errorf("Header indent (%d) != divider indent (%d)", headerLeadingSpaces, dividerLeadingSpaces)
	}
}

func TestGroupChatLabelingAndSnippetContext(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 100
	model.height = 30

	// Set cached contacts including a group
	model.contacts = []domain.Contact{
		{
			JID:     "120363311130744191@g.us",
			Name:    "Hiking Club",
			IsGroup: true,
		},
	}

	// Message from Meghaj in the Hiking Club group
	groupMsg := domain.Message{
		ID:         "MSG_GRP_1",
		ChatID:     "120363311130744191@g.us",
		ChatName:   "Hiking Club",
		Sender:     "919876543210@s.whatsapp.net",
		SenderName: "Meghaj S",
		Timestamp:  time.Now(),
		Body:       "Who is coming this Saturday?",
	}
	model.handleIncomingMessage(groupMsg)

	// Check unread chat was created with group name, NOT sender name
	chat, exists := model.unreadChats["120363311130744191@g.us"]
	if !exists {
		t.Fatalf("Expected group chat in unreadChats")
	}
	if chat.Name != "Hiking Club" {
		t.Errorf("Expected chat.Name to be 'Hiking Club', got %q", chat.Name)
	}
	if !chat.IsGroup {
		t.Errorf("Expected chat.IsGroup to be true")
	}

	// Check view output
	viewLines := model.renderUnreadListView()
	joined := strings.Join(viewLines, "\n")

	// Title should include [Group] tag and group name
	if !strings.Contains(joined, "[Group] Hiking Club") {
		t.Errorf("View should display '[Group] Hiking Club', got:\n%s", joined)
	}

	// Snippet must include sender name prefix
	if !strings.Contains(joined, "Meghaj S: Who is coming this Saturday?") {
		t.Errorf("Snippet should include 'Meghaj S: Who is coming this Saturday?', got:\n%s", joined)
	}
}

func TestContactCacheUpdatesUnreadChatNames(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Message arrives before contacts are loaded (ChatName unknown)
	model.handleIncomingMessage(domain.Message{
		ID:         "MSG_EARLY",
		ChatID:     "120363999999999999@g.us",
		ChatName:   "",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Early Sender",
		Timestamp:  time.Now(),
		Body:       "hello group",
	})

	chat := model.unreadChats["120363999999999999@g.us"]
	if chat.Name == "Early Sender" {
		t.Errorf("Chat name should NOT be the sender's individual name 'Early Sender'")
	}

	// Now contacts load with the real group name
	model.contacts = []domain.Contact{
		{
			JID:     "120363999999999999@g.us",
			Name:    "Alpha Project",
			IsGroup: true,
		},
	}
	model.updateUnreadChatNames()

	if chat.Name != "Alpha Project" {
		t.Errorf("Expected chat.Name to update to 'Alpha Project', got %q", chat.Name)
	}
}

func TestMutedChatsIgnored(t *testing.T) {
	adapter := &mockAdapter{}
	cfg := &config.Config{
		Mute: []string{
			"Spam Group",
			"9998887777",
			"120363000000000000@g.us",
		},
	}
	model := NewModel(context.Background(), adapter, cfg)

	// 1. Message to muted group by name
	model.handleIncomingMessage(domain.Message{
		ID:         "M1",
		ChatID:     "120363111111111111@g.us",
		ChatName:   "Spam Group",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Spammer",
		Timestamp:  time.Now(),
		Body:       "Buy crypto now!",
	})

	if len(model.chatOrder) != 0 {
		t.Errorf("Expected 0 unread chats for muted group by name, got %d", len(model.chatOrder))
	}

	// 2. Message to muted contact by phone
	model.handleIncomingMessage(domain.Message{
		ID:         "M2",
		ChatID:     "9998887777@s.whatsapp.net",
		ChatName:   "Unknown",
		Sender:     "9998887777@s.whatsapp.net",
		SenderName: "Caller",
		Timestamp:  time.Now(),
		Body:       "Limited offer",
	})

	if len(model.chatOrder) != 0 {
		t.Errorf("Expected 0 unread chats for muted phone, got %d", len(model.chatOrder))
	}

	// 3. Message to muted JID
	model.handleIncomingMessage(domain.Message{
		ID:         "M3",
		ChatID:     "120363000000000000@g.us",
		ChatName:   "Another Group",
		Sender:     "123@s.whatsapp.net",
		SenderName: "Member",
		Timestamp:  time.Now(),
		Body:       "Spam message",
	})

	if len(model.chatOrder) != 0 {
		t.Errorf("Expected 0 unread chats for muted JID, got %d", len(model.chatOrder))
	}

	// 4. Message to legitimate unmuted contact
	model.handleIncomingMessage(domain.Message{
		ID:         "M4",
		ChatID:     "1112223333@s.whatsapp.net",
		ChatName:   "Alice Friend",
		Sender:     "1112223333@s.whatsapp.net",
		SenderName: "Alice",
		Timestamp:  time.Now(),
		Body:       "Are we meeting today?",
	})

	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected 1 unread chat for regular message, got %d", len(model.chatOrder))
	}
}

func TestPinnedChatsLifecycle(t *testing.T) {
	adapter := &mockAdapter{}
	cfg := &config.Config{
		Pin: []string{
			"Work Team",
			"Alice Smith",
		},
	}
	model := NewModel(context.Background(), adapter, cfg)
	model.width = 100
	model.height = 30

	// 1. Initial state: Both pinned chats exist even with 0 unread
	if len(model.chatOrder) != 2 {
		t.Fatalf("Expected 2 pinned chats initially, got %d", len(model.chatOrder))
	}

	// View should display pinned chats with PIN badges
	view := model.renderUnreadListView()
	joinedView := strings.Join(view, "\n")
	if !strings.Contains(joinedView, "* Work Team") {
		t.Errorf("Expected view to show '* Work Team', got:\n%s", joinedView)
	}
	if !strings.Contains(joinedView, "* Alice Smith") {
		t.Errorf("Expected view to show '* Alice Smith', got:\n%s", joinedView)
	}
	if !strings.Contains(joinedView, "(pinned · press enter to chat)") {
		t.Errorf("Expected view to show pinned snippet fallback")
	}

	// 2. Incoming message arrives for an UNPINNED chat (Charlie)
	model.handleIncomingMessage(domain.Message{
		ID:         "UNPINNED_1",
		ChatID:     "333@s.whatsapp.net",
		ChatName:   "Charlie",
		Sender:     "333@s.whatsapp.net",
		SenderName: "Charlie",
		Timestamp:  time.Now(),
		Body:       "Hey watui",
	})

	// Pinned chats should remain at index 0 and 1, unpinned at index 2
	if len(model.chatOrder) != 3 {
		t.Fatalf("Expected 3 chats in list, got %d", len(model.chatOrder))
	}
	if !model.unreadChats[model.chatOrder[0]].IsPinned {
		t.Errorf("Expected chat at index 0 to be pinned")
	}
	if !model.unreadChats[model.chatOrder[1]].IsPinned {
		t.Errorf("Expected chat at index 1 to be pinned")
	}
	if model.unreadChats[model.chatOrder[2]].IsPinned {
		t.Errorf("Expected chat at index 2 to be unpinned")
	}

	// 3. Message arrives for pinned Work Team
	model.handleIncomingMessage(domain.Message{
		ID:         "WORK_1",
		ChatID:     "Work Team",
		ChatName:   "Work Team",
		Sender:     "boss@s.whatsapp.net",
		SenderName: "Boss",
		Timestamp:  time.Now(),
		Body:       "Sprint planning at 10",
	})

	workChat := model.unreadChats["Work Team"]
	if len(workChat.Messages) != 1 {
		t.Errorf("Expected 1 unread message in Work Team, got %d", len(workChat.Messages))
	}

	// 4. Dismiss unread on Work Team: messages cleared, but chat STAYS pinned at top!
	model.dismissUnread("Work Team")
	if len(workChat.Messages) != 0 {
		t.Errorf("Expected messages cleared after dismiss")
	}
	if len(model.chatOrder) != 3 {
		t.Errorf("Expected pinned chat to stay in list after dismiss, len=%d", len(model.chatOrder))
	}

	// 5. Open pinned chat with Enter: enters chat view
	model.cursor = 0
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if model.view != ViewChat {
		t.Errorf("Expected view to transition to ViewChat, got %v", model.view)
	}
	if model.activeChatID != model.chatOrder[0] {
		t.Errorf("Expected activeChatID to match selected pinned chat")
	}
}

func TestPinnedAndMutedByJID(t *testing.T) {
	adapter := &mockAdapter{}
	cfg := &config.Config{
		Pin: []string{
			"120363311130744191@g.us",
			"919635706699@s.whatsapp.net",
		},
		Mute: []string{
			"120363999999999999@g.us",
			"+91 98765 43210",
		},
	}
	model := NewModel(context.Background(), adapter, cfg)
	model.width = 100
	model.height = 30

	// Initially, pinned chats exist
	if len(model.chatOrder) != 2 {
		t.Fatalf("Expected 2 pinned chats, got %d", len(model.chatOrder))
	}

	// Now contacts load
	contacts := []domain.Contact{
		{
			JID:     "120363311130744191@g.us",
			Name:    "Dev Team",
			IsGroup: true,
		},
		{
			JID:      "919635706699@s.whatsapp.net",
			Name:     "Alice Partner",
			PushName: "Alice",
		},
	}
	model.Update(contactsLoadedMsg(contacts))

	// Names should now be resolved
	devChat := model.unreadChats["120363311130744191@g.us"]
	if devChat == nil || devChat.Name != "Dev Team" {
		t.Fatalf("Expected Dev Team name resolution, got: %+v", devChat)
	}
	aliceChat := model.unreadChats["919635706699@s.whatsapp.net"]
	if aliceChat == nil || aliceChat.Name != "Alice Partner" {
		t.Fatalf("Expected Alice Partner name resolution, got: %+v", aliceChat)
	}

	// Verify view rendering
	view := model.renderUnreadListView()
	joinedView := strings.Join(view, "\n")
	if !strings.Contains(joinedView, "* [Group] Dev Team") {
		t.Errorf("Expected view to show '* [Group] Dev Team', got:\n%s", joinedView)
	}
	if !strings.Contains(joinedView, "* Alice Partner") {
		t.Errorf("Expected view to show '* Alice Partner', got:\n%s", joinedView)
	}

	// Incoming message to pinned group (even with device suffix :1@g.us)
	model.handleIncomingMessage(domain.Message{
		ID:         "DEV_MSG_1",
		ChatID:     "120363311130744191:1@g.us",
		ChatName:   "Dev Team",
		Sender:     "bob@s.whatsapp.net",
		SenderName: "Bob",
		Timestamp:  time.Now(),
		Body:       "PR merged",
	})

	if len(devChat.Messages) != 1 {
		t.Errorf("Expected 1 message in devChat, got %d", len(devChat.Messages))
	}

	// Incoming message to muted JID
	model.handleIncomingMessage(domain.Message{
		ID:         "MUTED_1",
		ChatID:     "120363999999999999@g.us",
		ChatName:   "Spam Group",
		Sender:     "spammer@s.whatsapp.net",
		SenderName: "Spammer",
		Timestamp:  time.Now(),
		Body:       "Spam notification",
	})
	if len(model.chatOrder) != 2 {
		t.Errorf("Expected muted group JID to be ignored, got %d chats", len(model.chatOrder))
	}

	// Incoming message to muted phone number formatted with country code
	model.handleIncomingMessage(domain.Message{
		ID:         "MUTED_2",
		ChatID:     "919876543210@s.whatsapp.net",
		ChatName:   "Promotions",
		Sender:     "919876543210@s.whatsapp.net",
		SenderName: "Promo",
		Timestamp:  time.Now(),
		Body:       "Claim discount",
	})
	if len(model.chatOrder) != 2 {
		t.Errorf("Expected muted phone to be ignored, got %d chats", len(model.chatOrder))
	}
}

func TestMediaPreviewKeybindings(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Add an unread chat with two media messages
	model.handleIncomingMessage(domain.Message{
		ID:         "IMG_001",
		ChatID:     "12345@s.whatsapp.net",
		ChatName:   "Friend",
		Sender:     "12345@s.whatsapp.net",
		SenderName: "Friend",
		Timestamp:  time.Now().Add(-1 * time.Minute),
		Type:       domain.MessageTypeImage,
		Body:       "[Image] First photo",
	})
	model.handleIncomingMessage(domain.Message{
		ID:         "VID_002",
		ChatID:     "12345@s.whatsapp.net",
		ChatName:   "Friend",
		Sender:     "12345@s.whatsapp.net",
		SenderName: "Friend",
		Timestamp:  time.Now(),
		Type:       domain.MessageTypeVideo,
		Body:       "[Video] Vacation clip",
	})

	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected 1 chat, got %d", len(model.chatOrder))
	}

	// 1. In ViewUnreadList: 'p' is no longer a preview hotkey
	pCmd := model.updateUnreadList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if pCmd != nil {
		t.Errorf("Expected 'p' to not trigger preview in unread list")
	}

	// 'alt+p' triggers preview in unread list
	altPCmd := model.updateUnreadList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if altPCmd == nil {
		t.Errorf("Expected non-nil cmd when pressing alt+p on media chat")
	}
	if !strings.Contains(model.previewStatus, "Downloading video") {
		t.Errorf("Expected previewStatus to indicate downloading video, got %q", model.previewStatus)
	}

	// Simulate media preview success message
	model.Update(mediaPreviewSuccessMsg{Path: "/tmp/mock_media.mp4"})
	if model.previewStatus != "" {
		t.Errorf("Expected no lingering preview status, got %q", model.previewStatus)
	}
	if !model.confirmSave {
		t.Errorf("Expected confirmSave to be true after preview success")
	}

	// Pressing 'n' declines saving to Downloads
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if model.confirmSave {
		t.Errorf("Expected confirmSave to be false after declining")
	}

	// 2. Open chat to enter ViewChat
	model.updateUnreadList(tea.KeyMsg{Type: tea.KeyEnter})
	if model.view != ViewChat {
		t.Fatalf("Expected view to be ViewChat, got %v", model.view)
	}

	// ctrl+p is no longer a preview hotkey
	ctrlPCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyCtrlP})
	if ctrlPCmd != nil {
		t.Errorf("Expected ctrl+p to not trigger preview in chat view")
	}

	// Check media navigation: initially pointing to latest (index 1 / 2)
	mediaIndices := model.getChatMediaIndices()
	if len(mediaIndices) != 2 {
		t.Fatalf("Expected 2 media messages, got %d", len(mediaIndices))
	}
	if model.selectedMediaIdx != 1 {
		t.Errorf("Expected selectedMediaIdx to default to 1, got %d", model.selectedMediaIdx)
	}

	// Navigate to previous media using Alt+K (Alt+Shift+k)
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}, Alt: true})
	if model.selectedMediaIdx != 0 {
		t.Errorf("Expected selectedMediaIdx to be 0 after Alt+K, got %d", model.selectedMediaIdx)
	}

	// Press 'p' while hovering to preview the first media (image)
	chatPCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if chatPCmd == nil {
		t.Errorf("Expected non-nil cmd when pressing 'p' on media message")
	}
	if !strings.Contains(model.previewStatus, "Downloading image") {
		t.Errorf("Expected previewStatus to indicate downloading image, got %q", model.previewStatus)
	}

	// Simulate preview success and save confirmation with 'y'
	tmpFile := filepath.Join(t.TempDir(), "test_preview.jpg")
	_ = os.WriteFile(tmpFile, []byte("fake image data"), 0644)
	model.Update(mediaPreviewSuccessMsg{Path: tmpFile})

	if !model.confirmSave {
		t.Errorf("Expected confirmSave to be true")
	}

	// Confirm save with 'y'
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if model.confirmSave {
		t.Errorf("Expected confirmSave to be reset to false")
	}
	if !strings.Contains(model.previewStatus, "Saved to") {
		t.Errorf("Expected previewStatus to say 'Saved to...', got %q", model.previewStatus)
	}

	// Test Alt+X stops media playback
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}, Alt: true})
	if !strings.Contains(model.previewStatus, "Playback stopped") {
		t.Errorf("Expected previewStatus to say 'Playback stopped', got %q", model.previewStatus)
	}
}

func TestDocumentActionKeybindings(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Add an unread chat with a Document message
	model.handleIncomingMessage(domain.Message{
		ID:         "DOC_001",
		ChatID:     "12345@s.whatsapp.net",
		ChatName:   "Colleague",
		Sender:     "12345@s.whatsapp.net",
		SenderName: "Colleague",
		Timestamp:  time.Now(),
		Type:       domain.MessageTypeDocument,
		Body:       "[Document: Report.pdf]",
	})

	// 1. In ViewUnreadList: Press Alt+P on the document chat
	altPCmd := model.updateUnreadList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	// Should NOT download yet; should set confirmDocAction to true
	if altPCmd != nil {
		t.Errorf("Expected nil cmd before user chooses open or save for document")
	}
	if !model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be true")
	}

	// 2. Pressing 's' chooses to Save to Downloads
	m, saveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = m.(*Model)
	if saveCmd == nil {
		t.Errorf("Expected non-nil saveCmd after pressing 's'")
	}
	if model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be reset to false after choosing 's'")
	}

	// 3. Open chat to test in ViewChat
	model.updateUnreadList(tea.KeyMsg{Type: tea.KeyEnter})
	if model.view != ViewChat {
		t.Fatalf("Expected ViewChat, got %v", model.view)
	}

	// Press Alt+P in ViewChat
	chatAltPCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if chatAltPCmd != nil {
		t.Errorf("Expected nil cmd before choice in chat view")
	}
	if !model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be true in chat view")
	}

	// Pressing 'o' chooses to Open
	m2, openCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	model = m2.(*Model)
	if openCmd == nil {
		t.Errorf("Expected non-nil openCmd after pressing 'o'")
	}
	if model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be reset to false after choosing 'o'")
	}

	// Press Alt+P then 'esc' to cancel before downloading
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if !model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be true")
	}
	m3, escCmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m3.(*Model)
	if escCmd != nil {
		t.Errorf("Expected nil cmd on esc cancellation")
	}
	if model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be false after esc")
	}

	// Press Alt+P then 'w' to choose "Open with"
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if !model.confirmDocAction {
		t.Errorf("Expected confirmDocAction to be true")
	}
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	model = m4.(*Model)
	if !model.promptOpenWith {
		t.Errorf("Expected promptOpenWith to be true after pressing 'w'")
	}
	model.openWithInput.SetValue("nvim")
	m5, openWithCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m5.(*Model)
	if model.promptOpenWith {
		t.Errorf("Expected promptOpenWith to be false after pressing Enter")
	}
	if openWithCmd == nil {
		t.Errorf("Expected non-nil openWithCmd after pressing Enter")
	}
}

func TestFileAttachmentAndSending(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// 1. Test parseFileURI
	tmpDir := t.TempDir()
	sampleFile := filepath.Join(tmpDir, "report.pdf")
	_ = os.WriteFile(sampleFile, []byte("pdf content"), 0644)

	path, caption := parseFileURI("file://" + sampleFile + " Annual Report 2026")
	if path != sampleFile {
		t.Errorf("Expected path %q, got %q", sampleFile, path)
	}
	if caption != "Annual Report 2026" {
		t.Errorf("Expected caption 'Annual Report 2026', got %q", caption)
	}

	// Test quoted path with spaces
	quotedInput := fmt.Sprintf("file://\"%s\" Please review ASAP", sampleFile)
	qPath, qCap := parseFileURI(quotedInput)
	if qPath != sampleFile {
		t.Errorf("Expected qPath %q, got %q", sampleFile, qPath)
	}
	if qCap != "Please review ASAP" {
		t.Errorf("Expected qCap 'Please review ASAP', got %q", qCap)
	}

	// 2. Test sending attachment in ViewChat
	model.activeChatID = "12345@s.whatsapp.net"
	model.view = ViewChat
	model.input.SetValue("file://" + sampleFile + " Sent via Watui")

	cmd := model.updateChat(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("Expected non-nil cmd for Enter with file:// attachment")
	}
	msg := cmd()
	sent, ok := msg.(messageSentMsg)
	if !ok {
		t.Fatalf("Expected messageSentMsg, got %T: %v", msg, msg)
	}
	if sent.Type != domain.MessageTypeDocument {
		t.Errorf("Expected MessageTypeDocument, got %v", sent.Type)
	}
	if !strings.Contains(sent.Body, "report.pdf") {
		t.Errorf("Expected sent.Body to contain filename, got %q", sent.Body)
	}
	if !strings.Contains(sent.Body, "Sent via Watui") {
		t.Errorf("Expected sent.Body to contain caption, got %q", sent.Body)
	}

	// 3. Test filePickedMsg updates input box
	m, _ := model.Update(filePickedMsg{Path: sampleFile})
	updatedModel := m.(*Model)
	if !strings.HasPrefix(updatedModel.input.Value(), "file://"+sampleFile) {
		t.Errorf("Expected input to have file:// prefix, got %q", updatedModel.input.Value())
	}
}

func TestSendingStatusAndFilePickerConfig(t *testing.T) {
	ctx := context.Background()
	mock := &mockAdapter{}
	cfg := &config.Config{
		FilePicker: "echo /tmp/picked_by_custom_cmd.txt",
	}
	model := NewModel(ctx, mock, cfg)
	model.activeChatID = "12345@s.whatsapp.net"
	model.view = ViewChat

	// 1. When Enter is pressed on input, previewStatus becomes "Sending..."
	model.input.SetValue("hello world")
	cmd := model.updateChat(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("Expected non-nil cmd on Enter")
	}
	if model.previewStatus != "Sending..." {
		t.Errorf("Expected previewStatus to be 'Sending...', got %q", model.previewStatus)
	}

	// 2. When messageSentMsg arrives, previewStatus is cleared
	sentMsg := cmd()
	m, _ := model.Update(sentMsg)
	updated := m.(*Model)
	if updated.previewStatus != "" {
		t.Errorf("Expected previewStatus to be cleared after messageSentMsg, got %q", updated.previewStatus)
	}

	// 3. When sendErrMsg arrives, previewStatus is also cleared
	model.previewStatus = "Sending..."
	m, _ = model.Update(sendErrMsg{ChatID: model.activeChatID, Err: errors.New("network failure")})
	updated = m.(*Model)
	if updated.previewStatus != "" {
		t.Errorf("Expected previewStatus to be cleared after sendErrMsg, got %q", updated.previewStatus)
	}

	// 4. Test custom file picker execution
	pickCmd := model.pickFileCmd()
	res := pickCmd()
	picked, ok := res.(filePickedMsg)
	if !ok {
		t.Fatalf("Expected filePickedMsg from custom file picker, got %T: %v", res, res)
	}
	if picked.Path != "/tmp/picked_by_custom_cmd.txt" {
		t.Errorf("Expected path '/tmp/picked_by_custom_cmd.txt', got %q", picked.Path)
	}

	// 5. Test short brief message on errNoFilePicker
	m, _ = model.Update(filePickErrMsg{Err: errNoFilePicker})
	updated = m.(*Model)
	if updated.previewStatus != "No file picker found" {
		t.Errorf("Expected 'No file picker found', got %q", updated.previewStatus)
	}
	if len(updated.previewStatus) > 30 {
		t.Errorf("Status message too long for compact terminals: %d chars", len(updated.previewStatus))
	}
}

func TestResolvePickerAndTerminalPickerCmd(t *testing.T) {
	// 1. Terminal file picker detection
	cases := []struct {
		cmd          string
		expectedType string
		isTerm       bool
	}{
		{"yazi", "yazi", true},
		{"yazi --chooser-file=/tmp/foo && cat /tmp/foo", "yazi", true},
		{"ranger", "ranger", true},
		{"lf", "lf", true},
		{"nnn", "nnn", true},
		{"fzf", "fzf", true},
		{"zenity --file-selection", "", false},
		{"kdialog --getopenfilename", "", false},
	}

	for _, tc := range cases {
		pType, isTerm := resolvePicker(tc.cmd)
		if isTerm != tc.isTerm {
			t.Errorf("Cmd %q: expected isTerm=%v, got %v", tc.cmd, tc.isTerm, isTerm)
		}
		if pType != tc.expectedType {
			t.Errorf("Cmd %q: expected type=%q, got %q", tc.cmd, tc.expectedType, pType)
		}
	}

	// 2. buildTerminalPickerCmd for yazi
	cmd, outPath, err := buildTerminalPickerCmd("yazi", "yazi")
	if err != nil {
		t.Fatalf("buildTerminalPickerCmd failed: %v", err)
	}
	defer func() { _ = os.Remove(outPath) }()

	if cmd.Path != "yazi" && !strings.HasSuffix(cmd.Path, "yazi") {
		t.Errorf("Expected cmd to execute yazi, got %q", cmd.Path)
	}
	hasChooserArg := false
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--chooser-file=") {
			hasChooserArg = true
			if !strings.Contains(arg, outPath) {
				t.Errorf("Expected --chooser-file to contain outPath, got %q", arg)
			}
		}
	}
	if !hasChooserArg {
		t.Errorf("Expected --chooser-file arg in %v", cmd.Args)
	}
}

func TestChatHeaderDisplaysJIDOnlyInChatWindow(t *testing.T) {
	adapter := &mockAdapter{}
	m := NewModel(context.Background(), adapter)
	m.width = 80
	m.height = 24

	testJID := "919007088779-1525273602@g.us"
	m.activeChatID = testJID
	m.activeName = "Special Group"

	// 1. In ViewUnreadList: JID should NOT appear
	m.view = ViewUnreadList
	unreadView := m.View()
	if strings.Contains(unreadView, testJID) {
		t.Errorf("JID %q should NOT appear in ViewUnreadList", testJID)
	}

	// 2. In ViewContactPicker: JID should NOT appear
	m.view = ViewContactPicker
	contactView := m.View()
	if strings.Contains(contactView, testJID) {
		t.Errorf("JID %q should NOT appear in ViewContactPicker", testJID)
	}

	// 3. In ViewChat: JID MUST appear in the top-right header
	m.view = ViewChat
	chatLines := m.renderChatView()
	if len(chatLines) == 0 {
		t.Fatalf("Expected non-empty chat lines")
	}
	headerLine := chatLines[0]
	if !strings.Contains(headerLine, testJID) {
		t.Errorf("Expected chat header to contain JID %q, got: %s", testJID, headerLine)
	}
	if !strings.Contains(headerLine, "Special Group") {
		t.Errorf("Expected chat header to contain active name, got: %s", headerLine)
	}
}

func TestPinnedGroupJIDResolution(t *testing.T) {
	groupJID := "919007088779-1525273602@g.us"
	cfg := &config.Config{
		Pin: []string{groupJID},
	}

	var requestedJIDs []string
	adapter := &mockAdapter{}
	// Custom adapter tracking EnsureGroupNames
	adapterTracker := &mockGroupAdapter{
		mockAdapter: *adapter,
		onEnsure: func(jids []string) {
			requestedJIDs = append(requestedJIDs, jids...)
		},
	}

	m := NewModel(context.Background(), adapterTracker, cfg)

	// 1. Initial pinned chat name must NOT be raw JID with "@g.us"
	chat, ok := m.unreadChats[groupJID]
	if !ok {
		t.Fatalf("Expected pinned chat for %s", groupJID)
	}
	if strings.Contains(chat.Name, "@g.us") {
		t.Errorf("Pinned chat name must NOT contain '@g.us', got %q", chat.Name)
	}
	if chat.Name != "Group (919007088779-1525273602)" {
		t.Errorf("Expected fallback 'Group (919007088779-1525273602)', got %q", chat.Name)
	}

	// 2. EnsureGroupNames should have been requested
	if len(requestedJIDs) != 1 || requestedJIDs[0] != groupJID {
		t.Errorf("Expected EnsureGroupNames with %s, got %v", groupJID, requestedJIDs)
	}

	// 3. Now simulate contacts update providing the real group name
	m.Update(contactsLoadedMsg([]domain.Contact{
		{
			JID:     groupJID,
			Name:    "Family Vacation 2024",
			IsGroup: true,
		},
	}))

	updatedChat := m.unreadChats[groupJID]
	if updatedChat.Name != "Family Vacation 2024" {
		t.Errorf("Expected updated chat name 'Family Vacation 2024', got %q", updatedChat.Name)
	}

	// 4. Also verify group names containing '@' (like "1️⃣ 2027-Kareer School @ CSE") are accepted
	specialGroupJID := "120363336014495861@g.us"
	m.Update(contactsLoadedMsg([]domain.Contact{
		{
			JID:     specialGroupJID,
			Name:    "1️⃣ 2027-Kareer School @ CSE",
			IsGroup: true,
		},
	}))
	resolvedName, isGrp := m.resolveChatName(specialGroupJID, "", "")
	if resolvedName != "1️⃣ 2027-Kareer School @ CSE" || !isGrp {
		t.Errorf("Expected '1️⃣ 2027-Kareer School @ CSE', got %q (isGroup=%v)", resolvedName, isGrp)
	}
}

func TestIsRealChatName(t *testing.T) {
	tests := []struct {
		name     string
		chatID   string
		expected bool
	}{
		{"", "123@s.whatsapp.net", false},
		{"123@s.whatsapp.net", "123@s.whatsapp.net", false},
		{"120363336014495861@g.us", "120363336014495861@g.us", false},
		{"Group (120363336014495861)", "120363336014495861@g.us", false},
		{"120363336014495861", "120363336014495861@g.us", false},
		{"919007088779-1525273602", "919007088779-1525273602@g.us", false},
		{"1️⃣ 2027-Kareer School @ CSE", "120363336014495861@g.us", true},
		{"Tech Team @ HQ", "120363999999999999@g.us", true},
		{"Alice Smith", "1234567890@s.whatsapp.net", true},
	}
	for _, tt := range tests {
		got := isRealChatName(tt.name, tt.chatID)
		if got != tt.expected {
			t.Errorf("isRealChatName(%q, %q) = %v; want %v", tt.name, tt.chatID, got, tt.expected)
		}
	}
}

type mockGroupAdapter struct {
	mockAdapter
	onEnsure func(jids []string)
}

func (m *mockGroupAdapter) EnsureGroupNames(ctx context.Context, jids []string) {
	if m.onEnsure != nil {
		m.onEnsure(jids)
	}
}

func TestContactPickerShowsAllAndFiltersAll(t *testing.T) {
	adapter := &mockAdapter{}
	m := NewModel(context.Background(), adapter)

	// Create 60 contacts
	var allContacts []domain.Contact
	for i := 1; i <= 60; i++ {
		allContacts = append(allContacts, domain.Contact{
			JID:     fmt.Sprintf("user%d@s.whatsapp.net", i),
			Name:    fmt.Sprintf("Contact %02d", i),
			IsGroup: false,
		})
	}
	// Add 5 groups
	for i := 1; i <= 5; i++ {
		allContacts = append(allContacts, domain.Contact{
			JID:     fmt.Sprintf("120363000%d@g.us", i),
			Name:    fmt.Sprintf("Group Alpha %d", i),
			IsGroup: true,
		})
	}

	m.contacts = allContacts

	// 1. Empty filter should show all 65 contacts/groups, not truncated to 40
	m.filterContacts("")
	if len(m.filteredList) != 65 {
		t.Errorf("Expected 65 contacts in filtered list, got %d", len(m.filteredList))
	}

	// 2. Search query matching all 60 contacts ("contact") should return all 60, not capped at 40
	m.filterContacts("contact")
	if len(m.filteredList) != 60 {
		t.Errorf("Expected 60 matching contacts, got %d", len(m.filteredList))
	}

	// 3. Search query matching groups ("alpha") should return all 5 groups
	m.filterContacts("alpha")
	if len(m.filteredList) != 5 {
		t.Errorf("Expected 5 matching groups, got %d", len(m.filteredList))
	}
}

func TestArchivedChatsFilteringAndToggle(t *testing.T) {
	adapter := &mockAdapter{
		archivedChats: map[string]bool{
			"archived_group@g.us": true,
		},
	}
	m := NewModel(context.Background(), adapter)
	m.width = 80
	m.height = 24

	// Message 1 to active chat
	m.handleIncomingMessage(domain.Message{
		ID:         "M1",
		ChatID:     "normal@s.whatsapp.net",
		ChatName:   "Active Friend",
		Body:       "Hello active",
		Timestamp:  time.Now(),
		Sender:     "normal@s.whatsapp.net",
		SenderName: "Active Friend",
	})

	// Message 2 to archived group
	m.handleIncomingMessage(domain.Message{
		ID:         "M2",
		ChatID:     "archived_group@g.us",
		ChatName:   "Archived High Volume",
		Body:       "Spam update",
		Timestamp:  time.Now(),
		Sender:     "sender@s.whatsapp.net",
		SenderName: "Group Member",
	})

	// 1. By default, showArchived is false: only normal chat is in chatOrder
	if len(m.chatOrder) != 1 {
		t.Fatalf("Expected 1 visible chat in unread list, got %d", len(m.chatOrder))
	}
	if m.chatOrder[0] != "normal@s.whatsapp.net" {
		t.Errorf("Expected active chat in unread list, got %s", m.chatOrder[0])
	}

	viewStr := strings.Join(m.renderUnreadListView(), "\n")
	if !strings.Contains(viewStr, "Active Friend") {
		t.Errorf("Expected 'Active Friend' in unread list view")
	}
	if strings.Contains(viewStr, "Archived High Volume") {
		t.Errorf("Archived chat should NOT appear in default unread list view")
	}
	if !strings.Contains(viewStr, "[a] 1 archived") {
		t.Errorf("Expected '[a] 1 archived' badge in header, got view:\n%s", viewStr)
	}

	// 2. Press 'a' to toggle into archived view
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !m.showArchived {
		t.Fatalf("Expected showArchived to be true after pressing 'a'")
	}
	if len(m.chatOrder) != 1 {
		t.Fatalf("Expected 1 chat in archived list, got %d", len(m.chatOrder))
	}
	if m.chatOrder[0] != "archived_group@g.us" {
		t.Errorf("Expected archived chat in archived list, got %s", m.chatOrder[0])
	}

	archivedViewStr := strings.Join(m.renderUnreadListView(), "\n")
	if !strings.Contains(archivedViewStr, "Archived High Volume") {
		t.Errorf("Expected 'Archived High Volume' in archived list view, got:\n%s", archivedViewStr)
	}
	if strings.Contains(archivedViewStr, "Active Friend") {
		t.Errorf("Active unread chat should NOT appear in archived list view")
	}
	if !strings.Contains(archivedViewStr, "[ARCHIVED CHATS]") {
		t.Errorf("Expected '[ARCHIVED CHATS]' in header, got view:\n%s", archivedViewStr)
	}

	// 3. Press 'esc' to toggle back to unread view
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.showArchived {
		t.Fatalf("Expected showArchived to be false after pressing 'esc'")
	}
	if len(m.chatOrder) != 1 || m.chatOrder[0] != "normal@s.whatsapp.net" {
		t.Errorf("Expected back to active unread chats, got %v", m.chatOrder)
	}
}

func TestArchiveUnarchiveShiftA(t *testing.T) {
	adapter := &mockAdapter{
		archivedChats: make(map[string]bool),
	}
	m := NewModel(context.Background(), adapter)
	m.width = 100
	m.height = 30

	m.handleIncomingMessage(domain.Message{
		ID:         "M1",
		ChatID:     "chat1@s.whatsapp.net",
		ChatName:   "Alice",
		Body:       "Hello from Alice",
		Timestamp:  time.Now().Add(-1 * time.Minute),
		Sender:     "chat1@s.whatsapp.net",
		SenderName: "Alice",
	})
	m.handleIncomingMessage(domain.Message{
		ID:         "M2",
		ChatID:     "chat2@s.whatsapp.net",
		ChatName:   "Bob",
		Body:       "Hello from Bob",
		Timestamp:  time.Now(),
		Sender:     "chat2@s.whatsapp.net",
		SenderName: "Bob",
	})

	if len(m.chatOrder) != 2 {
		t.Fatalf("Expected 2 chats in active unread list, got %d", len(m.chatOrder))
	}

	// Cursor is at 0 (Bob, due to latest timestamp). Move cursor to Alice or archive top chat.
	topChatID := m.chatOrder[0]
	otherChatID := m.chatOrder[1]

	// 1. Press Shift+A ('A') on top chat to archive it
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	time.Sleep(10 * time.Millisecond)

	if !adapter.IsChatArchived(topChatID) {
		t.Errorf("Expected %s to be archived in adapter", topChatID)
	}
	if !m.unreadChats[topChatID].IsArchived {
		t.Errorf("Expected %s to have IsArchived=true in model", topChatID)
	}
	if len(m.chatOrder) != 1 || m.chatOrder[0] != otherChatID {
		t.Errorf("Expected only %s in active list after archiving top chat, got %v", otherChatID, m.chatOrder)
	}

	// 2. Press 'a' to enter archived view
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !m.showArchived {
		t.Fatalf("Expected to be in archived view")
	}
	if len(m.chatOrder) != 1 || m.chatOrder[0] != topChatID {
		t.Fatalf("Expected %s in archived view, got %v", topChatID, m.chatOrder)
	}

	// 3. Press Shift+A ('A') in archived view to unarchive top chat
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	time.Sleep(10 * time.Millisecond)

	if adapter.IsChatArchived(topChatID) {
		t.Errorf("Expected %s to be unarchived in adapter", topChatID)
	}
	if m.unreadChats[topChatID].IsArchived {
		t.Errorf("Expected %s to have IsArchived=false in model", topChatID)
	}
	if len(m.chatOrder) != 0 {
		t.Errorf("Expected 0 chats in archived view after unarchiving, got %d", len(m.chatOrder))
	}

	// 4. Press 'a' to return to unread list
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.showArchived {
		t.Fatalf("Expected to return to active unread view")
	}
	if len(m.chatOrder) != 2 {
		t.Fatalf("Expected both chats back in active unread view, got %d", len(m.chatOrder))
	}
}

func TestHelpToggleAndDynamicMediaBinds(t *testing.T) {
	adapter := &mockAdapter{}
	m := NewModel(context.Background(), adapter)
	m.width = 100
	m.height = 30

	// Add two chats: one with media, one text only
	chatWithMedia := &UnreadChat{
		ChatID: "media@s.whatsapp.net",
		Name:   "Media Sender",
		Messages: []domain.Message{
			{ID: "m1", Type: domain.MessageTypeImage, Body: "Photo"},
		},
		LastReceived: time.Now(),
	}
	chatTextOnly := &UnreadChat{
		ChatID: "text@s.whatsapp.net",
		Name:   "Text Sender",
		Messages: []domain.Message{
			{ID: "t1", Type: domain.MessageTypeText, Body: "Hello"},
		},
		LastReceived: time.Now().Add(-time.Minute),
	}

	m.unreadChats["media@s.whatsapp.net"] = chatWithMedia
	m.unreadChats["text@s.whatsapp.net"] = chatTextOnly
	m.chatOrder = []string{"media@s.whatsapp.net", "text@s.whatsapp.net"}
	m.cursor = 0 // pointing at chatWithMedia

	// 1. By default, view is ViewUnreadList
	if m.view != ViewUnreadList {
		t.Fatalf("Expected ViewUnreadList by default")
	}

	// Pressing '?' should NOT open help or change view (it's removed)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if m.view != ViewUnreadList {
		t.Fatalf("Expected view to remain ViewUnreadList after pressing '?'")
	}

	// Dynamic media preview hint SHOULD be present for media chat
	viewLines := m.renderUnreadListView()
	viewStr := strings.Join(viewLines, "\n")
	if !strings.Contains(viewStr, "[Alt+P] Preview Media") {
		t.Errorf("Expected '[Alt+P] Preview Media' to be shown for media chat")
	}

	// Move cursor to text only chat
	m.cursor = 1
	viewLines = m.renderUnreadListView()
	viewStr = strings.Join(viewLines, "\n")
	if strings.Contains(viewStr, "[Alt+P] Preview") {
		t.Errorf("Media preview hint should NOT be shown for chat without media")
	}

	// 2. Press 'ctrl+/' to open the all keybinds menu
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}, Alt: false}) // Note: Bubbletea emits "ctrl+/"
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlUnderscore})                        // Test ctrl+_ as well
	// Direct string update matching
	m.view = ViewUnreadList
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}, Alt: false})
	// Trigger with ctrl+/ string
	m.view = ViewUnreadList
	// Simulate ctrl+/ keymsg
	_ = m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}) // ensure no crash
	m.prevView = ViewUnreadList
	m.view = ViewKeybindsHelp
	m.keybindScrollOffset = 0

	helpLines := m.renderKeybindsHelpView()
	helpStr := strings.Join(helpLines, "\n")
	if !strings.Contains(helpStr, "Keyboard Shortcuts") {
		t.Errorf("Expected header 'Keyboard Shortcuts', got:\n%s", helpStr)
	}
	if !strings.Contains(helpStr, "NAVIGATION & GLOBAL") {
		t.Errorf("Expected group 'NAVIGATION & GLOBAL' in keybinds menu")
	}

	allHelp := strings.Join(m.getKeybindContentLines(80), "\n")
	if !strings.Contains(allHelp, "CHATS & UNREAD LIST") {
		t.Errorf("Expected group 'CHATS & UNREAD LIST' in keybinds content")
	}
	if !strings.Contains(allHelp, "CHAT WINDOW - COMPOSING & NAVIGATION") {
		t.Errorf("Expected group 'CHAT WINDOW - COMPOSING & NAVIGATION' in keybinds content")
	}
	if !strings.Contains(allHelp, "MESSAGE HOVER MODE") {
		t.Errorf("Expected group 'MESSAGE HOVER MODE' in keybinds content")
	}
	if !strings.Contains(allHelp, "DOCUMENT & MEDIA ACTIONS") {
		t.Errorf("Expected group 'DOCUMENT & MEDIA ACTIONS' in keybinds content")
	}
	if !strings.Contains(allHelp, "CONTACT PICKER / NEW CHAT") {
		t.Errorf("Expected group 'CONTACT PICKER / NEW CHAT' in keybinds content")
	}

	// Test scrolling down and up
	initOffset := m.keybindScrollOffset
	m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.keybindScrollOffset != initOffset+1 {
		t.Errorf("Expected keybindScrollOffset to increment to %d, got %d", initOffset+1, m.keybindScrollOffset)
	}
	m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.keybindScrollOffset != initOffset {
		t.Errorf("Expected keybindScrollOffset to return to %d, got %d", initOffset, m.keybindScrollOffset)
	}

	// Test closing with Esc
	m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != ViewUnreadList {
		t.Fatalf("Expected Esc to return view to ViewUnreadList, got %v", m.view)
	}

	// 3. Test opening from ViewChat and returning back
	m.view = ViewChat
	m.prevView = ViewChat
	m.view = ViewKeybindsHelp
	m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.view != ViewChat {
		t.Fatalf("Expected 'q' to return view to ViewChat, got %v", m.view)
	}

	// 4. Test opening from ViewContactPicker and returning back
	m.view = ViewContactPicker
	m.prevView = ViewContactPicker
	m.view = ViewKeybindsHelp
	m.updateKeybindsHelp(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != ViewContactPicker {
		t.Fatalf("Expected Esc to return view to ViewContactPicker, got %v", m.view)
	}
}

func TestMultiLineMessageShiftEnter(t *testing.T) {
	adapter := &mockAdapter{}
	m := NewModel(context.Background(), adapter)
	m.width = 80
	m.height = 24
	m.view = ViewChat
	m.activeChatID = "12345@s.whatsapp.net"
	m.activeName = "Tester"
	m.input.Focus()

	// 1. Initial height is 1
	if m.input.Height() != 1 {
		t.Fatalf("Expected initial height to be 1, got %d", m.input.Height())
	}

	// 2. Type "First Line"
	for _, r := range "First Line" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.input.LineCount() != 1 {
		t.Errorf("Expected 1 line, got %d", m.input.LineCount())
	}

	// 3. Press Shift+Enter
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("shift+enter")})
	if m.input.LineCount() != 2 {
		t.Fatalf("Expected 2 lines after shift+enter, got %d", m.input.LineCount())
	}
	if m.input.Height() != 2 {
		t.Fatalf("Expected input height to be 2, got %d", m.input.Height())
	}

	// 4. Type "Second Line"
	for _, r := range "Second Line" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// 5. Also test alt+enter for line 3
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alt+enter")})
	for _, r := range "Third Line" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.input.LineCount() != 3 {
		t.Fatalf("Expected 3 lines, got %d", m.input.LineCount())
	}
	if m.input.Height() != 3 {
		t.Fatalf("Expected input height to be 3, got %d", m.input.Height())
	}

	// 6. Verify renderChatView line count never overflows terminal height
	chatLines := m.renderChatView()
	if len(chatLines) > m.height {
		t.Errorf("Chat view rendered %d lines, exceeding terminal height %d", len(chatLines), m.height)
	}
	chatStr := strings.Join(chatLines, "\n")
	if !strings.Contains(chatStr, "First Line") || !strings.Contains(chatStr, "Second Line") || !strings.Contains(chatStr, "Third Line") {
		t.Errorf("Expected multi-line input to be rendered in chat view")
	}

	// 7. Press Enter (without Shift) to send
	m2, sendCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if sendCmd == nil {
		t.Fatalf("Expected sendCmd on enter")
	}
	// Execute sendCmd
	_ = sendCmd()
	if adapter.lastSentText != "First Line\nSecond Line\nThird Line" {
		t.Errorf("Expected sent message to preserve newlines, got: %q", adapter.lastSentText)
	}

	// 8. Verify input collapses back to height 1 and is empty
	model := m2.(*Model)
	if model.input.Value() != "" {
		t.Errorf("Expected input to be reset after send, got %q", model.input.Value())
	}
	if model.input.Height() != 1 {
		t.Errorf("Expected input height to reset to 1 after send, got %d", model.input.Height())
	}
}

func TestChatPersistenceAcrossCloseAndExitCleanup(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// 1. Incoming message from Alice
	aliceID := "alice@s.whatsapp.net"
	msg := domain.Message{
		ID:         "ALICE1",
		ChatID:     aliceID,
		Sender:     aliceID,
		SenderName: "Alice",
		Timestamp:  time.Now(),
		Body:       "Hey watui!",
	}
	model.handleIncomingMessage(msg)

	if len(model.chatOrder) != 1 {
		t.Fatalf("Expected 1 chat in list, got %d", len(model.chatOrder))
	}
	if model.unreadChats[aliceID].UnreadCount != 1 {
		t.Fatalf("Expected 1 unread message, got %d", model.unreadChats[aliceID].UnreadCount)
	}

	// 2. Open chat (Enter)
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	curModel := m.(*Model)
	if curModel.view != ViewChat {
		t.Fatalf("Expected view to be ViewChat, got %v", curModel.view)
	}
	if curModel.unreadChats[aliceID].UnreadCount != 0 {
		t.Errorf("Expected unread count to reset to 0 after opening, got %d", curModel.unreadChats[aliceID].UnreadCount)
	}

	// 3. Send a message to Alice
	sentMsg := domain.Message{
		ID:        "ME1",
		ChatID:    aliceID,
		Sender:    "me@s.whatsapp.net",
		Timestamp: time.Now(),
		IsFromMe:  true,
		Body:      "Hello Alice!",
	}
	m, _ = curModel.Update(messageSentMsg(sentMsg))
	curModel = m.(*Model)
	if len(curModel.activeMsgs) != 2 {
		t.Fatalf("Expected 2 active messages, got %d", len(curModel.activeMsgs))
	}

	// 4. Close chat window (Esc) -> return to ViewUnreadList
	m, _ = curModel.Update(tea.KeyMsg{Type: tea.KeyEsc})
	curModel = m.(*Model)
	if curModel.view != ViewUnreadList {
		t.Fatalf("Expected view to be ViewUnreadList after Esc, got %v", curModel.view)
	}

	// Verify chat is STILL present in chatOrder and unreadChats (persisted!)
	if len(curModel.chatOrder) != 1 {
		t.Fatalf("Expected chat to persist in chatOrder after Esc, got %d chats", len(curModel.chatOrder))
	}
	persistedChat, exists := curModel.unreadChats[aliceID]
	if !exists {
		t.Fatalf("Expected chat %s to exist in unreadChats after Esc", aliceID)
	}
	if len(persistedChat.Messages) != 2 {
		t.Errorf("Expected 2 persisted messages in chat, got %d", len(persistedChat.Messages))
	}
	if persistedChat.UnreadCount != 0 {
		t.Errorf("Expected unread badge count to be 0 for read chat, got %d", persistedChat.UnreadCount)
	}

	// Verify DismissUnread has NOT been called yet while TUI is open
	if len(adapter.dismissedChats) != 0 {
		t.Errorf("DismissUnread should not be called until TUI exits, got %v", adapter.dismissedChats)
	}

	// 5. Open chat again -> verify messages are loaded
	m, _ = curModel.Update(tea.KeyMsg{Type: tea.KeyEnter})
	curModel = m.(*Model)
	if curModel.view != ViewChat {
		t.Fatalf("Expected view to be ViewChat on second open, got %v", curModel.view)
	}
	if len(curModel.activeMsgs) != 2 {
		t.Errorf("Expected 2 active messages when re-opening chat, got %d", len(curModel.activeMsgs))
	}

	// 6. Close chat window again (Esc)
	m, _ = curModel.Update(tea.KeyMsg{Type: tea.KeyEsc})
	curModel = m.(*Model)

	// 7. Exit TUI -> CleanupOnExit is called
	curModel.CleanupOnExit()

	if len(adapter.dismissedChats) != 1 || adapter.dismissedChats[0] != aliceID {
		t.Errorf("Expected DismissUnread to be called for %s on exit, got %v", aliceID, adapter.dismissedChats)
	}
}

func TestChatHistoryCtrlUPagination(t *testing.T) {
	adapter := &mockAdapter{
		historyMessages: make(map[string][]domain.Message),
	}
	chatID := "friend@s.whatsapp.net"
	baseTime := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	// Seed 12 historical messages (H1 to H12, chronological order)
	for i := 1; i <= 12; i++ {
		adapter.historyMessages[chatID] = append(adapter.historyMessages[chatID], domain.Message{
			ID:        fmt.Sprintf("H%d", i),
			ChatID:    chatID,
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Body:      fmt.Sprintf("Message %d", i),
		})
	}

	persistTrue := true
	cfg := &config.Config{
		PersistChatHistory: &persistTrue,
	}

	model := NewModel(context.Background(), adapter, cfg)
	model.view = ViewChat
	model.activeChatID = chatID
	model.activeMsgs = nil

	// 1. First Ctrl+U: loads latest 5 messages (H8 to H12)
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if cmd == nil {
		t.Fatalf("Expected fetchHistoryCmd on Ctrl+U")
	}
	msg := cmd()
	m, _ = m.Update(msg)
	curModel := m.(*Model)

	if len(curModel.activeMsgs) != 5 {
		t.Fatalf("Expected 5 messages loaded, got %d", len(curModel.activeMsgs))
	}
	if curModel.activeMsgs[0].ID != "H8" || curModel.activeMsgs[4].ID != "H12" {
		t.Errorf("Expected H8 to H12, got %s to %s", curModel.activeMsgs[0].ID, curModel.activeMsgs[4].ID)
	}

	// 2. Second Ctrl+U: loads next 5 older messages (H3 to H7)
	m, cmd = curModel.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if cmd == nil {
		t.Fatalf("Expected fetchHistoryCmd on second Ctrl+U")
	}
	msg = cmd()
	m, _ = m.Update(msg)
	curModel = m.(*Model)

	if len(curModel.activeMsgs) != 10 {
		t.Fatalf("Expected 10 messages after second Ctrl+U, got %d", len(curModel.activeMsgs))
	}
	if curModel.activeMsgs[0].ID != "H3" || curModel.activeMsgs[9].ID != "H12" {
		t.Errorf("Expected H3 to H12, got %s to %s", curModel.activeMsgs[0].ID, curModel.activeMsgs[9].ID)
	}

	// 3. Third Ctrl+U: loads remaining 2 older messages (H1, H2)
	m, cmd = curModel.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	msg = cmd()
	m, _ = m.Update(msg)
	curModel = m.(*Model)

	if len(curModel.activeMsgs) != 12 {
		t.Fatalf("Expected 12 messages after third Ctrl+U, got %d", len(curModel.activeMsgs))
	}
	if curModel.activeMsgs[0].ID != "H1" {
		t.Errorf("Expected oldest message to be H1, got %s", curModel.activeMsgs[0].ID)
	}

	// 4. Fourth Ctrl+U: no more messages
	m, cmd = curModel.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	msg = cmd()
	m, _ = m.Update(msg)
	curModel = m.(*Model)

	if curModel.previewStatus != "No more history" {
		t.Errorf("Expected 'No more history', got %q", curModel.previewStatus)
	}
}

func TestChatHistoryAlwaysLoad(t *testing.T) {
	adapter := &mockAdapter{
		historyMessages: make(map[string][]domain.Message),
	}
	chatID := "friend@s.whatsapp.net"
	baseTime := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	for i := 1; i <= 10; i++ {
		adapter.historyMessages[chatID] = append(adapter.historyMessages[chatID], domain.Message{
			ID:        fmt.Sprintf("MSG%d", i),
			ChatID:    chatID,
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Body:      fmt.Sprintf("History %d", i),
		})
	}

	persistTrue := true
	alwaysLoadTrue := true
	cfg := &config.Config{
		PersistChatHistory:   &persistTrue,
		AlwaysLoadHistory:    &alwaysLoadTrue,
		CycleMsgCountPerChat: 30,
	}

	model := NewModel(context.Background(), adapter, cfg)
	model.unreadChats[chatID] = &UnreadChat{
		ChatID: chatID,
		Name:   "Friend",
	}
	model.chatOrder = []string{chatID}
	model.cursor = 0

	// Open chat via Enter -> should return Batch with history loader
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("Expected batch command when always-load-history is enabled")
	}

	// Run fetch history cmd
	fetchCmd := model.fetchHistoryCmd(chatID, 30, time.Time{}, true)
	res := fetchCmd()
	m, _ = m.Update(res)
	curModel := m.(*Model)

	if len(curModel.activeMsgs) != 10 {
		t.Fatalf("Expected 10 auto-loaded messages, got %d", len(curModel.activeMsgs))
	}
}

func TestChatMessageHoverNavigationAndActions(t *testing.T) {
	adapter := &mockAdapter{
		archivedChats: make(map[string]bool),
	}
	cfg := &config.Config{}
	model := NewModel(context.Background(), adapter, cfg)

	chatID := "friend@s.whatsapp.net"
	msg0 := domain.Message{
		ID:        "msg0",
		ChatID:    chatID,
		Sender:    chatID,
		Body:      "Check https://github.com/Arun0A/watui for the repo",
		Type:      domain.MessageTypeText,
		Timestamp: time.Now().Add(-10 * time.Minute),
	}
	msg1 := domain.Message{
		ID:        "msg1",
		ChatID:    chatID,
		Sender:    chatID,
		Body:      "Here is an image",
		Type:      domain.MessageTypeImage,
		Timestamp: time.Now().Add(-5 * time.Minute),
	}
	msg2 := domain.Message{
		ID:        "msg2",
		ChatID:    chatID,
		Sender:    chatID,
		Body:      "Plain text message",
		Type:      domain.MessageTypeText,
		Timestamp: time.Now().Add(-1 * time.Minute),
	}

	model.unreadChats[chatID] = &UnreadChat{
		ChatID:   chatID,
		Name:     "Friend",
		Messages: []domain.Message{msg0, msg1, msg2},
	}
	model.chatOrder = []string{chatID}
	model.cursor = 0

	// 1. In ViewUnreadList, test that Alt+P previews recent media (msg1) without opening chat
	unreadAltPCmd := model.updateUnreadList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if unreadAltPCmd == nil {
		t.Fatalf("Expected Alt+P in ViewUnreadList to return preview cmd for recent media")
	}

	// 2. Open chat
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if model.view != ViewChat {
		t.Fatalf("Expected ViewChat, got %v", model.view)
	}
	if model.selectedMsgIdx != -1 {
		t.Errorf("Expected selectedMsgIdx to be -1 on chat open, got %d", model.selectedMsgIdx)
	}

	// 3. Alt+P while focused on input box previews most recent media (msg1)
	chatAltPCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if chatAltPCmd == nil {
		t.Fatalf("Expected Alt+P while focused on input to preview most recent media")
	}

	// 4. Alt+k navigates to every message (starts at bottom: msg2)
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}, Alt: true})
	if model.selectedMsgIdx != 2 {
		t.Errorf("Expected selectedMsgIdx to be 2 after Alt+k, got %d", model.selectedMsgIdx)
	}
	if model.input.Focused() {
		t.Errorf("Expected text input to blur when hovering message")
	}

	// 5. 'k' moves up to msg1 (Image media)
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if model.selectedMsgIdx != 1 {
		t.Errorf("Expected selectedMsgIdx to be 1 after 'k', got %d", model.selectedMsgIdx)
	}

	// 6. Pressing 'p' on msg1 previews it
	previewCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if previewCmd == nil {
		t.Errorf("Expected preview cmd when pressing 'p' on media msg1")
	}

	// 7. Move up to msg0 (has link and text)
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if model.selectedMsgIdx != 0 {
		t.Errorf("Expected selectedMsgIdx to be 0 after 'k', got %d", model.selectedMsgIdx)
	}

	// 8. Press 'p' on non-media message (msg0) should not preview
	nonMediaCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if nonMediaCmd != nil {
		t.Errorf("Expected nil cmd when pressing 'p' on non-media msg")
	}
	if !strings.Contains(model.previewStatus, "no media") {
		t.Errorf("Expected previewStatus to indicate no media, got %q", model.previewStatus)
	}

	// 9. Press 'l' to open link in default browser
	origOpen := openInBrowser
	var openedURLs []string
	openInBrowser = func(u string) error {
		openedURLs = append(openedURLs, u)
		return nil
	}
	defer func() { openInBrowser = origOpen }()

	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if !strings.Contains(model.previewStatus, "Opened: https://github.com/Arun0A/watui") {
		t.Errorf("Expected previewStatus to indicate opened link, got %q", model.previewStatus)
	}
	if len(openedURLs) != 1 || openedURLs[0] != "https://github.com/Arun0A/watui" {
		t.Errorf("Expected openedURLs to have [https://github.com/Arun0A/watui], got %v", openedURLs)
	}

	// 10. Press 'y' to copy message
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !strings.Contains(model.previewStatus, "Copied message to clipboard") {
		t.Errorf("Expected previewStatus to indicate copied message, got %q", model.previewStatus)
	}

	// 11. Press 'r' to reply
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if model.replyToMsg == nil || model.replyToMsg.ID != "msg0" {
		t.Fatalf("Expected replyToMsg to be msg0, got %+v", model.replyToMsg)
	}
	if model.selectedMsgIdx != -1 {
		t.Errorf("Expected selectedMsgIdx to be -1 after pressing 'r', got %d", model.selectedMsgIdx)
	}
	if !model.input.Focused() {
		t.Errorf("Expected text input to be focused after pressing 'r'")
	}

	// Check that reply banner renders in View
	viewOutput := strings.Join(model.renderChatView(), "\n")
	if !strings.Contains(viewOutput, "Replying to") {
		t.Errorf("Expected chat view to render reply banner, got:\n%s", viewOutput)
	}

	// 12. Send a reply message
	model.input.SetValue("Awesome repo!")
	sendCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyEnter})
	if sendCmd != nil {
		_ = sendCmd()
	}
	if adapter.lastSentText != "Awesome repo!" {
		t.Errorf("Expected sent text 'Awesome repo!', got %q", adapter.lastSentText)
	}
	if adapter.lastQuotedID != "msg0" {
		t.Errorf("Expected quoted message ID 'msg0', got %q", adapter.lastQuotedID)
	}
	if model.replyToMsg != nil {
		t.Errorf("Expected replyToMsg to be cleared after sending")
	}

	// 13. Test Esc defocus from hover mode
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}, Alt: true})
	if model.selectedMsgIdx < 0 {
		t.Errorf("Expected selectedMsgIdx >= 0 after Alt+k")
	}
	model.updateChat(tea.KeyMsg{Type: tea.KeyEsc})
	if model.selectedMsgIdx != -1 {
		t.Errorf("Expected selectedMsgIdx to be -1 after Esc, got %d", model.selectedMsgIdx)
	}
	if !model.input.Focused() {
		t.Errorf("Expected text input to be focused after Esc from hover")
	}

	// 14. Test Alt+Shift+K jumps to media only
	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}, Alt: true})
	if model.selectedMsgIdx != 1 {
		t.Errorf("Expected Alt+K to jump to media msg1 (idx 1), got %d", model.selectedMsgIdx)
	}
}

func TestReplyRenderingWhenQuotedMessageNotVisible(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 80
	model.height = 24
	model.activeChatID = "group_test@g.us"
	model.activeName = "Group Test"
	model.view = ViewChat

	// 1. Message with QuotedText and QuotedSender
	m1 := domain.Message{
		ID:           "msg_reply_1",
		ChatID:       "group_test@g.us",
		Sender:       "user1@s.whatsapp.net",
		SenderName:   "User One",
		Timestamp:    time.Now(),
		Body:         "I agree with that",
		QuotedID:     "not_visible_1",
		QuotedText:   "Let's meet tomorrow",
		QuotedSender: "Alice",
	}

	// 2. Message with only QuotedID and QuotedSender
	m2 := domain.Message{
		ID:           "msg_reply_2",
		ChatID:       "group_test@g.us",
		Sender:       "user2@s.whatsapp.net",
		SenderName:   "User Two",
		Timestamp:    time.Now().Add(time.Minute),
		Body:         "Sure thing",
		QuotedID:     "not_visible_2",
		QuotedSender: "Bob",
	}

	// 3. Message with only QuotedID
	m3 := domain.Message{
		ID:         "msg_reply_3",
		ChatID:     "group_test@g.us",
		Sender:     "user3@s.whatsapp.net",
		SenderName: "User Three",
		Timestamp:  time.Now().Add(2 * time.Minute),
		Body:       "Count me in",
		QuotedID:   "not_visible_3",
	}

	model.activeMsgs = []domain.Message{m1, m2, m3}
	rendered := model.View()

	if !strings.Contains(rendered, "┌─ Alice: Let's meet tomorrow") {
		t.Errorf("Expected render to contain quote '┌─ Alice: Let's meet tomorrow', rendered:\n%s", rendered)
	}

	if !strings.Contains(rendered, "┌─ Replying to Bob") {
		t.Errorf("Expected render to contain quote '┌─ Replying to Bob', rendered:\n%s", rendered)
	}

	if !strings.Contains(rendered, "┌─ [Replying to message]") {
		t.Errorf("Expected render to contain quote '┌─ [Replying to message]', rendered:\n%s", rendered)
	}
}

func TestGroupChatSenderContactNameResolution(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 80
	model.height = 24

	// Load contact with both phone number and LID
	model.setContacts([]domain.Contact{
		{
			JID:  "919007088779@s.whatsapp.net",
			Name: "Dastageer Siddiqui Dastageer",
		},
		{
			JID:  "247403034730498@lid",
			Name: "Dastageer Siddiqui Dastageer",
		},
	})

	// Message with raw LID number as Sender and SenderName
	msg := domain.Message{
		ID:         "msg_group_1",
		ChatID:     "120363394253683284@g.us",
		Sender:     "247403034730498@lid",
		SenderName: "247403034730498",
		Timestamp:  time.Now(),
		Body:       "foooood",
	}

	model.activeChatID = "120363394253683284@g.us"
	model.activeName = "ajeeb bacha with read receipt"
	model.activeMsgs = []domain.Message{msg}
	model.view = ViewChat

	rendered := strings.Join(model.renderChatView(), "\n")
	if !strings.Contains(rendered, "Dastageer Siddiqui Dastageer") {
		t.Errorf("Expected chat view to render resolved contact name 'Dastageer Siddiqui Dastageer', got:\n%s", rendered)
	}
	if strings.Contains(rendered, "247403034730498 21:") || strings.Contains(rendered, "247403034730498  ") {
		t.Errorf("Expected chat view NOT to render raw LID number as sender header, got:\n%s", rendered)
	}

	// Test reply bar resolution
	model.replyToMsg = &msg
	replyRendered := strings.Join(model.renderChatView(), "\n")
	if !strings.Contains(replyRendered, "Replying to Dastageer Siddiqui Dastageer") {
		t.Errorf("Expected reply bar to show 'Replying to Dastageer Siddiqui Dastageer', got:\n%s", replyRendered)
	}

	// Test unread list snippet prefix
	model.unreadChats = map[string]*UnreadChat{
		"120363394253683284@g.us": {
			ChatID:       "120363394253683284@g.us",
			Name:         "ajeeb bacha with read receipt",
			IsGroup:      true,
			Messages:     []domain.Message{msg},
			LastReceived: time.Now(),
		},
	}
	model.chatOrder = []string{"120363394253683284@g.us"}
	model.view = ViewUnreadList
	inboxRendered := strings.Join(model.renderUnreadListView(), "\n")
	if !strings.Contains(inboxRendered, "Dastageer Siddiqui Dastageer: foooood") {
		t.Errorf("Expected inbox snippet to show 'Dastageer Siddiqui Dastageer: foooood', got:\n%s", inboxRendered)
	}
}

func TestExtractLinksAndOpenInBrowser(t *testing.T) {
	// 1. Test extractLinks function directly
	text := "Check out https://golang.org and https://github.com/Arun0A/watui, also www.example.com/test! And repeat https://golang.org."
	links := extractLinks(text)
	expected := []string{
		"https://golang.org",
		"https://github.com/Arun0A/watui",
		"https://www.example.com/test",
	}
	if len(links) != len(expected) {
		t.Fatalf("Expected %d links, got %d: %v", len(expected), len(links), links)
	}
	for i, exp := range expected {
		if links[i] != exp {
			t.Errorf("Link[%d]: expected %q, got %q", i, exp, links[i])
		}
	}

	// 2. Test opening multiple links when pressing 'l' in hover mode
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)
	model.width = 80
	model.height = 24

	multiLinkMsg := domain.Message{
		ID:        "msg_multi_link",
		ChatID:    "test@s.whatsapp.net",
		Sender:    "test@s.whatsapp.net",
		Body:      text,
		Type:      domain.MessageTypeText,
		Timestamp: time.Now(),
	}

	model.activeChatID = "test@s.whatsapp.net"
	model.activeMsgs = []domain.Message{multiLinkMsg}
	model.selectedMsgIdx = 0
	model.view = ViewChat

	origOpen := openInBrowser
	var opened []string
	openInBrowser = func(u string) error {
		opened = append(opened, u)
		return nil
	}
	defer func() { openInBrowser = origOpen }()

	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if len(opened) != 3 {
		t.Fatalf("Expected 3 opened links, got %d: %v", len(opened), opened)
	}
	if !strings.Contains(model.previewStatus, "Opened 3 links in browser") {
		t.Errorf("Expected previewStatus to indicate 3 opened links, got %q", model.previewStatus)
	}
}

func TestMentionResolutionInChatAndViews(t *testing.T) {
	adapter := &mockAdapter{}
	model := NewModel(context.Background(), adapter)

	// Set up contacts including "You" (both phone and LID) and another contact "Alice"
	contacts := []domain.Contact{
		{JID: "919000000000@s.whatsapp.net", Name: "You"},
		{JID: "218227607093431@lid", Name: "You"},
		{JID: "919876543210@s.whatsapp.net", Name: "Alice", PushName: "Alice P"},
	}
	model.setContacts(contacts)

	// 1. Test formatMentions directly
	rawMsg1 := "@218227607093431 hi"
	got1 := model.formatMentions(rawMsg1)
	if got1 != "@You hi" {
		t.Errorf("formatMentions(%q) = %q; want %q", rawMsg1, got1, "@You hi")
	}

	rawMsg2 := "Hello @919876543210, are you here?"
	got2 := model.formatMentions(rawMsg2)
	if got2 != "Hello @Alice, are you here?" {
		t.Errorf("formatMentions(%q) = %q; want %q", rawMsg2, got2, "Hello @Alice, are you here?")
	}

	// Email addresses should not be falsely matched as mentions
	emailMsg := "Contact me at user@example.com or user@123456789.com"
	gotEmail := model.formatMentions(emailMsg)
	if gotEmail != emailMsg {
		t.Errorf("formatMentions(%q) = %q; want %q", emailMsg, gotEmail, emailMsg)
	}

	// 2. Test styleMentionsForDisplay
	styled1 := model.styleMentionsForDisplay(rawMsg1)
	if !strings.Contains(styled1, "You") || !strings.Contains(styled1, "hi") {
		t.Errorf("styleMentionsForDisplay(%q) = %q; expected to contain 'You' and 'hi'", rawMsg1, styled1)
	}

	// 3. Test filterContacts does not expose "You"
	model.filterContacts("")
	for _, c := range model.filteredList {
		if c.Name == "You" {
			t.Errorf("filterContacts(\"\") exposed 'You' as a contact")
		}
	}
	model.filterContacts("you")
	for _, c := range model.filteredList {
		if c.Name == "You" {
			t.Errorf("filterContacts(\"you\") exposed 'You' as a contact")
		}
	}

	// 4. Test renderChatView resolves mention in message body and quote preview
	chatMsg := domain.Message{
		ID:         "msg_mention_1",
		ChatID:     "120363430194759319@g.us",
		Sender:     "919111111111@s.whatsapp.net",
		SenderName: "AAA-Self",
		Body:       "@218227607093431 hi",
		QuotedText: "@919876543210 please check",
		Type:       domain.MessageTypeText,
		Timestamp:  time.Now(),
	}
	model.activeChatID = chatMsg.ChatID
	model.activeMsgs = []domain.Message{chatMsg}
	model.width = 80
	model.height = 24
	model.view = ViewChat

	lines := model.renderChatView()
	fullView := strings.Join(lines, "\n")
	if !strings.Contains(fullView, "You") {
		t.Errorf("renderChatView output does not contain resolved mention 'You':\n%s", fullView)
	}
	if !strings.Contains(fullView, "Alice") {
		t.Errorf("renderChatView output does not contain resolved quote mention 'Alice':\n%s", fullView)
	}
	if strings.Contains(fullView, "@218227607093431") {
		t.Errorf("renderChatView still contains raw LID '@218227607093431':\n%s", fullView)
	}

	// 5. Test renderUnreadListView snippet resolves mentions
	model.unreadChats = map[string]*UnreadChat{
		chatMsg.ChatID: {
			ChatID:      chatMsg.ChatID,
			Name:        "Group test",
			IsGroup:     true,
			UnreadCount: 1,
			Messages:    []domain.Message{chatMsg},
		},
	}
	model.chatOrder = []string{chatMsg.ChatID}
	model.view = ViewUnreadList

	unreadLines := model.renderUnreadListView()
	fullUnread := strings.Join(unreadLines, "\n")
	if !strings.Contains(fullUnread, "AAA-Self: @You hi") {
		t.Errorf("renderUnreadListView output does not contain 'AAA-Self: @You hi':\n%s", fullUnread)
	}

	// 6. Test hover copy 'y' copies formatted mentions
	model.view = ViewChat
	model.selectedMsgIdx = 0

	// Capture clipboard write
	origWriteAll := clipboardWriteAll
	var copiedText string
	clipboardWriteAll = func(text string) error {
		copiedText = text
		return nil
	}
	defer func() { clipboardWriteAll = origWriteAll }()

	model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if copiedText != "@You hi" {
		t.Errorf("Hover copy 'y' copied %q; want %q", copiedText, "@You hi")
	}
}

func TestGroupMentionHintsAndCompletion(t *testing.T) {
	adapter := &mockAdapter{
		participants: []domain.Contact{
			{JID: "919876543210@s.whatsapp.net", Name: "Alice Smith"},
			{JID: "919876543211@s.whatsapp.net", Name: "Bob Jones"},
			{JID: "919876543212@s.whatsapp.net", Name: "Charlie"},
		},
	}
	model := NewModel(context.Background(), adapter)
	model.activeChatID = "120363430194759319@g.us"
	model.activeName = "Group test"
	model.view = ViewChat
	model.width = 80
	model.height = 24
	model.groupParticipants = adapter.participants

	// 1. In non-group chat, typing '@' should NOT activate mention hints
	model.activeChatID = "919000000000@s.whatsapp.net"
	model.input.SetValue("@")
	model.updateMentionHints()
	if len(model.mentionHints) != 0 {
		t.Fatalf("Expected 0 hints in 1-on-1 chat, got %d", len(model.mentionHints))
	}

	// 2. In group chat, typing '@' shows all group participants
	model.activeChatID = "120363430194759319@g.us"
	model.input.SetValue("@")
	model.updateMentionHints()
	if len(model.mentionHints) != 3 {
		t.Fatalf("Expected 3 hints for '@', got %d", len(model.mentionHints))
	}

	// 3. renderChatView displays mention hint list vertically below message box
	lines := model.renderChatView()
	fullView := strings.Join(lines, "\n")
	if !strings.Contains(fullView, "@ Mention") {
		t.Errorf("Expected '@ Mention' in renderChatView, got:\n%s", fullView)
	}
	if !strings.Contains(fullView, "Alice Smith") || !strings.Contains(fullView, "Bob Jones") {
		t.Errorf("Expected Alice Smith and Bob Jones in hints bar, got:\n%s", fullView)
	}
	if !strings.Contains(fullView, "▸") {
		t.Errorf("Expected selection pointer '▸' in vertical hints list, got:\n%s", fullView)
	}

	// 4. Filtering by query '@Ali'
	model.input.SetValue("@Ali")
	model.updateMentionHints()
	if len(model.mentionHints) != 1 || model.mentionHints[0].Name != "Alice Smith" {
		t.Fatalf("Expected 1 hint 'Alice Smith' for '@Ali', got %v", model.mentionHints)
	}

	// 5. Ctrl+N and Ctrl+P navigate hints (arrow keys do not hijack mention hints)
	model.input.SetValue("@")
	model.updateMentionHints()
	if model.mentionCursor != 0 {
		t.Errorf("Expected initial cursor 0, got %d", model.mentionCursor)
	}
	model.updateChat(tea.KeyMsg{Type: tea.KeyCtrlN})
	if model.mentionCursor != 1 {
		t.Errorf("Expected cursor 1 after Ctrl+N, got %d", model.mentionCursor)
	}
	model.updateChat(tea.KeyMsg{Type: tea.KeyCtrlP})
	if model.mentionCursor != 0 {
		t.Errorf("Expected cursor 0 after Ctrl+P, got %d", model.mentionCursor)
	}
	// Arrow keys should not alter mention cursor
	model.updateChat(tea.KeyMsg{Type: tea.KeyDown})
	if model.mentionCursor != 0 {
		t.Errorf("Expected cursor 0 (unchanged) after Down arrow, got %d", model.mentionCursor)
	}

	// 6. Tab applies the highlighted mention hint
	model.updateChat(tea.KeyMsg{Type: tea.KeyTab})
	if model.input.Value() != "@Alice Smith " {
		t.Errorf("Expected input to be '@Alice Smith ', got %q", model.input.Value())
	}
	if len(model.mentionHints) != 0 {
		t.Errorf("Expected hints to be cleared after Tab completion, got %d hints", len(model.mentionHints))
	}

	// 7. Esc dismisses hints without sending
	model.input.SetValue("hey @Bo")
	model.updateMentionHints()
	if len(model.mentionHints) != 1 {
		t.Fatalf("Expected 1 hint for '@Bo', got %d", len(model.mentionHints))
	}
	model.updateChat(tea.KeyMsg{Type: tea.KeyEsc})
	if len(model.mentionHints) != 0 {
		t.Errorf("Expected hints to be dismissed on Esc, got %d hints", len(model.mentionHints))
	}
	if model.input.Value() != "hey @Bo" {
		t.Errorf("Expected input to remain 'hey @Bo', got %q", model.input.Value())
	}
}
