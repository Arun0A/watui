package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/atotto/clipboard"
	osc52 "github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watui/internal/config"
	"watui/internal/domain"
	"watui/internal/media"
)

// ViewState represents the currently active screen in the TUI.
type ViewState int

const (
	ViewUnreadList ViewState = iota
	ViewChat
	ViewContactPicker
	ViewKeybindsHelp
)

// UnreadChat represents a conversation with pending unread messages or a pinned chat.
type UnreadChat struct {
	ChatID       string
	Name         string
	IsGroup      bool
	IsPinned     bool
	IsArchived   bool
	IsMuted      bool
	HasMention   bool
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
	mu                  sync.RWMutex
	unreadChats         map[string]*UnreadChat // chatID -> UnreadChat
	readChats           map[string]bool        // chatID -> true for chats read in active session
	chatOrder           []string               // visible chats according to view mode
	cursor              int
	showArchived        bool // true when viewing archived chats section
	prevView            ViewState
	keybindScrollOffset int
	cleanupDone         bool

	// Active conversation view
	activeChatID     string
	activeName       string
	activeMsgs       []domain.Message
	chatScrollOffset int
	input            textarea.Model
	selectedMsgIdx   int             // targeted message index within activeChat (-1 when unfocused)
	replyToMsg       *domain.Message // message being replied to (nil if none)
	editTargetMsg    *domain.Message // message currently being edited (nil if none)

	// Media navigation & saving
	selectedMediaIdx         int                 // targeted media index within activeChat (0-based)
	confirmSave              bool                // true when prompting "save? (y/N)"
	pendingSavePath          string              // path of the media file awaiting save confirmation
	confirmDocAction         bool                // true when prompting "Document: Open [o], Open with [w], or Save [s]?"
	promptOpenWith           bool                // true when typing custom viewer in "Open with: "
	openWithInput            textinput.Model     // textinput for custom application name
	pendingDocMsg            *domain.Message     // message awaiting document action choice
	confirmDelete            bool                // true when prompting "Delete message for you? (y/N)" or "Delete message for everyone? (y/N)"
	pendingDeleteForEveryone bool                // true if confirming delete for everyone, false if for me
	pendingDeleteMsg         *domain.Message     // message awaiting delete confirmation
	pendingDeleteMultiMsgs   []domain.Message    // messages awaiting batch deletion confirmation
	multiSelectedMsgs        map[string]struct{} // set of message IDs selected in hover mode

	// Global mute prompt
	promptGlobalMute   bool
	globalMuteChatID   string
	globalMuteChatName string
	localMuteToggled   bool

	// Group mention completion
	groupParticipants []domain.Contact // participants of active group chat
	mentionHints      []domain.Contact // filtered hints currently visible
	mentionCursor     int              // index of highlighted hint in mentionHints

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

	msgChan       chan domain.Message
	statusChan    chan domain.ConnectionStatus
	dismissChan   chan string
	contactsChan  chan []domain.Contact
	ephemeralChan chan ephemeralUpdateMsg

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
type ephemeralUpdateMsg struct {
	ChatID string
	Timer  uint32
}
type ephemeralSetResultMsg struct {
	ChatID string
	Timer  uint32
	Err    error
}
type sendErrMsg struct {
	ChatID string
	Text   string
	Err    error
}
type messageDeletedMsg struct {
	ChatID            string
	MessageID         string
	DeleteForEveryone bool
	Err               error
}
type messageEditedMsg struct {
	ChatID    string
	MessageID string
	NewText   string
	Err       error
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
type groupParticipantsMsg struct {
	ChatID       string
	Participants []domain.Contact
}
type multiMessagesSavedMsg struct {
	MediaCount int
	TextCount  int
	TextPath   string
	DestDir    string
	Err        error
}

type saveLocationPickedMsg struct {
	ChosenPath string
	SaveTarget saveTargetData
}

type saveTargetData struct {
	Mode            string // "cached_file", "single_msg", "multi_msgs"
	CachedPath      string
	Message         domain.Message
	MultiMsgs       []domain.Message
	PlaceholderPath string
}

// NewModel initializes the TUI model.
func NewModel(ctx context.Context, adapter domain.WhatsAppAdapter, cfgs ...*config.Config) *Model {
	_ = ClearClipboardCache()
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
	if cfg.IsClearOnExitEnabled() {
		_ = media.ClearMediaCache()
	}

	m := &Model{
		adapter:           adapter,
		ctx:               ctx,
		cfg:               cfg,
		view:              ViewUnreadList,
		unreadChats:       make(map[string]*UnreadChat),
		readChats:         make(map[string]bool),
		chatOrder:         make([]string, 0),
		input:             ti,
		selectedMsgIdx:    -1,
		contactSearch:     si,
		openWithInput:     oi,
		status:            domain.StatusConnecting,
		msgChan:           make(chan domain.Message, 200),
		statusChan:        make(chan domain.ConnectionStatus, 10),
		dismissChan:       make(chan string, 50),
		contactsChan:      make(chan []domain.Contact, 10),
		ephemeralChan:     make(chan ephemeralUpdateMsg, 50),
		contactsByJID:     make(map[string]*domain.Contact),
		contactsByUser:    make(map[string]*domain.Contact),
		multiSelectedMsgs: make(map[string]struct{}),
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
	adapter.OnChatEphemeral(func(chatID string, timer uint32) {
		select {
		case m.ephemeralChan <- ephemeralUpdateMsg{ChatID: chatID, Timer: timer}:
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
		m.waitForEphemeralUpdated(),
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
		fetchCtx, cancel := context.WithTimeout(m.ctx, 6*time.Second)
		defer cancel()
		msgs, err := m.adapter.GetChatHistory(fetchCtx, chatID, limit, beforeTS)
		return historyLoadedMsg{
			ChatID:   chatID,
			Messages: msgs,
			Err:      err,
			AutoLoad: autoLoad,
		}
	}
}

func (m *Model) fetchGroupParticipantsCmd(chatID string) tea.Cmd {
	return func() tea.Msg {
		if m.adapter == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		participants, err := m.adapter.GetGroupParticipants(ctx, chatID)
		if err != nil || len(participants) == 0 {
			return nil
		}
		return groupParticipantsMsg{
			ChatID:       chatID,
			Participants: participants,
		}
	}
}

func (m *Model) deleteMessageCmd(chatID string, target domain.Message, forEveryone bool) tea.Cmd {
	return func() tea.Msg {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sender := target.Sender
		err := m.adapter.DeleteMessage(delCtx, chatID, target.ID, forEveryone, sender)
		return messageDeletedMsg{
			ChatID:            chatID,
			MessageID:         target.ID,
			DeleteForEveryone: forEveryone,
			Err:               err,
		}
	}
}

func (m *Model) editMessageCmd(chatID, messageID, newText string) tea.Cmd {
	return func() tea.Msg {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		editCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := m.adapter.EditMessage(editCtx, chatID, messageID, newText)
		return messageEditedMsg{
			ChatID:    chatID,
			MessageID: messageID,
			NewText:   newText,
			Err:       err,
		}
	}
}

func (m *Model) updateMessageBody(chatID, messageID, newText string) {
	if m.activeChatID == chatID {
		for i := range m.activeMsgs {
			if m.activeMsgs[i].ID == messageID {
				m.activeMsgs[i].Body = newText
				break
			}
		}
		for i := range m.activeMsgs {
			if m.activeMsgs[i].QuotedID == messageID {
				m.activeMsgs[i].QuotedText = newText
			}
		}
	}
	m.mu.Lock()
	if chat, exists := m.unreadChats[chatID]; exists {
		for i := range chat.Messages {
			if chat.Messages[i].ID == messageID {
				chat.Messages[i].Body = newText
				break
			}
		}
		for i := range chat.Messages {
			if chat.Messages[i].QuotedID == messageID {
				chat.Messages[i].QuotedText = newText
			}
		}
	}
	m.mu.Unlock()
}

func (m *Model) handleConfirmDelete(msg tea.KeyMsg) (bool, tea.Cmd) {
	if !m.confirmDelete {
		return false, nil
	}
	if msg.Type == tea.KeyCtrlC || msg.String() == "ctrl+c" {
		return false, nil
	}
	if msg.Type == tea.KeyCtrlH || msg.Type == tea.KeyF1 || msg.String() == "ctrl+h" || msg.String() == "ctrl+/" || msg.String() == "ctrl+_" {
		return false, nil
	}
	switch msg.String() {
	case "y", "Y":
		m.confirmDelete = false
		if len(m.pendingDeleteMultiMsgs) > 0 {
			targets := m.pendingDeleteMultiMsgs
			m.pendingDeleteMultiMsgs = nil
			m.pendingDeleteMsg = nil
			m.multiSelectedMsgs = make(map[string]struct{})
			forEveryone := m.pendingDeleteForEveryone
			isGroup := strings.Contains(m.activeChatID, "@g.us")
			m.stopActiveViewer()
			var cmds []tea.Cmd
			for _, target := range targets {
				if m.replyToMsg != nil && m.replyToMsg.ID == target.ID {
					m.replyToMsg = nil
				}
				m.removeMessageFromActive(target.ID)
				m.removeMessageFromUnread(m.activeChatID, target.ID)
				delForEveryone := forEveryone
				if delForEveryone && !isGroup && !target.IsFromMe {
					delForEveryone = false
				}
				cmds = append(cmds, m.deleteMessageCmd(m.activeChatID, target, delForEveryone))
			}
			if len(m.activeMsgs) == 0 {
				m.selectedMsgIdx = -1
				m.input.Focus()
			} else {
				if m.selectedMsgIdx >= len(m.activeMsgs) {
					m.selectedMsgIdx = len(m.activeMsgs) - 1
				}
				m.scrollToMessage(m.selectedMsgIdx)
				m.syncSelectedMediaIdx()
			}
			if forEveryone {
				m.previewStatus = fmt.Sprintf("Deleting %d message(s) for everyone...", len(targets))
			} else {
				m.previewStatus = fmt.Sprintf("Deleted %d message(s) for you", len(targets))
			}
			return true, tea.Batch(cmds...)
		}
		if m.pendingDeleteMsg != nil {
			target := *m.pendingDeleteMsg
			m.pendingDeleteMsg = nil
			forEveryone := m.pendingDeleteForEveryone
			m.stopActiveViewer()
			if m.replyToMsg != nil && m.replyToMsg.ID == target.ID {
				m.replyToMsg = nil
			}
			m.removeMessageFromActive(target.ID)
			m.removeMessageFromUnread(m.activeChatID, target.ID)
			if len(m.activeMsgs) == 0 {
				m.selectedMsgIdx = -1
				m.input.Focus()
			} else {
				if m.selectedMsgIdx >= len(m.activeMsgs) {
					m.selectedMsgIdx = len(m.activeMsgs) - 1
				}
				m.scrollToMessage(m.selectedMsgIdx)
				m.syncSelectedMediaIdx()
			}
			if forEveryone {
				m.previewStatus = "Deleting message for everyone..."
			} else {
				m.previewStatus = "Deleted message for you"
			}
			return true, m.deleteMessageCmd(m.activeChatID, target, forEveryone)
		}
		return true, nil

	case "n", "N", "esc":
		m.confirmDelete = false
		m.pendingDeleteMsg = nil
		m.pendingDeleteMultiMsgs = nil
		m.previewStatus = "Deletion cancelled"
		return true, nil

	default:
		m.confirmDelete = false
		m.pendingDeleteMsg = nil
		m.pendingDeleteMultiMsgs = nil
		m.previewStatus = "Deletion cancelled"
		return true, nil
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

func (m *Model) waitForEphemeralUpdated() tea.Cmd {
	return func() tea.Msg {
		return <-m.ephemeralChan
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
				IsMuted:      m.isMuted(sent.ChatID, name),
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

	case ephemeralUpdateMsg:
		cmds = append(cmds, m.waitForEphemeralUpdated())

	case ephemeralSetResultMsg:
		if msg.Err != nil {
			m.previewStatus = fmt.Sprintf("Error setting disappearing messages: %v", msg.Err)
		} else if msg.Timer > 0 {
			m.previewStatus = fmt.Sprintf("Disappearing messages set to %s", domain.FormatDisappearingTimer(msg.Timer))
		} else {
			m.previewStatus = "Disappearing messages turned off"
		}
		return m, nil

	case unreadsLoadedMsg:
		var unreads []domain.Message
		for _, mMsg := range msg {
			if m.cfg.IsReactionsDisabled() && mMsg.Type == domain.MessageTypeReaction {
				continue
			}
			unreads = append(unreads, mMsg)
		}
		m.rebuildUnreadChats(unreads)
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
			if m.cfg.IsReactionsDisabled() && nm.Type == domain.MessageTypeReaction {
				continue
			}
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

	case groupParticipantsMsg:
		if msg.ChatID == m.activeChatID {
			m.groupParticipants = msg.Participants
			m.updateMentionHints()
		}
		return m, nil

	case messageDeletedMsg:
		if msg.Err != nil {
			m.previewStatus = fmt.Sprintf("Error deleting message: %v", msg.Err)
			return m, nil
		}
		if msg.DeleteForEveryone {
			m.previewStatus = "Deleted message for everyone"
		}
		return m, nil

	case messageEditedMsg:
		if msg.Err != nil {
			m.previewStatus = fmt.Sprintf("Error editing message: %v", msg.Err)
			return m, nil
		}
		m.updateMessageBody(msg.ChatID, msg.MessageID, msg.NewText)
		m.previewStatus = "Message edited"
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

	case multiMessagesSavedMsg:
		if msg.Err != nil && msg.MediaCount == 0 && msg.TextCount == 0 {
			m.previewStatus = fmt.Sprintf("Failed to save: %v", msg.Err)
			return m, nil
		}
		var parts []string
		if msg.MediaCount > 0 {
			parts = append(parts, fmt.Sprintf("%d attachment(s)", msg.MediaCount))
		}
		if msg.TextCount > 0 {
			parts = append(parts, fmt.Sprintf("%d text message(s)", msg.TextCount))
		}
		if len(parts) > 0 {
			destDisplay := "Downloads"
			if msg.DestDir != "" && ((m.cfg != nil && m.cfg.GetDownloadDir() != "" && msg.DestDir == m.cfg.GetDownloadDir()) || msg.DestDir != m.getDownloadDir()) {
				home, _ := os.UserHomeDir()
				destDisplay = msg.DestDir
				if home != "" && strings.HasPrefix(msg.DestDir, home) {
					destDisplay = "~" + strings.TrimPrefix(msg.DestDir, home)
				}
			}
			m.previewStatus = fmt.Sprintf("Saved %s to %s", strings.Join(parts, " and "), destDisplay)
		} else {
			m.previewStatus = "Nothing to save"
		}
		return m, nil

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
			destPath, saveErr := m.saveToDownloads(msg.Path)
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
		if m.editTargetMsg != nil {
			m.previewStatus = "Cannot attach files while editing a message"
			return m, tea.ClearScreen
		}
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

	case saveLocationPickedMsg:
		if msg.ChosenPath == "" {
			if msg.SaveTarget.PlaceholderPath != "" {
				_ = os.Remove(msg.SaveTarget.PlaceholderPath)
			}
			m.previewStatus = "Save cancelled"
			return m, tea.ClearScreen
		}

		cleanChosen := config.ExpandHome(strings.TrimSpace(msg.ChosenPath))
		if msg.SaveTarget.PlaceholderPath != "" && msg.SaveTarget.PlaceholderPath != cleanChosen {
			_ = os.Remove(msg.SaveTarget.PlaceholderPath)
		}

		fi, statErr := os.Stat(cleanChosen)
		isDir := (statErr == nil && fi.IsDir()) || strings.HasSuffix(cleanChosen, "/") || strings.HasSuffix(cleanChosen, string(filepath.Separator))

		if isDir {
			destDir := cleanChosen
			preferredFile := getTargetFileName(msg.SaveTarget)
			switch msg.SaveTarget.Mode {
			case "cached_file":
				destPath, err := saveFileToDir(msg.SaveTarget.CachedPath, destDir, preferredFile)
				if err != nil {
					m.previewStatus = fmt.Sprintf("Failed to save: %v", err)
				} else {
					m.previewStatus = fmt.Sprintf("Saved to %s", destPath)
				}
				return m, tea.ClearScreen

			case "single_msg":
				return m, tea.Batch(tea.ClearScreen, m.downloadAndSaveMsgToDirCmd(msg.SaveTarget.Message, destDir, preferredFile))

			case "multi_msgs":
				return m, tea.Batch(tea.ClearScreen, m.saveMultiMessagesToDirCmd(msg.SaveTarget.MultiMsgs, destDir))
			}
		} else {
			// Specific file chosen
			switch msg.SaveTarget.Mode {
			case "cached_file":
				destPath, err := saveFileToPath(msg.SaveTarget.CachedPath, cleanChosen)
				if err != nil {
					m.previewStatus = fmt.Sprintf("Failed to save: %v", err)
				} else {
					m.previewStatus = fmt.Sprintf("Saved to %s", destPath)
				}
				return m, tea.ClearScreen

			case "single_msg":
				return m, tea.Batch(tea.ClearScreen, m.downloadAndSaveMsgToPathCmd(msg.SaveTarget.Message, cleanChosen))

			case "multi_msgs":
				destDir := filepath.Dir(cleanChosen)
				return m, tea.Batch(tea.ClearScreen, m.saveMultiMessagesToDirCmd(msg.SaveTarget.MultiMsgs, destDir))
			}
		}
		return m, tea.ClearScreen

	case clipboardPasteMsg:
		if !m.cfg.IsClipboardPasteEnabled() {
			return m, nil
		}
		if msg.Err != nil || msg.Item == nil {
			m.previewStatus = "Clipboard empty or unavailable"
			return m, nil
		}
		if msg.Item.IsMedia && msg.Item.FilePath != "" {
			if m.editTargetMsg != nil {
				m.previewStatus = "Cannot attach files while editing a message"
				return m, nil
			}
			cleanPath := strings.TrimSpace(msg.Item.FilePath)
			formatted := "file://" + cleanPath + " "
			if strings.Contains(cleanPath, " ") {
				formatted = fmt.Sprintf("file://\"%s\" ", cleanPath)
			}
			existing := strings.TrimSpace(m.input.Value())
			if existing != "" {
				if strings.HasPrefix(existing, "file://") {
					_, prevCaption := parseFileURI(existing)
					if prevCaption != "" {
						formatted = formatted + prevCaption
					}
				} else {
					formatted = formatted + existing
				}
			}
			m.input.SetValue(formatted)
			m.input.CursorEnd()
			m.previewStatus = fmt.Sprintf("Attached %s (press Enter to send)", filepath.Base(cleanPath))
			return m, nil
		}
		if msg.Item.Text != "" {
			m.input.InsertString(msg.Item.Text)
			m.input.SetHeight(min(5, max(1, m.input.LineCount())))
			m.updateMentionHints()
			m.previewStatus = ""
			return m, nil
		}
		m.previewStatus = "Clipboard empty"
		return m, nil

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

		if m.promptGlobalMute {
			switch msg.String() {
			case "1":
				m.promptGlobalMute = false
				chatID := m.globalMuteChatID
				name := m.globalMuteChatName
				m.globalMuteChatID = ""
				m.globalMuteChatName = ""
				m.mu.Lock()
				if chat := m.unreadChats[chatID]; chat != nil {
					chat.IsMuted = true
				}
				m.mu.Unlock()
				if m.adapter != nil {
					go func() {
						_ = m.adapter.SetChatMuted(m.ctx, chatID, true, 1*time.Hour)
					}()
				}
				m.previewStatus = fmt.Sprintf("Muted %s for 1 hour globally", name)
				return m, nil
			case "2":
				m.promptGlobalMute = false
				chatID := m.globalMuteChatID
				name := m.globalMuteChatName
				m.globalMuteChatID = ""
				m.globalMuteChatName = ""
				m.mu.Lock()
				if chat := m.unreadChats[chatID]; chat != nil {
					chat.IsMuted = true
				}
				m.mu.Unlock()
				if m.adapter != nil {
					go func() {
						_ = m.adapter.SetChatMuted(m.ctx, chatID, true, 8*time.Hour)
					}()
				}
				m.previewStatus = fmt.Sprintf("Muted %s for 8 hours globally", name)
				return m, nil
			case "3":
				m.promptGlobalMute = false
				chatID := m.globalMuteChatID
				name := m.globalMuteChatName
				m.globalMuteChatID = ""
				m.globalMuteChatName = ""
				m.mu.Lock()
				if chat := m.unreadChats[chatID]; chat != nil {
					chat.IsMuted = true
				}
				m.mu.Unlock()
				if m.adapter != nil {
					go func() {
						_ = m.adapter.SetChatMuted(m.ctx, chatID, true, 0)
					}()
				}
				m.previewStatus = fmt.Sprintf("Muted %s permanently globally", name)
				return m, nil
			case "esc", "ctrl+c", "q":
				m.promptGlobalMute = false
				m.globalMuteChatID = ""
				m.globalMuteChatName = ""
				m.previewStatus = ""
				return m, nil
			default:
				return m, nil
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
			case "s":
				m.confirmDocAction = false
				if m.pendingDocMsg != nil {
					targetMsg := *m.pendingDocMsg
					m.pendingDocMsg = nil
					return m, m.downloadAndSaveDocCmd(targetMsg)
				}
				return m, nil
			case "shift+s", "S":
				m.confirmDocAction = false
				if m.pendingDocMsg != nil {
					targetMsg := *m.pendingDocMsg
					m.pendingDocMsg = nil
					return m, m.pickSaveLocationCmd(saveTargetData{Mode: "single_msg", Message: targetMsg})
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
			case "y", "Y", "s":
				m.confirmSave = false
				srcPath := m.pendingSavePath
				m.pendingSavePath = ""
				destPath, err := m.saveToDownloads(srcPath)
				if err != nil {
					m.previewStatus = fmt.Sprintf("Failed to save: %v", err)
				} else {
					m.previewStatus = fmt.Sprintf("Saved to %s", destPath)
				}
				return m, nil
			case "shift+s", "S":
				m.confirmSave = false
				srcPath := m.pendingSavePath
				m.pendingSavePath = ""
				return m, m.pickSaveLocationCmd(saveTargetData{Mode: "cached_file", CachedPath: srcPath})
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

		if m.confirmDelete {
			handled, cmd := m.handleConfirmDelete(msg)
			if handled {
				return m, cmd
			}
		}

		if msg.String() == "ctrl+/" || msg.String() == "ctrl+_" || msg.String() == "ctrl+h" || msg.Type == tea.KeyCtrlH || msg.Type == tea.KeyF1 {
			if m.view == ViewKeybindsHelp {
				m.view = m.prevView
			} else {
				m.prevView = m.view
				m.view = ViewKeybindsHelp
				m.keybindScrollOffset = 0
			}
			return m, tea.ClearScreen
		}

		switch m.view {
		case ViewUnreadList:
			cmds = append(cmds, m.updateUnreadList(msg))
		case ViewChat:
			cmds = append(cmds, m.updateChat(msg))
		case ViewContactPicker:
			cmds = append(cmds, m.updateContactPicker(msg))
		case ViewKeybindsHelp:
			cmds = append(cmds, m.updateKeybindsHelp(msg))
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
	byUser := make(map[string]*domain.Contact, len(contacts)*2)
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
		if existing, ok := byUser[user]; !ok || (!strings.HasSuffix(c.JID, "@lid") && strings.HasSuffix(existing.JID, "@lid")) {
			byUser[user] = c
		}
	}
	m.contactsByJID = byJID
	m.contactsByUser = byUser
}

func (m *Model) resolveSenderNameByID(rawID string) string {
	if rawID == "" || rawID == "You" {
		return rawID
	}
	isAllDigits := true
	for _, r := range rawID {
		if r < '0' || r > '9' {
			isAllDigits = false
			break
		}
	}
	if !isAllDigits && isRealChatName(rawID, "") {
		return rawID
	}
	if len(m.contacts) > 0 && len(m.contactsByJID) < len(m.contacts) {
		m.setContacts(m.contacts)
	}
	cleanID := rawID
	if idx := strings.Index(cleanID, ":"); idx != -1 {
		if atIdx := strings.Index(cleanID, "@"); atIdx != -1 {
			cleanID = cleanID[:idx] + cleanID[atIdx:]
		}
	}
	baseUser := strings.Split(cleanID, "@")[0]
	if m.contactsByJID != nil {
		if c := m.contactsByJID[cleanID]; c != nil {
			if isRealChatName(c.Name, c.JID) {
				return c.Name
			}
			if isRealChatName(c.PushName, c.JID) {
				return c.PushName
			}
		}
		if c := m.contactsByJID[rawID]; c != nil {
			if isRealChatName(c.Name, c.JID) {
				return c.Name
			}
			if isRealChatName(c.PushName, c.JID) {
				return c.PushName
			}
		}
	}
	if m.contactsByUser != nil {
		if c := m.contactsByUser[baseUser]; c != nil {
			if isRealChatName(c.Name, c.JID) {
				return c.Name
			}
			if isRealChatName(c.PushName, c.JID) {
				return c.PushName
			}
		}
	}
	return ""
}

func (m *Model) resolveMsgSenderName(msg domain.Message) string {
	if msg.IsFromMe {
		return "You"
	}
	if isRealChatName(msg.SenderName, msg.Sender) {
		return msg.SenderName
	}
	if resolved := m.resolveSenderNameByID(msg.Sender); resolved != "" && isRealChatName(resolved, msg.Sender) {
		return resolved
	}
	if isRealChatName(msg.SenderName, "") {
		return msg.SenderName
	}
	cleanSender := msg.Sender
	if idx := strings.Index(cleanSender, ":"); idx != -1 {
		if atIdx := strings.Index(cleanSender, "@"); atIdx != -1 {
			cleanSender = cleanSender[:idx] + cleanSender[atIdx:]
		}
	}
	baseUser := strings.Split(cleanSender, "@")[0]
	if baseUser != "" {
		return baseUser
	}
	if msg.SenderName != "" {
		return msg.SenderName
	}
	return "Them"
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

func (m *Model) isHidden(chatID string, chatNames ...string) bool {
	if m.cfg == nil {
		return false
	}
	allNames := m.getChatMatchNames(chatID, chatNames...)
	if m.isPinned(chatID, allNames...) {
		return false
	}
	return m.cfg.IsHidden(chatID, allNames...)
}

func (m *Model) isMuted(chatID string, chatNames ...string) bool {
	if m.adapter != nil && m.adapter.IsChatMuted(chatID) {
		return true
	}
	if m.cfg == nil {
		return false
	}
	allNames := m.getChatMatchNames(chatID, chatNames...)
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
			IsMuted:    m.isMuted(chatID, name),
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
				chat.IsMuted = m.isMuted(chat.ChatID, chat.Name)
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
				chat.IsMuted = m.isMuted(chat.ChatID, chat.Name)
				m.unreadChats[matchedContact.JID] = chat
			} else if matchedID != "" {
				chat := m.unreadChats[matchedID]
				if realName != "" {
					chat.Name = realName
				}
				chat.IsGroup = matchedContact.IsGroup
				chat.IsPinned = true
				chat.IsMuted = m.isMuted(chat.ChatID, chat.Name)
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
					IsMuted:    m.isMuted(matchedContact.JID, chatName),
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
		return !chat.IsArchived || chat.HasMention
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

		chat.IsMuted = m.isMuted(chat.ChatID, chat.Name)
		if !chat.IsPinned && m.isHidden(chat.ChatID, chat.Name) {
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
		if m.cfg.IsReactionsDisabled() && msg.Type == domain.MessageTypeReaction {
			continue
		}
		if m.isHidden(msg.ChatID, msg.ChatName, msg.SenderName) {
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

		if msg.IsEdit && msg.QuotedText == "" && msg.QuotedID != "" && chat != nil {
			for _, existing := range chat.Messages {
				if existing.ID == msg.QuotedID {
					msg.QuotedText = existing.Body
					if msg.QuotedSender == "" {
						msg.QuotedSender = existing.Sender
					}
					break
				}
			}
		}

		isPinned := m.isPinned(msg.ChatID, msg.ChatName)
		isMuted := m.isMuted(msg.ChatID, msg.ChatName, msg.SenderName)
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
				IsMuted:      isMuted,
				HasMention:   msg.MentionsMe(),
				UnreadCount:  1,
				Sender:       msg.Sender,
				Messages:     []domain.Message{msg},
				LastReceived: msg.Timestamp,
			}
			m.unreadChats[msg.ChatID] = chat
		} else {
			alreadyPresent := false
			for i, existing := range chat.Messages {
				if existing.ID == msg.ID {
					alreadyPresent = true
					chat.Messages[i].Body = msg.Body
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
			if msg.MentionsMe() {
				chat.HasMention = true
			}
			if isPinned {
				chat.IsPinned = true
			}
			chat.IsMuted = isMuted
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

	if m.cfg.IsReactionsDisabled() && msg.Type == domain.MessageTypeReaction {
		return
	}

	// Archived chats should never trigger notifications and are muted by default
	isArchived := false
	if m.adapter != nil {
		isArchived = m.adapter.IsChatArchived(msg.ChatID)
	}

	// Determine if message belongs to the currently active chat
	userActive := strings.Split(strings.Split(m.activeChatID, "@")[0], ":")[0]
	userMsg := strings.Split(strings.Split(msg.ChatID, "@")[0], ":")[0]
	isForActiveChat := (m.activeChatID == msg.ChatID) || (userActive != "" && userActive == userMsg)
	if !isForActiveChat && m.view == ViewChat && msg.IsEdit && msg.QuotedID != "" {
		for _, prev := range m.activeMsgs {
			if prev.ID == msg.QuotedID {
				isForActiveChat = true
				break
			}
		}
	}

	// 1. Check if hidden
	if m.isHidden(msg.ChatID, msg.ChatName, msg.SenderName) {
		if m.view == ViewChat && isForActiveChat {
			m.activeMsgs = append(m.activeMsgs, msg)
			m.chatScrollOffset = 0
			go func() {
				_ = m.adapter.MarkRead(m.ctx, msg.ChatID, msg.Sender, []string{msg.ID})
			}()
		}
		return
	}

	// If message is an edit from an official WhatsApp client, link it as a reply to the original message
	if msg.IsEdit {
		for i, prev := range m.activeMsgs {
			if prev.ID == msg.QuotedID {
				if msg.QuotedText == "" {
					msg.QuotedText = prev.Body
				}
				if msg.QuotedSender == "" {
					msg.QuotedSender = prev.Sender
				}
				m.activeMsgs[i].Body = msg.Body
				break
			}
		}
		if chat, ok := m.unreadChats[msg.ChatID]; ok && chat != nil {
			for i, prev := range chat.Messages {
				if prev.ID == msg.QuotedID {
					if msg.QuotedText == "" {
						msg.QuotedText = prev.Body
					}
					if msg.QuotedSender == "" {
						msg.QuotedSender = prev.Sender
					}
					chat.Messages[i].Body = msg.Body
					break
				}
			}
		}
	}

	// 2. If currently viewing this chat, update if already present or append directly
	if m.view == ViewChat && isForActiveChat {
		foundInActive := false
		for i := range m.activeMsgs {
			if m.activeMsgs[i].ID == msg.ID {
				m.activeMsgs[i].Body = msg.Body
				foundInActive = true
				break
			}
		}
		if !foundInActive {
			m.activeMsgs = append(m.activeMsgs, msg)
			m.chatScrollOffset = 0
		}
		go func() {
			_ = m.adapter.MarkRead(m.ctx, msg.ChatID, msg.Sender, []string{msg.ID})
		}()
		if chat, exists := m.unreadChats[msg.ChatID]; exists {
			foundInChat := false
			for i := range chat.Messages {
				if chat.Messages[i].ID == msg.ID {
					chat.Messages[i].Body = msg.Body
					foundInChat = true
					break
				}
			}
			if !foundInChat {
				if len(chat.Messages) < 50 {
					chat.Messages = append(chat.Messages, msg)
				} else {
					copy(chat.Messages, chat.Messages[1:])
					chat.Messages[len(chat.Messages)-1] = msg
				}
				chat.LastReceived = msg.Timestamp
			}
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
	isMuted := m.isMuted(msg.ChatID, msg.ChatName, msg.SenderName)
	if !exists {
		name, isGroup := m.resolveChatName(msg.ChatID, msg.ChatName, msg.SenderName)
		chat = &UnreadChat{
			ChatID:       msg.ChatID,
			Name:         name,
			IsGroup:      isGroup,
			IsPinned:     isPinned,
			IsArchived:   isArchived,
			IsMuted:      isMuted,
			HasMention:   msg.MentionsMe(),
			UnreadCount:  1,
			Sender:       msg.Sender,
			Messages:     []domain.Message{msg},
			LastReceived: msg.Timestamp,
		}
		m.unreadChats[msg.ChatID] = chat
	} else {
		chat.UnreadCount++
		if msg.MentionsMe() {
			chat.HasMention = true
		}
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
		chat.IsMuted = isMuted
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

	case "j", "down", "ctrl+n":
		m.previewStatus = ""
		if m.cursor < len(m.chatOrder)-1 {
			m.cursor++
		}

	case "k", "up", "ctrl+p":
		m.previewStatus = ""
		if m.cursor > 0 {
			m.cursor--
		}

	case "g", "home":
		m.previewStatus = ""
		m.cursor = 0

	case "G", "shift+g", "end":
		m.previewStatus = ""
		if len(m.chatOrder) > 0 {
			m.cursor = len(m.chatOrder) - 1
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

	case "M", "shift+m": // toggle global WhatsApp mute
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			if chat != nil {
				isGloballyMuted := false
				if m.adapter != nil {
					isGloballyMuted = m.adapter.IsChatMuted(chatID)
				}
				if isGloballyMuted {
					// Toggle off (unmute)
					if m.adapter != nil {
						go func() {
							_ = m.adapter.SetChatMuted(m.ctx, chatID, false, 0)
						}()
					}
					chat.IsMuted = m.cfg != nil && m.cfg.IsMuted(chatID, m.getChatMatchNames(chatID, chat.Name)...)
					m.previewStatus = fmt.Sprintf("Unmuted %s globally", chat.Name)
				} else {
					// Prompt options: 1: 1 hour, 2: 8 hours, 3: Always
					m.promptGlobalMute = true
					m.globalMuteChatID = chatID
					m.globalMuteChatName = chat.Name
				}
			}
		}
		return nil

	case "m": // toggle local watui.yaml mute
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		m.promptGlobalMute = false
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			if chat != nil && m.cfg != nil {
				m.localMuteToggled = true
				allNames := m.getChatMatchNames(chatID, chat.Name)
				if m.cfg.IsMuted(chatID, allNames...) {
					if err := m.cfg.RemoveMuteChat(chatID, allNames...); err != nil {
						m.previewStatus = fmt.Sprintf("Error unmuting in config: %v", err)
					} else {
						chat.IsMuted = m.isMuted(chatID, chat.Name)
						m.previewStatus = fmt.Sprintf("Unmuted %s in watui.yaml", chat.Name)
					}
				} else {
					if err := m.cfg.AddMuteChat(chatID); err != nil {
						m.previewStatus = fmt.Sprintf("Error muting in config: %v", err)
					} else {
						chat.IsMuted = true
						m.previewStatus = fmt.Sprintf("Muted %s in watui.yaml", chat.Name)
					}
				}
			}
		}
		return nil

	case "p": // toggle local watui.yaml pin
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		m.promptGlobalMute = false
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			if chat != nil && m.cfg != nil {
				m.localMuteToggled = true
				allNames := m.getChatMatchNames(chatID, chat.Name)
				if m.cfg.IsPinned(chatID, allNames...) {
					if err := m.cfg.RemovePinChat(chatID, allNames...); err != nil {
						m.previewStatus = fmt.Sprintf("Error unpinning in config: %v", err)
					} else {
						m.mu.Lock()
						chat.IsPinned = false
						if chat.UnreadCount == 0 && len(chat.Messages) == 0 {
							delete(m.unreadChats, chatID)
						}
						m.sortChatOrderLocked()
						m.cursor = 0
						for idx, id := range m.chatOrder {
							if id == chatID {
								m.cursor = idx
								break
							}
						}
						if m.cursor >= len(m.chatOrder) && len(m.chatOrder) > 0 {
							m.cursor = len(m.chatOrder) - 1
						}
						m.mu.Unlock()
						m.previewStatus = fmt.Sprintf("Unpinned %s in watui.yaml", chat.Name)
					}
				} else {
					if err := m.cfg.AddPinChat(chatID); err != nil {
						m.previewStatus = fmt.Sprintf("Error pinning in config: %v", err)
					} else {
						m.mu.Lock()
						chat.IsPinned = true
						m.sortChatOrderLocked()
						m.cursor = 0
						for idx, id := range m.chatOrder {
							if id == chatID {
								m.cursor = idx
								break
							}
						}
						m.mu.Unlock()
						m.previewStatus = fmt.Sprintf("Pinned %s in watui.yaml", chat.Name)
					}
				}
			}
		}
		return nil

	case "enter", "l", "right": // open chat
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.promptGlobalMute = false
		m.pendingDocMsg = nil
		if len(m.chatOrder) > 0 && m.cursor < len(m.chatOrder) {
			chatID := m.chatOrder[m.cursor]
			chat := m.unreadChats[chatID]
			m.activeChatID = chatID
			m.activeName = chat.Name
			m.activeMsgs = nil
			for _, item := range chat.Messages {
				if m.cfg.IsReactionsDisabled() && item.Type == domain.MessageTypeReaction {
					continue
				}
				m.activeMsgs = append(m.activeMsgs, item)
			}
			m.chatScrollOffset = 0
			m.input.Reset()
			m.input.SetHeight(1)
			m.input.Focus()
			m.view = ViewChat
			m.selectedMsgIdx = -1
			m.replyToMsg = nil
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
			chat.HasMention = false
			if m.readChats == nil {
				m.readChats = make(map[string]bool)
			}
			m.readChats[chatID] = true

			cmds := []tea.Cmd{tea.ClearScreen, textinput.Blink}
			if chat.IsGroup || strings.Contains(chatID, "@g.us") {
				m.groupParticipants = nil
				m.mentionHints = nil
				m.mentionCursor = 0
				cmds = append(cmds, m.fetchGroupParticipantsCmd(chatID))
			}
			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				cmds = append(cmds, m.fetchHistoryCmd(chatID, limit, time.Time{}, true))
			}

			return tea.Batch(cmds...)
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
		chat.HasMention = false
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

func (m *Model) removeMessageFromActive(msgID string) {
	if msgID == "" || len(m.activeMsgs) == 0 {
		return
	}
	idx := -1
	for i, msg := range m.activeMsgs {
		if msg.ID == msgID {
			idx = i
			break
		}
	}
	if idx != -1 {
		m.activeMsgs = append(m.activeMsgs[:idx], m.activeMsgs[idx+1:]...)
	}
}

func (m *Model) removeMessageFromUnread(chatID, msgID string) {
	if chatID == "" || msgID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	chat, exists := m.unreadChats[chatID]
	if !exists {
		return
	}
	idx := -1
	for i, msg := range chat.Messages {
		if msg.ID == msgID {
			idx = i
			break
		}
	}
	if idx != -1 {
		chat.Messages = append(chat.Messages[:idx], chat.Messages[idx+1:]...)
		if len(chat.Messages) > 0 {
			last := chat.Messages[len(chat.Messages)-1]
			chat.LastReceived = last.Timestamp
			chat.Sender = last.Sender
		}
		if chat.UnreadCount > 0 {
			chat.UnreadCount--
		}
	}
}

// LocalMuteToggled returns true if a chat was locally muted or unmuted during this session.
func (m *Model) LocalMuteToggled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.localMuteToggled
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

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	dismissed := make(map[string]bool)
	var targets []string

	for id, chat := range m.unreadChats {
		if chat != nil && chat.UnreadCount == 0 && !chat.IsPinned {
			if !dismissed[id] {
				targets = append(targets, id)
				dismissed[id] = true
			}
		}
	}
	for id := range m.readChats {
		if !dismissed[id] {
			targets = append(targets, id)
			dismissed[id] = true
		}
	}

	if len(targets) > 0 {
		var wg sync.WaitGroup
		for _, id := range targets {
			wg.Add(1)
			go func(cID string) {
				defer wg.Done()
				_ = m.adapter.DismissUnread(cleanupCtx, cID)
			}(id)
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-cleanupCtx.Done():
		}
	}
	_ = ClearClipboardCache()
	if m.cfg != nil && m.cfg.IsClearOnExitEnabled() {
		_ = media.ClearMediaCache()
	} else {
		expireHours := 72
		if m.cfg != nil {
			expireHours = m.cfg.GetExpireMediaHours()
		}
		if expireHours > 0 {
			_, _ = media.CleanExpiredMedia(expireHours)
		}
	}
}

var linkRegex = regexp.MustCompile(`(?:https?://|www\.)[^\s<>"']+[^\s<>"'.,!?;:)]`)

func extractLinks(body string) []string {
	matches := linkRegex.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var links []string
	for _, m := range matches {
		u := m
		if strings.HasPrefix(strings.ToLower(u), "www.") {
			u = "https://" + u
		}
		if !seen[u] {
			seen[u] = true
			links = append(links, u)
		}
	}
	return links
}

var openInBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

var clipboardWriteAll = func(text string) error {
	_, _ = fmt.Fprint(os.Stderr, osc52.New(text))
	return clipboard.WriteAll(text)
}

func copyToClipboard(text string) error {
	return clipboardWriteAll(text)
}

func (m *Model) syncSelectedMediaIdx() {
	if m.selectedMsgIdx < 0 || m.selectedMsgIdx >= len(m.activeMsgs) {
		return
	}
	mediaIndices := m.getChatMediaIndices()
	if len(mediaIndices) == 0 {
		return
	}
	for mIdx, origIdx := range mediaIndices {
		if origIdx == m.selectedMsgIdx {
			m.selectedMediaIdx = mIdx
			return
		}
	}
	for mIdx, origIdx := range mediaIndices {
		if origIdx >= m.selectedMsgIdx {
			m.selectedMediaIdx = mIdx
			return
		}
	}
	m.selectedMediaIdx = len(mediaIndices) - 1
}

func (m *Model) jumpToNextMedia() bool {
	m.confirmDocAction = false
	m.pendingDocMsg = nil
	mediaIndices := m.getChatMediaIndices()
	if len(mediaIndices) == 0 {
		m.previewStatus = "No media found in this chat"
		return false
	}
	if m.selectedMsgIdx < 0 {
		if m.selectedMediaIdx < len(mediaIndices)-1 {
			m.selectedMediaIdx++
		} else {
			m.selectedMediaIdx = 0
		}
		m.selectedMsgIdx = mediaIndices[m.selectedMediaIdx]
		m.input.Blur()
		m.scrollToMessage(m.selectedMsgIdx)
		m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[m.selectedMsgIdx].Type)
		return true
	}
	targetMediaIdx := -1
	for mIdx, origIdx := range mediaIndices {
		if origIdx > m.selectedMsgIdx {
			targetMediaIdx = mIdx
			break
		}
	}
	if targetMediaIdx == -1 {
		targetMediaIdx = len(mediaIndices) - 1
	}
	m.selectedMediaIdx = targetMediaIdx
	m.selectedMsgIdx = mediaIndices[m.selectedMediaIdx]
	m.input.Blur()
	m.scrollToMessage(m.selectedMsgIdx)
	m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[m.selectedMsgIdx].Type)
	return true
}

func (m *Model) jumpToPrevMedia() bool {
	m.confirmDocAction = false
	m.pendingDocMsg = nil
	mediaIndices := m.getChatMediaIndices()
	if len(mediaIndices) == 0 {
		m.previewStatus = "No media found in this chat"
		return false
	}
	if m.selectedMsgIdx < 0 {
		if m.selectedMediaIdx > 0 {
			m.selectedMediaIdx--
		} else {
			m.selectedMediaIdx = len(mediaIndices) - 1
		}
		m.selectedMsgIdx = mediaIndices[m.selectedMediaIdx]
		m.input.Blur()
		m.scrollToMessage(m.selectedMsgIdx)
		m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[m.selectedMsgIdx].Type)
		return true
	}
	targetMediaIdx := -1
	for i := len(mediaIndices) - 1; i >= 0; i-- {
		if mediaIndices[i] < m.selectedMsgIdx {
			targetMediaIdx = i
			break
		}
	}
	if targetMediaIdx == -1 {
		targetMediaIdx = 0
	}
	m.selectedMediaIdx = targetMediaIdx
	m.selectedMsgIdx = mediaIndices[m.selectedMediaIdx]
	m.input.Blur()
	m.scrollToMessage(m.selectedMsgIdx)
	m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[m.selectedMsgIdx].Type)
	return true
}

func (m *Model) updateChat(msg tea.KeyMsg) tea.Cmd {
	if m.confirmDelete {
		handled, cmd := m.handleConfirmDelete(msg)
		if handled {
			return cmd
		}
	}

	// Global hotkeys in chat view
	switch msg.String() {
	case "ctrl+c":
		m.stopActiveViewer()
		m.CleanupOnExit()
		return tea.Quit

	case "alt+x":
		m.stopActiveViewer()
		m.previewStatus = "Playback stopped"
		return nil
	}

	// 1. Message Hover Mode: user is navigating messages in chat
	if m.selectedMsgIdx >= 0 {
		switch msg.String() {
		case "esc":
			if len(m.multiSelectedMsgs) > 0 {
				m.multiSelectedMsgs = make(map[string]struct{})
				m.previewStatus = "Selection cleared"
				return nil
			}
			m.selectedMsgIdx = -1
			m.input.Focus()
			m.previewStatus = ""
			return textinput.Blink

		case "r":
			m.multiSelectedMsgs = make(map[string]struct{})
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				m.replyToMsg = &target
			}
			m.editTargetMsg = nil
			m.selectedMsgIdx = -1
			m.input.Focus()
			m.previewStatus = ""
			return textinput.Blink

		case "e":
			m.multiSelectedMsgs = make(map[string]struct{})
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if !target.IsFromMe {
					m.previewStatus = "Cannot edit: only your own sent messages can be edited"
					return nil
				}
				if target.IsMedia() || (target.Type != "" && target.Type != domain.MessageTypeText) ||
					strings.HasPrefix(target.Body, "[Document") ||
					strings.HasPrefix(target.Body, "[Image") ||
					strings.HasPrefix(target.Body, "[Video") ||
					strings.HasPrefix(target.Body, "[Audio") ||
					strings.HasPrefix(target.Body, "[GIF") ||
					strings.HasPrefix(target.Body, "[Sticker") {
					m.previewStatus = "Cannot edit media or attachment messages"
					return nil
				}
				if target.Body == "" {
					m.previewStatus = "Cannot edit empty message"
					return nil
				}
				m.editTargetMsg = &target
				m.replyToMsg = nil
				m.input.SetValue(target.Body)
				m.input.CursorEnd()
				m.input.SetHeight(min(5, max(1, m.input.LineCount())))
				m.selectedMsgIdx = -1
				m.input.Focus()
				m.previewStatus = ""
				return textinput.Blink
			}
			return nil

		case "ctrl+v", "alt+v":
			if !m.cfg.IsClipboardPasteEnabled() {
				return nil
			}
			m.selectedMsgIdx = -1
			m.input.Focus()
			m.previewStatus = "Pasting from clipboard..."
			return m.pasteClipboardCmd()

		case "y":
			if len(m.multiSelectedMsgs) > 0 {
				selectedMsgs := m.getSelectedMsgsInOrder()
				var texts []string
				for _, msg := range selectedMsgs {
					body := strings.TrimSpace(m.formatMentions(msg.Body))
					if body == "" {
						continue
					}
					if strings.HasPrefix(body, "[Document") ||
						strings.HasPrefix(body, "[Image") ||
						strings.HasPrefix(body, "[Video") ||
						strings.HasPrefix(body, "[Audio") ||
						strings.HasPrefix(body, "[GIF") ||
						strings.HasPrefix(body, "[Sticker") {
						continue
					}
					texts = append(texts, body)
				}
				if len(texts) == 0 {
					m.previewStatus = "No text messages selected to copy"
					return nil
				}
				joined := strings.Join(texts, "\n")
				_ = copyToClipboard(joined)
				m.previewStatus = fmt.Sprintf("Copied %d message(s) to clipboard", len(texts))
				return nil
			}
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				textToCopy := m.formatMentions(target.Body)
				if textToCopy == "" {
					m.previewStatus = "Message body is empty"
				} else {
					_ = copyToClipboard(textToCopy)
					m.previewStatus = "Copied message to clipboard"
				}
			}
			return nil

		case "l":
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				links := extractLinks(target.Body)
				if len(links) == 0 {
					m.previewStatus = "No link in message"
					return nil
				}
				opened := 0
				for _, link := range links {
					if err := openInBrowser(link); err == nil {
						opened++
					}
				}
				if opened == 0 {
					m.previewStatus = "Failed to open link(s) in browser"
				} else if opened == 1 {
					disp := links[0]
					if len(disp) > 35 {
						disp = disp[:32] + "..."
					}
					m.previewStatus = "Opened: " + disp
				} else {
					m.previewStatus = fmt.Sprintf("Opened %d links in browser", opened)
				}
			}
			return nil

		case "p":
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if !target.IsMedia() {
					m.previewStatus = "Selected message has no media"
					return nil
				}
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.pendingDocMsg = nil
				if target.Type == domain.MessageTypeDocument {
					m.confirmDocAction = true
					m.pendingDocMsg = &target
					m.previewStatus = ""
					return nil
				}
				return m.previewMediaCmd(target)
			}
			return nil

		case " ":
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if m.multiSelectedMsgs == nil {
					m.multiSelectedMsgs = make(map[string]struct{})
				}
				if _, ok := m.multiSelectedMsgs[target.ID]; ok {
					delete(m.multiSelectedMsgs, target.ID)
				} else {
					m.multiSelectedMsgs[target.ID] = struct{}{}
				}
				if m.selectedMsgIdx < len(m.activeMsgs)-1 {
					m.selectedMsgIdx++
				}
				m.scrollToMessage(m.selectedMsgIdx)
				m.syncSelectedMediaIdx()
				if len(m.multiSelectedMsgs) > 0 {
					m.previewStatus = fmt.Sprintf("%d message(s) selected", len(m.multiSelectedMsgs))
				} else {
					m.previewStatus = "Selection cleared"
				}
			}
			return nil

		case "s":
			if len(m.multiSelectedMsgs) > 0 {
				targets := m.getSelectedMsgsInOrder()
				m.multiSelectedMsgs = make(map[string]struct{})
				return m.saveMultiMessagesCmd(targets)
			}
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if target.IsMedia() {
					if target.Type == domain.MessageTypeDocument {
						m.confirmDocAction = true
						m.pendingDocMsg = &target
						m.previewStatus = ""
						return nil
					}
					return m.downloadAndSaveMediaCmd(target)
				}
			}
			return nil

		case "shift+s", "S":
			if len(m.multiSelectedMsgs) > 0 {
				targets := m.getSelectedMsgsInOrder()
				m.multiSelectedMsgs = make(map[string]struct{})
				return m.pickSaveLocationCmd(saveTargetData{Mode: "multi_msgs", MultiMsgs: targets})
			}
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if target.IsMedia() {
					return m.pickSaveLocationCmd(saveTargetData{Mode: "single_msg", Message: target})
				}
			}
			return nil

		case "d":
			if len(m.multiSelectedMsgs) > 0 {
				m.confirmDelete = true
				m.pendingDeleteForEveryone = false
				m.pendingDeleteMultiMsgs = m.getSelectedMsgsInOrder()
				m.pendingDeleteMsg = nil
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.previewStatus = ""
				return nil
			}
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				m.confirmDelete = true
				m.pendingDeleteForEveryone = false
				m.pendingDeleteMsg = &target
				m.pendingDeleteMultiMsgs = nil
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.previewStatus = ""
			}
			return nil

		case "shift+d", "D":
			if len(m.multiSelectedMsgs) > 0 {
				selectedMsgs := m.getSelectedMsgsInOrder()
				isGroup := strings.Contains(m.activeChatID, "@g.us")
				if !isGroup {
					hasFromMe := false
					for _, sm := range selectedMsgs {
						if sm.IsFromMe {
							hasFromMe = true
							break
						}
					}
					if !hasFromMe {
						m.previewStatus = "Cannot delete for all: only your own sent messages can be deleted for everyone in direct chats (press 'd' to delete for you)"
						return nil
					}
				}
				m.confirmDelete = true
				m.pendingDeleteForEveryone = true
				m.pendingDeleteMultiMsgs = selectedMsgs
				m.pendingDeleteMsg = nil
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.previewStatus = ""
				return nil
			}
			if m.selectedMsgIdx < len(m.activeMsgs) {
				target := m.activeMsgs[m.selectedMsgIdx]
				if !target.IsFromMe && !strings.Contains(m.activeChatID, "@g.us") {
					m.previewStatus = "Cannot delete for all: only sender can delete for everyone in direct chats (press 'd' to delete for you)"
					return nil
				}
				m.confirmDelete = true
				m.pendingDeleteForEveryone = true
				m.pendingDeleteMsg = &target
				m.pendingDeleteMultiMsgs = nil
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.previewStatus = ""
			}
			return nil

		case "k", "up", "alt+k", "alt+up":
			m.confirmDocAction = false
			m.pendingDocMsg = nil
			if len(m.activeMsgs) > 0 {
				if m.selectedMsgIdx > 0 {
					m.selectedMsgIdx--
				} else {
					m.selectedMsgIdx = len(m.activeMsgs) - 1
				}
				m.scrollToMessage(m.selectedMsgIdx)
				m.syncSelectedMediaIdx()
				m.previewStatus = fmt.Sprintf("Message %d/%d", m.selectedMsgIdx+1, len(m.activeMsgs))
			}
			return nil

		case "j", "down", "alt+j", "alt+down":
			m.confirmDocAction = false
			m.pendingDocMsg = nil
			if len(m.activeMsgs) > 0 {
				if m.selectedMsgIdx < len(m.activeMsgs)-1 {
					m.selectedMsgIdx++
				} else {
					m.selectedMsgIdx = 0
				}
				m.scrollToMessage(m.selectedMsgIdx)
				m.syncSelectedMediaIdx()
				m.previewStatus = fmt.Sprintf("Message %d/%d", m.selectedMsgIdx+1, len(m.activeMsgs))
			}
			return nil

		case "alt+shift+k", "alt+K", "alt+shift+up", "alt+left", "K", "shift+up", "M", "[":
			m.jumpToPrevMedia()
			return nil

		case "alt+shift+j", "alt+J", "alt+shift+down", "alt+right", "J", "shift+down", "m", "]":
			m.jumpToNextMedia()
			return nil

		case "alt+p":
			if m.selectedMsgIdx < len(m.activeMsgs) && m.activeMsgs[m.selectedMsgIdx].IsMedia() {
				target := m.activeMsgs[m.selectedMsgIdx]
				m.confirmSave = false
				m.confirmDocAction = false
				m.promptOpenWith = false
				m.pendingDocMsg = nil
				if target.Type == domain.MessageTypeDocument {
					m.confirmDocAction = true
					m.pendingDocMsg = &target
					m.previewStatus = ""
					return nil
				}
				return m.previewMediaCmd(target)
			}
			mediaIndices := m.getChatMediaIndices()
			if len(mediaIndices) == 0 {
				m.previewStatus = "No media found in this chat"
				return nil
			}
			target := m.activeMsgs[mediaIndices[len(mediaIndices)-1]]
			m.confirmSave = false
			m.confirmDocAction = false
			m.promptOpenWith = false
			m.pendingDocMsg = nil
			if target.Type == domain.MessageTypeDocument {
				m.confirmDocAction = true
				m.pendingDocMsg = &target
				m.previewStatus = ""
				return nil
			}
			return m.previewMediaCmd(target)

		case "pgup", "ctrl+y":
			m.chatScrollOffset += 5
			return nil

		case "pgdown", "ctrl+d", "ctrl+e":
			m.chatScrollOffset = max(0, m.chatScrollOffset-5)
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

		default:
			return nil
		}
	}

	// 2. Focused on message input box
	switch msg.String() {
	case "esc":
		if len(m.mentionHints) > 0 {
			m.mentionHints = nil
			m.mentionCursor = 0
			return nil
		}
		if m.editTargetMsg != nil {
			m.editTargetMsg = nil
			m.input.SetValue("")
			m.input.SetHeight(1)
			m.previewStatus = "Edit cancelled"
			return nil
		}
		if m.replyToMsg != nil {
			m.replyToMsg = nil
			m.previewStatus = "Reply cancelled"
			return nil
		}
		m.stopActiveViewer()
		m.previewStatus = ""
		m.confirmSave = false
		m.confirmDocAction = false
		m.promptOpenWith = false
		m.pendingDocMsg = nil
		m.multiSelectedMsgs = make(map[string]struct{})
		m.pendingDeleteMultiMsgs = nil
		m.mu.Lock()
		if chat, exists := m.unreadChats[m.activeChatID]; exists {
			chat.Messages = append([]domain.Message(nil), m.activeMsgs...)
			chat.UnreadCount = 0
		}
		m.sortChatOrderLocked()
		m.mu.Unlock()
		m.view = ViewUnreadList
		return tea.ClearScreen

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
		targetMsg := m.activeMsgs[mediaIndices[len(mediaIndices)-1]]
		if targetMsg.Type == domain.MessageTypeDocument {
			m.confirmDocAction = true
			m.promptOpenWith = false
			m.pendingDocMsg = &targetMsg
			m.previewStatus = ""
			return nil
		}
		return m.previewMediaCmd(targetMsg)

	case "alt+up", "alt+k":
		m.confirmDocAction = false
		m.pendingDocMsg = nil
		if len(m.activeMsgs) > 0 {
			m.selectedMsgIdx = len(m.activeMsgs) - 1
			m.input.Blur()
			m.scrollToMessage(m.selectedMsgIdx)
			m.syncSelectedMediaIdx()
			m.previewStatus = fmt.Sprintf("Message %d/%d", m.selectedMsgIdx+1, len(m.activeMsgs))
		}
		return nil

	case "alt+down", "alt+j":
		m.confirmDocAction = false
		m.pendingDocMsg = nil
		if len(m.activeMsgs) > 0 {
			m.selectedMsgIdx = 0
			m.input.Blur()
			m.scrollToMessage(m.selectedMsgIdx)
			m.syncSelectedMediaIdx()
			m.previewStatus = fmt.Sprintf("Message %d/%d", m.selectedMsgIdx+1, len(m.activeMsgs))
		}
		return nil

	case "alt+m":
		if m.selectedMsgIdx < 0 {
			mediaIndices := m.getChatMediaIndices()
			if len(mediaIndices) == 0 {
				m.previewStatus = "No media found in this chat"
				return nil
			}
			if m.selectedMediaIdx < 0 || m.selectedMediaIdx >= len(mediaIndices) {
				m.selectedMediaIdx = len(mediaIndices) - 1
			}
			m.selectedMsgIdx = mediaIndices[m.selectedMediaIdx]
			m.input.Blur()
			m.scrollToMessage(m.selectedMsgIdx)
			m.previewStatus = fmt.Sprintf("Media %d/%d (%s)", m.selectedMediaIdx+1, len(mediaIndices), m.activeMsgs[m.selectedMsgIdx].Type)
			return nil
		}
		m.jumpToNextMedia()
		return nil

	case "alt+shift+k", "alt+K", "alt+shift+up", "alt+left", "alt+M", "alt+shift+m", "alt+[":
		m.jumpToPrevMedia()
		return nil

	case "alt+shift+j", "alt+J", "alt+shift+down", "alt+right", "alt+]":
		m.jumpToNextMedia()
		return nil

	case "alt+f":
		if m.editTargetMsg != nil {
			m.previewStatus = "Cannot attach files while editing a message"
			return nil
		}
		m.previewStatus = "Opening file selector..."
		return m.pickFileCmd()

	case "ctrl+v", "alt+v":
		if m.cfg.IsClipboardPasteEnabled() {
			m.previewStatus = "Pasting from clipboard..."
			return m.pasteClipboardCmd()
		}

	case "pgup", "ctrl+y":
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

	case "pgdown", "ctrl+d", "ctrl+e":
		m.chatScrollOffset = max(0, m.chatScrollOffset-5)
		return nil

	case "tab":
		if len(m.mentionHints) > 0 {
			m.applyMentionHint()
			return nil
		}

	case "ctrl+n":
		if len(m.mentionHints) > 0 {
			m.mentionCursor = (m.mentionCursor + 1) % len(m.mentionHints)
			return nil
		}

	case "ctrl+p":
		if len(m.mentionHints) > 0 {
			m.mentionCursor = (m.mentionCursor - 1 + len(m.mentionHints)) % len(m.mentionHints)
			return nil
		}

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
		m.updateMentionHints()
		return nil

	case "enter":
		if len(m.mentionHints) > 0 {
			m.applyMentionHint()
			return nil
		}
		if m.editTargetMsg != nil {
			text := strings.TrimSpace(m.input.Value())
			if strings.HasPrefix(text, "file://") {
				m.previewStatus = "Cannot attach files while editing a message"
				return nil
			}
			target := *m.editTargetMsg
			m.editTargetMsg = nil
			m.input.Reset()
			m.input.SetHeight(1)
			m.mentionHints = nil
			m.mentionCursor = 0
			if text == "" {
				m.previewStatus = "Cannot send empty message"
				return nil
			}
			if text == target.Body {
				m.previewStatus = "Message unchanged"
				return nil
			}
			m.previewStatus = "Editing message..."
			return m.editMessageCmd(m.activeChatID, target.ID, text)
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		if strings.HasPrefix(text, "/ephemeral") || strings.HasPrefix(text, "/disappearing") {
			m.input.Reset()
			m.input.SetHeight(1)
			m.mentionHints = nil
			m.mentionCursor = 0
			chatID := m.activeChatID
			if chatID == "" {
				m.previewStatus = "No active chat selected"
				return nil
			}
			parts := strings.Fields(text)
			arg := ""
			if len(parts) > 1 {
				arg = strings.ToLower(parts[1])
			}
			currTimer := m.adapter.GetChatEphemeralTimer(chatID)
			var targetDur time.Duration
			var actionName string
			switch arg {
			case "":
				if currTimer > 0 {
					targetDur = time.Duration(currTimer) * time.Second
					actionName = fmt.Sprintf("Re-applying disappearing timer (%s)...", domain.FormatDisappearingTimer(currTimer))
				} else {
					m.previewStatus = "Disappearing messages are off. Use: /ephemeral [24h | 7d | 90d | off]"
					return nil
				}
			case "status":
				if currTimer > 0 {
					m.previewStatus = fmt.Sprintf("Disappearing messages: %s", domain.FormatDisappearingTimer(currTimer))
				} else {
					m.previewStatus = "Disappearing messages: off"
				}
				return nil
			case "24h", "24", "1d":
				targetDur = 24 * time.Hour
				actionName = "Setting disappearing timer to 24h..."
			case "7d", "7", "1w":
				targetDur = 7 * 24 * time.Hour
				actionName = "Setting disappearing timer to 7d..."
			case "90d", "90", "3m":
				targetDur = 90 * 24 * time.Hour
				actionName = "Setting disappearing timer to 90d..."
			case "off", "0", "disable":
				targetDur = 0
				actionName = "Turning off disappearing messages..."
			default:
				m.previewStatus = "Invalid option. Use: /ephemeral [24h | 7d | 90d | off | status]"
				return nil
			}

			m.previewStatus = actionName
			return func() tea.Msg {
				err := m.adapter.SetChatDisappearingTimer(m.ctx, chatID, targetDur)
				return ephemeralSetResultMsg{ChatID: chatID, Timer: uint32(targetDur.Seconds()), Err: err}
			}
		}
		m.previewStatus = "Sending..."
		m.input.Reset()
		m.input.SetHeight(1)
		m.mentionHints = nil
		m.mentionCursor = 0
		chatID := m.activeChatID
		replyTarget := m.replyToMsg
		m.replyToMsg = nil

		if strings.HasPrefix(text, "file://") {
			filePath, caption := parseFileURI(text)
			return func() tea.Msg {
				var sentMsg domain.Message
				var err error
				if replyTarget != nil {
					sentMsg, err = m.adapter.SendFileMessage(m.ctx, chatID, filePath, caption, replyTarget.ID, replyTarget.Body, replyTarget.Sender)
				} else {
					sentMsg, err = m.adapter.SendFileMessage(m.ctx, chatID, filePath, caption)
				}
				if err != nil {
					return sendErrMsg{ChatID: chatID, Text: text, Err: err}
				}
				return messageSentMsg(sentMsg)
			}
		}

		return func() tea.Msg {
			var sentMsg domain.Message
			var err error
			if replyTarget != nil {
				sentMsg, err = m.adapter.SendTextMessage(m.ctx, chatID, text, replyTarget.ID, replyTarget.Body, replyTarget.Sender)
			} else {
				sentMsg, err = m.adapter.SendTextMessage(m.ctx, chatID, text)
			}
			if err != nil {
				return sendErrMsg{ChatID: chatID, Text: text, Err: err}
			}
			return messageSentMsg(sentMsg)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.input.SetHeight(min(5, max(1, m.input.LineCount())))
	m.updateMentionHints()
	return cmd
}

func (m *Model) updateContactPicker(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.view = ViewUnreadList
		return tea.ClearScreen

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
				for _, item := range chat.Messages {
					if m.cfg.IsReactionsDisabled() && item.Type == domain.MessageTypeReaction {
						continue
					}
					m.activeMsgs = append(m.activeMsgs, item)
				}
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
			m.selectedMsgIdx = -1
			m.replyToMsg = nil
			mediaIndices := m.getChatMediaIndices()
			m.selectedMediaIdx = len(mediaIndices) - 1
			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				cmds := []tea.Cmd{tea.ClearScreen, textinput.Blink, m.fetchHistoryCmd(contact.JID, limit, time.Time{}, true)}
				if contact.IsGroup || strings.Contains(contact.JID, "@g.us") {
					m.groupParticipants = nil
					m.mentionHints = nil
					m.mentionCursor = 0
					cmds = append(cmds, m.fetchGroupParticipantsCmd(contact.JID))
				}
				return tea.Batch(cmds...)
			}
			cmds := []tea.Cmd{tea.ClearScreen, textinput.Blink}
			if contact.IsGroup || strings.Contains(contact.JID, "@g.us") {
				m.groupParticipants = nil
				m.mentionHints = nil
				m.mentionCursor = 0
				cmds = append(cmds, m.fetchGroupParticipantsCmd(contact.JID))
			}
			return tea.Batch(cmds...)
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
				for _, item := range chat.Messages {
					if m.cfg.IsReactionsDisabled() && item.Type == domain.MessageTypeReaction {
						continue
					}
					m.activeMsgs = append(m.activeMsgs, item)
				}
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
			m.selectedMsgIdx = -1
			m.replyToMsg = nil
			mediaIndices := m.getChatMediaIndices()
			m.selectedMediaIdx = len(mediaIndices) - 1
			cmds := []tea.Cmd{tea.ClearScreen, textinput.Blink}
			if strings.Contains(targetID, "@g.us") {
				m.groupParticipants = nil
				m.mentionHints = nil
				m.mentionCursor = 0
				cmds = append(cmds, m.fetchGroupParticipantsCmd(targetID))
			}
			if m.cfg.IsAlwaysLoadHistoryEnabled() && m.cfg.IsHistoryPersistEnabled() {
				limit := m.cfg.GetCycleMsgCountPerChat()
				cmds = append(cmds, m.fetchHistoryCmd(targetID, limit, time.Time{}, true))
			}
			return tea.Batch(cmds...)
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
			if strings.HasSuffix(c.JID, "@lid") {
				continue
			}
			if c.Name == "You" {
				continue
			}
			if !m.isHidden(c.JID, c.Name, c.PushName) {
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
		if strings.HasSuffix(c.JID, "@lid") {
			continue
		}
		if c.Name == "You" {
			continue
		}
		if m.isHidden(c.JID, c.Name, c.PushName) {
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
			Background(lipgloss.Color("#A6E3A1")).
			Padding(0, 1)

	pinBadgeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#11111B")).
			Background(lipgloss.Color("#89B4FA")).
			Padding(0, 1)

	mentionBadgeStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#11111B")).
				Background(lipgloss.Color("#A6E3A1")).
				Padding(0, 1)

	ephemeralBadgeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FAB387"))

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

	mentionYouStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#A6E3A1"))

	mentionOtherStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#89B4FA"))
)

var (
	mentionRegex    = regexp.MustCompile(`(?:^|[^\w@])@(\d{5,20})\b`)
	mentionYouRegex = regexp.MustCompile(`(?:^|[^\w@])(@You)\b`)
)

func (m *Model) formatMentions(text string) string {
	if text == "" {
		return text
	}
	matches := mentionRegex.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	for i := len(matches) - 1; i >= 0; i-- {
		sub := matches[i]
		digitStart, digitEnd := sub[2], sub[3]
		digits := text[digitStart:digitEnd]
		resolved := m.resolveSenderNameByID(digits)
		if resolved != "" && resolved != digits && !strings.Contains(resolved, "@") {
			text = text[:digitStart] + resolved + text[digitEnd:]
		}
	}
	return text
}

func (m *Model) styleMentionsForDisplay(text string) string {
	if text == "" {
		return text
	}
	// First resolve raw numbers in mentions
	matches := mentionRegex.FindAllStringSubmatchIndex(text, -1)
	if len(matches) > 0 {
		for i := len(matches) - 1; i >= 0; i-- {
			sub := matches[i]
			digitStart, digitEnd := sub[2], sub[3]
			digits := text[digitStart:digitEnd]
			resolved := m.resolveSenderNameByID(digits)
			if resolved != "" && resolved != digits && !strings.Contains(resolved, "@") {
				var styled string
				if resolved == "You" {
					styled = mentionYouStyle.Render("@You")
				} else {
					styled = mentionOtherStyle.Render("@" + resolved)
				}
				atIdx := digitStart - 1
				text = text[:atIdx] + styled + text[digitEnd:]
			}
		}
	}
	// Also check if text already had "@You" (e.g. from adapter or database)
	youMatches := mentionYouRegex.FindAllStringSubmatchIndex(text, -1)
	for i := len(youMatches) - 1; i >= 0; i-- {
		sub := youMatches[i]
		start, end := sub[2], sub[3]
		styled := mentionYouStyle.Render("@You")
		text = text[:start] + styled + text[end:]
	}
	return text
}

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
	case ViewKeybindsHelp:
		lines = m.renderKeybindsHelpView()
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
				if chat.HasMention {
					badge = mentionBadgeStyle.Render(fmt.Sprintf("@ %d", chat.UnreadCount))
				} else {
					badge = badgeStyle.Render(fmt.Sprintf("%d", chat.UnreadCount))
				}
			} else if chat.IsPinned {
				badge = pinBadgeStyle.Render("PIN")
			}

			name := chat.Name
			if chat.IsGroup || strings.Contains(chat.ChatID, "@g.us") {
				name = "[Group] " + chat.Name
			}
			if chat.IsArchived && !m.showArchived {
				name = "[Archived] " + name
			}
			if chat.IsMuted || m.isMuted(chat.ChatID, chat.Name) {
				name = "[Muted] " + name
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
				clean = m.formatMentions(clean)

				// For groups, prefix snippet with sender name so context is clear: "Sender: message"
				if (chat.IsGroup || strings.Contains(chat.ChatID, "@g.us")) && !last.IsFromMe {
					sender := m.resolveMsgSenderName(last)
					if sender != "" {
						clean = sender + ": " + clean
					}
				}

				if last.IsEdit {
					clean = "[EDIT] " + clean
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
	if m.promptGlobalMute {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render(fmt.Sprintf("Mute %s globally: 1: 1 hour, 2: 8 hours, 3: Always (Esc to cancel)", m.globalMuteChatName))
	} else if m.promptOpenWith {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Open with: ") + m.openWithInput.View() + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(" (Enter to open, Esc to cancel)")
	} else if m.confirmDocAction {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Document: Open [o], Open with [w], Save [s], or Choose location [S]? (Esc to cancel)")
	} else if m.confirmSave {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Save? [y/s: default dir, S: choose location, n: cancel]")
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
	return lines
}

func (m *Model) renderChatView() []string {
	cw := m.contentWidth()
	var lines []string

	chatPrefix := "Chat with"
	if strings.Contains(m.activeChatID, "@g.us") {
		chatPrefix = "Group"
	}
	ephBadge := ""
	if m.adapter != nil && m.activeChatID != "" {
		if timer := m.adapter.GetChatEphemeralTimer(m.activeChatID); timer > 0 {
			ephBadge = " " + ephemeralBadgeStyle.Render(fmt.Sprintf("[⏱ %s]", domain.FormatDisappearingTimer(timer)))
		}
	}
	leftTitle := fmt.Sprintf("%s %s%s", titleStyle.Render(chatPrefix), selectedTitleStyle.Render(m.activeName), ephBadge)
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
		leftTitle = fmt.Sprintf("%s %s%s", titleStyle.Render(chatPrefix), selectedTitleStyle.Render(truncName), ephBadge)
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
		var lastDayKey string
		for i, msg := range m.activeMsgs {
			if m.cfg.IsReactionsDisabled() && msg.Type == domain.MessageTypeReaction {
				continue
			}

			if !msg.Timestamp.IsZero() {
				dayKey := msg.Timestamp.Local().Format("2006-01-02")
				if dayKey != lastDayKey {
					lastDayKey = dayKey
					if len(msgLines) > 0 && msgLines[len(msgLines)-1] != "" {
						msgLines = append(msgLines, "")
					}
					msgLines = append(msgLines, m.renderDateDivider(msg.Timestamp, cw))
					msgLines = append(msgLines, "")
				}
			}

			timeStr := ""
			if !msg.Timestamp.IsZero() {
				timeStr = msg.Timestamp.Format("15:04")
			}
			var header string
			mediaBadge := ""
			isHovered := (i == m.selectedMsgIdx)

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

			cursorPrefix := "  "
			if isHovered {
				cursorPrefix = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("▸ ")
			}

			isSelected := m.isMsgMultiSelected(msg.ID)
			selectPrefix := ""
			if isSelected {
				selectPrefix = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render("[✓] ")
			}

			if msg.IsFromMe {
				header = fmt.Sprintf("%s%s%s %s%s",
					cursorPrefix,
					selectPrefix,
					lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render("You"),
					statusStyle.Render(timeStr),
					mediaBadge,
				)
			} else {
				sender := m.resolveMsgSenderName(msg)
				if sender == "" {
					sender = "Them"
				}
				header = fmt.Sprintf("%s%s%s %s%s",
					cursorPrefix,
					selectPrefix,
					selectedTitleStyle.Render(sender),
					statusStyle.Render(timeStr),
					mediaBadge,
				)
			}
			msgLines = append(msgLines, header)

			bodyPrefix := "    "
			if isHovered {
				bodyPrefix = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("#FAB387")).Render("┃ ")
			} else if isSelected {
				bodyPrefix = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1")).Render("┃ ")
			}

			if msg.QuotedText != "" || msg.QuotedID != "" {
				quoteContent := msg.QuotedText
				quoteSender := msg.QuotedSender
				if quoteContent == "" {
					// 1. Try to find the quoted message in activeMsgs or chatMessages
					for _, prev := range m.activeMsgs {
						if prev.ID == msg.QuotedID && prev.Body != "" {
							quoteContent = prev.Body
							if quoteSender == "" {
								quoteSender = m.resolveMsgSenderName(prev)
							}
							break
						}
					}
					if quoteContent == "" {
						if chat, ok := m.unreadChats[m.activeChatID]; ok && chat != nil {
							for _, prev := range chat.Messages {
								if prev.ID == msg.QuotedID && prev.Body != "" {
									quoteContent = prev.Body
									if quoteSender == "" {
										quoteSender = m.resolveMsgSenderName(prev)
									}
									break
								}
							}
						}
					}
				}
				if quoteSender != "" && !isRealChatName(quoteSender, "") {
					if resolved := m.resolveSenderNameByID(quoteSender); resolved != "" && isRealChatName(resolved, "") {
						quoteSender = resolved
					}
				}
				if quoteContent == "" {
					if quoteSender != "" {
						if msg.IsEdit {
							quoteContent = quoteSender
						} else if msg.Type == domain.MessageTypeReaction {
							quoteContent = "Reacted to " + quoteSender
						} else {
							quoteContent = "Replying to " + quoteSender
						}
					} else {
						if msg.IsEdit {
							quoteContent = "message"
						} else if msg.Type == domain.MessageTypeReaction {
							quoteContent = "[Reacted to message]"
						} else {
							quoteContent = "[Replying to message]"
						}
					}
				} else if quoteSender != "" && !strings.HasPrefix(quoteContent, quoteSender+":") && !strings.HasPrefix(quoteContent, "Replying to ") && !strings.HasPrefix(quoteContent, "Reacted to ") {
					quoteContent = quoteSender + ": " + quoteContent
				}

				quotePreview := strings.ReplaceAll(m.formatMentions(quoteContent), "\n", " ")
				maxQuoteW := max(15, cw-12)
				if len(quotePreview) > maxQuoteW {
					quotePreview = quotePreview[:maxQuoteW-3] + "..."
				}
				var quoteLine string
				if msg.IsEdit {
					cleanText := strings.TrimPrefix(quotePreview, "[EDIT] ")
					editTag := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("[EDIT] ")
					quoteLine = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render("┌─ ") + editTag + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Italic(true).Render(cleanText)
				} else {
					quoteLine = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Italic(true).Render("┌─ " + quotePreview)
				}
				msgLines = append(msgLines, bodyPrefix+quoteLine)
			}

			wrapped := wrapStyle.Render(m.styleMentionsForDisplay(msg.Body))
			for _, wl := range strings.Split(wrapped, "\n") {
				msgLines = append(msgLines, bodyPrefix+wl)
			}
			if i < len(m.activeMsgs)-1 {
				msgLines = append(msgLines, "")
			}
		}
	}

	replyBarLines := 0
	var replyBarStr string
	if m.replyToMsg != nil {
		replySender := m.resolveMsgSenderName(*m.replyToMsg)
		if replySender == "" {
			if m.replyToMsg.IsFromMe {
				replySender = "You"
			} else {
				replySender = "Them"
			}
		}
		replySnippet := strings.ReplaceAll(m.formatMentions(m.replyToMsg.Body), "\n", " ")
		maxSnippetW := max(15, cw-len(replySender)-25)
		if len(replySnippet) > maxSnippetW {
			replySnippet = replySnippet[:maxSnippetW-3] + "..."
		}
		replyBarStr = "  " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("↩ Replying to ") +
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#CDD6F4")).Render(replySender) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#A6ADC8")).Render(": "+replySnippet) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render("  (Esc to cancel)")
		replyBarLines = 1
	}

	editBarLines := 0
	var editBarStr string
	if m.editTargetMsg != nil {
		origSnippet := strings.ReplaceAll(m.formatMentions(m.editTargetMsg.Body), "\n", " ")
		maxSnippetW := max(15, cw-35)
		if len(origSnippet) > maxSnippetW {
			origSnippet = origSnippet[:maxSnippetW-3] + "..."
		}
		editBarStr = "  " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("✎ Editing: ") +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#A6ADC8")).Render(origSnippet) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render("  (Enter to save, Esc to cancel)")
		editBarLines = 1
	}

	var mentionHintLines []string
	if len(m.mentionHints) > 0 {
		header := "  " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89DCEB")).Render("@ Mention") +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(" [Tab/Enter: tag · Ctrl+N/P: select · Esc: close]:")
		mentionHintLines = append(mentionHintLines, header)

		maxVisible := 4
		if m.maxCanvasHeight() < 16 {
			maxVisible = 2
		}
		startIdx := 0
		if m.mentionCursor >= maxVisible {
			startIdx = m.mentionCursor - maxVisible + 1
		}
		endIdx := min(len(m.mentionHints), startIdx+maxVisible)

		for i := startIdx; i < endIdx; i++ {
			h := m.mentionHints[i]
			user := strings.Split(h.JID, "@")[0]
			if idx := strings.Index(user, ":"); idx != -1 {
				user = user[:idx]
			}
			phoneInfo := ""
			if user != "" && user != h.Name {
				phoneInfo = fmt.Sprintf(" (@%s)", user)
			}
			name := h.Name

			prefix := "    "
			if i == m.mentionCursor {
				prefix = "  ▸ "
			}

			// Ensure item fits within terminal content width cw
			prefixLen := 4
			phoneLen := len([]rune(phoneInfo))
			availName := max(8, cw-prefixLen-phoneLen-2)
			rName := []rune(name)
			if len(rName) > availName {
				name = string(rName[:availName-3]) + "..."
			}

			var itemLine string
			if i == m.mentionCursor {
				styledPrefix := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render(prefix)
				styledName := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6E3A1")).Render(name)
				styledPhone := lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(phoneInfo)
				itemLine = styledPrefix + styledName + styledPhone
			} else {
				styledPrefix := prefix
				styledName := lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4")).Render(name)
				styledPhone := lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(phoneInfo)
				itemLine = styledPrefix + styledName + styledPhone
			}
			mentionHintLines = append(mentionHintLines, itemLine)
		}

		if len(m.mentionHints) > maxVisible {
			footer := "    " + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(
				fmt.Sprintf("... (%d/%d matches · type to filter)", m.mentionCursor+1, len(m.mentionHints)),
			)
			mentionHintLines = append(mentionHintLines, footer)
		}
	}

	m.input.SetWidth(max(20, cw-6))
	inputLines := strings.Split(m.input.View(), "\n")
	inputH := len(inputLines) + replyBarLines + editBarLines + len(mentionHintLines)
	availH := max(1, m.maxCanvasHeight()-5-inputH)
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

	// One line padding on top of the message box / reply bar / edit bar
	lines = append(lines, "")
	if replyBarStr != "" {
		lines = append(lines, replyBarStr)
	} else if editBarStr != "" {
		lines = append(lines, editBarStr)
	}
	lines = append(lines, inputLines...)
	if len(mentionHintLines) > 0 {
		lines = append(lines, mentionHintLines...)
	}

	mediaIndices := m.getChatMediaIndices()
	var dynamicHelp string

	if m.selectedMsgIdx >= 0 {
		if len(m.multiSelectedMsgs) > 0 {
			dynamicHelp = fmt.Sprintf("[%d selected] [Space] Toggle · [y] Copy · [d/D] Delete · [s] Save · [Esc] Clear", len(m.multiSelectedMsgs))
		} else {
			dynamicHelp = "[j/k] Msg · [Space] Select · [J/K] Media · [p] Preview · [r] Reply · [e] Edit · [y] Copy · [d/D] Delete · [Esc] Input"
		}
	} else if m.editTargetMsg != nil {
		dynamicHelp = "[Enter] Save Edit · [Esc] Cancel Edit"
	} else {
		mediaHelp := ""
		if len(mediaIndices) > 1 {
			mediaHelp = fmt.Sprintf("[Alt+P] Preview (%d/%d) · [Alt+M] Media", m.selectedMediaIdx+1, len(mediaIndices))
		} else if len(mediaIndices) == 1 {
			mediaHelp = "[Alt+P] Preview Media"
		}

		dynamicHelp = mediaHelp
		if dynamicHelp != "" && scrollInfo != "" {
			dynamicHelp = dynamicHelp + " " + scrollInfo
		} else if dynamicHelp == "" && scrollInfo != "" {
			dynamicHelp = strings.TrimPrefix(scrollInfo, " · ")
		}
	}

	statusNotice := ""
	if m.promptOpenWith {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Open with: ") + m.openWithInput.View() + lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Render(" (Enter to open, Esc to cancel)")
	} else if m.confirmDocAction {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Document: Open [o], Open with [w], Save [s], or Choose location [S]? (Esc to cancel)")
	} else if m.confirmSave {
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387")).Render("Save? [y/s: default dir, S: choose location, n: cancel]")
	} else if m.confirmDelete {
		promptText := "Delete message for you? (y/N)"
		if len(m.pendingDeleteMultiMsgs) > 0 {
			promptText = fmt.Sprintf("Delete %d messages for you? (y/N)", len(m.pendingDeleteMultiMsgs))
			if m.pendingDeleteForEveryone {
				promptText = fmt.Sprintf("Delete %d messages for everyone? (y/N)", len(m.pendingDeleteMultiMsgs))
			}
		} else if m.pendingDeleteForEveryone {
			promptText = "Delete message for everyone? (y/N)"
		}
		statusNotice = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F38BA8")).Render(promptText)
	} else if m.previewStatus != "" {
		statusNotice = lipgloss.NewStyle().Foreground(lipgloss.Color("#89DCEB")).Render(m.previewStatus)
	}

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

	return lines
}

func (m *Model) renderDateDivider(t time.Time, width int) string {
	dateStr := t.Format("02/01/2006")
	label := dateStr
	now := time.Now()
	tLoc := t.Local()
	nowLoc := now.Local()
	if tLoc.Year() == nowLoc.Year() && tLoc.YearDay() == nowLoc.YearDay() {
		if width >= 34 {
			label = "Today · " + dateStr
		}
	} else {
		yesterday := nowLoc.AddDate(0, 0, -1)
		if tLoc.Year() == yesterday.Year() && tLoc.YearDay() == yesterday.YearDay() {
			if width >= 38 {
				label = "Yesterday · " + dateStr
			}
		}
	}

	text := " " + label + " "
	textW := lipgloss.Width(text)
	availW := max(textW+4, width-4)
	if availW <= textW+4 {
		leftPad := max(0, (width-textW)/2)
		return strings.Repeat(" ", leftPad) + statusStyle.Render(text)
	}

	lineW := (availW - textW) / 2
	leftRule := strings.Repeat("─", max(2, lineW))
	rightRule := strings.Repeat("─", max(2, availW-textW-len([]rune(leftRule))))

	styledText := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#A6ADC8")).Render(text)
	line := dividerStyle.Render(leftRule) + styledText + dividerStyle.Render(rightRule)
	totalW := lipgloss.Width(line)
	leftPad := max(0, (width-totalW)/2)
	return strings.Repeat(" ", leftPad) + line
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
	lines = append(lines, "")

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

func (m *Model) scrollToMessage(msgIdx int) {
	if msgIdx < 0 || msgIdx >= len(m.activeMsgs) {
		return
	}
	cw := m.contentWidth()
	msgWrapWidth := max(20, cw-6)
	wrapStyle := lipgloss.NewStyle().Width(msgWrapWidth)

	type msgSpan struct {
		start int
		end   int
	}
	spans := make([]msgSpan, len(m.activeMsgs))
	currentLine := 0
	var lastDayKey string
	for i, msg := range m.activeMsgs {
		if m.cfg.IsReactionsDisabled() && msg.Type == domain.MessageTypeReaction {
			continue
		}
		if !msg.Timestamp.IsZero() {
			dayKey := msg.Timestamp.Local().Format("2006-01-02")
			if dayKey != lastDayKey {
				lastDayKey = dayKey
				currentLine += 2
			}
		}
		start := currentLine
		currentLine += 1 // header
		if msg.QuotedText != "" || msg.QuotedID != "" {
			currentLine += 1 // quote preview
		}
		wrapped := wrapStyle.Render(m.formatMentions(msg.Body))
		currentLine += len(strings.Split(wrapped, "\n"))
		end := currentLine
		if i < len(m.activeMsgs)-1 {
			currentLine += 1 // spacing
		}
		spans[i] = msgSpan{start: start, end: end}
	}
	totalLines := currentLine
	inputLines := strings.Split(m.input.View(), "\n")
	extraH := 0
	if m.replyToMsg != nil {
		extraH = 1
	}
	availH := max(3, m.maxCanvasHeight()-5-len(inputLines)-extraH)

	if totalLines <= availH {
		m.chatScrollOffset = 0
		return
	}

	target := spans[msgIdx]
	currentEnd := totalLines - m.chatScrollOffset
	currentStart := max(0, currentEnd-availH)

	if target.start < currentStart {
		m.chatScrollOffset = totalLines - (target.start + availH)
		if m.chatScrollOffset > totalLines-availH {
			m.chatScrollOffset = totalLines - availH
		}
	} else if target.end > currentEnd {
		m.chatScrollOffset = totalLines - target.end
		if m.chatScrollOffset < 0 {
			m.chatScrollOffset = 0
		}
	}
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

func (m *Model) downloadAndSaveDocCmd(msg domain.Message, customDestDir ...string) tea.Cmd {
	m.previewStatus = "Downloading & saving document..."
	m.confirmSave = false
	destDir := ""
	if len(customDestDir) > 0 {
		destDir = customDestDir[0]
	} else {
		destDir = m.getDownloadDir()
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		destPath, err := m.saveToDownloads(filePath, destDir)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return docSavedMsg{DestPath: destPath}
	}
}

func (m *Model) downloadAndSaveMediaCmd(msg domain.Message, customDestDir ...string) tea.Cmd {
	m.previewStatus = fmt.Sprintf("Downloading & saving %s...", msg.Type)
	m.confirmSave = false
	destDir := ""
	if len(customDestDir) > 0 {
		destDir = customDestDir[0]
	} else {
		destDir = m.getDownloadDir()
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		destPath, err := m.saveToDownloads(filePath, destDir)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return docSavedMsg{DestPath: destPath}
	}
}

func (m *Model) downloadAndSaveMsgToDirCmd(msg domain.Message, destDir, preferredFile string) tea.Cmd {
	m.previewStatus = fmt.Sprintf("Downloading & saving %s...", msg.Type)
	m.confirmSave = false
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		destPath, err := saveFileToDir(filePath, destDir, preferredFile)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return docSavedMsg{DestPath: destPath}
	}
}

func (m *Model) isMsgMultiSelected(id string) bool {
	if m.multiSelectedMsgs == nil {
		return false
	}
	_, ok := m.multiSelectedMsgs[id]
	return ok
}

func (m *Model) getSelectedMsgsInOrder() []domain.Message {
	if len(m.multiSelectedMsgs) == 0 {
		return nil
	}
	var res []domain.Message
	for _, msg := range m.activeMsgs {
		if _, ok := m.multiSelectedMsgs[msg.ID]; ok {
			res = append(res, msg)
		}
	}
	return res
}

func (m *Model) saveMultiMessagesCmd(msgs []domain.Message) tea.Cmd {
	return m.saveMultiMessagesToDirCmd(msgs, m.getDownloadDir())
}

func (m *Model) saveMultiMessagesToDirCmd(msgs []domain.Message, destDir string) tea.Cmd {
	destDisplay := destDir
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(destDir, home) {
		destDisplay = "~" + strings.TrimPrefix(destDir, home)
	}
	m.previewStatus = fmt.Sprintf("Saving selected messages to %s...", destDisplay)
	chatName := m.activeName
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		var mediaMsgs []domain.Message
		var textMsgs []domain.Message
		for _, msg := range msgs {
			if msg.IsMedia() {
				mediaMsgs = append(mediaMsgs, msg)
			} else {
				textMsgs = append(textMsgs, msg)
			}
		}

		savedMediaCount := 0
		var firstErr error
		for _, mm := range mediaMsgs {
			dlPath, err := m.adapter.DownloadMedia(ctx, mm)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			_, err = saveFileToDir(dlPath, destDir, "")
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			savedMediaCount++
		}

		savedTextCount := 0
		var textFilePath string
		if len(textMsgs) > 0 {
			_ = os.MkdirAll(destDir, 0755)
			safeName := ""
			for _, r := range chatName {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
					safeName += string(r)
				}
			}
			if len(safeName) > 20 {
				safeName = safeName[:20]
			}
			fileName := fmt.Sprintf("messages_%s_%s.txt", safeName, time.Now().Format("20060102_150405"))
			if safeName == "" {
				fileName = fmt.Sprintf("messages_%s.txt", time.Now().Format("20060102_150405"))
			}
			textFilePath = filepath.Join(destDir, fileName)

			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("--- Watui Saved Messages: %s (%s) ---\n\n", chatName, time.Now().Format("02/01/2006 15:04:05")))
			for _, tm := range textMsgs {
				timeStr := tm.Timestamp.Format("02/01/2006 15:04:05")
				sender := "You"
				if !tm.IsFromMe {
					name := m.resolveMsgSenderName(tm)
					if name != "" && tm.Sender != "" && name != tm.Sender {
						sender = fmt.Sprintf("%s (%s)", name, tm.Sender)
					} else if name != "" {
						sender = name
					} else if tm.Sender != "" {
						sender = tm.Sender
					} else {
						sender = "Them"
					}
				}
				body := m.formatMentions(tm.Body)
				sb.WriteString(fmt.Sprintf("[%s] %s:\n%s\n\n", timeStr, sender, body))
			}

			if err := os.WriteFile(textFilePath, []byte(sb.String()), 0644); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				savedTextCount = len(textMsgs)
			}
		}

		return multiMessagesSavedMsg{
			MediaCount: savedMediaCount,
			TextCount:  savedTextCount,
			TextPath:   textFilePath,
			DestDir:    destDir,
			Err:        firstErr,
		}
	}
}

func (m *Model) getDownloadDir() string {
	if m.cfg != nil {
		if dir := m.cfg.GetDownloadDir(); dir != "" {
			return dir
		}
	}
	if xdg := os.Getenv("XDG_DOWNLOAD_DIR"); xdg != "" {
		return xdg
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, "Downloads")
	}
	return "."
}

func (m *Model) saveToDownloads(srcPath string, customDestDir ...string) (string, error) {
	destDir := ""
	if len(customDestDir) > 0 && strings.TrimSpace(customDestDir[0]) != "" {
		destDir = customDestDir[0]
	} else {
		destDir = m.getDownloadDir()
	}
	return saveFileToDir(srcPath, destDir, "")
}

func saveFileToDir(srcPath, destDir, preferredFile string) (string, error) {
	if srcPath == "" {
		return "", fmt.Errorf("no media file to save")
	}
	destDir = strings.TrimSpace(destDir)
	if destDir == "" {
		return "", fmt.Errorf("destination directory is empty")
	}
	destDir = config.ExpandHome(destDir)

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create destination directory: %w", err)
	}

	fileName := strings.TrimSpace(preferredFile)
	if fileName == "" {
		fileName = cleanCachedFileName(srcPath)
	}

	destPath := filepath.Join(destDir, fileName)

	// Avoid overwriting existing files
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(fileName)
		base := strings.TrimSuffix(fileName, ext)
		for i := 1; i < 1000; i++ {
			candidate := filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, i, ext))
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
		return "", fmt.Errorf("failed to write to destination: %w", err)
	}

	home, _ := os.UserHomeDir()
	displayPath := destPath
	if home != "" && strings.HasPrefix(destPath, home) {
		displayPath = "~" + strings.TrimPrefix(destPath, home)
	}

	return displayPath, nil
}

func cleanCachedFileName(fileName string) string {
	fileName = filepath.Base(fileName)
	if idx := strings.Index(fileName, "_"); idx != -1 && idx < len(fileName)-1 {
		prefix := fileName[:idx]
		if len(prefix) >= 8 {
			fileName = fileName[idx+1:]
		}
	}
	return fileName
}

func getTargetFileName(target saveTargetData) string {
	if target.Mode == "cached_file" && target.CachedPath != "" {
		return cleanCachedFileName(target.CachedPath)
	}
	if target.Mode == "single_msg" {
		msg := target.Message
		if msg.Type == domain.MessageTypeDocument {
			if strings.HasPrefix(msg.Body, "[Document: ") && strings.HasSuffix(msg.Body, "]") {
				fn := strings.TrimSpace(msg.Body[len("[Document: ") : len(msg.Body)-1])
				if fn != "" {
					return filepath.Base(fn)
				}
			}
		}
		safeID := strings.ReplaceAll(msg.ID, "/", "_")
		if cacheDir, err := media.GetMediaCacheDir(); err == nil {
			if matches, _ := filepath.Glob(filepath.Join(cacheDir, safeID+"*")); len(matches) > 0 {
				return cleanCachedFileName(filepath.Base(matches[0]))
			}
		}
		shortID := safeID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		switch msg.Type {
		case domain.MessageTypeImage:
			return fmt.Sprintf("image_%s.jpg", shortID)
		case domain.MessageTypeVideo:
			return fmt.Sprintf("video_%s.mp4", shortID)
		case domain.MessageTypeAudio:
			return fmt.Sprintf("audio_%s.ogg", shortID)
		case domain.MessageTypeSticker:
			return fmt.Sprintf("sticker_%s.webp", shortID)
		default:
			return fmt.Sprintf("document_%s.bin", shortID)
		}
	}
	return ""
}

func saveFileToPath(srcPath, destPath string) (string, error) {
	if srcPath == "" {
		return "", fmt.Errorf("no media file to save")
	}
	destPath = strings.TrimSpace(destPath)
	if destPath == "" {
		return "", fmt.Errorf("destination path is empty")
	}
	destPath = config.ExpandHome(destPath)

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", fmt.Errorf("cannot create destination directory: %w", err)
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("failed to read cached file: %w", err)
	}

	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write to destination: %w", err)
	}

	home, _ := os.UserHomeDir()
	displayPath := destPath
	if home != "" && strings.HasPrefix(destPath, home) {
		displayPath = "~" + strings.TrimPrefix(destPath, home)
	}

	return displayPath, nil
}

func (m *Model) downloadAndSaveMsgToPathCmd(msg domain.Message, destPath string) tea.Cmd {
	m.previewStatus = fmt.Sprintf("Downloading & saving %s...", msg.Type)
	m.confirmSave = false
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		filePath, err := m.adapter.DownloadMedia(ctx, msg)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		savedPath, err := saveFileToPath(filePath, destPath)
		if err != nil {
			return mediaPreviewErrMsg{Err: err}
		}
		return docSavedMsg{DestPath: savedPath}
	}
}

func createSavePlaceholderFile(startDir, fileName string) (string, error) {
	if startDir == "" {
		startDir = "."
	}
	startDir = config.ExpandHome(startDir)
	if err := os.MkdirAll(startDir, 0755); err != nil {
		return "", err
	}
	if fileName == "" {
		fileName = "download"
	}
	candidatePath := filepath.Join(startDir, fileName)
	if _, err := os.Stat(candidatePath); err == nil {
		ext := filepath.Ext(fileName)
		base := strings.TrimSuffix(fileName, ext)
		for i := 1; i < 1000; i++ {
			c := filepath.Join(startDir, fmt.Sprintf("%s_%d%s", base, i, ext))
			if _, err := os.Stat(c); os.IsNotExist(err) {
				candidatePath = c
				break
			}
		}
	}

	instructions := `* watui save instructions *
---------------------------------------------------
			!!! WARNING !!!
Opening a file OVERWRITES it with the saved media!
How to save:
1) Move this file to your desired location (e.g. 'x' then 'p' in yazi).
2) Rename the file if needed (e.g. 'r' in yazi).
3) Press Enter to confirm saving to this file.
Tips:
- Press Enter on directories to navigate into them normally.
- If you quit ('q') without opening a file, saving is cancelled.
---------------------------------------------------
`
	if err := os.WriteFile(candidatePath, []byte(instructions), 0644); err != nil {
		return "", err
	}
	return candidatePath, nil
}

func buildTerminalSavePickerCmd(pickerType, customCmd, suggestedPath string, isDirectory bool) (*exec.Cmd, string, string, error) {
	tmpFile, err := os.CreateTemp("", "watui_picker_out_*")
	if err != nil {
		return nil, "", "", err
	}
	outPath := tmpFile.Name()
	_ = tmpFile.Close()

	var cwdPath string
	if isDirectory {
		cwdTmp, err := os.CreateTemp("", "watui_picker_cwd_*")
		if err == nil {
			cwdPath = cwdTmp.Name()
			_ = cwdTmp.Close()
		}
	}

	// Check if customCmd is a termfilechooser wrapper script
	isWrapper := strings.HasSuffix(customCmd, ".sh") || strings.Contains(customCmd, "wrapper") || strings.Contains(customCmd, "termfilechooser")
	if isWrapper && customCmd != "" {
		multiple := "0"
		dirFlag := "0"
		saveFlag := "1"
		if isDirectory {
			dirFlag = "1"
			saveFlag = "0"
		}
		cmd := exec.Command("sh", "-c", customCmd+` "$1" "$2" "$3" "$4" "$5" "$6"`, "--", multiple, dirFlag, saveFlag, suggestedPath, outPath, "0")
		return cmd, outPath, cwdPath, nil
	}

	switch pickerType {
	case "yazi":
		args := []string{"--chooser-file=" + outPath}
		if isDirectory && cwdPath != "" {
			args = append(args, "--cwd-file="+cwdPath)
		}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "--chooser-file") || strings.HasPrefix(w, "--cwd-file") {
					continue
				}
				args = append(args, w)
			}
		}
		if suggestedPath != "" {
			args = append(args, suggestedPath)
		}
		return exec.Command("yazi", args...), outPath, cwdPath, nil

	case "ranger":
		args := []string{}
		if isDirectory {
			args = append(args, "--choosedir="+outPath)
		} else {
			args = append(args, "--choosefile="+outPath)
			if suggestedPath != "" {
				args = append(args, "--selectfile="+suggestedPath)
			}
		}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "--choosefile") || strings.HasPrefix(w, "--choosedir") || strings.HasPrefix(w, "--selectfile") {
					continue
				}
				args = append(args, w)
			}
		}
		if isDirectory && suggestedPath != "" {
			args = append(args, suggestedPath)
		}
		return exec.Command("ranger", args...), outPath, cwdPath, nil

	case "lf":
		args := []string{"-selection-path=" + outPath}
		if isDirectory && cwdPath != "" {
			args = append(args, "-last-dir-path="+cwdPath)
		}
		if customCmd != "" {
			parts := strings.Split(customCmd, "&&")
			firstPart := strings.TrimSpace(parts[0])
			words := strings.Fields(firstPart)
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "-selection-path") || strings.HasPrefix(w, "-last-dir-path") {
					continue
				}
				args = append(args, w)
			}
		}
		if suggestedPath != "" {
			args = append(args, suggestedPath)
		}
		return exec.Command("lf", args...), outPath, cwdPath, nil

	case "nnn":
		args := []string{"-p", outPath}
		if suggestedPath != "" {
			args = append(args, suggestedPath)
		}
		return exec.Command("nnn", args...), outPath, cwdPath, nil

	case "fzf":
		return exec.Command("sh", "-c", `fzf > "$1"`, "--", outPath), outPath, cwdPath, nil

	default:
		cmd := exec.Command("sh", "-c", customCmd+` > "$1"`, "--", outPath)
		cmd.Env = append(os.Environ(), "WATUI_PICKER_FILE="+outPath)
		return cmd, outPath, cwdPath, nil
	}
}

func (m *Model) pickSaveLocationCmd(target saveTargetData) tea.Cmd {
	customCmd := ""
	if m.cfg != nil {
		customCmd = m.cfg.GetFilePickerCommand()
	}
	startDir := m.getDownloadDir()
	isDirectory := (target.Mode == "multi_msgs")

	suggestedPath := startDir
	if !isDirectory {
		fileName := getTargetFileName(target)
		candidatePath, err := createSavePlaceholderFile(startDir, fileName)
		if err == nil {
			target.PlaceholderPath = candidatePath
			suggestedPath = candidatePath
		}
	}

	pickerType, isTerm := resolvePicker(customCmd)
	if isTerm {
		execCmd, outPath, cwdPath, err := buildTerminalSavePickerCmd(pickerType, customCmd, suggestedPath, isDirectory)
		if err != nil {
			if target.PlaceholderPath != "" {
				_ = os.Remove(target.PlaceholderPath)
			}
			return func() tea.Msg {
				return filePickErrMsg{Err: err}
			}
		}
		if startDir != "" && isDirectory {
			execCmd.Dir = startDir
		}
		return tea.ExecProcess(execCmd, func(err error) tea.Msg {
			defer func() {
				_ = os.Remove(outPath)
				if cwdPath != "" {
					_ = os.Remove(cwdPath)
				}
			}()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					if target.PlaceholderPath != "" {
						_ = os.Remove(target.PlaceholderPath)
					}
					return saveLocationPickedMsg{ChosenPath: "", SaveTarget: target}
				}
				data, readErr := os.ReadFile(outPath)
				if readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
					selected := strings.TrimSpace(string(data))
					if idx := strings.Index(selected, "\n"); idx != -1 {
						selected = strings.TrimSpace(selected[:idx])
					}
					return saveLocationPickedMsg{ChosenPath: selected, SaveTarget: target}
				}
				if target.PlaceholderPath != "" {
					_ = os.Remove(target.PlaceholderPath)
				}
				return filePickErrMsg{Err: err}
			}
			data, readErr := os.ReadFile(outPath)
			selected := ""
			if readErr == nil {
				selected = strings.TrimSpace(string(data))
				if idx := strings.Index(selected, "\n"); idx != -1 {
					selected = strings.TrimSpace(selected[:idx])
				}
			}
			if selected == "" && isDirectory && cwdPath != "" {
				cwdData, cwdErr := os.ReadFile(cwdPath)
				if cwdErr == nil {
					selected = strings.TrimSpace(string(cwdData))
					if idx := strings.Index(selected, "\n"); idx != -1 {
						selected = strings.TrimSpace(selected[:idx])
					}
				}
			}
			if selected == "" {
				if target.PlaceholderPath != "" {
					_ = os.Remove(target.PlaceholderPath)
				}
				return saveLocationPickedMsg{ChosenPath: "", SaveTarget: target}
			}
			return saveLocationPickedMsg{ChosenPath: selected, SaveTarget: target}
		})
	}

	return func() tea.Msg {
		path, err := openSavePicker(customCmd, suggestedPath, isDirectory)
		if err != nil {
			if target.PlaceholderPath != "" {
				_ = os.Remove(target.PlaceholderPath)
			}
			return filePickErrMsg{Err: err}
		}
		if path == "" && target.PlaceholderPath != "" {
			_ = os.Remove(target.PlaceholderPath)
		}
		return saveLocationPickedMsg{ChosenPath: path, SaveTarget: target}
	}
}

func openSavePicker(customCmd, suggestedPath string, isDirectory bool) (string, error) {
	startDir := suggestedPath
	if !isDirectory && suggestedPath != "" {
		startDir = filepath.Dir(suggestedPath)
	}

	if strings.TrimSpace(customCmd) != "" {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", customCmd)
		} else {
			cmd = exec.Command("sh", "-c", customCmd)
		}
		if startDir != "" {
			cmd.Dir = startDir
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
		if isDirectory {
			cmd := exec.Command("powershell", "-NoProfile", "-Command",
				"[System.Reflection.Assembly]::LoadWithPartialName('System.windows.forms') | Out-Null; $f = New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description = 'Select download location'; if ($f.ShowDialog() -eq 'OK') { $f.SelectedPath }")
			out, err := cmd.Output()
			if err != nil {
				return "", errNoFilePicker
			}
			return strings.TrimSpace(string(out)), nil
		}
		escPath := strings.ReplaceAll(suggestedPath, "'", "''")
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("[System.Reflection.Assembly]::LoadWithPartialName('System.windows.forms') | Out-Null; $f = New-Object System.Windows.Forms.SaveFileDialog; $f.Title = 'Save file'; $f.FileName = '%s'; if ($f.ShowDialog() -eq 'OK') { $f.FileName }", escPath))
		out, err := cmd.Output()
		if err != nil {
			return "", errNoFilePicker
		}
		return strings.TrimSpace(string(out)), nil

	case "darwin":
		if isDirectory {
			cmd := exec.Command("osascript", "-e", "POSIX path of (choose folder with prompt \"Select download location:\")")
			out, err := cmd.Output()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					return "", nil
				}
				return "", errNoFilePicker
			}
			return strings.TrimSpace(string(out)), nil
		}
		fileName := filepath.Base(suggestedPath)
		dirName := filepath.Dir(suggestedPath)
		script := fmt.Sprintf("POSIX path of (choose file name with prompt \"Save as:\" default name \"%s\" default location \"%s\")", fileName, dirName)
		cmd := exec.Command("osascript", "-e", script)
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
			var args []string
			if isDirectory {
				args = []string{"--file-selection", "--directory", "--title=Select download location"}
			} else {
				args = []string{"--file-selection", "--save", "--confirm-overwrite", "--title=Save File"}
				if suggestedPath != "" {
					args = append(args, "--filename="+suggestedPath)
				}
			}
			cmd := exec.Command("zenity", args...)
			if startDir != "" {
				cmd.Dir = startDir
			}
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
			var args []string
			if isDirectory {
				args = []string{"--getexistingdirectory", "--title", "Select download location"}
				if suggestedPath != "" {
					args = append(args, suggestedPath)
				}
			} else {
				args = []string{"--getsavefilename"}
				if suggestedPath != "" {
					args = append(args, suggestedPath)
				}
				args = append(args, "--title", "Save File")
			}
			cmd := exec.Command("kdialog", args...)
			if startDir != "" {
				cmd.Dir = startDir
			}
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
			var pyScript string
			if isDirectory {
				pyScript = "import tkinter as tk, tkinter.filedialog as fd; root = tk.Tk(); root.withdraw(); print(fd.askdirectory() or '')"
			} else {
				pyScript = fmt.Sprintf("import tkinter as tk, tkinter.filedialog as fd; root = tk.Tk(); root.withdraw(); print(fd.asksaveasfilename(initialfile='%s', initialdir='%s') or '')", filepath.Base(suggestedPath), startDir)
			}
			cmd := exec.Command("python3", "-c", pyScript)
			if startDir != "" {
				cmd.Dir = startDir
			}
			out, err := cmd.Output()
			if err == nil {
				return strings.TrimSpace(string(out)), nil
			}
		}
		return "", errNoFilePicker
	}
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

type clipboardPasteMsg struct {
	Item *ClipboardItem
	Err  error
}

func (m *Model) pasteClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		item, err := readClipboardFunc(ctx)
		return clipboardPasteMsg{Item: item, Err: err}
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

// ParseFileURI extracts the local file path and optional caption from a file:// URI.
func ParseFileURI(input string) (string, string) {
	return parseFileURI(input)
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

func (m *Model) updateMentionHints() {
	isGroup := strings.Contains(m.activeChatID, "@g.us")
	if !isGroup {
		if chat, ok := m.unreadChats[m.activeChatID]; ok && chat != nil && chat.IsGroup {
			isGroup = true
		}
	}
	if m.view != ViewChat || !isGroup {
		m.mentionHints = nil
		m.mentionCursor = 0
		return
	}
	val := m.input.Value()
	lastAt := strings.LastIndex(val, "@")
	if lastAt == -1 {
		m.mentionHints = nil
		m.mentionCursor = 0
		return
	}
	if lastAt > 0 {
		prev := rune(val[lastAt-1])
		if !unicode.IsSpace(prev) && prev != '(' && prev != '[' && prev != '{' {
			m.mentionHints = nil
			m.mentionCursor = 0
			return
		}
	}
	query := val[lastAt+1:]
	if strings.ContainsAny(query, " \n\r\t") {
		m.mentionHints = nil
		m.mentionCursor = 0
		return
	}
	q := strings.ToLower(strings.TrimSpace(query))

	// Collect pool of candidates: groupParticipants + senders from activeMsgs
	var candidates []domain.Contact
	seen := make(map[string]bool)
	for _, p := range m.groupParticipants {
		if p.Name == "" || p.Name == "You" {
			continue
		}
		if !seen[p.JID] {
			seen[p.JID] = true
			candidates = append(candidates, p)
		}
	}
	for _, msg := range m.activeMsgs {
		if msg.IsFromMe {
			continue
		}
		name := m.resolveMsgSenderName(msg)
		if name == "" || name == "You" || name == "Them" || isRawJID(name) {
			continue
		}
		if !seen[msg.Sender] {
			seen[msg.Sender] = true
			candidates = append(candidates, domain.Contact{
				JID:  msg.Sender,
				Name: name,
			})
		}
	}

	var matches []domain.Contact
	for _, c := range candidates {
		if q == "" || strings.Contains(strings.ToLower(c.Name), q) || strings.Contains(strings.ToLower(c.PushName), q) || strings.Contains(c.JID, q) {
			matches = append(matches, c)
		}
	}

	m.mentionHints = matches
	if m.mentionCursor >= len(matches) {
		m.mentionCursor = 0
	}
}

func (m *Model) applyMentionHint() {
	if len(m.mentionHints) == 0 || m.mentionCursor >= len(m.mentionHints) {
		return
	}
	c := m.mentionHints[m.mentionCursor]
	val := m.input.Value()
	lastAt := strings.LastIndex(val, "@")
	if lastAt == -1 {
		return
	}
	tag := "@" + c.Name + " "
	newVal := val[:lastAt] + tag
	m.input.SetValue(newVal)
	m.input.CursorEnd()
	m.mentionHints = nil
	m.mentionCursor = 0
}
