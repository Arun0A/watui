package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watui/internal/domain"
)

// ViewState defines which screen is currently rendered.
type ViewState int

const (
	ViewUnreadList ViewState = iota
	ViewChat
	ViewContactPicker
)

// UnreadChat represents a conversation with pending unread messages.
type UnreadChat struct {
	ChatID       string
	Name         string
	Sender       string
	Messages     []domain.Message
	LastReceived time.Time
}

// Model is the top-level Bubble Tea model for watui.
type Model struct {
	adapter domain.WhatsAppAdapter
	ctx     context.Context

	view ViewState

	// Unread state
	mu          sync.RWMutex
	unreadChats map[string]*UnreadChat // chatID -> UnreadChat
	chatOrder   []string               // sorted by LastReceived desc
	cursor      int

	// Active conversation view
	activeChatID string
	activeName   string
	activeMsgs   []domain.Message
	input        textinput.Model

	// Contact search view
	contacts       []domain.Contact
	filteredList   []domain.Contact
	contactSearch  textinput.Model
	contactCursor  int
	loadingContact bool

	// App state
	status domain.ConnectionStatus
	width  int
	height int
	err    error

	msgChan      chan domain.Message
	statusChan   chan domain.ConnectionStatus
	dismissChan  chan string
	contactsChan chan []domain.Contact
}

// Msg types for Tea event loop
type incomingMsg domain.Message
type statusChangeMsg domain.ConnectionStatus
type contactsLoadedMsg []domain.Contact
type messageSentMsg domain.Message
type unreadsLoadedMsg []domain.Message
type chatDismissedMsg string
type sendErrMsg struct {
	ChatID string
	Err    error
}

// NewModel initializes the TUI model.
func NewModel(ctx context.Context, adapter domain.WhatsAppAdapter) *Model {
	ti := textinput.New()
	ti.Placeholder = "Type a message... (Enter to send, Esc to back)"
	ti.CharLimit = 1000
	ti.Width = 60

	si := textinput.New()
	si.Placeholder = "Search contact name, group, or type phone number..."
	si.CharLimit = 100
	si.Width = 50

	m := &Model{
		adapter:       adapter,
		ctx:           ctx,
		view:          ViewUnreadList,
		unreadChats:   make(map[string]*UnreadChat),
		chatOrder:     make([]string, 0),
		input:         ti,
		contactSearch: si,
		status:        domain.StatusConnecting,
		msgChan:       make(chan domain.Message, 100),
		statusChan:    make(chan domain.ConnectionStatus, 10),
		dismissChan:   make(chan string, 50),
		contactsChan:  make(chan []domain.Contact, 10),
	}

	adapter.OnMessage(func(msg domain.Message) {
		select {
		case m.msgChan <- msg:
		default:
		}
	})
	adapter.OnStatus(func(s domain.ConnectionStatus) {
		select {
		case m.statusChan <- s:
		default:
		}
	})
	adapter.OnChatDismissed(func(chatID string) {
		select {
		case m.dismissChan <- chatID:
		default:
		}
	})
	adapter.OnContactsUpdated(func(contacts []domain.Contact) {
		select {
		case m.contactsChan <- contacts:
		default:
		}
	})

	return m
}

// Init sets up subscriptions for incoming WhatsApp events and restores persisted unread state.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.waitForMessages(),
		m.waitForStatus(),
		m.waitForChatDismissed(),
		m.loadPersistedUnread(),
		m.loadContacts(),
		m.waitForContactsUpdated(),
	)
}

func (m *Model) loadPersistedUnread() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.adapter.GetUnreadMessages(m.ctx)
		if err != nil {
			return nil
		}
		return unreadsLoadedMsg(msgs)
	}
}

func (m *Model) performRefresh() tea.Cmd {
	return func() tea.Msg {
		_ = m.adapter.Sync(m.ctx)
		msgs, err := m.adapter.GetUnreadMessages(m.ctx)
		if err != nil {
			return nil
		}
		return unreadsLoadedMsg(msgs)
	}
}

func (m *Model) waitForMessages() tea.Cmd {
	return func() tea.Msg {
		msg := <-m.msgChan
		return incomingMsg(msg)
	}
}

func (m *Model) waitForStatus() tea.Cmd {
	return func() tea.Msg {
		s := <-m.statusChan
		return statusChangeMsg(s)
	}
}

func (m *Model) waitForChatDismissed() tea.Cmd {
	return func() tea.Msg {
		chatID := <-m.dismissChan
		return chatDismissedMsg(chatID)
	}
}

func (m *Model) waitForContactsUpdated() tea.Cmd {
	return func() tea.Msg {
		c := <-m.contactsChan
		return contactsLoadedMsg(c)
	}
}

func (m *Model) loadContacts() tea.Cmd {
	return func() tea.Msg {
		contacts, err := m.adapter.GetContacts(m.ctx)
		if err != nil {
			return nil
		}
		return contactsLoadedMsg(contacts)
	}
}

// Update handles state transitions and keyboard commands.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		cw := m.contentWidth()
		m.input.Width = max(20, cw-6)
		m.contactSearch.Width = max(20, cw-6)

	case statusChangeMsg:
		m.status = domain.ConnectionStatus(msg)

	case incomingMsg:
		dMsg := domain.Message(msg)
		m.handleIncomingMessage(dMsg)
		cmds = append(cmds, m.waitForMessages()) // re-listen

	case contactsLoadedMsg:
		m.contacts = []domain.Contact(msg)
		m.filterContacts(m.contactSearch.Value())
		m.loadingContact = false
		cmds = append(cmds, m.waitForContactsUpdated()) // re-listen for live updates

	case messageSentMsg:
		sent := domain.Message(msg)
		if m.activeChatID == sent.ChatID {
			m.activeMsgs = append(m.activeMsgs, sent)
		}

	case sendErrMsg:
		if m.activeChatID == msg.ChatID {
			m.activeMsgs = append(m.activeMsgs, domain.Message{
				ID:         "err-" + time.Now().Format("150405"),
				ChatID:     msg.ChatID,
				Sender:     "system",
				SenderName: "System Error",
				Timestamp:  time.Now(),
				IsFromMe:   false,
				Type:       domain.MessageTypeText,
				Body:       fmt.Sprintf("[Failed to send: %v]", msg.Err),
				Status:     domain.MessageStatusFailed,
			})
		}

	case chatDismissedMsg:
		m.dismissUnread(string(msg))
		cmds = append(cmds, m.waitForChatDismissed())

	case unreadsLoadedMsg:
		m.rebuildUnreadChats(msg)

	case tea.KeyMsg:
		switch m.view {
		case ViewUnreadList:
			cmds = append(cmds, m.updateUnreadList(msg))
		case ViewChat:
			cmds = append(cmds, m.updateChat(msg))
		case ViewContactPicker:
			cmds = append(cmds, m.updateContactPicker(msg))
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) rebuildUnreadChats(msgs []domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.unreadChats = make(map[string]*UnreadChat)
	m.chatOrder = nil

	for _, msg := range msgs {
		chat, exists := m.unreadChats[msg.ChatID]
		if !exists {
			name := msg.SenderName
			if name == "" {
				name = msg.Sender
			}
			chat = &UnreadChat{
				ChatID:       msg.ChatID,
				Name:         name,
				Sender:       msg.Sender,
				Messages:     []domain.Message{msg},
				LastReceived: msg.Timestamp,
			}
			m.unreadChats[msg.ChatID] = chat
			m.chatOrder = append([]string{msg.ChatID}, m.chatOrder...)
		} else {
			chat.Messages = append(chat.Messages, msg)
			chat.LastReceived = msg.Timestamp
			if msg.SenderName != "" {
				chat.Name = msg.SenderName
			}
			m.reorderChatToTop(msg.ChatID)
		}
	}

	if m.cursor >= len(m.chatOrder) {
		m.cursor = max(0, len(m.chatOrder)-1)
	}
}

func (m *Model) handleIncomingMessage(msg domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If currently viewing this chat, append directly
	if m.view == ViewChat && m.activeChatID == msg.ChatID {
		m.activeMsgs = append(m.activeMsgs, msg)
		// Auto mark as read
		go func() {
			_ = m.adapter.MarkRead(m.ctx, msg.ChatID, msg.Sender, []string{msg.ID})
		}()
		return
	}

	// Otherwise, add or update unread chat
	chat, exists := m.unreadChats[msg.ChatID]
	if !exists {
		name := msg.SenderName
		if name == "" {
			name = msg.Sender
		}
		chat = &UnreadChat{
			ChatID:       msg.ChatID,
			Name:         name,
			Sender:       msg.Sender,
			Messages:     []domain.Message{msg},
			LastReceived: msg.Timestamp,
		}
		m.unreadChats[msg.ChatID] = chat
		m.chatOrder = append([]string{msg.ChatID}, m.chatOrder...)
	} else {
		chat.Messages = append(chat.Messages, msg)
		chat.LastReceived = msg.Timestamp
		if msg.SenderName != "" {
			chat.Name = msg.SenderName
		}
		// Move to top of chatOrder
		m.reorderChatToTop(msg.ChatID)
	}
}

func (m *Model) reorderChatToTop(chatID string) {
	newOrder := []string{chatID}
	for _, id := range m.chatOrder {
		if id != chatID {
			newOrder = append(newOrder, id)
		}
	}
	m.chatOrder = newOrder
}

func (m *Model) updateUnreadList(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c":
		return tea.Quit

	case "j", "down":
		if m.cursor < len(m.chatOrder)-1 {
			m.cursor++
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}

	case "r", "d": // mark as read / dismiss from unread list
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			m.dismissUnread(chatID)
		}

	case "enter": // open chat
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			m.activeChatID = chatID
			m.activeName = chat.Name
			m.activeMsgs = append([]domain.Message(nil), chat.Messages...)
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat

			// Mark as read in WhatsApp
			var ids []string
			for _, item := range chat.Messages {
				ids = append(ids, item.ID)
			}
			go func(cID, sID string, mIDs []string) {
				_ = m.adapter.MarkRead(m.ctx, cID, sID, mIDs)
			}(chat.ChatID, chat.Sender, ids)

			// Remove from unread queue
			m.dismissUnread(chatID)
			return textinput.Blink
		}

	case "n", "c": // new message / contact picker
		m.view = ViewContactPicker
		m.contactSearch.Reset()
		m.contactSearch.Focus()
		m.contactCursor = 0
		m.filterContacts("")
		if len(m.contacts) == 0 {
			m.loadingContact = true
			return tea.Batch(textinput.Blink, m.loadContacts())
		}
		m.loadingContact = false
		return textinput.Blink

	case "R", "ctrl+r": // manual refresh unreads and contacts
		return tea.Batch(
			m.performRefresh(),
			m.loadContacts(),
		)
	}
	return nil
}

func (m *Model) dismissUnread(chatID string) {
	m.mu.Lock()
	delete(m.unreadChats, chatID)

	baseID := chatID
	if idx := strings.Index(chatID, ":"); idx != -1 {
		if atIdx := strings.Index(chatID, "@"); atIdx != -1 && atIdx > idx {
			baseID = chatID[:idx] + chatID[atIdx:]
			delete(m.unreadChats, baseID)
		}
	} else {
		for k := range m.unreadChats {
			if strings.HasPrefix(k, baseID+":") || (strings.Contains(baseID, "@") && strings.HasPrefix(k, strings.Split(baseID, "@")[0]+":")) {
				delete(m.unreadChats, k)
			}
		}
	}

	var newOrder []string
	for _, id := range m.chatOrder {
		if id != chatID && id != baseID && !strings.HasPrefix(id, baseID+":") {
			newOrder = append(newOrder, id)
		}
	}
	m.chatOrder = newOrder
	if m.cursor >= len(m.chatOrder) {
		m.cursor = max(0, len(m.chatOrder)-1)
	}
	m.mu.Unlock()

	go func(id string) {
		_ = m.adapter.DismissUnread(m.ctx, id)
	}(chatID)
}

func (m *Model) updateChat(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.view = ViewUnreadList
		return nil

	case "ctrl+c":
		return tea.Quit

	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.input.Reset()
		chatID := m.activeChatID
		return func() tea.Msg {
			sentMsg, err := m.adapter.SendTextMessage(m.ctx, chatID, text)
			if err != nil {
				return sendErrMsg{ChatID: chatID, Err: err}
			}
			return messageSentMsg(sentMsg)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *Model) updateContactPicker(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.view = ViewUnreadList
		return nil

	case "ctrl+c":
		return tea.Quit

	case "down", "ctrl+n":
		if m.contactCursor < len(m.filteredList)-1 {
			m.contactCursor++
		}
		return nil

	case "up", "ctrl+p":
		if m.contactCursor > 0 {
			m.contactCursor--
		}
		return nil

	case "enter":
		if len(m.filteredList) > 0 && m.contactCursor < len(m.filteredList) {
			contact := m.filteredList[m.contactCursor]
			m.activeChatID = contact.JID
			m.activeName = contact.Name
			m.activeMsgs = nil
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat
			return textinput.Blink
		}
		// Direct input fallback (user typed a raw phone number or JID)
		rawInput := strings.TrimSpace(m.contactSearch.Value())
		if rawInput != "" {
			m.activeChatID = rawInput
			m.activeName = rawInput
			m.activeMsgs = nil
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat
			return textinput.Blink
		}
		return nil
	}

	var cmd tea.Cmd
	oldVal := m.contactSearch.Value()
	m.contactSearch, cmd = m.contactSearch.Update(msg)
	if m.contactSearch.Value() != oldVal {
		m.filterContacts(m.contactSearch.Value())
		m.contactCursor = 0
	}
	return cmd
}

func (m *Model) filterContacts(query string) {
	trimmed := strings.TrimSpace(query)
	q := strings.ToLower(trimmed)
	if q == "" {
		if len(m.contacts) > 40 {
			m.filteredList = m.contacts[:40]
		} else {
			m.filteredList = m.contacts
		}
		return
	}

	var res []domain.Contact

	// If query looks like a phone number (5+ digits), offer direct message at top of list
	var digits strings.Builder
	for _, r := range trimmed {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	if digits.Len() >= 5 {
		res = append(res, domain.Contact{
			JID:  trimmed,
			Name: fmt.Sprintf("💬 Direct message to %s", trimmed),
		})
	}

	maxMatches := 40
	for _, c := range m.contacts {
		if strings.Contains(strings.ToLower(c.Name), q) ||
			strings.Contains(strings.ToLower(c.PushName), q) ||
			strings.Contains(c.JID, q) {
			res = append(res, c)
			if len(res) >= maxMatches {
				break
			}
		}
	}
	m.filteredList = res
}

// -------------------------------------------------------------
// View Renderers with Minimalist Styling
// -------------------------------------------------------------

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#A6E3A1"))

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086"))

	headerStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(lipgloss.Color("#313244")).
			Padding(0, 1)

	badgeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#11111B")).
			Background(lipgloss.Color("#FAB387")).
			Padding(0, 1)

	selectedChatStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#89B4FA")).
				PaddingLeft(1)

	normalChatStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CDD6F4")).
			PaddingLeft(2)

	snippetStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086")).
			PaddingLeft(4)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585B70")).
			Padding(0, 1)

	boxStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#45475A")).
			Padding(1, 2)
)

func (m *Model) contentWidth() int {
	if m.width <= 0 {
		return 76
	}
	return min(76, max(36, m.width-4))
}

func (m *Model) View() string {
	var content string
	switch m.view {
	case ViewUnreadList:
		content = m.renderUnreadListView()
	case ViewChat:
		content = m.renderChatView()
	case ViewContactPicker:
		content = m.renderContactPickerView()
	default:
		return ""
	}

	if m.width <= 0 || m.height <= 0 {
		return content
	}

	cw := m.contentWidth()
	container := lipgloss.NewStyle().
		Width(cw).
		Align(lipgloss.Left).
		Render(content)

	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		container,
	)
}

func (m *Model) renderUnreadListView() string {
	cw := m.contentWidth()
	var b strings.Builder

	// Top Bar
	unreadCount := len(m.chatOrder)
	statusText := string(m.status)
	if m.status == domain.StatusConnected {
		statusText = fmt.Sprintf("connected · %d unread", unreadCount)
	}

	header := fmt.Sprintf("%s  %s",
		titleStyle.Render("watui"),
		statusStyle.Render("· "+statusText),
	)
	b.WriteString(headerStyle.Copy().Width(cw - 2).Render(header) + "\n\n")

	// Center unread queue
	if unreadCount == 0 {
		emptyBox := boxStyle.Copy().Width(cw - 6).Render(
			titleStyle.Render("✓ Inbox Zero") + "\n\n" +
				"No unread messages.\n\n" +
				statusStyle.Render("Press [n] to compose to a contact, or wait for incoming messages."),
		)
		b.WriteString(emptyBox + "\n\n")
	} else {
		for i, chatID := range m.chatOrder {
			chat := m.unreadChats[chatID]
			badge := badgeStyle.Render(fmt.Sprintf("%d", len(chat.Messages)))

			lastMsg := ""
			if len(chat.Messages) > 0 {
				last := chat.Messages[len(chat.Messages)-1]
				lastMsg = last.Body
				maxLen := max(20, cw-20)
				if len(lastMsg) > maxLen {
					lastMsg = lastMsg[:maxLen] + "..."
				}
			}

			timeStr := chat.LastReceived.Format("15:04")
			var item string
			if i == m.cursor {
				item = fmt.Sprintf("> %s %s  %s\n%s",
					badge,
					selectedChatStyle.Render(chat.Name),
					statusStyle.Render(timeStr),
					snippetStyle.Render(lastMsg),
				)
			} else {
				item = fmt.Sprintf("  %s %s  %s\n%s",
					badge,
					normalChatStyle.Render(chat.Name),
					statusStyle.Render(timeStr),
					snippetStyle.Render(lastMsg),
				)
			}
			b.WriteString(item + "\n\n")
		}
	}

	// Bottom Keybindings
	footer := helpStyle.Render("[Enter] Open · [r] Dismiss/Read · [n] New Message · [R] Refresh · [q] Quit")
	b.WriteString(footer)

	return b.String()
}

func (m *Model) renderChatView() string {
	cw := m.contentWidth()
	var b strings.Builder

	// Header
	header := fmt.Sprintf("%s %s  %s",
		titleStyle.Render("Chat with"),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89B4FA")).Render(m.activeName),
		statusStyle.Render("· [Esc] Back to unread"),
	)
	b.WriteString(headerStyle.Copy().Width(cw - 2).Render(header) + "\n\n")

	// Message history in current session (scrolled to latest messages that fit)
	msgsToShow := m.activeMsgs
	if m.height > 12 {
		maxMsgs := max(3, (m.height-10)/3)
		if len(msgsToShow) > maxMsgs {
			msgsToShow = msgsToShow[len(msgsToShow)-maxMsgs:]
		}
	}

	if len(msgsToShow) == 0 {
		b.WriteString(statusStyle.Render("  (No messages in this session yet. Type below to send.)\n\n"))
	} else {
		for _, msg := range msgsToShow {
			timeStr := msg.Timestamp.Format("15:04")
			if msg.IsFromMe {
				line := fmt.Sprintf("  %s %s\n    %s",
					lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render("You"),
					statusStyle.Render(timeStr),
					msg.Body,
				)
				b.WriteString(line + "\n\n")
			} else {
				sender := msg.SenderName
				if sender == "" {
					sender = "Them"
				}
				line := fmt.Sprintf("  %s %s\n    %s",
					lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89B4FA")).Render(sender),
					statusStyle.Render(timeStr),
					msg.Body,
				)
				b.WriteString(line + "\n\n")
			}
		}
	}

	// Bottom Input
	b.WriteString("\n" + m.input.View() + "\n\n")
	b.WriteString(helpStyle.Render("[Enter] Send · [Esc] Back to inbox"))

	return b.String()
}

func (m *Model) renderContactPickerView() string {
	cw := m.contentWidth()
	var b strings.Builder

	header := fmt.Sprintf("%s  %s",
		titleStyle.Render("Send Message Upfront"),
		statusStyle.Render("· Select contact without loading history"),
	)
	b.WriteString(headerStyle.Copy().Width(cw - 2).Render(header) + "\n\n")

	// Search bar
	b.WriteString(m.contactSearch.View() + "\n\n")

	if m.loadingContact {
		b.WriteString(statusStyle.Render("  Loading contact cache from local session...\n\n"))
	} else if len(m.filteredList) == 0 {
		b.WriteString(statusStyle.Render("  No contacts found matching search.\n\n"))
	} else {
		maxItems := 10
		if m.height > 12 {
			maxItems = max(5, m.height-12)
		}
		start := 0
		if m.contactCursor >= maxItems {
			start = m.contactCursor - maxItems + 1
		}
		end := min(len(m.filteredList), start+maxItems)

		for i := start; i < end; i++ {
			c := m.filteredList[i]
			label := c.Name
			if c.IsGroup {
				label = "👥 [Group] " + c.Name
			} else if c.PushName != "" && c.PushName != c.Name && !strings.HasPrefix(c.Name, "💬") {
				label += fmt.Sprintf(" (%s)", c.PushName)
			}
			phone := c.JID
			if strings.Contains(c.JID, "@") {
				phone = strings.Split(c.JID, "@")[0]
			}

			if i == m.contactCursor {
				b.WriteString(fmt.Sprintf("> %s  %s\n",
					selectedChatStyle.Render(label),
					statusStyle.Render(phone),
				))
			} else {
				b.WriteString(fmt.Sprintf("  %s  %s\n",
					normalChatStyle.Render(label),
					statusStyle.Render(phone),
				))
			}
		}
	}

	b.WriteString("\n" + helpStyle.Render("[↑/↓] Navigate · [Enter] Select & Compose · [Esc] Cancel"))

	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
