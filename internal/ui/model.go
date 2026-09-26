package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
	mu          sync.RWMutex
	unreadChats map[string]*UnreadChat // chatID -> UnreadChat
	chatOrder   []string               // pinned chats first, then sorted by LastReceived desc
	cursor      int

	// Active conversation view
	activeChatID     string
	activeName       string
	activeMsgs       []domain.Message
	chatScrollOffset int
	input            textinput.Model

	// Contact search view
	contacts       []domain.Contact
	filteredList   []domain.Contact
	contactSearch  textinput.Model
	contactCursor  int
	contactOffset  int
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
func NewModel(ctx context.Context, adapter domain.WhatsAppAdapter, cfgs ...*config.Config) *Model {
	ti := textinput.New()
	ti.Placeholder = "Type a message... (Enter to send, Esc to back)"
	ti.CharLimit = 1000
	ti.Width = 60

	si := textinput.New()
	si.Placeholder = "Search contact name, group, or phone number..."
	si.CharLimit = 100
	si.Width = 50

	var cfg *config.Config
	if len(cfgs) > 0 && cfgs[0] != nil {
		cfg = cfgs[0]
	} else {
		cfg = &config.Config{}
	}

	m := &Model{
		adapter:       adapter,
		ctx:           ctx,
		cfg:           cfg,
		view:          ViewUnreadList,
		unreadChats:   make(map[string]*UnreadChat),
		chatOrder:     make([]string, 0),
		input:         ti,
		contactSearch: si,
		status:        domain.StatusConnecting,
		msgChan:       make(chan domain.Message, 200),
		statusChan:    make(chan domain.ConnectionStatus, 10),
		dismissChan:   make(chan string, 50),
		contactsChan:  make(chan []domain.Contact, 10),
	}

	m.initPinnedChats()

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
		m.contacts = []domain.Contact(msg)
		m.filterContacts(m.contactSearch.Value())
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
		m.mu.Lock()
		if chat, exists := m.unreadChats[sent.ChatID]; exists && chat.IsPinned {
			chat.Messages = []domain.Message{sent}
			chat.LastReceived = sent.Timestamp
		}
		m.mu.Unlock()

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
			m.chatScrollOffset = 0
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

func (m *Model) resolveChatName(chatID string, msgChatName string, fallback string) (string, bool) {
	isGroup := strings.Contains(chatID, "@g.us")

	cleanID := chatID
	if idx := strings.Index(cleanID, ":"); idx != -1 {
		if atIdx := strings.Index(cleanID, "@"); atIdx != -1 {
			cleanID = cleanID[:idx] + cleanID[atIdx:]
		}
	}
	baseUser := strings.Split(cleanID, "@")[0]

	// 1. Check m.contacts first (contains address book contacts & local groups)
	for _, c := range m.contacts {
		cBase := strings.Split(c.JID, "@")[0]
		if c.JID == cleanID || cBase == baseUser {
			if c.Name != "" && !strings.HasPrefix(c.Name, "Group (") && !strings.HasPrefix(c.Name, "120363") {
				return c.Name, c.IsGroup || isGroup
			}
		}
	}

	// 2. If message already had a real group or chat name
	if msgChatName != "" && !strings.HasPrefix(msgChatName, "120363") && !strings.HasPrefix(msgChatName, "Group (") {
		return msgChatName, isGroup
	}

	// 3. Fallback for group:
	if isGroup {
		return "Group (" + baseUser + ")", true
	}

	// 4. Fallback for 1-on-1:
	if fallback != "" && fallback != chatID && !strings.HasPrefix(fallback, "120363") {
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
	for _, c := range m.contacts {
		cBase := strings.Split(c.JID, "@")[0]
		if c.JID == cleanID || cBase == baseUser {
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
		name, isGroup := m.resolveChatName(chatID, rule, rule)
		chat := &UnreadChat{
			ChatID:   chatID,
			Name:     name,
			IsGroup:  isGroup,
			IsPinned: true,
		}
		m.unreadChats[chatID] = chat
	}
	m.sortChatOrderLocked()
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
			if matchedID != "" && matchedID != matchedContact.JID {
				chat := m.unreadChats[matchedID]
				delete(m.unreadChats, matchedID)
				chat.ChatID = matchedContact.JID
				chat.Name = matchedContact.Name
				chat.IsGroup = matchedContact.IsGroup
				chat.IsPinned = true
				m.unreadChats[matchedContact.JID] = chat
			} else if matchedID != "" {
				chat := m.unreadChats[matchedID]
				chat.Name = matchedContact.Name
				chat.IsGroup = matchedContact.IsGroup
				chat.IsPinned = true
			} else {
				m.unreadChats[matchedContact.JID] = &UnreadChat{
					ChatID:   matchedContact.JID,
					Name:     matchedContact.Name,
					IsGroup:  matchedContact.IsGroup,
					IsPinned: true,
				}
			}
		}
	}
	m.sortChatOrderLocked()
}

func (m *Model) sortChatOrderLocked() {
	var pinned []string
	var unpinned []string

	pinnedRules := m.cfg.GetPinned()
	used := make(map[string]bool)

	// 1. Pinned chats matching config order
	for _, rule := range pinnedRules {
		for id, chat := range m.unreadChats {
			if chat != nil && chat.IsPinned && !used[id] {
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
		if chat != nil && chat.IsPinned && !used[id] {
			pinned = append(pinned, id)
			used[id] = true
		}
	}

	// 3. Unpinned chats
	for id, chat := range m.unreadChats {
		if chat != nil && !chat.IsPinned && !used[id] {
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
	for id, chat := range m.unreadChats {
		lastMsgName := ""
		if len(chat.Messages) > 0 {
			lastMsgName = chat.Messages[len(chat.Messages)-1].ChatName
		}
		name, isGroup := m.resolveChatName(chat.ChatID, lastMsgName, chat.Name)
		chat.Name = name
		chat.IsGroup = isGroup

		if !chat.IsPinned && m.isMuted(chat.ChatID, chat.Name) {
			delete(m.unreadChats, id)
		}
	}
	m.sortChatOrderLocked()
}

func (m *Model) rebuildUnreadChats(msgs []domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Retain pinned chats, resetting their message slice
	newUnreadChats := make(map[string]*UnreadChat)
	for id, chat := range m.unreadChats {
		if chat.IsPinned {
			chat.Messages = nil
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
		if !exists {
			name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, msg.SenderName)
			chat = &UnreadChat{
				ChatID:       msg.ChatID,
				Name:         name,
				IsGroup:      isGroup,
				IsPinned:     isPinned,
				Sender:       msg.Sender,
				Messages:     []domain.Message{msg},
				LastReceived: msg.Timestamp,
			}
			m.unreadChats[msg.ChatID] = chat
		} else {
			chat.Messages = append(chat.Messages, msg)
			chat.LastReceived = msg.Timestamp
			if isPinned {
				chat.IsPinned = true
			}
			if chat.Name == "" || strings.HasPrefix(chat.Name, "Group (") || strings.HasPrefix(chat.Name, "120363") {
				name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, chat.Name)
				chat.Name = name
				chat.IsGroup = isGroup
			}
		}
	}

	m.sortChatOrderLocked()
}

func (m *Model) handleIncomingMessage(msg domain.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		if chat, exists := m.unreadChats[msg.ChatID]; exists && chat.IsPinned {
			chat.Messages = []domain.Message{msg}
			chat.LastReceived = msg.Timestamp
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
			Sender:       msg.Sender,
			Messages:     []domain.Message{msg},
			LastReceived: msg.Timestamp,
		}
		m.unreadChats[msg.ChatID] = chat
	} else {
		chat.Messages = append(chat.Messages, msg)
		chat.LastReceived = msg.Timestamp
		if isPinned {
			chat.IsPinned = true
		}
		if chat.Name == "" || strings.HasPrefix(chat.Name, "Group (") || strings.HasPrefix(chat.Name, "120363") {
			name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, chat.Name)
			chat.Name = name
			chat.IsGroup = isGroup
		}
	}

	m.sortChatOrderLocked()
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

	case "r", "d": // mark as read / dismiss
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
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat

			var ids []string
			for _, item := range chat.Messages {
				ids = append(ids, item.ID)
			}
			go func(cID, sID string, mIDs []string) {
				_ = m.adapter.MarkRead(m.ctx, cID, sID, mIDs)
			}(chat.ChatID, chat.Sender, ids)

			m.dismissUnread(chatID)
			return tea.Batch(tea.ClearScreen, textinput.Blink)
		}

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

func (m *Model) updateChat(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.view = ViewUnreadList
		return tea.ClearScreen

	case "ctrl+c":
		return tea.Quit

	case "pgup", "ctrl+u":
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
		return tea.ClearScreen

	case "ctrl+c":
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
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat
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
			m.activeChatID = rawInput
			m.activeName = rawInput
			m.activeMsgs = nil
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.Focus()
			m.view = ViewChat
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
		if len(m.contacts) > 40 {
			m.filteredList = m.contacts[:40]
		} else {
			m.filteredList = m.contacts
		}
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

	maxMatches := 40
	for _, c := range m.contacts {
		nameMatch := strings.Contains(strings.ToLower(c.Name), q)
		if c.IsGroup {
			// Never match group JID (e.g. 120363@g.us)
			if nameMatch {
				res = append(res, c)
				if len(res) >= maxMatches {
					break
				}
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
			if len(res) >= maxMatches {
				break
			}
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

	// Header: count chats that actually have unread messages
	unreadCount := 0
	for _, chat := range m.unreadChats {
		if len(chat.Messages) > 0 {
			unreadCount++
		}
	}
	statusText := string(m.status)
	if m.status == domain.StatusConnected {
		statusText = fmt.Sprintf("connected · %d unread", unreadCount)
	}

	headerText := fmt.Sprintf("%s  %s",
		titleStyle.Render("watui"),
		statusStyle.Render("· "+statusText),
	)
	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	if len(m.chatOrder) == 0 {
		lines = append(lines, "  Inbox Zero")
		lines = append(lines, "")
		lines = append(lines, "  No unread messages.")
		lines = append(lines, "")
		lines = append(lines, statusStyle.Render("  Press [n] to compose to a contact, or wait for incoming messages."))
		lines = append(lines, "")
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
			if len(chat.Messages) > 0 {
				badge = badgeStyle.Render(fmt.Sprintf("%d", len(chat.Messages)))
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

	lines = append(lines, helpStyle.Render("[Enter] Open · [r] Dismiss · [n] New Message · [R] Refresh · [q] Quit"))
	return lines
}

func (m *Model) renderChatView() []string {
	cw := m.contentWidth()
	var lines []string

	chatPrefix := "Chat with"
	if strings.Contains(m.activeChatID, "@g.us") {
		chatPrefix = "Group"
	}
	headerText := fmt.Sprintf("%s %s  %s",
		titleStyle.Render(chatPrefix),
		selectedTitleStyle.Render(m.activeName),
		statusStyle.Render("· [Esc] Back"),
	)
	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	var msgLines []string
	if len(m.activeMsgs) == 0 {
		msgLines = append(msgLines, statusStyle.Render("  (No messages in this session yet. Type below to send.)"))
	} else {
		msgWrapWidth := max(20, cw-6)
		wrapStyle := lipgloss.NewStyle().Width(msgWrapWidth)

		for i, msg := range m.activeMsgs {
			timeStr := msg.Timestamp.Format("15:04")
			var header string
			if msg.IsFromMe {
				header = fmt.Sprintf("  %s %s",
					lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render("You"),
					statusStyle.Render(timeStr),
				)
			} else {
				sender := msg.SenderName
				if sender == "" {
					sender = "Them"
				}
				header = fmt.Sprintf("  %s %s",
					selectedTitleStyle.Render(sender),
					statusStyle.Render(timeStr),
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

	availH := max(3, m.maxCanvasHeight()-6)
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
	lines = append(lines, m.input.View())
	lines = append(lines, helpStyle.Render("[Enter] Send · [Esc] Back"+scrollInfo))

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
	lines = append(lines, helpStyle.Render("[Up/Down] Navigate · [Enter] Select & Compose · [Esc] Cancel"))

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
