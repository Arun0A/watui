package ui

import (
	"context"
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
func (m *mockAdapter) OnContactsUpdated(h func([]domain.Contact))                     {}
func (m *mockAdapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	return nil, nil
}
func (m *mockAdapter) DismissUnread(ctx context.Context, chatID string) error { return nil }
func (m *mockAdapter) Sync(ctx context.Context) error                         { return nil }
func (m *mockAdapter) OnChatDismissed(h func(chatID string))                   {}
func (m *mockAdapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	return "/tmp/mock_media.jpg", nil
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

	// Navigate to previous media using Alt+Up
	model.updateChat(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if model.selectedMediaIdx != 0 {
		t.Errorf("Expected selectedMediaIdx to be 0 after Alt+Up, got %d", model.selectedMediaIdx)
	}

	// Press Alt+P to preview the first media (image)
	chatAltPCmd := model.updateChat(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if chatAltPCmd == nil {
		t.Errorf("Expected non-nil cmd when pressing alt+p in chat with media")
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
}

