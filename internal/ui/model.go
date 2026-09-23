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
}

// Msg types for Tea event loop
type incomingMsg domain.Message
type statusChangeMsg domain.ConnectionStatus
type contactsLoadedMsg []domain.Contact
type messageSentMsg domain.Message

// NewModel initializes the TUI model.
func NewModel(ctx context.Context, adapter domain.WhatsAppAdapter) *Model {
	ti := textinput.New()
	ti.Placeholder = "Type a message... (Enter to send, Esc to back)"
	ti.CharLimit = 1000
	ti.Width = 60

	si := textinput.New()
	si.Placeholder = "Search contact name or number..."
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
	}

	return m
}

// Init sets up subscriptions for incoming WhatsApp events.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.waitForMessages(),
		m.waitForStatus(),
	)
}

func (m *Model) waitForMessages() tea.Cmd {
	return func() tea.Msg {
		msgChan := make(chan domain.Message, 1)
		m.adapter.OnMessage(func(msg domain.Message) {
			select {
			case msgChan <- msg:
			default:
			}
		})
		msg := <-msgChan
		return incomingMsg(msg)
	}
}

func (m *Model) waitForStatus() tea.Cmd {
	return func() tea.Msg {
		statusChan := make(chan domain.ConnectionStatus, 1)
		m.adapter.OnStatus(func(s domain.ConnectionStatus) {
			select {
			case statusChan <- s:
			default:
			}
		})
		s := <-statusChan
		return statusChangeMsg(s)
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
		m.input.Width = max(20, msg.Width-10)
		m.contactSearch.Width = max(20, msg.Width-10)

	case statusChangeMsg:
		m.status = domain.ConnectionStatus(msg)

	case incomingMsg:
		dMsg := domain.Message(msg)
		m.handleIncomingMessage(dMsg)
		cmds = append(cmds, m.waitForMessages()) // re-listen

	case contactsLoadedMsg:
		m.contacts = []domain.Contact(msg)
		m.filterContacts("")
		m.loadingContact = false

	case messageSentMsg:
		sent := domain.Message(msg)
		if m.activeChatID == sent.ChatID {
			m.activeMsgs = append(m.activeMsgs, sent)
		}

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
		m.loadingContact = true
		return tea.Batch(textinput.Blink, m.loadContacts())
	}
	return nil
}

func (m *Model) dismissUnread(chatID string) {
	delete(m.unreadChats, chatID)
	var newOrder []string
	for _, id := range m.chatOrder {
		if id != chatID {
			newOrder = append(newOrder, id)
		}
	}
	m.chatOrder = newOrder
	if m.cursor >= len(m.chatOrder) && m.cursor > 0 {
		m.cursor--
	}
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
				return nil
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
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		m.filteredList = m.contacts
		return
	}
	var res []domain.Contact
	for _, c := range m.contacts {
		if strings.Contains(strings.ToLower(c.Name), q) ||
			strings.Contains(strings.ToLower(c.PushName), q) ||
			strings.Contains(strings.ToLower(c.JID), q) {
			res = append(res, c)
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

func (m *Model) View() string {
	switch m.view {
	case ViewUnreadList:
		return m.renderUnreadListView()
	case ViewChat:
		return m.renderChatView()
	case ViewContactPicker:
		return m.renderContactPickerView()
	default:
		return ""
	}
}

func (m *Model) renderUnreadListView() string {
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
	b.WriteString(headerStyle.Render(header) + "\n\n")

	// Center unread queue
	if unreadCount == 0 {
		emptyBox := boxStyle.Render(
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
				if len(lastMsg) > 55 {
					lastMsg = lastMsg[:55] + "..."
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
	footer := helpStyle.Render("[Enter] Open · [r] Dismiss/Read · [n] New Message · [q] Quit")
	b.WriteString(footer)

	return b.String()
}

func (m *Model) renderChatView() string {
	var b strings.Builder

	// Header
	header := fmt.Sprintf("%s %s  %s",
		titleStyle.Render("Chat with"),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89B4FA")).Render(m.activeName),
		statusStyle.Render("· [Esc] Back to unread"),
	)
	b.WriteString(headerStyle.Render(header) + "\n\n")

	// Message history in current session
	if len(m.activeMsgs) == 0 {
		b.WriteString(statusStyle.Render("  (No messages in this session yet. Type below to send.)\n\n"))
	} else {
		for _, msg := range m.activeMsgs {
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
	var b strings.Builder

	header := fmt.Sprintf("%s  %s",
		titleStyle.Render("Send Message Upfront"),
		statusStyle.Render("· Select contact without loading history"),
	)
	b.WriteString(headerStyle.Render(header) + "\n\n")

	// Search bar
	b.WriteString(m.contactSearch.View() + "\n\n")

	if m.loadingContact {
		b.WriteString(statusStyle.Render("  Loading contact cache from local session...\n\n"))
	} else if len(m.filteredList) == 0 {
		b.WriteString(statusStyle.Render("  No contacts found matching search.\n\n"))
	} else {
		maxItems := 12
		start := 0
		if m.contactCursor >= maxItems {
			start = m.contactCursor - maxItems + 1
		}
		end := min(len(m.filteredList), start+maxItems)

		for i := start; i < end; i++ {
			c := m.filteredList[i]
			label := c.Name
			if c.PushName != "" && c.PushName != c.Name {
				label += fmt.Sprintf(" (%s)", c.PushName)
			}
			phone := strings.Split(c.JID, "@")[0]

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
