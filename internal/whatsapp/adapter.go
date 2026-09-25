package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"watui/internal/domain"
)

// Config holds configuration for the WhatsApp adapter.
type Config struct {
	DBPath   string
	LogFile  string
	LogLevel string
}

// Adapter implements domain.WhatsAppAdapter using whatsmeow.
// It completely encapsulates all WhatsApp-specific protocol, crypto, and wire details.
type Adapter struct {
	client    *whatsmeow.Client
	container *sqlstore.Container
	localDB   *sql.DB
	config    Config

	mu              sync.RWMutex
	messageHandlers []domain.MessageHandler
	statusHandlers  []domain.StatusHandler
	dismissHandlers []func(chatID string)
	currentStatus   domain.ConnectionStatus

	contactsMu       sync.RWMutex
	cachedContacts   []domain.Contact
	contactsLoaded   bool
	contactsHandlers []func([]domain.Contact)
	refreshMu        sync.Mutex
	isRefreshing     bool
}

// NewAdapter creates and initializes a WhatsApp adapter instance.
func NewAdapter(ctx context.Context, cfg Config) (*Adapter, error) {
	if cfg.DBPath == "" {
		cfg.DBPath = "watui.db"
	}
	if cfg.LogFile == "" {
		cfg.LogFile = "watui.log"
	}

	dbLog, err := NewFileLogger(cfg.LogFile, "Database", cfg.LogLevel)
	if err != nil {
		dbLog = waLog.Noop
	}
	clientLog, err := NewFileLogger(cfg.LogFile, "Client", cfg.LogLevel)
	if err != nil {
		clientLog = waLog.Noop
	}

	container, err := sqlstore.New(ctx, "sqlite3", "file:"+cfg.DBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, clientLog)

	localDB, _ := sql.Open("sqlite3", "file:"+cfg.DBPath+"?_foreign_keys=on")
	if localDB != nil {
		_, _ = localDB.Exec(`CREATE TABLE IF NOT EXISTS watui_groups (
			jid TEXT PRIMARY KEY,
			name TEXT
		);
		CREATE TABLE IF NOT EXISTS watui_unread_messages (
			id TEXT PRIMARY KEY,
			chat_id TEXT NOT NULL,
			sender TEXT NOT NULL,
			sender_name TEXT,
			timestamp INTEGER NOT NULL,
			body TEXT,
			type TEXT,
			is_from_me BOOLEAN
		);`)
	}

	adapter := &Adapter{
		client:        client,
		container:     container,
		localDB:       localDB,
		config:        cfg,
		currentStatus: domain.StatusDisconnected,
	}

	client.AddEventHandler(adapter.handleEvent)

	return adapter, nil
}

// IsLoggedIn checks whether valid session credentials exist in SQLite.
func (a *Adapter) IsLoggedIn() bool {
	return a.client.Store.ID != nil
}

// OnMessage registers an incoming message handler.
func (a *Adapter) OnMessage(handler domain.MessageHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messageHandlers = append(a.messageHandlers, handler)
}

// OnStatus registers a connection status handler.
func (a *Adapter) OnStatus(handler domain.StatusHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statusHandlers = append(a.statusHandlers, handler)
}

func (a *Adapter) setStatus(status domain.ConnectionStatus) {
	a.mu.Lock()
	a.currentStatus = status
	handlers := append([]domain.StatusHandler(nil), a.statusHandlers...)
	a.mu.Unlock()

	for _, h := range handlers {
		h(status)
	}
}

// Connect starts the WebSocket and completes authentication or QR pairing.
func (a *Adapter) Connect(ctx context.Context) error {
	a.setStatus(domain.StatusConnecting)

	if a.client.Store.ID == nil {
		// Device is not paired yet. Request QR channel.
		qrChan, err := a.client.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("failed to get QR channel: %w", err)
		}

		err = a.client.Connect()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		a.setStatus(domain.StatusWaitingQR)

		// Handle pairing loop in background
		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					a.renderQRInTerminal(evt.Code)
				case "success":
					fmt.Println("\n Successfully authenticated with WhatsApp!")
					a.setStatus(domain.StatusConnected)
				case "timeout":
					fmt.Println("\n QR code pairing timed out. Restart watui to try again.")
					a.setStatus(domain.StatusDisconnected)
				}
			}
		}()
	} else {
		// Session already exists in SQLite, connect directly
		err := a.client.Connect()
		if err != nil {
			return fmt.Errorf("failed to connect with existing session: %w", err)
		}
	}

	return nil
}

// Disconnect gracefully shuts down the connection.
func (a *Adapter) Disconnect() {
	if a.client != nil {
		a.client.Disconnect()
	}
	if a.localDB != nil {
		_ = a.localDB.Close()
	}
	a.setStatus(domain.StatusDisconnected)
}

// NormalizeJID parses or normalizes a raw chat target (full JID, phone number with +, etc.)
func NormalizeJID(chatID string) (types.JID, error) {
	chatID = strings.TrimSpace(chatID)
	if strings.Contains(chatID, "@") {
		return types.ParseJID(chatID)
	}

	var digits strings.Builder
	for _, r := range chatID {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	num := digits.String()
	if num == "" {
		return types.JID{}, fmt.Errorf("invalid empty phone number %q", chatID)
	}
	return types.NewJID(num, types.DefaultUserServer), nil
}

// SendTextMessage sends a basic text message to a WhatsApp chat JID or raw phone number.
func (a *Adapter) SendTextMessage(ctx context.Context, chatID string, text string) (domain.Message, error) {
	recipientJID, err := NormalizeJID(chatID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	msg := &waE2E.Message{
		Conversation: proto.String(text),
	}

	resp, err := a.client.SendMessage(ctx, recipientJID, msg)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to send message: %w", err)
	}

	senderID := ""
	if a.client.Store.ID != nil {
		senderID = a.client.Store.ID.ToNonAD().String()
	}

	return domain.Message{
		ID:         resp.ID,
		ChatID:     recipientJID.String(),
		Sender:     senderID,
		SenderName: "Me",
		Timestamp:  resp.Timestamp,
		IsFromMe:   true,
		Type:       domain.MessageTypeText,
		Body:       text,
		Status:     domain.MessageStatusSent,
	}, nil
}

// GetContacts retrieves contacts from in-memory cache if available, or falls back to fast local SQLite.
func (a *Adapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	a.contactsMu.RLock()
	if a.contactsLoaded && len(a.cachedContacts) > 0 {
		res := make([]domain.Contact, len(a.cachedContacts))
		copy(res, a.cachedContacts)
		a.contactsMu.RUnlock()
		return res, nil
	}
	a.contactsMu.RUnlock()

	// If not preloaded yet, do an instant local SQLite read (contacts + cached groups)
	contacts := a.fetchLocalContacts(ctx)

	// Refresh cache in background (including network groups from WhatsApp)
	go a.refreshContactsCache(context.Background())

	return contacts, nil
}

func (a *Adapter) getLocalGroups() []domain.Contact {
	if a.localDB == nil {
		return nil
	}
	rows, err := a.localDB.Query("SELECT jid, name FROM watui_groups ORDER BY name ASC")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var groups []domain.Contact
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err == nil {
			groups = append(groups, domain.Contact{
				JID:     jid,
				Name:    name,
				IsGroup: true,
			})
		}
	}
	return groups
}

func (a *Adapter) saveLocalGroups(groups []*types.GroupInfo) {
	if a.localDB == nil || len(groups) == 0 {
		return
	}
	tx, err := a.localDB.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO watui_groups (jid, name) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, g := range groups {
		name := strings.TrimSpace(g.GroupName.Name)
		if name == "" {
			name = "Group (" + g.JID.User + ")"
		}
		_, _ = stmt.Exec(g.JID.String(), name)
	}
	_ = tx.Commit()
}

func (a *Adapter) fetchLocalContacts(ctx context.Context) []domain.Contact {
	var list []domain.Contact

	// 1. Instantly read cached groups from local SQLite
	list = append(list, a.getLocalGroups()...)

	// 2. Read contacts from whatsmeow SQLite store
	if a.client != nil && a.client.Store != nil && a.client.Store.Contacts != nil {
		rawMap, err := a.client.Store.Contacts.GetAllContacts(ctx)
		if err == nil {
			for jid, info := range rawMap {
				if jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer {
					continue
				}
				name := strings.TrimSpace(info.FullName)
				if name == "" {
					name = strings.TrimSpace(info.BusinessName)
				}
				if name == "" {
					name = strings.TrimSpace(info.PushName)
				}
				if name == "" {
					name = jid.User
				}
				list = append(list, domain.Contact{
					JID:          jid.String(),
					Name:         name,
					PushName:     info.PushName,
					BusinessName: info.BusinessName,
					IsGroup:      jid.Server == types.GroupServer,
				})
			}
		}
	}

	sortContacts(list)
	return list
}

func (a *Adapter) refreshContactsCache(ctx context.Context) {
	if a.client == nil {
		return
	}

	a.refreshMu.Lock()
	if a.isRefreshing {
		a.refreshMu.Unlock()
		return
	}
	a.isRefreshing = true
	a.refreshMu.Unlock()

	defer func() {
		a.refreshMu.Lock()
		a.isRefreshing = false
		a.refreshMu.Unlock()
	}()

	// Wait up to 10 seconds for socket connection to complete
	if !a.client.WaitForConnection(10 * time.Second) {
		return
	}

	// 1. Fetch joined groups from WhatsApp over WebSocket
	groups, err := a.client.GetJoinedGroups(ctx)
	if err == nil && len(groups) > 0 {
		a.saveLocalGroups(groups)
	}

	// 2. Re-read combined local list
	combined := a.fetchLocalContacts(ctx)

	// Deduplicate by JID
	seen := make(map[string]bool)
	var deduped []domain.Contact
	for _, c := range combined {
		if !seen[c.JID] {
			seen[c.JID] = true
			deduped = append(deduped, c)
		}
	}

	sortContacts(deduped)

	a.contactsMu.Lock()
	a.cachedContacts = deduped
	a.contactsLoaded = true
	var handlers []func([]domain.Contact)
	if len(a.contactsHandlers) > 0 {
		handlers = make([]func([]domain.Contact), len(a.contactsHandlers))
		copy(handlers, a.contactsHandlers)
	}
	a.contactsMu.Unlock()

	for _, h := range handlers {
		h(deduped)
	}
}

// OnContactsUpdated registers a callback triggered whenever contacts or groups are updated in background.
func (a *Adapter) OnContactsUpdated(handler func([]domain.Contact)) {
	a.contactsMu.Lock()
	a.contactsHandlers = append(a.contactsHandlers, handler)
	var cached []domain.Contact
	if a.contactsLoaded && len(a.cachedContacts) > 0 {
		cached = make([]domain.Contact, len(a.cachedContacts))
		copy(cached, a.cachedContacts)
	}
	a.contactsMu.Unlock()

	if len(cached) > 0 {
		go handler(cached)
	}
}

func sortContacts(list []domain.Contact) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsGroup != list[j].IsGroup {
			return list[i].IsGroup
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
}

// MarkRead sends a read receipt to WhatsApp for the given message IDs and clears from unread persistence.
func (a *Adapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
	_ = a.DismissUnread(ctx, chatID)

	if len(messageIDs) == 0 || a.client == nil {
		return nil
	}

	cJID, err := NormalizeJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid chat JID %q: %w", chatID, err)
	}

	sJID := cJID
	if senderID != "" && senderID != chatID {
		if parsed, err := NormalizeJID(senderID); err == nil {
			sJID = parsed
		}
	}

	waMsgIDs := make([]types.MessageID, len(messageIDs))
	for i, id := range messageIDs {
		waMsgIDs[i] = types.MessageID(id)
	}

	return a.client.MarkRead(ctx, waMsgIDs, time.Now(), cJID, sJID)
}

// OnChatDismissed registers a listener triggered when a chat has been marked read remotely.
func (a *Adapter) OnChatDismissed(handler func(chatID string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dismissHandlers = append(a.dismissHandlers, handler)
}

func (a *Adapter) notifyChatDismissed(chatID string) {
	if chatID == "" {
		return
	}
	a.mu.RLock()
	handlers := make([]func(chatID string), len(a.dismissHandlers))
	copy(handlers, a.dismissHandlers)
	a.mu.RUnlock()

	for _, h := range handlers {
		h(chatID)
	}
}

func (a *Adapter) notifyMessage(msg domain.Message) {
	a.mu.RLock()
	handlers := make([]domain.MessageHandler, len(a.messageHandlers))
	copy(handlers, a.messageHandlers)
	a.mu.RUnlock()

	for _, h := range handlers {
		h(msg)
	}
}

// Sync explicitly pulls server updates (such as read state mutations) from WhatsApp.
func (a *Adapter) Sync(ctx context.Context) error {
	if a.client == nil || !a.client.IsConnected() {
		return nil
	}
	// Fetch regular_low (contains read/unread statuses) and regular_high
	_ = a.client.FetchAppState(ctx, appstate.WAPatchRegularLow, false, false)
	_ = a.client.FetchAppState(ctx, appstate.WAPatchRegularHigh, false, false)
	return nil
}

func (a *Adapter) saveUnreadMessage(msg domain.Message) {
	if a.localDB == nil {
		return
	}
	_, _ = a.localDB.Exec(`INSERT OR REPLACE INTO watui_unread_messages
		(id, chat_id, sender, sender_name, timestamp, body, type, is_from_me)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChatID, msg.Sender, msg.SenderName, msg.Timestamp.Unix(), msg.Body, string(msg.Type), msg.IsFromMe,
	)
}

func (a *Adapter) isChatUnread(chatID string) bool {
	if a.localDB == nil || chatID == "" {
		return false
	}
	var count int
	_ = a.localDB.QueryRow("SELECT COUNT(*) FROM watui_unread_messages WHERE chat_id = ?", chatID).Scan(&count)
	return count > 0
}

func (a *Adapter) deleteUnreadMessageIDs(ids []types.MessageID) []string {
	if a.localDB == nil || len(ids) == 0 {
		return nil
	}
	var affectedChats []string
	for _, id := range ids {
		var chatID string
		row := a.localDB.QueryRow("SELECT chat_id FROM watui_unread_messages WHERE id = ?", string(id))
		if err := row.Scan(&chatID); err == nil && chatID != "" {
			affectedChats = append(affectedChats, chatID)
		}
		_, _ = a.localDB.Exec("DELETE FROM watui_unread_messages WHERE id = ?", string(id))
	}
	return affectedChats
}

// GetUnreadMessages returns all unread messages persisted in local SQLite.
func (a *Adapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	if a.localDB == nil {
		return nil, nil
	}
	rows, err := a.localDB.QueryContext(ctx, `SELECT id, chat_id, sender, sender_name, timestamp, body, type, is_from_me
		FROM watui_unread_messages ORDER BY timestamp ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []domain.Message
	for rows.Next() {
		var m domain.Message
		var ts int64
		var tStr string
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Sender, &m.SenderName, &ts, &m.Body, &tStr, &m.IsFromMe); err == nil {
			m.Timestamp = time.Unix(ts, 0)
			m.Type = domain.MessageType(tStr)
			m.Status = domain.MessageStatusDelivered
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

// DismissUnread removes unread messages for a given chat from local SQLite.
func (a *Adapter) DismissUnread(ctx context.Context, chatID string) error {
	if a.localDB == nil {
		return nil
	}
	nonAD := chatID
	if parsed, err := NormalizeJID(chatID); err == nil {
		nonAD = parsed.ToNonAD().String()
	}
	_, err := a.localDB.ExecContext(ctx, "DELETE FROM watui_unread_messages WHERE chat_id = ? OR chat_id = ?", chatID, nonAD)
	return err
}

// handleEvent processes internal whatsmeow events and maps them to clean domain models.
func (a *Adapter) handleEvent(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		a.setStatus(domain.StatusConnected)
		go a.refreshContactsCache(context.Background())
		go func() {
			// Catch up on any app state mutations (e.g. chats read on other devices)
			_ = a.client.FetchAppState(context.Background(), appstate.WAPatchRegularLow, false, false)
		}()

	case *events.LoggedOut:
		a.setStatus(domain.StatusLoggedOut)

	case *events.Receipt:
		// When read on phone or companion, delete those messages from unread persistence
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf {
			affectedChats := a.deleteUnreadMessageIDs(evt.MessageIDs)
			for _, chatID := range affectedChats {
				if !a.isChatUnread(chatID) {
					a.notifyChatDismissed(chatID)
				}
			}
			if !evt.Chat.IsEmpty() {
				chatID := evt.Chat.ToNonAD().String()
				if !a.isChatUnread(chatID) {
					a.notifyChatDismissed(chatID)
				}
			}
		}

	case *events.MarkChatAsRead:
		if evt.Action != nil && evt.Action.GetRead() {
			chatID := evt.JID.ToNonAD().String()
			_ = a.DismissUnread(context.Background(), chatID)
			_ = a.DismissUnread(context.Background(), evt.JID.String())
			a.notifyChatDismissed(chatID)
			a.notifyChatDismissed(evt.JID.String())
		}

	case *events.HistorySync:
		if evt.Data == nil {
			return
		}
		for _, conv := range evt.Data.GetConversations() {
			if conv == nil || conv.ID == nil {
				continue
			}
			rawJID := *conv.ID
			parsedJID, err := types.ParseJID(rawJID)
			var chatID string
			if err == nil {
				chatID = parsedJID.ToNonAD().String()
			} else {
				chatID = rawJID
			}

			if conv.GetUnreadCount() == 0 {
				_ = a.DismissUnread(context.Background(), chatID)
				_ = a.DismissUnread(context.Background(), rawJID)
				a.notifyChatDismissed(chatID)
				a.notifyChatDismissed(rawJID)
				continue
			}

			// If unread, process messages
			for _, hMsg := range conv.GetMessages() {
				if hMsg == nil || hMsg.Message == nil {
					continue
				}
				webMsg := hMsg.Message
				if webMsg.Key == nil || webMsg.Key.GetFromMe() {
					continue
				}
				domainMsg, ok := a.extractWebMessage(chatID, webMsg)
				if ok {
					a.saveUnreadMessage(domainMsg)
					a.notifyMessage(domainMsg)
				}
			}
		}

	case *events.Message:
		domainMsg, ok := a.extractDomainMessage(evt)
		if !ok {
			// Internal protocol, sender-key-distribution, or empty control message; do not leak to UI
			return
		}

		if !domainMsg.IsFromMe {
			a.saveUnreadMessage(domainMsg)
		}

		a.notifyMessage(domainMsg)
	}
}

func extractMessageContent(m *waE2E.Message) (string, domain.MessageType, bool) {
	if m == nil {
		return "", domain.MessageTypeText, false
	}

	if m.Conversation != nil && *m.Conversation != "" {
		return *m.Conversation, domain.MessageTypeText, true
	} else if m.ExtendedTextMessage != nil && m.ExtendedTextMessage.Text != nil && *m.ExtendedTextMessage.Text != "" {
		return *m.ExtendedTextMessage.Text, domain.MessageTypeText, true
	} else if m.ImageMessage != nil {
		if m.ImageMessage.Caption != nil && *m.ImageMessage.Caption != "" {
			return *m.ImageMessage.Caption, domain.MessageTypeImage, true
		}
		return "[Image]", domain.MessageTypeImage, true
	} else if m.AudioMessage != nil {
		return "[Audio / Voice Note]", domain.MessageTypeAudio, true
	} else if m.VideoMessage != nil {
		if m.VideoMessage.Caption != nil && *m.VideoMessage.Caption != "" {
			return *m.VideoMessage.Caption, domain.MessageTypeVideo, true
		}
		return "[Video]", domain.MessageTypeVideo, true
	} else if m.DocumentMessage != nil {
		if m.DocumentMessage.FileName != nil && *m.DocumentMessage.FileName != "" {
			return fmt.Sprintf("[Document: %s]", *m.DocumentMessage.FileName), domain.MessageTypeDocument, true
		}
		return "[Document]", domain.MessageTypeDocument, true
	} else if m.StickerMessage != nil {
		return "[Sticker]", domain.MessageTypeImage, true
	} else if m.LocationMessage != nil {
		return "[Location]", domain.MessageTypeText, true
	} else if m.ContactMessage != nil {
		return "[Contact Card]", domain.MessageTypeText, true
	} else if m.ReactionMessage != nil {
		if m.ReactionMessage.Text != nil && *m.ReactionMessage.Text != "" {
			return fmt.Sprintf("[Reaction: %s]", *m.ReactionMessage.Text), domain.MessageTypeReaction, true
		}
		return "", domain.MessageTypeReaction, false
	}
	return "", domain.MessageTypeText, false
}

// extractDomainMessage extracts semantic fields from the raw wire message event.
func (a *Adapter) extractDomainMessage(evt *events.Message) (domain.Message, bool) {
	body, msgType, ok := extractMessageContent(evt.Message)
	if !ok {
		return domain.Message{}, false
	}

	senderName := evt.Info.PushName
	if senderName == "" {
		senderName = evt.Info.Sender.User
	}

	return domain.Message{
		ID:         evt.Info.ID,
		ChatID:     evt.Info.Chat.ToNonAD().String(),
		Sender:     evt.Info.Sender.ToNonAD().String(),
		SenderName: senderName,
		Timestamp:  evt.Info.Timestamp,
		IsFromMe:   evt.Info.IsFromMe,
		Type:       msgType,
		Body:       body,
		Status:     domain.MessageStatusDelivered,
	}, true
}

func (a *Adapter) extractWebMessage(chatID string, webMsg *waWeb.WebMessageInfo) (domain.Message, bool) {
	if webMsg.Message == nil {
		return domain.Message{}, false
	}
	body, msgType, ok := extractMessageContent(webMsg.Message)
	if !ok {
		return domain.Message{}, false
	}

	sender := chatID
	if webMsg.Participant != nil && *webMsg.Participant != "" {
		sender = *webMsg.Participant
	} else if webMsg.Key != nil && webMsg.Key.Participant != nil && *webMsg.Key.Participant != "" {
		sender = *webMsg.Key.Participant
	}

	senderName := webMsg.GetPushName()
	if senderName == "" {
		senderName = sender
	}

	ts := time.Unix(int64(webMsg.GetMessageTimestamp()), 0)
	msgID := ""
	if webMsg.Key != nil && webMsg.Key.ID != nil {
		msgID = *webMsg.Key.ID
	}

	return domain.Message{
		ID:         msgID,
		ChatID:     chatID,
		Sender:     sender,
		SenderName: senderName,
		Timestamp:  ts,
		IsFromMe:   false,
		Type:       msgType,
		Body:       body,
		Status:     domain.MessageStatusDelivered,
	}, true
}

// renderQRInTerminal prints a clear ANSI/UTF-8 QR code for WhatsApp companion linking.
func (a *Adapter) renderQRInTerminal(code string) {
	qr, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		fmt.Printf("Error generating QR code: %v\n", err)
		return
	}

	fmt.Println("\n=======================================================")
	fmt.Println(" Scan this QR code in WhatsApp on your phone:")
	fmt.Println(" Settings -> Linked Devices -> Link a Device")
	fmt.Println("=======================================================")
	fmt.Print(qr.ToSmallString(false))
	fmt.Println("=======================================================")
}

