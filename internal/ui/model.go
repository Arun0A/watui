package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watui/internal/config"
	"watui/internal/domain"
)

// ViewState represents the currently active screen in the TUI.
type ViewState int

const (
	ViewUnreadList ViewState = iota
	ViewChat
	ViewContactPicker
)

// UnreadChat represents a conversation with pending unread messages or a pinned chat.
type UnreadChat struct {
	ChatID       string
	Name         string
	IsGroup      bool
	IsPinned     bool
	IsArchived   bool
	UnreadCount  int
	Sender       string
	Messages     []domain.Message
	LastReceived time.Time
}

// Model is the top-level Bubble Tea model for watui.
type Model struct {
	adapter domain.WhatsAppAdapter
	ctx     context.Context
	cfg     *config.Config

	view ViewState

	// Unread state
	mu           sync.RWMutex
	unreadChats  map[string]*UnreadChat // chatID -> UnreadChat
	readChats    map[string]bool        // chatID -> true for chats read in active session
	chatOrder    []string               // visible chats according to view mode
	cursor       int
	showArchived bool // true when viewing archived chats section
	showHelp     bool // toggled with '?' to show static keybinds
	cleanupDone  bool

	// Active conversation view
	activeChatID     string
	activeName       string
	activeMsgs       []domain.Message
	chatScrollOffset int
	input            textarea.Model

	// Media navigation & saving
	selectedMediaIdx int             // targeted media index within activeChat (0-based)
	confirmSave      bool            // true when prompting "save? (y/N)"
	pendingSavePath  string          // path of the media file awaiting save confirmation
	confirmDocAction bool            // true when prompting "Document: Open [o], Open with [w], or Save [s]?"
	promptOpenWith   bool            // true when typing custom viewer in "Open with: "
	openWithInput    textinput.Model // textinput for custom application name
	pendingDocMsg    *domain.Message // message awaiting document action choice

	// Contact search view
	contacts       []domain.Contact
	contactsByJID  map[string]*domain.Contact
	contactsByUser map[string]*domain.Contact
	filteredList   []domain.Contact
	contactSearch  textinput.Model
	contactCursor  int
	contactOffset  int
	loadingContact bool

	// App state
	status        domain.ConnectionStatus
	previewStatus string
	width         int
	height        int

	msgChan      chan domain.Message
	statusChan   chan domain.ConnectionStatus
	dismissChan  chan string
	contactsChan chan []domain.Contact

	// Active external preview/player process
	activeViewerCmd *exec.Cmd
	viewerMu        sync.Mutex
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
	Text   string
	Err    error
}
type mediaPreviewErrMsg struct {
	Err error
}
type mediaPreviewSuccessMsg struct {
	Path string
}
type docOpenedMsg struct {
	Path string
}
type docSavedMsg struct {
	DestPath string
}
type docFallbackSavedMsg struct {
	DestPath string
	OpenErr  error
}
type filePickedMsg struct {
	Path string
}
type filePickErrMsg struct {
	Err error
}
type docDownloadedToOpenMsg struct {
	Path string
	Cmd  string
}
type historyLoadedMsg struct {
	ChatID   string
	Messages []domain.Message
	Err      error
	AutoLoad bool
}

// NewModel initializes the TUI model.
func NewModel(ctx context.Context, adapter domain.WhatsAppAdapter, cfgs ...*config.Config) *Model {
	ti := textarea.New()
	ti.Placeholder = "Type a message..."
	ti.CharLimit = 4096
	ti.SetWidth(60)
	ti.SetHeight(1)
	ti.ShowLineNumbers = false
	ti.EndOfBufferCharacter = 0
	ti.KeyMap.InsertNewline = key.NewBinding(key.WithDisabled())
	ti.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ti.FocusedStyle.Base = lipgloss.NewStyle()
	ti.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086"))
	ti.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#89DCEB"))
	ti.FocusedStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4"))
	ti.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ti.BlurredStyle.Base = lipgloss.NewStyle()
	ti.BlurredStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086"))
	ti.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086"))
	ti.BlurredStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6ADC8"))
	ti.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "> "
		}
		return "  "
	})

	si := textinput.New()
	si.Placeholder = "Search contact name, group, or phone number..."
	si.CharLimit = 100
	si.Width = 50

	oi := textinput.New()
	oi.Placeholder = ""
	oi.CharLimit = 100
	oi.Width = 35

	var cfg *config.Config
	if len(cfgs) > 0 && cfgs[0] != nil {
		cfg = cfgs[0]
	} else {
		cfg = &config.Config{}
	}

	m := &Model{
		adapter:        adapter,
		ctx:            ctx,
		cfg:            cfg,
		view:           ViewUnreadList,
		unreadChats:    make(map[string]*UnreadChat),
		readChats:      make(map[string]bool),
		chatOrder:      make([]string, 0),
		input:          ti,
		contactSearch:  si,
		openWithInput:  oi,
		status:         domain.StatusConnecting,
		msgChan:        make(chan domain.Message, 200),
		statusChan:     make(chan domain.ConnectionStatus, 10),
		dismissChan:    make(chan string, 50),
		contactsChan:   make(chan []domain.Contact, 10),
		contactsByJID:  make(map[string]*domain.Contact),
		contactsByUser: make(map[string]*domain.Contact),
	}

	m.initPinnedChats()

	adapter.OnMessage(func(msg domain.Message) {
		select {
		case m.msgChan <- msg:
		default:
		}
	})
	adapter.OnStatus(func(s domain.ConnectionStatus) {
		m.status = s
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

func (m *Model) fetchHistoryCmd(chatID string, limit int, beforeTS time.Time, autoLoad bool) tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.adapter.GetChatHistory(m.ctx, chatID, limit, beforeTS)
		return historyLoadedMsg{
			ChatID:   chatID,
			Messages: msgs,
			Err:      err,
			AutoLoad: autoLoad,
		}
	}
}

func (m *Model) performRefresh() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			_ = m.adapter.Sync(m.ctx)
			msgs, err := m.adapter.GetUnreadMessages(m.ctx)
			if err != nil {
				return nil
			}
			return unreadsLoadedMsg(msgs)
		},
		m.loadContacts(),
	)
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
			return contactsLoadedMsg(nil)
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
		m.input.SetWidth(max(20, cw-6))
		m.contactSearch.Width = max(20, cw-6)
		return m, nil

	case statusChangeMsg:
		m.status = domain.ConnectionStatus(msg)
		cmds = append(cmds, m.waitForStatus())

	case incomingMsg:
		dMsg := domain.Message(msg)
		m.handleIncomingMessage(dMsg)
		// Drain any queued messages in msgChan to batch process bursts
		for {
			select {
			case nextMsg := <-m.msgChan:
				m.handleIncomingMessage(nextMsg)
			default:
				goto msgsDrained
			}
		}
	msgsDrained:
		cmds = append(cmds, m.waitForMessages())

	case contactsLoadedMsg:
		if len(msg) > 0 {
			m.setContacts([]domain.Contact(msg))
			m.filterContacts(m.contactSearch.Value())
		}
		m.loadingContact = false
		m.updateUnreadChatNames()
		m.syncPinnedChats()
		cmds = append(cmds, m.waitForContactsUpdated())

	case messageSentMsg:
		sent := domain.Message(msg)
		if m.activeChatID == sent.ChatID {
			m.activeMsgs = append(m.activeMsgs, sent)
			m.chatScrollOffset = 0
		}
		if m.previewStatus == "Sending..." {
			m.previewStatus = ""
		}
		m.mu.Lock()
		chat, exists := m.unreadChats[sent.ChatID]
		if !exists {
			name := m.activeName
			if name == "" {
				name, _ = m.resolveChatName(sent.ChatID, sent.ChatName, sent.SenderName)
			}
			chat = &UnreadChat{
				ChatID:       sent.ChatID,
				Name:         name,
				IsGroup:      strings.Contains(sent.ChatID, "@g.us"),
				IsPinned:     m.isPinned(sent.ChatID, name),
				UnreadCount:  0,
				Sender:       sent.Sender,
				Messages:     []domain.Message{sent},
				LastReceived: sent.Timestamp,
			}
			m.unreadChats[sent.ChatID] = chat
		} else {
			if len(chat.Messages) < 50 {
				chat.Messages = append(chat.Messages, sent)
			} else {
				copy(chat.Messages, chat.Messages[1:])
				chat.Messages[len(chat.Messages)-1] = sent
			}
			chat.LastReceived = sent.Timestamp
		}
		m.sortChatOrderLocked()
		m.mu.Unlock()

	case sendErrMsg:
		if m.previewStatus == "Sending..." {
			m.previewStatus = ""
		}
		if m.activeChatID == msg.ChatID {
			if m.input.Value() == "" && msg.Text != "" {
				m.input.SetValue(msg.Text)
				m.input.SetHeight(min(5, max(1, m.input.LineCount())))
				m.input.CursorEnd()
			}
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
			m.chatScrollOffset = 0
		}

	case chatDismissedMsg:
		m.dismissUnread(string(msg))
		cmds = append(cmds, m.waitForChatDismissed())

	case unreadsLoadedMsg:
		m.rebuildUnreadChats(msg)
		m.updateUnreadChatNames()

	case historyLoadedMsg:
		if msg.Err != nil {
			m.previewStatus = fmt.Sprintf("Error loading history: %v", msg.Err)
			return m, nil
		}
		if m.activeChatID != msg.ChatID {
			return m, nil
		}
		if len(msg.Messages) == 0 {
			if !msg.AutoLoad {
				m.previewStatus = "No more history"
			}
			return m, nil
		}

		existingIDs := make(map[string]bool, len(m.activeMsgs))
		for _, em := range m.activeMsgs {
			existingIDs[em.ID] = true
		}
		var newOlder []domain.Message
		for _, nm := range msg.Messages {
			if !existingIDs[nm.ID] {
				newOlder = append(newOlder, nm)
			}
		}

		if len(newOlder) == 0 {
			if !msg.AutoLoad {
				m.previewStatus = "No more history"
			}
			return m, nil
		}

		m.activeMsgs = append(newOlder, m.activeMsgs...)
		if !msg.AutoLoad {
			m.previewStatus = fmt.Sprintf("Loaded %d older message(s)", len(newOlder))
		}
		m.mu.Lock()
		if chat, exists := m.unreadChats[msg.ChatID]; exists {
			chat.Messages = append([]domain.Message(nil), m.activeMsgs...)
		}
		m.mu.Unlock()
		return m, nil

	case mediaPreviewErrMsg:
		m.confirmSave = false
		m.confirmDocAction = false
		m.pendingDocMsg = nil
		m.previewStatus = fmt.Sprintf("Error: %v", msg.Err)

	case mediaPreviewSuccessMsg:
		m.previewStatus = ""
		m.confirmSave = true
		m.pendingSavePath = msg.Path

	case docOpenedMsg:
		m.previewStatus = ""
		m.confirmSave = true
		m.pendingSavePath = msg.Path

	case docSavedMsg:
		m.confirmSave = false
		m.previewStatus = fmt.Sprintf("Saved to %s", msg.DestPath)

	case docFallbackSavedMsg:
		m.confirmSave = false
		m.previewStatus = fmt.Sprintf("No default app; saved to %s", msg.DestPath)

	case docDownloadedToOpenMsg:
		m.previewStatus = ""
		if isTerminalViewer(msg.Cmd) {
			execCmd := buildTerminalViewerCmd(msg.Cmd, msg.Path)
			return m, tea.ExecProcess(execCmd, func(err error) tea.Msg {
				if err != nil {
					return mediaPreviewErrMsg{Err: err}
				}
				return docOpenedMsg{Path: msg.Path}
			})
		}
		if err := m.launchViewer(msg.Cmd, msg.Path); err != nil {
			destPath, saveErr := saveToDownloads(msg.Path)
			if saveErr != nil {
				return m, func() tea.Msg {
					return mediaPreviewErrMsg{Err: fmt.Errorf("open failed (%v) and save failed (%w)", err, saveErr)}
				}
			}
			return m, func() tea.Msg {
				return docFallbackSavedMsg{DestPath: destPath, OpenErr: err}
			}
		}
		return m, func() tea.Msg {
			return docOpenedMsg{Path: msg.Path}
		}

	case filePickedMsg:
		if msg.Path != "" {
			cleanPath := strings.TrimSpace(msg.Path)
			m.input.SetValue("file://" + cleanPath + " ")
			m.input.CursorEnd()
			m.previewStatus = fmt.Sprintf("Attached %s (press Enter to send)", filepath.Base(cleanPath))
		} else {
			m.previewStatus = ""
		}
		return m, tea.ClearScreen

	case filePickErrMsg:
		if errors.Is(msg.Err, errNoFilePicker) {
			m.previewStatus = "No file picker found"
		} else {
			m.previewStatus = "File picker failed"
		}
		return m, tea.ClearScreen

	case tea.KeyMsg:
		if m.promptOpenWith {
			switch msg.String() {
			case "enter":
				appCmd := strings.TrimSpace(m.openWithInput.Value())
				m.promptOpenWith = false
				if appCmd != "" && m.pendingDocMsg != nil {
					targetMsg := *m.pendingDocMsg
					m.pendingDocMsg = nil
					return m, m.downloadAndOpenDocCmd(targetMsg, appCmd)
				}
				m.pendingDocMsg = nil
				return m, nil
			case "esc":
				m.promptOpenWith = false
				m.pendingDocMsg = nil
				m.previewStatus = ""
				return m, nil
			default:
				var cmd tea.Cmd
				m.openWithInput, cmd = m.openWithInput.Update(msg)
				return m, cmd
			}
		}

		if m.confirmDocAction {
			switch msg.String() {
			case "o", "O":
				m.confirmDocAction = false
				if m.pendingDocMsg != nil {
					targetMsg := *m.pendingDocMsg
					m.pendingDocMsg = nil
					return m, m.downloadAndOpenDocCmd(targetMsg)
				}
				return m, nil
			case "w", "W":
				m.confirmDocAction = false
				m.promptOpenWith = true
				m.openWithInput.SetValue("")
				m.openWithInput.Focus()
				return m, textinput.Blink
			case "s", "S":
				m.confirmDocAction = false
				if m.pendingDocMsg != nil {
					targetMsg := *m.pendingDocMsg
					m.pendingDocMsg = nil
					return m, m.downloadAndSaveDocCmd(targetMsg)
				}
				return m, nil
			case "esc":
				m.confirmDocAction = false
				m.pendingDocMsg = nil
				m.previewStatus = ""
				return m, nil
			default:
				m.confirmDocAction = false
				m.pendingDocMsg = nil
				m.previewStatus = ""
			}
		}

		if m.confirmSave {
			switch msg.String() {
			case "y", "Y":
				m.confirmSave = false
				srcPath := m.pendingSavePath
				m.pendingSavePath = ""
				destPath, err := saveToDownloads(srcPath)
				if err != nil {
					m.previewStatus = fmt.Sprintf("Failed to save: %v", err)
				} else {
					m.previewStatus = fmt.Sprintf("Saved to %s", destPath)
				}
				return m, nil
			case "n", "N", "enter", "esc":
				m.confirmSave = false
				m.pendingSavePath = ""
				m.previewStatus = ""
				return m, nil
			default:
				m.confirmSave = false
				m.pendingSavePath = ""
				m.previewStatus = ""
			}
		}

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

func isRawJID(s string) bool {
	if strings.Contains(s, " ") {
		return false
	}
	servers := []string{
		"@g.us",
		"@s.whatsapp.net",
		"@lid",
		"@broadcast",
		"@newsletter",
		"@hosted",
		"@call",
		"@bot",
	}
	for _, srv := range servers {
		if strings.Contains(s, srv) {
			return true
		}
	}
	return false
}

func isRealChatName(name string, chatID string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if isRawJID(name) {
		return false
	}
	if strings.HasPrefix(name, "Group (") {
		return false
	}
	cleanID := chatID
	if idx := strings.Index(cleanID, ":"); idx != -1 {
		if atIdx := strings.Index(cleanID, "@"); atIdx != -1 {
			cleanID = cleanID[:idx] + cleanID[atIdx:]
		}
	}
	baseUser := strings.Split(cleanID, "@")[0]
	if name == chatID || name == cleanID || name == baseUser {
		return false
	}
	isDigitsAndHyphens := true
	for _, r := range name {
		if (r < '0' || r > '9') && r != '-' && r != '+' && r != ' ' {
			isDigitsAndHyphens = false
			break
		}
	}
	if isDigitsAndHyphens && (strings.Contains(name, "-") || len(strings.ReplaceAll(name, " ", "")) >= 10 || strings.HasPrefix(name, "120363")) {
		return false
	}
	return true
}

func (m *Model) setContacts(contacts []domain.Contact) {
	m.contacts = contacts
	byJID := make(map[string]*domain.Contact, len(contacts)*2)
	byUser := make(map[string]*domain.Contact, len(contacts))
	for i := range m.contacts {
		c := &m.contacts[i]
		clean := c.JID
		if idx := strings.Index(clean, ":"); idx != -1 {
			if atIdx := strings.Index(clean, "@"); atIdx != -1 {
				clean = clean[:idx] + clean[atIdx:]
			}
		}
		byJID[clean] = c
		byJID[c.JID] = c
		user := strings.Split(clean, "@")[0]
		if _, ok := byUser[user]; !ok {
			byUser[user] = c
		}
	}
	m.contactsByJID = byJID
	m.contactsByUser = byUser
}

func (m *Model) resolveChatName(chatID string, msgChatName string, fallback string) (string, bool) {
	isGroup := strings.Contains(chatID, "@g.us")

	cleanID := chatID
	if idx := strings.Index(cleanID, ":"); idx != -1 {
		if atIdx := strings.Index(cleanID, "@"); atIdx != -1 {
			cleanID = cleanID[:idx] + cleanID[atIdx:]
		}
	}
	baseUser := strings.Split(cleanID, "@")[0]

	if len(m.contacts) > 0 && len(m.contactsByJID) < len(m.contacts) {
		m.setContacts(m.contacts)
	}

	// 1. Check m.contacts first (O(1) map lookup)
	var contact *domain.Contact
	if m.contactsByJID != nil {
		contact = m.contactsByJID[cleanID]
		if contact == nil {
			contact = m.contactsByJID[chatID]
		}
	}
	if contact == nil && m.contactsByUser != nil {
		contact = m.contactsByUser[baseUser]
	}
	if contact != nil && isRealChatName(contact.Name, contact.JID) {
		return contact.Name, contact.IsGroup || isGroup
	}

	// 2. If message already had a real group or chat name
	if isRealChatName(msgChatName, chatID) {
		return msgChatName, isGroup
	}

	// 3. Fallback for group:
	if isGroup {
		return "Group (" + baseUser + ")", true
	}

	// 4. Fallback for 1-on-1:
	if isRealChatName(fallback, chatID) {
		return fallback, false
	}
	return baseUser, false
}

func (m *Model) getChatMatchNames(chatID string, explicitNames ...string) []string {
	names := append([]string(nil), explicitNames...)
	cleanID := chatID
	if idx := strings.Index(cleanID, ":"); idx != -1 {
		if atIdx := strings.Index(cleanID, "@"); atIdx != -1 {
			cleanID = cleanID[:idx] + cleanID[atIdx:]
		}
	}
	baseUser := strings.Split(cleanID, "@")[0]

	if len(m.contacts) > 0 && len(m.contactsByJID) < len(m.contacts) {
		m.setContacts(m.contacts)
	}

	var c *domain.Contact
	if m.contactsByJID != nil {
		c = m.contactsByJID[cleanID]
		if c == nil {
			c = m.contactsByJID[chatID]
		}
	}
	if c == nil && m.contactsByUser != nil {
		c = m.contactsByUser[baseUser]
	}
	if c != nil {
		if c.Name != "" {
			names = append(names, c.Name)
		}
		if c.PushName != "" {
			names = append(names, c.PushName)
		}
		if c.BusinessName != "" {
			names = append(names, c.BusinessName)
		}
		if c.JID != "" && c.JID != chatID {
			names = append(names, c.JID)
		}
	}
	return names
}

func (m *Model) isMuted(chatID string, chatNames ...string) bool {
	if m.cfg == nil {
		return false
	}
	allNames := m.getChatMatchNames(chatID, chatNames...)
	if m.isPinned(chatID, allNames...) {
		return false
	}
	return m.cfg.IsMuted(chatID, allNames...)
}

func (m *Model) isPinned(chatID string, chatNames ...string) bool {
	if m.cfg == nil {
		return false
	}
	allNames := m.getChatMatchNames(chatID, chatNames...)
	return m.cfg.IsPinned(chatID, allNames...)
}

func (m *Model) initPinnedChats() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var groupJIDs []string
	for _, rule := range m.cfg.GetPinned() {
		chatID := rule
		if !strings.Contains(chatID, "@") {
			digits := config.DigitsOnly(chatID)
			if len(digits) >= 6 && digits == chatID {
				if strings.HasPrefix(digits, "120363") {
					chatID = digits + "@g.us"
				} else {
					chatID = digits + "@s.whatsapp.net"
				}
			}
		}
		if strings.Contains(chatID, "@g.us") {
			groupJIDs = append(groupJIDs, chatID)
		}
		name, isGroup := m.resolveChatName(chatID, "", "")
		isArchived := false
		if m.adapter != nil {
			isArchived = m.adapter.IsChatArchived(chatID)
		}
		chat := &UnreadChat{
			ChatID:     chatID,
			Name:       name,
			IsGroup:    isGroup,
			IsPinned:   true,
			IsArchived: isArchived,
		}
		m.unreadChats[chatID] = chat
	}
	m.sortChatOrderLocked()

	if len(groupJIDs) > 0 && m.adapter != nil {
		m.adapter.EnsureGroupNames(m.ctx, groupJIDs)
	}
}

func (m *Model) syncPinnedChats() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncPinnedChatsLocked()
}

func (m *Model) syncPinnedChatsLocked() {
	for _, rule := range m.cfg.GetPinned() {
		var matchedID string
		for id, chat := range m.unreadChats {
			if config.MatchTarget(rule, id, chat.Name) {
				matchedID = id
				chat.IsPinned = true
				break
			}
		}

		var matchedContact *domain.Contact
		for i := range m.contacts {
			c := &m.contacts[i]
			if config.MatchTarget(rule, c.JID, c.Name, c.PushName, c.BusinessName) {
				matchedContact = c
				break
			}
		}

		if matchedContact != nil {
			realName := matchedContact.Name
			if !isRealChatName(realName, matchedContact.JID) {
				realName = ""
			}

			if matchedID != "" && matchedID != matchedContact.JID {
				chat := m.unreadChats[matchedID]
				delete(m.unreadChats, matchedID)
				chat.ChatID = matchedContact.JID
				if realName != "" {
					chat.Name = realName
				}
				chat.IsGroup = matchedContact.IsGroup
				chat.IsPinned = true
				m.unreadChats[matchedContact.JID] = chat
			} else if matchedID != "" {
				chat := m.unreadChats[matchedID]
				if realName != "" {
					chat.Name = realName
				}
				chat.IsGroup = matchedContact.IsGroup
				chat.IsPinned = true
			} else {
				chatName := realName
				if chatName == "" {
					chatName, _ = m.resolveChatName(matchedContact.JID, "", "")
				}
				isArchived := false
				if m.adapter != nil {
					isArchived = m.adapter.IsChatArchived(matchedContact.JID)
				}
				m.unreadChats[matchedContact.JID] = &UnreadChat{
					ChatID:     matchedContact.JID,
					Name:       chatName,
					IsGroup:    matchedContact.IsGroup,
					IsPinned:   true,
					IsArchived: isArchived,
				}
			}
		}
	}
	m.sortChatOrderLocked()
}

func (m *Model) sortChatOrderLocked() {
	var pinned []string
	var unpinned []string

	filterChat := func(chat *UnreadChat) bool {
		if chat == nil {
			return false
		}
		if m.showArchived {
			return chat.IsArchived
		}
		return !chat.IsArchived
	}

	pinnedRules := m.cfg.GetPinned()
	used := make(map[string]bool)

	// 1. Pinned chats matching config order (only shown in active unread view)
	if !m.showArchived {
		for _, rule := range pinnedRules {
			for id, chat := range m.unreadChats {
				if filterChat(chat) && chat.IsPinned && !used[id] {
					if config.MatchTarget(rule, id, chat.Name) {
						pinned = append(pinned, id)
						used[id] = true
						break
					}
				}
			}
		}
		// 2. Any other pinned chats
		for id, chat := range m.unreadChats {
			if filterChat(chat) && chat.IsPinned && !used[id] {
				pinned = append(pinned, id)
				used[id] = true
			}
		}
	}

	// 3. Unpinned chats (or all matching chats when in archived mode)
	for id, chat := range m.unreadChats {
		if filterChat(chat) && (!chat.IsPinned || m.showArchived) && !used[id] {
			unpinned = append(unpinned, id)
			used[id] = true
		}
	}

	// Sort unpinned by LastReceived desc
	sort.SliceStable(unpinned, func(i, j int) bool {
		c1 := m.unreadChats[unpinned[i]]
		c2 := m.unreadChats[unpinned[j]]
		if c1 == nil || c2 == nil {
			return false
		}
		return c1.LastReceived.After(c2.LastReceived)
	})

	m.chatOrder = append(pinned, unpinned...)

	if m.cursor >= len(m.chatOrder) {
		m.cursor = max(0, len(m.chatOrder)-1)
	}
}

func (m *Model) updateUnreadChatNames() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.contacts) > 0 {
		m.setContacts(m.contacts)
	}
	for id, chat := range m.unreadChats {
		lastMsgName := ""
		if len(chat.Messages) > 0 {
			lastMsgName = chat.Messages[len(chat.Messages)-1].ChatName
		}
		name, isGroup := m.resolveChatName(chat.ChatID, lastMsgName, chat.Name)
		chat.Name = name
		chat.IsGroup = isGroup
		if m.adapter != nil {
			chat.IsArchived = m.adapter.IsChatArchived(chat.ChatID)
		}

		if !chat.IsPinned && m.isMuted(chat.ChatID, chat.Name) {
			delete(m.unreadChats, id)
		}
	}
	m.sortChatOrderLocked()
}

func (m *Model) rebuildUnreadChats(msgs []domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Retain pinned chats and chats already read or active in this session
	newUnreadChats := make(map[string]*UnreadChat)
	for id, chat := range m.unreadChats {
		if chat.IsPinned || m.readChats[id] || id == m.activeChatID {
			newUnreadChats[id] = chat
		}
	}
	m.unreadChats = newUnreadChats

	for _, msg := range msgs {
		if m.isMuted(msg.ChatID, msg.ChatName, msg.SenderName) {
			continue
		}

		chat, exists := m.unreadChats[msg.ChatID]
		if !exists {
			for _, existing := range m.unreadChats {
				if existing.IsPinned && config.MatchTarget(existing.ChatID, msg.ChatID, msg.ChatName) {
					chat = existing
					exists = true
					break
				}
			}
		}

		isPinned := m.isPinned(msg.ChatID, msg.ChatName)
		isArchived := false
		if m.adapter != nil {
			isArchived = m.adapter.IsChatArchived(msg.ChatID)
		}
		if !exists {
			name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, msg.SenderName)
			chat = &UnreadChat{
				ChatID:       msg.ChatID,
				Name:         name,
				IsGroup:      isGroup,
				IsPinned:     isPinned,
				IsArchived:   isArchived,
				UnreadCount:  1,
				Sender:       msg.Sender,
				Messages:     []domain.Message{msg},
				LastReceived: msg.Timestamp,
			}
			m.unreadChats[msg.ChatID] = chat
		} else {
			alreadyPresent := false
			for _, existing := range chat.Messages {
				if existing.ID == msg.ID {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				if !m.readChats[msg.ChatID] && msg.ChatID != m.activeChatID {
					chat.UnreadCount++
				}
				if len(chat.Messages) < 50 {
					chat.Messages = append(chat.Messages, msg)
				} else {
					copy(chat.Messages, chat.Messages[1:])
					chat.Messages[len(chat.Messages)-1] = msg
				}
				chat.LastReceived = msg.Timestamp
			}
			if isPinned {
				chat.IsPinned = true
			}
			chat.IsArchived = isArchived
			if !isRealChatName(chat.Name, chat.ChatID) {
				name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, chat.Name)
				chat.Name = name
				chat.IsGroup = isGroup
			}
		}
	}

	var unknownGroups []string
	for _, chat := range m.unreadChats {
		if (chat.IsGroup || strings.Contains(chat.ChatID, "@g.us") || strings.HasPrefix(chat.ChatID, "120363")) && !isRealChatName(chat.Name, chat.ChatID) {
			unknownGroups = append(unknownGroups, chat.ChatID)
		}
	}
	if len(unknownGroups) > 0 && m.adapter != nil {
		m.adapter.EnsureGroupNames(m.ctx, unknownGroups)
	}

	m.sortChatOrderLocked()
}

func (m *Model) handleIncomingMessage(msg domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Archived chats should never trigger notifications and are muted by default
	isArchived := false
	if m.adapter != nil {
		isArchived = m.adapter.IsChatArchived(msg.ChatID)
	}

	// 1. Check if muted
	if m.isMuted(msg.ChatID, msg.ChatName, msg.SenderName) {
		if m.view == ViewChat && m.activeChatID == msg.ChatID {
			m.activeMsgs = append(m.activeMsgs, msg)
			m.chatScrollOffset = 0
			go func() {
				_ = m.adapter.MarkRead(m.ctx, msg.ChatID, msg.Sender, []string{msg.ID})
			}()
		}
		return
	}

	// 2. If currently viewing this chat, append directly
	if m.view == ViewChat && m.activeChatID == msg.ChatID {
		m.activeMsgs = append(m.activeMsgs, msg)
		m.chatScrollOffset = 0
		go func() {
			_ = m.adapter.MarkRead(m.ctx, msg.ChatID, msg.Sender, []string{msg.ID})
		}()
		if chat, exists := m.unreadChats[msg.ChatID]; exists {
			if len(chat.Messages) < 50 {
				chat.Messages = append(chat.Messages, msg)
			} else {
				copy(chat.Messages, chat.Messages[1:])
				chat.Messages[len(chat.Messages)-1] = msg
			}
			chat.LastReceived = msg.Timestamp
			chat.UnreadCount = 0
		}
		return
	}

	chat, exists := m.unreadChats[msg.ChatID]
	if !exists {
		for id, existing := range m.unreadChats {
			if existing.IsPinned && config.MatchTarget(id, msg.ChatID, msg.ChatName) {
				delete(m.unreadChats, id)
				existing.ChatID = msg.ChatID
				m.unreadChats[msg.ChatID] = existing
				chat = existing
				exists = true
				break
			}
		}
	}

	isPinned := m.isPinned(msg.ChatID, msg.ChatName)
	if !exists {
		name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, msg.SenderName)
		chat = &UnreadChat{
			ChatID:       msg.ChatID,
			Name:         name,
			IsGroup:      isGroup,
			IsPinned:     isPinned,
			IsArchived:   isArchived,
			UnreadCount:  1,
			Sender:       msg.Sender,
			Messages:     []domain.Message{msg},
			LastReceived: msg.Timestamp,
		}
		m.unreadChats[msg.ChatID] = chat
	} else {
		chat.UnreadCount++
		if len(chat.Messages) < 50 {
			chat.Messages = append(chat.Messages, msg)
		} else {
			copy(chat.Messages, chat.Messages[1:])
			chat.Messages[len(chat.Messages)-1] = msg
		}
		chat.LastReceived = msg.Timestamp
		if isPinned {
			chat.IsPinned = true
		}
		chat.IsArchived = isArchived
		if !isRealChatName(chat.Name, chat.ChatID) {
			name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, chat.Name)
			chat.Name = name
			chat.IsGroup = isGroup
		}
	}

	if (chat.IsGroup || strings.Contains(chat.ChatID, "@g.us") || strings.HasPrefix(chat.ChatID, "120363")) && !isRealChatName(chat.Name, chat.ChatID) && m.adapter != nil {
		m.adapter.EnsureGroupNames(m.ctx, []string{chat.ChatID})
	}

	m.sortChatOrderLocked()
}

func (m *Model) updateUnreadList(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c":
		m.stopActiveViewer()
		m.CleanupOnExit()
		return tea.Quit

	case "j", "down":
		m.previewStatus = ""
		if m.cursor < len(m.chatOrder)-1 {
			m.cursor++
		}

	case "k", "up":
		m.previewStatus = ""
		if m.cursor > 0 {
			m.cursor--
		}

	case "r", "d": // mark as read / dismiss
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			m.dismissUnread(chatID)
		}

	case "alt+p": // preview media from unread chat
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			if chat != nil {
				var targetMsg *domain.Message
				for i := len(chat.Messages) - 1; i >= 0; i-- {
					if chat.Messages[i].IsMedia() {
						targetMsg = &chat.Messages[i]
						break
					}
				}
				if targetMsg != nil {
					if targetMsg.Type == domain.MessageTypeDocument {
						m.confirmDocAction = true
						m.promptOpenWith = false
						m.pendingDocMsg = targetMsg
						m.previewStatus = ""
						return nil
					}
					return m.previewMediaCmd(*targetMsg)
				}
				m.previewStatus = "No media found in this chat"
				return nil
			}
		}

	case "enter": // open chat
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			m.activeChatID = chatID
			m.activeName = chat.Name
			m.activeMsgs = append([]domain.Message(nil), chat.Messages...)
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.SetHeight(1)
			m.input.Focus()
			m.view = ViewChat
			mediaIndices := m.getChatMediaIndices()
			m.selectedMediaIdx = len(mediaIndices) - 1

			var ids []string
			for _, item := range chat.Messages {
				ids = append(ids, item.ID)
			}
			go func(cID, sID string, mIDs []string) {
				_ = m.adapter.MarkRead(m.ctx, cID, sID, mIDs)
			}(chat.ChatID, chat.Sender, ids)

			chat.UnreadCount = 0
			if m.readChats == nil {
				m.readChats = make(map[string]bool)
			}
			m.readChats[chatID] = true

			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				return tea.Batch(tea.ClearScreen, textinput.Blink, m.fetchHistoryCmd(chatID, limit, time.Time{}, true))
			}

			return tea.Batch(tea.ClearScreen, textinput.Blink)
		}

	case "a": // toggle archived chats
		m.mu.Lock()
		m.showArchived = !m.showArchived
		m.cursor = 0
		m.sortChatOrderLocked()
		m.mu.Unlock()
		return tea.ClearScreen

	case "A", "shift+a": // archive / unarchive selected chat
		m.mu.Lock()
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			if chat != nil {
				targetArchived := !chat.IsArchived
				chat.IsArchived = targetArchived
				if targetArchived {
					chat.IsPinned = false
					if len(chat.Messages) == 0 {
						delete(m.unreadChats, chatID)
					}
				}
				m.sortChatOrderLocked()
				if m.cursor >= len(m.chatOrder) {
					if len(m.chatOrder) > 0 {
						m.cursor = len(m.chatOrder) - 1
					} else {
						m.cursor = 0
					}
				}
				if m.adapter != nil {
					go func(cID string, isArchived bool) {
						_ = m.adapter.SetChatArchived(m.ctx, cID, isArchived)
					}(chatID, targetArchived)
				}
			}
		}
		m.mu.Unlock()
		return tea.ClearScreen

	case "esc":
		if m.showArchived {
			m.mu.Lock()
			m.showArchived = false
			m.cursor = 0
			m.sortChatOrderLocked()
			m.mu.Unlock()
			return tea.ClearScreen
		}

	case "?", "alt+?", "f1":
		m.showHelp = !m.showHelp
		return nil

	case "n", "c": // new message / contact picker
		m.view = ViewContactPicker
		m.contactSearch.Reset()
		m.contactSearch.Focus()
		m.contactCursor = 0
		m.contactOffset = 0
		m.filterContacts("")
		if len(m.contacts) == 0 {
			m.loadingContact = true
			return tea.Batch(tea.ClearScreen, textinput.Blink, m.loadContacts())
		}
		m.loadingContact = false
		return tea.Batch(tea.ClearScreen, textinput.Blink)

	case "R", "ctrl+r": // manual refresh
		return tea.Batch(
			m.performRefresh(),
			m.loadContacts(),
		)
	}
	return nil
}

func (m *Model) dismissUnread(chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	chat, exists := m.unreadChats[chatID]
	baseID := chatID
	if idx := strings.Index(chatID, ":"); idx != -1 {
		if atIdx := strings.Index(chatID, "@"); atIdx != -1 && atIdx > idx {
			baseID = chatID[:idx] + chatID[atIdx:]
			if !exists {
				chat = m.unreadChats[baseID]
			}
		}
	}

	go func(id string) {
		_ = m.adapter.DismissUnread(m.ctx, id)
	}(chatID)

	if chat != nil && chat.IsPinned {
		chat.Messages = nil
		return
	}

	delete(m.unreadChats, chatID)
	if baseID != chatID {
		delete(m.unreadChats, baseID)
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
}

// CleanupOnExit dismisses all read chats from persistent storage when the TUI quits.
func (m *Model) CleanupOnExit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cleanupDone || m.adapter == nil {
		return
	}
	m.cleanupDone = true

	m.stopActiveViewer()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dismissed := make(map[string]bool)

	for id, chat := range m.unreadChats {
		if chat != nil && chat.UnreadCount == 0 && !chat.IsPinned {
			if !dismissed[id] {
				_ = m.adapter.DismissUnread(cleanupCtx, id)
				dismissed[id] = true
			}
		}
	}
	for id := range m.readChats {
		if !dismissed[id] {
			_ = m.adapter.DismissUnread(cleanupCtx, id)
			dismissed[id] = true
		}
	}
}

func (m *Model) updateChat(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.stopActiveViewer()
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		m.mu.Lock()
		if chat, exists := m.unreadChats[m.activeChatID]; exists {
			chat.Messages = append([]domain.Message(nil), m.activeMsgs...)
			chat.UnreadCount = 0
		}
		m.sortChatOrderLocked()
		m.mu.Unlock()
		m.view = ViewUnreadList
		return tea.ClearScreen

	case "alt+?", "f1":
		m.showHelp = !m.showHelp
		return nil

	case "ctrl+c":
		m.stopActiveViewer()
		m.CleanupOnExit()
		return tea.Quit

	case "alt+x":
		m.stopActiveViewer()
		m.previewStatus = "Playback stopped"
		return nil

	case "alt+p":
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		mediaIndices := m.getChatMediaIndices()
		if len(mediaIndices) == 0 {
			m.previewStatus = "No media found in this chat"
			return nil
		}
		if m.selectedMediaIdx < 0 || m.selectedMediaIdx >= len(mediaIndices) {
			m.selectedMediaIdx = len(mediaIndices) - 1
		}
		targetMsg := m.activeMsgs[mediaIndices[m.selectedMediaIdx]]
		if targetMsg.Type == domain.MessageTypeDocument {
			m.confirmDocAction = true
			m.promptOpenWith = false
			m.pendingDocMsg = &targetMsg
			m.previewStatus = ""
			return nil
		}
		return m.previewMediaCmd(targetMsg)

	case "alt+up", "alt+k", "alt+left":
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		mediaIndices := m.getChatMediaIndices()
		if len(mediaIndices) > 0 {
			if m.selectedMediaIdx > 0 {
				m.selectedMediaIdx--
			} else {
				m.selectedMediaIdx = len(mediaIndices) - 1
			}
			m.scrollToMediaMessage(mediaIndices[m.selectedMediaIdx])
			m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[mediaIndices[m.selectedMediaIdx]].Type)
		}
		return nil

	case "alt+down", "alt+j", "alt+right":
		m.confirmDocAction = false
		m.pendingDocMsg = nil
		mediaIndices := m.getChatMediaIndices()
		if len(mediaIndices) > 0 {
			if m.selectedMediaIdx < len(mediaIndices)-1 {
				m.selectedMediaIdx++
			} else {
				m.selectedMediaIdx = 0
			}
			m.scrollToMediaMessage(mediaIndices[m.selectedMediaIdx])
			m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[mediaIndices[m.selectedMediaIdx]].Type)
		}
		return nil

	case "alt+f":
		m.previewStatus = "Opening file selector..."
		return m.pickFileCmd()

	case "pgup":
		m.chatScrollOffset += 5
		return nil

	case "ctrl+u":
		if m.cfg.IsHistoryPersistEnabled() {
			var beforeTS time.Time
			if len(m.activeMsgs) > 0 {
				beforeTS = m.activeMsgs[0].Timestamp
			}
			m.previewStatus = "Fetching history..."
			return m.fetchHistoryCmd(m.activeChatID, 5, beforeTS, false)
		}
		m.chatScrollOffset += 5
		return nil

	case "pgdown", "ctrl+d":
		m.chatScrollOffset = max(0, m.chatScrollOffset-5)
		return nil

	case "up":
		if m.input.Value() == "" {
			m.chatScrollOffset++
			return nil
		}

	case "down":
		if m.input.Value() == "" {
			m.chatScrollOffset = max(0, m.chatScrollOffset-1)
			return nil
		}

	case "shift+enter", "alt+enter", "ctrl+j", "ctrl+enter":
		m.input.InsertString("\n")
		m.input.SetHeight(min(5, max(1, m.input.LineCount())))
		return nil

	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.previewStatus = "Sending..."
		m.input.Reset()
		m.input.SetHeight(1)
		chatID := m.activeChatID

		if strings.HasPrefix(text, "file://") {
			filePath, caption := parseFileURI(text)
			return func() tea.Msg {
				sentMsg, err := m.adapter.SendFileMessage(m.ctx, chatID, filePath, caption)
				if err != nil {
					return sendErrMsg{ChatID: chatID, Text: text, Err: err}
				}
				return messageSentMsg(sentMsg)
			}
		}

		return func() tea.Msg {
			sentMsg, err := m.adapter.SendTextMessage(m.ctx, chatID, text)
			if err != nil {
				return sendErrMsg{ChatID: chatID, Text: text, Err: err}
			}
			return messageSentMsg(sentMsg)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.input.SetHeight(min(5, max(1, m.input.LineCount())))
	return cmd
}

func (m *Model) updateContactPicker(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.view = ViewUnreadList
		return tea.ClearScreen

	case "alt+?", "f1":
		m.showHelp = !m.showHelp
		return nil

	case "ctrl+c":
		m.stopActiveViewer()
		m.CleanupOnExit()
		return tea.Quit

	case "down", "ctrl+n":
		if m.contactCursor < len(m.filteredList)-1 {
			m.contactCursor++
			maxItems := m.maxVisibleContacts()
			if m.contactCursor >= m.contactOffset+maxItems {
				m.contactOffset = m.contactCursor - maxItems + 1
			}
		}
		return nil

	case "up", "ctrl+p":
		if m.contactCursor > 0 {
			m.contactCursor--
			if m.contactCursor < m.contactOffset {
				m.contactOffset = m.contactCursor
			}
		}
		return nil

	case "enter":
		if len(m.filteredList) > 0 && m.contactCursor < len(m.filteredList) {
			contact := m.filteredList[m.contactCursor]
			m.activeChatID = contact.JID
			m.activeName = contact.Name
			m.activeMsgs = nil
			m.mu.Lock()
			if chat, exists := m.unreadChats[contact.JID]; exists {
				m.activeMsgs = append([]domain.Message(nil), chat.Messages...)
				chat.UnreadCount = 0
			}
			if m.readChats == nil {
				m.readChats = make(map[string]bool)
			}
			m.readChats[contact.JID] = true
			m.mu.Unlock()
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.SetHeight(1)
			m.input.Focus()
			m.view = ViewChat
			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				return tea.Batch(tea.ClearScreen, textinput.Blink, m.fetchHistoryCmd(contact.JID, limit, time.Time{}, true))
			}
			return tea.Batch(tea.ClearScreen, textinput.Blink)
		}
		// Direct input fallback
		rawInput := strings.TrimSpace(m.contactSearch.Value())
		var digitCount int
		for _, r := range rawInput {
			if r >= '0' && r <= '9' {
				digitCount++
			}
		}
		if digitCount >= 5 || strings.Contains(rawInput, "@") {
			targetID := rawInput
			if !strings.Contains(targetID, "@") {
				targetID = targetID + "@s.whatsapp.net"
			}
			m.activeChatID = targetID
			m.activeName = rawInput
			m.activeMsgs = nil
			m.mu.Lock()
			if chat, exists := m.unreadChats[targetID]; exists {
				m.activeMsgs = append([]domain.Message(nil), chat.Messages...)
				chat.UnreadCount = 0
			}
			if m.readChats == nil {
				m.readChats = make(map[string]bool)
			}
			m.readChats[targetID] = true
			m.mu.Unlock()
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.SetHeight(1)
			m.input.Focus()
			m.view = ViewChat
			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				return tea.Batch(tea.ClearScreen, textinput.Blink, m.fetchHistoryCmd(targetID, limit, time.Time{}, true))
			}
			return tea.Batch(tea.ClearScreen, textinput.Blink)
		}
		return nil
	}

	var cmd tea.Cmd
	oldVal := m.contactSearch.Value()
	m.contactSearch, cmd = m.contactSearch.Update(msg)
	if m.contactSearch.Value() != oldVal {
		m.filterContacts(m.contactSearch.Value())
		m.contactCursor = 0
		m.contactOffset = 0
	}
	return cmd
}

func (m *Model) filterContacts(query string) {
	trimmed := strings.TrimSpace(query)
	q := strings.ToLower(trimmed)
	if q == "" {
		var list []domain.Contact
		for _, c := range m.contacts {
			if !m.isMuted(c.JID, c.Name, c.PushName) {
				list = append(list, c)
			}
		}
		m.filteredList = list
		m.contactCursor = 0
		m.contactOffset = 0
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
			Name: fmt.Sprintf("Direct message to %s", trimmed),
		})
	}

	for _, c := range m.contacts {
		if m.isMuted(c.JID, c.Name, c.PushName) {
			continue
		}

		nameMatch := strings.Contains(strings.ToLower(c.Name), q)
		if c.IsGroup {
			// Never match group JID (e.g. 120363@g.us)
			if nameMatch {
				res = append(res, c)
			}
			continue
		}

		pushMatch := c.PushName != "" && strings.Contains(strings.ToLower(c.PushName), q)
		phone := c.JID
		if idx := strings.Index(phone, "@"); idx != -1 {
			phone = phone[:idx]
		}
		phoneMatch := strings.Contains(phone, q)

		if nameMatch || pushMatch || phoneMatch {
			res = append(res, c)
		}
	}
	m.filteredList = res
	m.contactCursor = 0
	m.contactOffset = 0
}

// -------------------------------------------------------------
// Styles and Layout Dimensions
// -------------------------------------------------------------

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#A6E3A1"))

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086"))

	dividerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#313244"))

	badgeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#11111B")).
			Background(lipgloss.Color("#FAB387")).
			Padding(0, 1)

	pinBadgeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#11111B")).
			Background(lipgloss.Color("#F9E2AF")).
			Padding(0, 1)

	selectedTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#89B4FA"))

	normalTitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#CDD6F4"))

	snippetStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585B70"))

	jidStyle = lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color("#585B70"))
)

func (m *Model) contentWidth() int {
	if m.width <= 0 {
		return 76
	}
	if m.width <= 40 {
		return max(20, m.width-2)
	}
	return min(76, m.width-4)
}

func (m *Model) maxCanvasHeight() int {
	if m.height <= 0 {
		return 18
	}
	return max(8, min(30, m.height-4))
}

func (m *Model) maxVisibleChats() int {
	ch := m.maxCanvasHeight()
	// Header (3) + Footer (1) = 4 lines. Each chat is 3 lines.
	return max(1, (ch-4)/3)
}

func (m *Model) maxVisibleContacts() int {
	ch := m.maxCanvasHeight()
	// Header (3) + Search (2) + Footer (2) = 7 lines.
	return max(4, ch-7)
}

// -------------------------------------------------------------
// Core Viewport-Safe Render Pipeline
// -------------------------------------------------------------

func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}

	var lines []string
	switch m.view {
	case ViewUnreadList:
		lines = m.renderUnreadListView()
	case ViewChat:
		lines = m.renderChatView()
	case ViewContactPicker:
		lines = m.renderContactPickerView()
	default:
		return ""
	}

	cw := m.contentWidth()
	maxH := max(4, m.height-4)
	if len(lines) > maxH {
		lines = lines[:maxH]
	}

	leftPad := max(0, (m.width-cw)/2)
	prefix := strings.Repeat(" ", leftPad)

	topPad := max(0, (m.height-len(lines))/2)
	// Safety limit: ensure topPad + len(lines) <= m.height - 2
	if topPad+len(lines) > m.height-2 {
		topPad = max(0, m.height-2-len(lines))
	}

	var b strings.Builder
	for i := 0; i < topPad; i++ {
		b.WriteString("\n")
	}
	for i, l := range lines {
		b.WriteString(prefix)
		b.WriteString(l)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

func (m *Model) renderUnreadListView() []string {
	cw := m.contentWidth()
	var lines []string

	// Header: count active unread vs archived chats
	unreadCount := 0
	archivedCount := 0
	for _, chat := range m.unreadChats {
		if chat != nil {
			if chat.IsArchived {
				if chat.UnreadCount > 0 {
					archivedCount++
				}
			} else {
				if chat.UnreadCount > 0 {
					unreadCount++
				}
			}
		}
	}

	statusText := string(m.status)
	if m.showArchived {
		statusText = fmt.Sprintf("archived (%d chats) · [a/Esc] back to unreads", len(m.chatOrder))
	} else if m.status == domain.StatusConnected {
		statusText = fmt.Sprintf("connected · %d unread", unreadCount)
	}
	if !m.showArchived && archivedCount > 0 {
		statusText += fmt.Sprintf(" · [a] %d archived", archivedCount)
	}

	var headerText string
	if m.showArchived {
		headerText = fmt.Sprintf("%s  %s",
			titleStyle.Render("watui"),
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("· [ARCHIVED CHATS]")+statusStyle.Render(fmt.Sprintf(" (%d)", len(m.chatOrder))),
		)
	} else {
		headerText = fmt.Sprintf("%s  %s",
			titleStyle.Render("watui"),
			statusStyle.Render("· "+statusText),
		)
	}
	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	if len(m.chatOrder) == 0 {
		if m.showArchived {
			lines = append(lines, "  Archived Inbox Zero")
			lines = append(lines, "")
			lines = append(lines, "  No archived chats with unread messages.")
			lines = append(lines, "")
			lines = append(lines, statusStyle.Render("  Press [a] or [Esc] to return to unread messages."))
			lines = append(lines, "")
		} else {
			lines = append(lines, "  Inbox Zero")
			lines = append(lines, "")
			lines = append(lines, "  No unread messages.")
			lines = append(lines, "")
			if archivedCount > 0 {
				lines = append(lines, statusStyle.Render(fmt.Sprintf("  Press [a] to view %d archived chat(s) with unread messages.", archivedCount)))
				lines = append(lines, "")
			}
			lines = append(lines, statusStyle.Render("  Press [n] to compose to a contact, or wait for incoming messages."))
			lines = append(lines, "")
		}
	} else {
		maxChats := m.maxVisibleChats()
		start := 0
		if m.cursor >= maxChats {
			start = m.cursor - maxChats + 1
		}
		end := min(len(m.chatOrder), start+maxChats)

		for i := start; i < end; i++ {
			chatID := m.chatOrder[i]
			chat := m.unreadChats[chatID]

			var badge string
			if chat.UnreadCount > 0 {
				badge = badgeStyle.Render(fmt.Sprintf("%d", chat.UnreadCount))
			} else if chat.IsPinned {
				badge = pinBadgeStyle.Render("PIN")
			}

			name := chat.Name
			if chat.IsGroup || strings.Contains(chat.ChatID, "@g.us") {
				name = "[Group] " + chat.Name
			}
			if chat.IsPinned {
				name = "* " + name
			}
			maxNameLen := max(10, cw-22)
			rName := []rune(name)
			if len(rName) > maxNameLen {
				name = string(rName[:maxNameLen-3]) + "..."
			}

			lastMsg := ""
			if len(chat.Messages) > 0 {
				last := chat.Messages[len(chat.Messages)-1]
				clean := strings.ReplaceAll(last.Body, "\r", "")
				clean = strings.ReplaceAll(clean, "\n", " ")
				clean = strings.TrimSpace(clean)

				// For groups, prefix snippet with sender name so context is clear: "Sender: message"
				if (chat.IsGroup || strings.Contains(chat.ChatID, "@g.us")) && !last.IsFromMe {
					sender := last.SenderName
					if sender == "" {
						sender = last.Sender
						if idx := strings.Index(sender, "@"); idx != -1 {
							sender = sender[:idx]
						}
					}
					if sender != "" {
						clean = sender + ": " + clean
					}
				}

				lastMsg = clean
			} else if chat.IsPinned {
				lastMsg = "(pinned · press enter to chat)"
			}

			maxMsgLen := max(15, cw-10)
			rMsg := []rune(lastMsg)
			if len(rMsg) > maxMsgLen {
				lastMsg = string(rMsg[:maxMsgLen-3]) + "..."
			}

			timeStr := ""
			if !chat.LastReceived.IsZero() {
				timeStr = chat.LastReceived.Format("15:04")
			} else if chat.IsPinned {
				timeStr = "pin"
			}

			cursorPrefix := "  "
			if i == m.cursor {
				cursorPrefix = "> "
			}

			badgePrefix := ""
			if badge != "" {
				badgePrefix = badge + " "
			}

			if i == m.cursor {
				lines = append(lines, fmt.Sprintf("%s%s%s  %s",
					cursorPrefix,
					badgePrefix,
					selectedTitleStyle.Render(name),
					statusStyle.Render(timeStr),
				))
				lines = append(lines, snippetStyle.Render("    "+lastMsg))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s%s  %s",
					cursorPrefix,
					badgePrefix,
					normalTitleStyle.Render(name),
					statusStyle.Render(timeStr),
				))
				lines = append(lines, snippetStyle.Render("    "+lastMsg))
			}
			lines = append(lines, "")
		}
	}

	statusNotice := ""
	if m.promptOpenWith {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Open with: ") + m.openWithInput.View() + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(" (Enter to open, Esc to cancel)")
	} else if m.confirmDocAction {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Document: Open [o], Open with [w], or Save [s]? (Esc to cancel)")
	} else if m.confirmSave {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Save to Downloads? (y/N)")
	} else if m.previewStatus != "" {
		statusNotice = lipgloss.NewStyle().Foreground(lipgloss.Color("#89DCEB")).Render(m.previewStatus)
	}

	selectedHasMedia := false
	if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
		if chat := m.unreadChats[m.chatOrder[m.cursor]]; chat != nil {
			for _, msg := range chat.Messages {
				if msg.IsMedia() {
					selectedHasMedia = true
					break
				}
			}
		}
	}

	helpText := "[Enter] Open · [Shift+A] Archive · [r] Dismiss · [n] New · [q] Quit"
	if m.showArchived {
		if selectedHasMedia {
			helpText = "[Enter] Open · [Shift+A] Unarchive · [a/Esc] Back to Unreads · [Alt+P] Preview · [r] Dismiss · [q] Quit"
		} else {
			helpText = "[Enter] Open · [Shift+A] Unarchive · [a/Esc] Back to Unreads · [r] Dismiss · [q] Quit"
		}
	} else if archivedCount > 0 {
		if selectedHasMedia {
			helpText = fmt.Sprintf("[Enter] Open · [Shift+A] Archive · [a] Archived (%d) · [Alt+P] Preview · [r] Dismiss · [n] New · [q] Quit", archivedCount)
		} else {
			helpText = fmt.Sprintf("[Enter] Open · [Shift+A] Archive · [a] Archived (%d) · [r] Dismiss · [n] New · [q] Quit", archivedCount)
		}
	} else if selectedHasMedia {
		helpText = "[Enter] Open · [Shift+A] Archive · [Alt+P] Preview · [r] Dismiss · [n] New · [q] Quit"
	}

	if m.showHelp {
		if statusNotice != "" {
			if lipgloss.Width(helpText)+3+lipgloss.Width(statusNotice) > cw {
				if cw >= lipgloss.Width(statusNotice)+12 {
					lines = append(lines, helpStyle.Render("[q] Quit")+" · "+statusNotice)
				} else {
					lines = append(lines, statusNotice)
				}
			} else {
				lines = append(lines, helpStyle.Render(helpText)+" · "+statusNotice)
			}
		} else {
			lines = append(lines, helpStyle.Render(helpText))
		}
	} else {
		if selectedHasMedia && statusNotice != "" {
			mediaHint := "[Alt+P] Preview Media"
			if lipgloss.Width(mediaHint)+3+lipgloss.Width(statusNotice) <= cw {
				lines = append(lines, helpStyle.Render(mediaHint)+" · "+statusNotice)
			} else {
				lines = append(lines, statusNotice)
			}
		} else if selectedHasMedia {
			lines = append(lines, helpStyle.Render("[Alt+P] Preview Media"))
		} else if statusNotice != "" {
			lines = append(lines, statusNotice)
		} else {
			lines = append(lines, "")
		}
	}
	return lines
}

func (m *Model) renderChatView() []string {
	cw := m.contentWidth()
	var lines []string

	chatPrefix := "Chat with"
	if strings.Contains(m.activeChatID, "@g.us") {
		chatPrefix = "Group"
	}
	leftTitle := fmt.Sprintf("%s %s", titleStyle.Render(chatPrefix), selectedTitleStyle.Render(m.activeName))
	escBack := statusStyle.Render("· [Esc] Back")
	leftPart := fmt.Sprintf("%s  %s", leftTitle, escBack)

	rightPart := ""
	if m.activeChatID != "" {
		rightPart = jidStyle.Render(m.activeChatID)
	}

	leftW := lipgloss.Width(leftPart)
	rightW := lipgloss.Width(rightPart)

	var headerText string
	if rightPart != "" && leftW+2+rightW <= cw {
		spaces := strings.Repeat(" ", max(1, cw-leftW-rightW))
		headerText = leftPart + spaces + rightPart
	} else if rightPart != "" && rightW+10 < cw {
		// Truncate name so JID still fits flush to the right
		availForName := max(3, cw-rightW-len(chatPrefix)-len(" · [Esc] Back")-5)
		truncName := m.activeName
		if len(truncName) > availForName {
			truncName = truncName[:max(1, availForName-1)] + "…"
		}
		leftTitle = fmt.Sprintf("%s %s", titleStyle.Render(chatPrefix), selectedTitleStyle.Render(truncName))
		leftPart = fmt.Sprintf("%s  %s", leftTitle, escBack)
		leftW = lipgloss.Width(leftPart)
		spaces := strings.Repeat(" ", max(1, cw-leftW-rightW))
		headerText = leftPart + spaces + rightPart
	} else {
		headerText = leftPart
	}

	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	var msgLines []string
	if len(m.activeMsgs) == 0 {
		msgLines = append(msgLines, statusStyle.Render("  (No messages in this session yet. Type below to send.)"))
	} else {
		msgWrapWidth := max(20, cw-6)
		wrapStyle := lipgloss.NewStyle().Width(msgWrapWidth)

		mediaIndices := m.getChatMediaIndices()
		for i, msg := range m.activeMsgs {
			timeStr := msg.Timestamp.Format("15:04")
			var header string
			mediaBadge := ""
			if msg.IsMedia() {
				pos := -1
				for mIdx, origIdx := range mediaIndices {
					if origIdx == i {
						pos = mIdx
						break
					}
				}
				if pos != -1 {
					if len(mediaIndices) > 1 {
						if pos == m.selectedMediaIdx {
							mediaBadge = " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#11111B")).Background(lipgloss.Color("#FAB387")).Render(fmt.Sprintf(" ▶ [%d/%d] ", pos+1, len(mediaIndices)))
						} else {
							mediaBadge = " " + statusStyle.Render(fmt.Sprintf("[%d/%d]", pos+1, len(mediaIndices)))
						}
					} else {
						if pos == m.selectedMediaIdx {
							mediaBadge = " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#11111B")).Background(lipgloss.Color("#FAB387")).Render(" ▶ MEDIA ")
						}
					}
				}
			}

			if msg.IsFromMe {
				header = fmt.Sprintf("  %s %s%s",
					lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render("You"),
					statusStyle.Render(timeStr),
					mediaBadge,
				)
			} else {
				sender := msg.SenderName
				if sender == "" {
					sender = "Them"
				}
				header = fmt.Sprintf("  %s %s%s",
					selectedTitleStyle.Render(sender),
					statusStyle.Render(timeStr),
					mediaBadge,
				)
			}
			msgLines = append(msgLines, header)

			wrapped := wrapStyle.Render(msg.Body)
			for _, wl := range strings.Split(wrapped, "\n") {
				msgLines = append(msgLines, "    "+wl)
			}
			if i < len(m.activeMsgs)-1 {
				msgLines = append(msgLines, "")
			}
		}
	}

	m.input.SetWidth(max(20, cw-6))
	inputLines := strings.Split(m.input.View(), "\n")
	inputH := len(inputLines)
	availH := max(3, m.maxCanvasHeight()-5-inputH)
	scrollInfo := ""

	if len(msgLines) <= availH {
		lines = append(lines, msgLines...)
	} else {
		maxOffset := len(msgLines) - availH
		if m.chatScrollOffset > maxOffset {
			m.chatScrollOffset = maxOffset
		}
		if m.chatScrollOffset < 0 {
			m.chatScrollOffset = 0
		}
		end := len(msgLines) - m.chatScrollOffset
		start := max(0, end-availH)
		lines = append(lines, msgLines[start:end]...)

		if m.chatScrollOffset > 0 {
			scrollInfo = fmt.Sprintf(" · [PgUp/PgDn] (%d lines up)", m.chatScrollOffset)
		}
	}

	// Pad with blank lines if fewer than availH
	renderedMsgLines := len(lines) - 3
	for k := renderedMsgLines; k < availH; k++ {
		lines = append(lines, "")
	}

	// One line padding on top of the message box
	lines = append(lines, "")
	lines = append(lines, inputLines...)

	mediaIndices := m.getChatMediaIndices()
	mediaHelp := ""
	if len(mediaIndices) > 1 {
		mediaHelp = fmt.Sprintf("[Alt+P] Preview (%d/%d) · [Alt+↑/↓] Media · [Alt+X] Stop", m.selectedMediaIdx+1, len(mediaIndices))
	} else if len(mediaIndices) == 1 {
		mediaHelp = "[Alt+P] Preview Media · [Alt+X] Stop"
	}

	helpText := "[Enter] Send · [Alt+F] Attach · [Esc] Back"
	if m.cfg.IsHistoryPersistEnabled() {
		helpText = "[Enter] Send · [Ctrl+U] Load 5 · [Alt+F] Attach · [Esc] Back"
	}
	if mediaHelp != "" {
		helpText += " · " + mediaHelp
	}
	helpText += scrollInfo

	dynamicHelp := mediaHelp
	if dynamicHelp != "" && scrollInfo != "" {
		dynamicHelp = dynamicHelp + " " + scrollInfo
	} else if dynamicHelp == "" && scrollInfo != "" {
		dynamicHelp = strings.TrimPrefix(scrollInfo, " · ")
	}

	statusNotice := ""
	if m.promptOpenWith {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Open with: ") + m.openWithInput.View() + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(" (Enter to open, Esc to cancel)")
	} else if m.confirmDocAction {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Document: Open [o], Open with [w], or Save [s]? (Esc to cancel)")
	} else if m.confirmSave {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Save to Downloads? (y/N)")
	} else if m.previewStatus != "" {
		statusNotice = lipgloss.NewStyle().Foreground(lipgloss.Color("#89DCEB")).Render(m.previewStatus)
	}

	if m.showHelp {
		if statusNotice != "" {
			if lipgloss.Width(helpText)+3+lipgloss.Width(statusNotice) > cw {
				if cw >= lipgloss.Width(statusNotice)+14 {
					lines = append(lines, helpStyle.Render("[Esc] Back")+" · "+statusNotice)
				} else {
					lines = append(lines, statusNotice)
				}
			} else {
				lines = append(lines, helpStyle.Render(helpText)+" · "+statusNotice)
			}
		} else {
			lines = append(lines, helpStyle.Render(helpText))
		}
	} else {
		if dynamicHelp != "" && statusNotice != "" {
			if lipgloss.Width(dynamicHelp)+3+lipgloss.Width(statusNotice) <= cw {
				lines = append(lines, helpStyle.Render(dynamicHelp)+" · "+statusNotice)
			} else {
				lines = append(lines, statusNotice)
			}
		} else if dynamicHelp != "" {
			lines = append(lines, helpStyle.Render(dynamicHelp))
		} else if statusNotice != "" {
			lines = append(lines, statusNotice)
		} else {
			lines = append(lines, "")
		}
	}

	return lines
}

func (m *Model) renderContactPickerView() []string {
	cw := m.contentWidth()
	var lines []string

	headerText := fmt.Sprintf("%s  %s",
		titleStyle.Render("Send Message Upfront"),
		statusStyle.Render("· Select contact without loading history"),
	)
	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	lines = append(lines, m.contactSearch.View())
	lines = append(lines, "")

	maxItems := m.maxVisibleContacts()

	if m.loadingContact {
		lines = append(lines, statusStyle.Render("  Loading contacts..."))
		for k := 1; k < maxItems; k++ {
			lines = append(lines, "")
		}
	} else if len(m.filteredList) == 0 {
		query := m.contactSearch.Value()
		lines = append(lines, statusStyle.Render(fmt.Sprintf("  No contacts found matching %q", query)))
		for k := 1; k < maxItems; k++ {
			lines = append(lines, "")
		}
	} else {
		start := m.contactOffset
		if start < 0 {
			start = 0
		}
		if start >= len(m.filteredList) {
			start = max(0, len(m.filteredList)-1)
		}
		end := min(len(m.filteredList), start+maxItems)

		for i := start; i < end; i++ {
			c := m.filteredList[i]
			label := c.Name
			phone := ""
			if c.IsGroup {
				phone = "[Group]"
			} else {
				phone = c.JID
				if idx := strings.Index(phone, "@"); idx != -1 {
					phone = phone[:idx]
				}
				if c.PushName != "" && c.PushName != c.Name && !strings.HasPrefix(c.Name, "Direct message to ") {
					label += fmt.Sprintf(" (%s)", c.PushName)
				}
			}

			prefix := "  "
			isSelected := (i == m.contactCursor)
			if isSelected {
				prefix = "> "
			}

			prefixLen := 2
			phoneLen := len([]rune(phone))
			avail := max(10, cw-prefixLen-phoneLen-2)
			rLabel := []rune(label)
			if len(rLabel) > avail {
				label = string(rLabel[:avail-3]) + "..."
			}

			labelLen := len([]rune(label))
			gapLen := max(2, cw-(prefixLen+labelLen+phoneLen))
			gap := strings.Repeat(" ", gapLen)

			var styledLabel string
			if isSelected {
				styledLabel = selectedTitleStyle.Render(label)
			} else {
				styledLabel = normalTitleStyle.Render(label)
			}
			styledPhone := statusStyle.Render(phone)

			lines = append(lines, prefix+styledLabel+gap+styledPhone)
		}

		rendered := end - start
		for k := rendered; k < maxItems; k++ {
			lines = append(lines, "")
		}
	}

	lines = append(lines, "")
	if m.showHelp {
		lines = append(lines, helpStyle.Render("[Up/Down] Navigate · [Enter] Select & Compose · [Esc] Cancel"))
	} else {
		lines = append(lines, "")
	}

	return lines
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

func (m *Model) previewMediaCmd(msg domain.Message) tea.Cmd {
	m.previewStatus = fmt.Sprintf("Downloading %s...", msg.Type)
	m.confirmSave = false
	cmdStr := m.cfg.GetPreviewCommand(string(msg.Type))
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(m.ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		if err := m.launchViewer(cmdStr, filePath); err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return mediaPreviewSuccessMsg{Path: filePath}
	}
}

func (m *Model) getChatMediaIndices() []int {
	var indices []int
	for i, msg := range m.activeMsgs {
		if msg.IsMedia() {
			indices = append(indices, i)
		}
	}
	return indices
}

func (m *Model) scrollToMediaMessage(msgIdx int) {
	cw := m.contentWidth()
	msgWrapWidth := max(20, cw-6)
	wrapStyle := lipgloss.NewStyle().Width(msgWrapWidth)

	linesAfter := 0
	for i := msgIdx + 1; i < len(m.activeMsgs); i++ {
		linesAfter += 1 // header
		wrapped := wrapStyle.Render(m.activeMsgs[i].Body)
		linesAfter += len(strings.Split(wrapped, "\n"))
		linesAfter += 1 // spacing
	}
	m.chatScrollOffset = linesAfter
}

func (m *Model) downloadAndOpenDocCmd(msg domain.Message, customCmd ...string) tea.Cmd {
	m.previewStatus = "Downloading document..."
	m.confirmSave = false
	m.confirmDocAction = false
	m.promptOpenWith = false
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(m.ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		cmdStr := ""
		if len(customCmd) > 0 && strings.TrimSpace(customCmd[0]) != "" {
			cmdStr = strings.TrimSpace(customCmd[0])
		} else {
			cmdStr = m.cfg.GetPreviewCommandForFile(filePath, "document")
		}
		return docDownloadedToOpenMsg{Path: filePath, Cmd: cmdStr}
	}
}

func (m *Model) downloadAndSaveDocCmd(msg domain.Message) tea.Cmd {
	m.previewStatus = "Downloading & saving document..."
	m.confirmSave = false
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(m.ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		destPath, err := saveToDownloads(filePath)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return docSavedMsg{DestPath: destPath}
	}
}

func saveToDownloads(srcPath string) (string, error) {
	if srcPath == "" {
		return "", fmt.Errorf("no media file to save")
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot find home directory: %w", err)
	}

	downloadsDir := filepath.Join(home, "Downloads")
	if xdg := os.Getenv("XDG_DOWNLOAD_DIR"); xdg != "" {
		downloadsDir = xdg
	}

	if err := os.MkdirAll(downloadsDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create downloads directory: %w", err)
	}

	fileName := filepath.Base(srcPath)
	// If cached filename has prefix like <msgID>_<realFileName>, clean it up for ~/Downloads
	if idx := strings.Index(fileName, "_"); idx != -1 && idx < len(fileName)-1 {
		prefix := fileName[:idx]
		if len(prefix) >= 8 {
			fileName = fileName[idx+1:]
		}
	}

	destPath := filepath.Join(downloadsDir, fileName)

	// Avoid overwriting existing files
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(fileName)
		base := strings.TrimSuffix(fileName, ext)
		for i := 1; i < 1000; i++ {
			candidate := filepath.Join(downloadsDir, fmt.Sprintf("%s_%d%s", base, i, ext))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				destPath = candidate
				break
			}
		}
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("failed to read cached file: %w", err)
	}

	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write to downloads: %w", err)
	}

	displayPath := destPath
	if strings.HasPrefix(destPath, home) {
		displayPath = "~" + strings.TrimPrefix(destPath, home)
	}

	return displayPath, nil
}

func (m *Model) stopActiveViewer() {
	m.viewerMu.Lock()
	defer m.viewerMu.Unlock()
	if m.activeViewerCmd != nil && m.activeViewerCmd.Process != nil {
		_ = m.activeViewerCmd.Process.Kill()
		m.activeViewerCmd = nil
	}
}

func (m *Model) launchViewer(cmdStr string, filePath string) error {
	m.stopActiveViewer()

	if strings.TrimSpace(cmdStr) == "" {
		cmdStr = config.DefaultOpenCommand()
	}

	cleanPath := filepath.Clean(filePath)

	var cmd *exec.Cmd
	if strings.Contains(cmdStr, "%s") {
		fullCmd := fmt.Sprintf(cmdStr, cleanPath)
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", fullCmd)
		} else {
			cmd = exec.Command("sh", "-c", fullCmd)
		}
	} else {
		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			parts = strings.Fields(config.DefaultOpenCommand())
		}
		args := append(parts[1:], cleanPath)
		cmd = exec.Command(parts[0], args...)
	}

	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	m.viewerMu.Lock()
	m.activeViewerCmd = cmd
	m.viewerMu.Unlock()

	go func() {
		_ = cmd.Wait()
		m.viewerMu.Lock()
		if m.activeViewerCmd == cmd {
			m.activeViewerCmd = nil
		}
		m.viewerMu.Unlock()
	}()
	return nil
}

var knownTerminalViewers = []string{
	"nvim", "vim", "vi", "nano", "less", "more", "bat", "cat", "micro",
	"helix", "hx", "kak", "glow", "mdcat", "vd", "visidata", "emacs",
}

func isTerminalViewer(cmdStr string) bool {
	fields := strings.Fields(cmdStr)
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	for _, term := range knownTerminalViewers {
		if base == term {
			return true
		}
	}
	return false
}

func buildTerminalViewerCmd(cmdStr string, filePath string) *exec.Cmd {
	cleanPath := filepath.Clean(filePath)
	if strings.Contains(cmdStr, "%s") {
		fullCmd := fmt.Sprintf(cmdStr, cleanPath)
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", fullCmd)
		}
		return exec.Command("sh", "-c", fullCmd)
	}
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		parts = []string{"less"}
	}
	args := append(parts[1:], cleanPath)
	return exec.Command(parts[0], args...)
}

var errNoFilePicker = errors.New("no file selector found")

func resolvePicker(customCmd string) (pickerType string, isTerminal bool) {
	if strings.TrimSpace(customCmd) != "" {
		trimmed := strings.TrimSpace(customCmd)
		for _, term := range []string{"yazi", "ranger", "lf", "nnn", "fzf", "vifm"} {
			if strings.HasPrefix(trimmed, term) || strings.Contains(trimmed, term) {
				return term, true
			}
		}
		return "", false
	}

	// Auto-detect based on OS and installed tools:
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return "", false // GUI dialogs default on Windows (PowerShell) and macOS (osascript)
	}

	// Linux / BSD: check if GUI dialogs exist
	if _, err := exec.LookPath("zenity"); err == nil {
		return "", false
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		return "", false
	}

	// If no GUI dialogs, check for installed terminal file managers:
	for _, term := range []string{"yazi", "ranger", "lf", "nnn", "fzf"} {
		if _, err := exec.LookPath(term); err == nil {
			return term, true
		}
	}

	return "", false
}

func buildTerminalPickerCmd(pickerType, customCmd string) (*exec.Cmd, string, error) {
	tmpFile, err := os.CreateTemp("", "watui_"+pickerType+"_*")
	if err != nil {
		return nil, "", err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	switch pickerType {
	case "yazi":
		args := []string{"--chooser-file=" + outPath}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "--chooser-file") {
					continue
				}
				args = append(args, w)
			}
		}
		return exec.Command("yazi", args...), outPath, nil

	case "ranger":
		args := []string{"--choosefile=" + outPath}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "--choosefile") {
					continue
				}
				args = append(args, w)
			}
		}
		return exec.Command("ranger", args...), outPath, nil

	case "lf":
		args := []string{"-selection-path=" + outPath}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "-selection-path") {
					continue
				}
				args = append(args, w)
			}
		}
		return exec.Command("lf", args...), outPath, nil

	case "nnn":
		return exec.Command("nnn", "-p", outPath), outPath, nil

	case "fzf":
		return exec.Command("sh", "-c", `fzf > "$1"`, "--", outPath), outPath, nil

	default:
		cmd := exec.Command("sh", "-c", customCmd+` > "$1"`, "--", outPath)
		cmd.Env = append(os.Environ(), "WATUI_PICKER_FILE="+outPath)
		return cmd, outPath, nil
	}
}

func (m *Model) pickFileCmd() tea.Cmd {
	customCmd := ""
	if m.cfg != nil {
		customCmd = m.cfg.GetFilePickerCommand()
	}

	pickerType, isTerm := resolvePicker(customCmd)
	if isTerm {
		execCmd, outPath, err := buildTerminalPickerCmd(pickerType, customCmd)
		if err != nil {
			return func() tea.Msg {
				return filePickErrMsg{Err: err}
			}
		}
		return tea.ExecProcess(execCmd, func(err error) tea.Msg {
			defer func() { _ = os.Remove(outPath) }()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					return filePickedMsg{Path: ""}
				}
				data, readErr := os.ReadFile(outPath)
				if readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
					selected := strings.TrimSpace(string(data))
					if idx := strings.Index(selected, "\n"); idx != -1 {
						selected = strings.TrimSpace(selected[:idx])
					}
					return filePickedMsg{Path: selected}
				}
				return filePickErrMsg{Err: err}
			}
			data, readErr := os.ReadFile(outPath)
			if readErr != nil {
				return filePickErrMsg{Err: readErr}
			}
			selected := strings.TrimSpace(string(data))
			if idx := strings.Index(selected, "\n"); idx != -1 {
				selected = strings.TrimSpace(selected[:idx])
			}
			return filePickedMsg{Path: selected}
		})
	}

	return func() tea.Msg {
		path, err := openFilePicker(customCmd)
		if err != nil {
			return filePickErrMsg{Err: err}
		}
		return filePickedMsg{Path: path}
	}
}

func openFilePicker(customCmd string) (string, error) {
	if strings.TrimSpace(customCmd) != "" {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", customCmd)
		} else {
			cmd = exec.Command("sh", "-c", customCmd)
		}
		out, err := cmd.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
				return "", nil
			}
			return "", fmt.Errorf("picker failed: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			"[System.Reflection.Assembly]::LoadWithPartialName('System.windows.forms') | Out-Null; $f = New-Object System.Windows.Forms.OpenFileDialog; $f.Title = 'Select file to send'; if ($f.ShowDialog() -eq 'OK') { $f.FileName }")
		out, err := cmd.Output()
		if err != nil {
			return "", errNoFilePicker
		}
		return strings.TrimSpace(string(out)), nil

	case "darwin":
		cmd := exec.Command("osascript", "-e", "POSIX path of (choose file with prompt \"Select file to send:\")")
		out, err := cmd.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return "", nil
			}
			return "", errNoFilePicker
		}
		return strings.TrimSpace(string(out)), nil

	default: // Linux / BSD
		if _, err := exec.LookPath("zenity"); err == nil {
			cmd := exec.Command("zenity", "--file-selection", "--title=Select file to send")
			out, err := cmd.Output()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					return "", nil
				}
				return "", fmt.Errorf("zenity failed: %w", err)
			}
			return strings.TrimSpace(string(out)), nil
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			cmd := exec.Command("kdialog", "--getopenfilename", "--title", "Select file to send")
			out, err := cmd.Output()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					return "", nil
				}
				return "", fmt.Errorf("kdialog failed: %w", err)
			}
			return strings.TrimSpace(string(out)), nil
		}
		if _, err := exec.LookPath("python3"); err == nil {
			pyScript := "import tkinter as tk, tkinter.filedialog as fd; root = tk.Tk(); root.withdraw(); print(fd.askopenfilename() or '')"
			cmd := exec.Command("python3", "-c", pyScript)
			out, err := cmd.Output()
			if err == nil {
				return strings.TrimSpace(string(out)), nil
			}
		}
		return "", errNoFilePicker
	}
}

func parseFileURI(input string) (string, string) {
	rem := strings.TrimPrefix(input, "file://")
	rem = strings.TrimSpace(rem)

	// If enclosed in quotes, e.g. "path/with spaces.pdf" caption here
	if strings.HasPrefix(rem, "\"") {
		if endIdx := strings.Index(rem[1:], "\""); endIdx != -1 {
			filePath := rem[1 : endIdx+1]
			caption := strings.TrimSpace(rem[endIdx+2:])
			return expandPath(filePath), caption
		}
	} else if strings.HasPrefix(rem, "'") {
		if endIdx := strings.Index(rem[1:], "'"); endIdx != -1 {
			filePath := rem[1 : endIdx+1]
			caption := strings.TrimSpace(rem[endIdx+2:])
			return expandPath(filePath), caption
		}
	}

	// If the entire remainder exists as a file directly (e.g. unquoted path with spaces)
	if _, err := os.Stat(expandPath(rem)); err == nil {
		return expandPath(rem), ""
	}

	// Otherwise, find the split point between file path and caption
	parts := strings.Split(rem, " ")
	for i := len(parts); i >= 1; i-- {
		candidate := strings.Join(parts[:i], " ")
		if _, err := os.Stat(expandPath(candidate)); err == nil {
			caption := strings.TrimSpace(strings.Join(parts[i:], " "))
			return expandPath(candidate), caption
		}
	}

	// Fallback: first word is file, rest is caption
	idx := strings.Index(rem, " ")
	if idx != -1 {
		return expandPath(rem[:idx]), strings.TrimSpace(rem[idx+1:])
	}
	return expandPath(rem), ""
}

func expandPath(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
