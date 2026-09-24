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
	"go.mau.fi/whatsmeow/proto/waE2E"
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

// MarkRead sends a read receipt to WhatsApp for the given message IDs.
func (a *Adapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
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

// handleEvent processes internal whatsmeow events and maps them to clean domain models.
func (a *Adapter) handleEvent(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		a.setStatus(domain.StatusConnected)
		go a.refreshContactsCache(context.Background())

	case *events.LoggedOut:
		a.setStatus(domain.StatusLoggedOut)

	case *events.Message:
		domainMsg, ok := a.extractDomainMessage(evt)
		if !ok {
			// Internal protocol, sender-key-distribution, or empty control message; do not leak to UI
			return
		}

		a.mu.RLock()
		handlers := append([]domain.MessageHandler(nil), a.messageHandlers...)
		a.mu.RUnlock()

		for _, h := range handlers {
			h(domainMsg)
		}
	}
}

// extractDomainMessage extracts semantic fields from the raw wire message event.
// Returns ok=false for purely internal protocol/key-distribution packets.
func (a *Adapter) extractDomainMessage(evt *events.Message) (domain.Message, bool) {
	m := evt.Message
	if m == nil {
		return domain.Message{}, false
	}

	msgType := domain.MessageTypeText
	body := ""

	if m.Conversation != nil && *m.Conversation != "" {
		body = *m.Conversation
		msgType = domain.MessageTypeText
	} else if m.ExtendedTextMessage != nil && m.ExtendedTextMessage.Text != nil && *m.ExtendedTextMessage.Text != "" {
		body = *m.ExtendedTextMessage.Text
		msgType = domain.MessageTypeText
	} else if m.ImageMessage != nil {
		msgType = domain.MessageTypeImage
		if m.ImageMessage.Caption != nil && *m.ImageMessage.Caption != "" {
			body = *m.ImageMessage.Caption
		} else {
			body = "[Image]"
		}
	} else if m.AudioMessage != nil {
		msgType = domain.MessageTypeAudio
		body = "[Audio / Voice Note]"
	} else if m.VideoMessage != nil {
		msgType = domain.MessageTypeVideo
		if m.VideoMessage.Caption != nil && *m.VideoMessage.Caption != "" {
			body = *m.VideoMessage.Caption
		} else {
			body = "[Video]"
		}
	} else if m.DocumentMessage != nil {
		msgType = domain.MessageTypeDocument
		if m.DocumentMessage.FileName != nil && *m.DocumentMessage.FileName != "" {
			body = fmt.Sprintf("[Document: %s]", *m.DocumentMessage.FileName)
		} else {
			body = "[Document]"
		}
	} else if m.StickerMessage != nil {
		msgType = domain.MessageTypeImage
		body = "[Sticker]"
	} else if m.LocationMessage != nil {
		msgType = domain.MessageTypeText
		body = "[Location]"
	} else if m.ContactMessage != nil {
		msgType = domain.MessageTypeText
		body = "[Contact Card]"
	} else if m.ReactionMessage != nil {
		msgType = domain.MessageTypeReaction
		if m.ReactionMessage.Text != nil && *m.ReactionMessage.Text != "" {
			body = fmt.Sprintf("[Reaction: %s]", *m.ReactionMessage.Text)
		} else {
			// Removed reaction
			return domain.Message{}, false
		}
	} else {
		// Purely internal control/key-distribution/protocol message with no visible content
		return domain.Message{}, false
	}

	senderName := evt.Info.PushName
	if senderName == "" {
		senderName = evt.Info.Sender.User
	}

	return domain.Message{
		ID:         evt.Info.ID,
		ChatID:     evt.Info.Chat.String(),
		Sender:     evt.Info.Sender.String(),
		SenderName: senderName,
		Timestamp:  evt.Info.Timestamp,
		IsFromMe:   evt.Info.IsFromMe,
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

