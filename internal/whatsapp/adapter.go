package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"watui/internal/config"
	"watui/internal/domain"
	"watui/internal/security"
)

func init() {
	store.SetOSInfo("WA-TUI", [3]uint32{0, 1, 0})
}

// Config holds configuration for the WhatsApp adapter.
type Config struct {
	DBPath     string
	LogFile    string
	LogLevel   string
	DeviceName string
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

	mediaMu    sync.RWMutex
	mediaCache map[string]*waE2E.Message

	ctx    context.Context
	cancel context.CancelFunc
}

func (a *Adapter) cacheMediaMessage(id string, raw *waE2E.Message) {
	if raw == nil || id == "" {
		return
	}
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if a.mediaCache == nil {
		a.mediaCache = make(map[string]*waE2E.Message)
	}
	a.mediaCache[id] = raw
}

func (a *Adapter) getCachedMediaMessage(id string) *waE2E.Message {
	if id == "" {
		return nil
	}
	a.mediaMu.RLock()
	defer a.mediaMu.RUnlock()
	if a.mediaCache == nil {
		return nil
	}
	return a.mediaCache[id]
}

// NewAdapter creates and initializes a WhatsApp adapter instance.
func NewAdapter(ctx context.Context, cfg Config) (*Adapter, error) {
	if cfg.DBPath == "" {
		cfg.DBPath = "watui.db"
	}

	deviceName := strings.TrimSpace(cfg.DeviceName)
	if deviceName == "" {
		deviceName = "WA-TUI"
	}
	store.SetOSInfo(deviceName, [3]uint32{0, 1, 0})

	cleanLog := strings.TrimSpace(cfg.LogFile)
	dbLog, err := NewFileLogger(cleanLog, "Database", cfg.LogLevel)
	if err != nil {
		dbLog = waLog.Noop
	}
	clientLog, err := NewFileLogger(cleanLog, "Client", cfg.LogLevel)
	if err != nil {
		clientLog = waLog.Noop
	}

	dbDir := filepath.Dir(cfg.DBPath)
	if dbDir == "" || dbDir == "." {
		if exeDir := config.GetExeDir(); exeDir != "" && exeDir != "." {
			if _, err := os.Stat(filepath.Join(exeDir, "watui.db")); err == nil {
				dbDir = exeDir
			} else if _, err := os.Stat(filepath.Join(exeDir, security.KeyFileName)); err == nil {
				dbDir = exeDir
			}
		}
		if dbDir == "" || dbDir == "." {
			if runtime.GOOS == "windows" {
				dbDir = "."
			} else if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
				dbDir = filepath.Join(xdgData, "watui")
			} else if home, err := os.UserHomeDir(); err == nil {
				dbDir = filepath.Join(home, ".local", "share", "watui")
			} else {
				dbDir = "."
			}
		}
	}
	dbKey, err := security.DeriveDatabaseKey(dbDir)
	if err != nil {
		return nil, fmt.Errorf("failed to derive database encryption key: %w", err)
	}

	// Auto-migrate any existing unencrypted SQLite database to SQLCipher
	if security.IsPlaintextSQLite(cfg.DBPath) {
		if err := security.MigratePlaintextDatabase(cfg.DBPath, dbKey); err != nil {
			return nil, fmt.Errorf("failed to migrate existing plaintext database: %w", err)
		}
	}

	dsn := security.BuildEncryptedDSN(cfg.DBPath, dbKey)

	container, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		if strings.Contains(err.Error(), "file is not a database") {
			return nil, fmt.Errorf("failed to open encrypted database %q: encryption key mismatch or file corrupted (is this database from another machine or user?): %w", cfg.DBPath, err)
		}
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, clientLog)

	localDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open local database: %w", err)
	}
	localDB.SetMaxOpenConns(10)
	localDB.SetMaxIdleConns(5)

	security.EnsureSecurePermissions(cfg.DBPath)
	if localDB != nil {
		_, _ = localDB.Exec(`CREATE TABLE IF NOT EXISTS watui_groups (
			jid TEXT PRIMARY KEY,
			name TEXT
		);
		CREATE TABLE IF NOT EXISTS watui_unread_messages (
			id TEXT PRIMARY KEY,
			chat_id TEXT NOT NULL,
			chat_name TEXT,
			sender TEXT NOT NULL,
			sender_name TEXT,
			timestamp INTEGER NOT NULL,
			body TEXT,
			type TEXT,
			is_from_me BOOLEAN,
			raw_message BLOB
		);
		CREATE INDEX IF NOT EXISTS idx_watui_unread_chat_id ON watui_unread_messages(chat_id);`)
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN chat_name TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN raw_message BLOB;")

		// Only run LID migration if there are actually @lid chats present in watui_unread_messages
		var hasLID int
		_ = localDB.QueryRow("SELECT COUNT(*) FROM watui_unread_messages WHERE chat_id LIKE '%@lid'").Scan(&hasLID)
		if hasLID > 0 {
			rows, err := localDB.Query(`SELECT DISTINCT m.chat_id, l.pn FROM watui_unread_messages m 
				JOIN whatsmeow_lid_map l ON (m.chat_id = l.lid || '@lid' OR m.chat_id LIKE l.lid || ':%@lid')`)
			if err == nil {
				type lidUpdate struct {
					oldChatID string
					newChatID string
				}
				var updates []lidUpdate
				for rows.Next() {
					var oldID, pn string
					if err := rows.Scan(&oldID, &pn); err == nil && pn != "" {
						updates = append(updates, lidUpdate{oldChatID: oldID, newChatID: pn + "@s.whatsapp.net"})
					}
				}
				iterErr := rows.Err()
				_ = rows.Close()
				if iterErr == nil {
					for _, u := range updates {
						_, _ = localDB.Exec("UPDATE watui_unread_messages SET chat_id = ? WHERE chat_id = ?", u.newChatID, u.oldChatID)
					}
				}
			}
		}
	}

	adapterCtx, cancel := context.WithCancel(ctx)
	adapter := &Adapter{
		client:        client,
		container:     container,
		localDB:       localDB,
		config:        cfg,
		currentStatus: domain.StatusDisconnected,
		mediaCache:    make(map[string]*waE2E.Message),
		ctx:           adapterCtx,
		cancel:        cancel,
	}

	client.AddEventHandler(adapter.handleEvent)

	// Preload local SQLite contacts and cached groups immediately so UI starts with full names
	if adapter.IsLoggedIn() {
		initial := adapter.fetchLocalContacts(adapterCtx)
		if len(initial) > 0 {
			adapter.cachedContacts = initial
			adapter.contactsLoaded = true
		}
	}

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
			a.setStatus(domain.StatusDisconnected)
			return fmt.Errorf("failed to connect with existing session: %w", err)
		}
	}

	return nil
}

// Disconnect gracefully shuts down the connection within 1 second.
func (a *Adapter) Disconnect() {
	if a.cancel != nil {
		a.cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if a.client != nil {
			a.client.Disconnect()
		}
		if a.localDB != nil {
			_ = a.localDB.Close()
		}
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		if a.localDB != nil {
			_ = a.localDB.Close()
		}
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
	if strings.HasPrefix(num, "120363") {
		return types.NewJID(num, types.GroupServer), nil
	}
	return types.NewJID(num, types.DefaultUserServer), nil
}

// ResolveLIDToPhone maps an internal WhatsApp LID user to its real phone number using SQLite cache.
func (a *Adapter) ResolveLIDToPhone(lidUser string) string {
	if a.localDB == nil || lidUser == "" {
		return ""
	}
	if idx := strings.Index(lidUser, ":"); idx != -1 {
		lidUser = lidUser[:idx]
	}
	var pn string
	_ = a.localDB.QueryRow("SELECT pn FROM whatsmeow_lid_map WHERE lid = ?", lidUser).Scan(&pn)
	return pn
}

// ResolvePhoneToLID maps a phone number user to its internal WhatsApp LID using SQLite cache.
func (a *Adapter) ResolvePhoneToLID(phoneUser string) string {
	if a.localDB == nil || phoneUser == "" {
		return ""
	}
	if idx := strings.Index(phoneUser, ":"); idx != -1 {
		phoneUser = phoneUser[:idx]
	}
	var lid string
	_ = a.localDB.QueryRow("SELECT lid FROM whatsmeow_lid_map WHERE pn = ?", phoneUser).Scan(&lid)
	return lid
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

// SendFileMessage uploads and sends a file attachment to a WhatsApp chat JID or raw phone number.
func (a *Adapter) SendFileMessage(ctx context.Context, chatID string, filePath string, caption string) (domain.Message, error) {
	if a.client == nil {
		return domain.Message{}, fmt.Errorf("client not connected")
	}

	recipientJID, err := NormalizeJID(chatID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	// Expand ~ to user home directory if present
	if strings.HasPrefix(filePath, "~") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			filePath = filepath.Join(home, strings.TrimPrefix(filePath, "~"))
		}
	}

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to read file %q: %w", filePath, err)
	}

	fileName := filepath.Base(filePath)
	ext := strings.ToLower(filepath.Ext(filePath))
	mimeType := http.DetectContentType(fileData)
	if detectedMime := mime.TypeByExtension(ext); detectedMime != "" {
		mimeType = detectedMime
	}

	var appInfo whatsmeow.MediaType
	var msgType domain.MessageType
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp":
		appInfo = whatsmeow.MediaImage
		msgType = domain.MessageTypeImage
	case ".mp4", ".mov", ".avi", ".mkv", ".webm":
		appInfo = whatsmeow.MediaVideo
		msgType = domain.MessageTypeVideo
	case ".mp3", ".ogg", ".wav", ".m4a", ".aac", ".flac":
		appInfo = whatsmeow.MediaAudio
		msgType = domain.MessageTypeAudio
	default:
		appInfo = whatsmeow.MediaDocument
		msgType = domain.MessageTypeDocument
	}

	resp, err := a.client.Upload(ctx, fileData, appInfo)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to upload media: %w", err)
	}

	var waMsg *waE2E.Message
	label := "Document"
	switch msgType {
	case domain.MessageTypeImage:
		label = "Image"
	case domain.MessageTypeVideo:
		label = "Video"
	case domain.MessageTypeAudio:
		label = "Audio"
	}
	bodyText := fmt.Sprintf("[%s: %s]", label, fileName)
	if caption != "" {
		bodyText += " " + caption
	}

	switch msgType {
	case domain.MessageTypeImage:
		imgMsg := &waE2E.ImageMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		if caption != "" {
			imgMsg.Caption = proto.String(caption)
		}
		waMsg = &waE2E.Message{ImageMessage: imgMsg}

	case domain.MessageTypeVideo:
		vidMsg := &waE2E.VideoMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		if caption != "" {
			vidMsg.Caption = proto.String(caption)
		}
		waMsg = &waE2E.Message{VideoMessage: vidMsg}

	case domain.MessageTypeAudio:
		audioMsg := &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		waMsg = &waE2E.Message{AudioMessage: audioMsg}

	default: // Document
		docMsg := &waE2E.DocumentMessage{
			FileName:      proto.String(fileName),
			Title:         proto.String(fileName),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		if caption != "" {
			docMsg.Caption = proto.String(caption)
		}
		waMsg = &waE2E.Message{DocumentMessage: docMsg}
	}

	sendResp, err := a.client.SendMessage(ctx, recipientJID, waMsg)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to send media message: %w", err)
	}

	a.cacheMediaMessage(sendResp.ID, waMsg)

	senderID := ""
	if a.client.Store.ID != nil {
		senderID = a.client.Store.ID.ToNonAD().String()
	}

	return domain.Message{
		ID:         sendResp.ID,
		ChatID:     recipientJID.String(),
		Sender:     senderID,
		SenderName: "Me",
		Timestamp:  sendResp.Timestamp,
		IsFromMe:   true,
		Type:       msgType,
		Body:       bodyText,
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

	a.contactsMu.Lock()
	a.cachedContacts = contacts
	a.contactsLoaded = true
	a.contactsMu.Unlock()

	// Refresh cache in background (including network groups from WhatsApp)
	go a.refreshContactsCache(a.ctx)

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
	defer func() { _ = rows.Close() }()

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
	if err := rows.Err(); err != nil {
		return groups
	}
	return groups
}

func isGenericName(name, jid string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if strings.Contains(name, "@") {
		return true
	}
	if strings.HasPrefix(name, "Group (") {
		return true
	}
	cleanJID := jid
	if idx := strings.Index(cleanJID, "@"); idx != -1 {
		cleanJID = cleanJID[:idx]
	}
	if name == cleanJID || name == jid {
		return true
	}
	isDigitsAndHyphens := true
	for _, r := range name {
		if (r < '0' || r > '9') && r != '-' && r != '+' && r != ' ' {
			isDigitsAndHyphens = false
			break
		}
	}
	if isDigitsAndHyphens && (strings.Contains(name, "-") || len(strings.ReplaceAll(name, " ", "")) >= 10) {
		return true
	}
	return false
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
	defer func() { _ = stmt.Close() }()

	for _, g := range groups {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			continue
		}
		_, _ = stmt.Exec(g.JID.ToNonAD().String(), name)
	}
	_ = tx.Commit()
}

func (a *Adapter) getLIDMap() map[string]string {
	m := make(map[string]string)
	if a.localDB == nil {
		return m
	}
	rows, err := a.localDB.Query("SELECT lid, pn FROM whatsmeow_lid_map")
	if err != nil {
		return m
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err == nil && lid != "" && pn != "" {
			m[lid] = pn
		}
	}
	if err := rows.Err(); err != nil {
		return m
	}
	return m
}

func (a *Adapter) fetchLocalContacts(ctx context.Context) []domain.Contact {
	var list []domain.Contact

	// 1. Instantly read cached groups from local SQLite
	list = append(list, a.getLocalGroups()...)

	// 2. Read contacts from whatsmeow SQLite store
	if a.client != nil && a.client.Store != nil && a.client.Store.Contacts != nil {
		rawMap, err := a.client.Store.Contacts.GetAllContacts(ctx)
		if err == nil {
			lidMap := a.getLIDMap()
			for jid, info := range rawMap {
				if jid.Server != types.DefaultUserServer && jid.Server != "lid" {
					continue
				}
				name := strings.TrimSpace(info.FullName)
				if name == "" {
					name = strings.TrimSpace(info.BusinessName)
				}
				// Skip anonymous LIDs that have no address book name
				if jid.Server == "lid" && name == "" {
					continue
				}
				if name == "" {
					name = strings.TrimSpace(info.PushName)
				}
				// Skip contacts without any human name or push name
				if name == "" {
					continue
				}

				actualJID := jid
				if jid.Server == "lid" {
					if pn, ok := lidMap[jid.User]; ok && pn != "" {
						actualJID = types.NewJID(pn, types.DefaultUserServer)
					}
				}

				list = append(list, domain.Contact{
					JID:          actualJID.String(),
					Name:         name,
					PushName:     info.PushName,
					BusinessName: info.BusinessName,
					IsGroup:      false,
				})
			}
		}
	}

	// 3. Deduplicate by JID (preferring real human names over generic IDs)
	best := make(map[string]domain.Contact, len(list))
	for _, c := range list {
		existing, found := best[c.JID]
		if !found {
			best[c.JID] = c
			continue
		}
		if isGenericName(existing.Name, existing.JID) && !isGenericName(c.Name, c.JID) {
			best[c.JID] = c
		}
	}

	deduped := make([]domain.Contact, 0, len(best))
	for _, c := range best {
		deduped = append(deduped, c)
	}

	sortContacts(deduped)
	return deduped
}

// EnsureGroupNames ensures that metadata for the provided group JIDs is fetched and cached in background.
func (a *Adapter) EnsureGroupNames(ctx context.Context, jids []string) {
	if len(jids) == 0 || a.localDB == nil {
		return
	}
	var needed []string
	for _, jid := range jids {
		jid = strings.TrimSpace(jid)
		if !strings.Contains(jid, "@g.us") && !strings.HasPrefix(jid, "120363") {
			continue
		}
		var name string
		err := a.localDB.QueryRow("SELECT name FROM watui_groups WHERE (jid = ? OR jid LIKE ?) AND name NOT LIKE 'Group (%' AND name != ''", jid, strings.Split(jid, "@")[0]+"%").Scan(&name)
		if err != nil || name == "" {
			needed = append(needed, jid)
		}
	}
	if len(needed) == 0 {
		return
	}

	if len(needed) > 3 {
		needed = needed[:3]
	}

	go func() {
		if a.client != nil && !a.client.IsConnected() {
			if !a.client.WaitForConnection(5 * time.Second) {
				return
			}
		}
		updated := false
		for _, jidStr := range needed {
			parsedJID, err := NormalizeJID(jidStr)
			if err != nil {
				continue
			}
			reqCtx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
			info, err := a.client.GetGroupInfo(reqCtx, parsedJID.ToNonAD())
			cancel()
			if err == nil && info != nil && info.Name != "" {
				name := strings.TrimSpace(info.Name)
				_, _ = a.localDB.Exec("INSERT OR REPLACE INTO watui_groups (jid, name) VALUES (?, ?)", parsedJID.ToNonAD().String(), name)
				updated = true
			}
		}
		if updated {
			go a.refreshContactsCache(a.ctx)
		}
	}()
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

	// If socket is connected, fetch joined groups from WhatsApp in a fast single network call
	if a.client.IsConnected() {
		groupCtx, groupCancel := context.WithTimeout(ctx, 8*time.Second)
		groups, err := a.client.GetJoinedGroups(groupCtx)
		groupCancel()
		if err == nil && len(groups) > 0 {
			a.saveLocalGroups(groups)
		}
	}

	// Re-read combined local list
	combined := a.fetchLocalContacts(ctx)

	a.contactsMu.Lock()
	a.cachedContacts = combined
	a.contactsLoaded = true
	var handlers []func([]domain.Contact)
	if len(a.contactsHandlers) > 0 {
		handlers = make([]func([]domain.Contact), len(a.contactsHandlers))
		copy(handlers, a.contactsHandlers)
	}
	a.contactsMu.Unlock()

	for _, h := range handlers {
		h(combined)
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

func (a *Adapter) saveUnreadMessage(msg domain.Message, rawMsg ...*waE2E.Message) {
	if a.localDB == nil {
		return
	}
	var rawBytes []byte
	if len(rawMsg) > 0 && rawMsg[0] != nil {
		rawBytes, _ = proto.Marshal(rawMsg[0])
	}
	_, _ = a.localDB.Exec(`INSERT OR REPLACE INTO watui_unread_messages
		(id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, raw_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChatID, msg.ChatName, msg.Sender, msg.SenderName, msg.Timestamp.Unix(), msg.Body, string(msg.Type), msg.IsFromMe, rawBytes,
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
	lidMap := a.getLIDMap()

	rows, err := a.localDB.QueryContext(ctx, `SELECT id, chat_id, COALESCE(chat_name, ''), sender, sender_name, timestamp, body, type, is_from_me, raw_message
		FROM watui_unread_messages ORDER BY timestamp ASC`)
	if err != nil {
		rows, err = a.localDB.QueryContext(ctx, `SELECT id, chat_id, COALESCE(chat_name, ''), sender, sender_name, timestamp, body, type, is_from_me
			FROM watui_unread_messages ORDER BY timestamp ASC`)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	cols, _ := rows.Columns()
	hasRaw := len(cols) >= 10

	for rows.Next() {
		var m domain.Message
		var ts int64
		var tStr string
		var rawBytes []byte
		var scanErr error
		if hasRaw {
			scanErr = rows.Scan(&m.ID, &m.ChatID, &m.ChatName, &m.Sender, &m.SenderName, &ts, &m.Body, &tStr, &m.IsFromMe, &rawBytes)
		} else {
			scanErr = rows.Scan(&m.ID, &m.ChatID, &m.ChatName, &m.Sender, &m.SenderName, &ts, &m.Body, &tStr, &m.IsFromMe)
		}
		if scanErr == nil {
			m.Timestamp = time.Unix(ts, 0)
			m.Type = domain.MessageType(tStr)
			m.Status = domain.MessageStatusDelivered
			if strings.HasSuffix(m.ChatID, "@lid") {
				lidUser := strings.TrimSuffix(m.ChatID, "@lid")
				if idx := strings.Index(lidUser, ":"); idx != -1 {
					lidUser = lidUser[:idx]
				}
				if pn, ok := lidMap[lidUser]; ok && pn != "" {
					m.ChatID = pn + "@s.whatsapp.net"
				}
			}
			if strings.HasSuffix(m.Sender, "@lid") {
				lidUser := strings.TrimSuffix(m.Sender, "@lid")
				if idx := strings.Index(lidUser, ":"); idx != -1 {
					lidUser = lidUser[:idx]
				}
				if pn, ok := lidMap[lidUser]; ok && pn != "" {
					m.Sender = pn + "@s.whatsapp.net"
				}
			}
			if len(rawBytes) > 0 {
				var parsed waE2E.Message
				if err := proto.Unmarshal(rawBytes, &parsed); err == nil {
					a.cacheMediaMessage(m.ID, &parsed)
				}
			}
			msgs = append(msgs, m)
		}
	}
	if err := rows.Err(); err != nil {
		return msgs, fmt.Errorf("failed during unread messages row iteration: %w", err)
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

	var altTarget string
	if strings.HasSuffix(nonAD, "@s.whatsapp.net") {
		user := strings.TrimSuffix(nonAD, "@s.whatsapp.net")
		if lid := a.ResolvePhoneToLID(user); lid != "" {
			altTarget = lid + "@lid"
		}
	} else if strings.HasSuffix(nonAD, "@lid") {
		user := strings.TrimSuffix(nonAD, "@lid")
		if pn := a.ResolveLIDToPhone(user); pn != "" {
			altTarget = pn + "@s.whatsapp.net"
		}
	}

	_, err := a.localDB.ExecContext(ctx, "DELETE FROM watui_unread_messages WHERE chat_id = ? OR chat_id = ? OR chat_id = ?", chatID, nonAD, altTarget)
	return err
}

// handleEvent processes internal whatsmeow events and maps them to clean domain models.
func (a *Adapter) handleEvent(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		a.setStatus(domain.StatusConnected)
		go a.refreshContactsCache(a.ctx)
		go func() {
			// Catch up on any app state mutations (e.g. chats read on other devices)
			_ = a.client.FetchAppState(a.ctx, appstate.WAPatchRegularLow, false, false)
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
					if webMsg.Message != nil {
						a.cacheMediaMessage(domainMsg.ID, webMsg.Message)
					}
					a.saveUnreadMessage(domainMsg, webMsg.Message)
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

		if evt.Message != nil {
			a.cacheMediaMessage(domainMsg.ID, evt.Message)
		}

		if !domainMsg.IsFromMe {
			a.saveUnreadMessage(domainMsg, evt.Message)
		}

		a.notifyMessage(domainMsg)

	case *events.GroupInfo:
		if evt.Name != nil && evt.Name.Name != "" {
			name := strings.TrimSpace(evt.Name.Name)
			if a.localDB != nil {
				_, _ = a.localDB.Exec("INSERT OR REPLACE INTO watui_groups (jid, name) VALUES (?, ?)", evt.JID.ToNonAD().String(), name)
			}
			go a.refreshContactsCache(a.ctx)
		}

	case *events.JoinedGroup:
		name := strings.TrimSpace(evt.GroupName.Name)
		if name != "" && a.localDB != nil {
			_, _ = a.localDB.Exec("INSERT OR REPLACE INTO watui_groups (jid, name) VALUES (?, ?)", evt.JID.ToNonAD().String(), name)
		}
		go a.refreshContactsCache(a.ctx)
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
		return "[Sticker]", domain.MessageTypeSticker, true
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

func (a *Adapter) resolveChatName(chat types.JID) string {
	chatNonAD := chat.ToNonAD()
	if chatNonAD.Server == "lid" {
		if pn := a.ResolveLIDToPhone(chatNonAD.User); pn != "" {
			chatNonAD = types.NewJID(pn, types.DefaultUserServer)
		}
	}
	chatStr := chatNonAD.String()

	if chatNonAD.Server == types.GroupServer {
		if a.localDB != nil {
			var name string
			if err := a.localDB.QueryRow("SELECT name FROM watui_groups WHERE (jid = ? OR jid LIKE ?) AND name NOT LIKE 'Group (%' AND name != ''", chatStr, chatNonAD.User+"%").Scan(&name); err == nil && name != "" {
				return name
			}
		}
		if a.client != nil && a.client.IsConnected() {
			infoCtx, infoCancel := context.WithTimeout(context.Background(), 3*time.Second)
			info, err := a.client.GetGroupInfo(infoCtx, chatNonAD)
			infoCancel()
			if err == nil && info != nil && info.Name != "" {
				name := strings.TrimSpace(info.Name)
				if a.localDB != nil {
					_, _ = a.localDB.Exec("INSERT OR REPLACE INTO watui_groups (jid, name) VALUES (?, ?)", chatStr, name)
				}
				return name
			}
		}
		return "Group (" + chatNonAD.User + ")"
	}

	// 1-on-1 contact
	if a.client != nil && a.client.Store != nil && a.client.Store.Contacts != nil {
		contact, err := a.client.Store.Contacts.GetContact(context.Background(), chatNonAD)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.BusinessName != "" {
				return contact.BusinessName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return ""
}

func (a *Adapter) extractDomainMessage(evt *events.Message) (domain.Message, bool) {
	body, msgType, ok := extractMessageContent(evt.Message)
	if !ok {
		return domain.Message{}, false
	}

	senderName := evt.Info.PushName
	if senderName == "" {
		senderName = evt.Info.Sender.User
	}

	chatJID := evt.Info.Chat.ToNonAD()
	if chatJID.Server == "lid" {
		if pn := a.ResolveLIDToPhone(chatJID.User); pn != "" {
			chatJID = types.NewJID(pn, types.DefaultUserServer)
		}
	}

	senderJID := evt.Info.Sender.ToNonAD()
	if senderJID.Server == "lid" {
		if pn := a.ResolveLIDToPhone(senderJID.User); pn != "" {
			senderJID = types.NewJID(pn, types.DefaultUserServer)
		}
	}

	chatName := a.resolveChatName(chatJID)
	if chatName == "" && !evt.Info.IsGroup {
		chatName = senderName
	}

	return domain.Message{
		ID:         evt.Info.ID,
		ChatID:     chatJID.String(),
		ChatName:   chatName,
		Sender:     senderJID.String(),
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

	chatJID, _ := types.ParseJID(chatID)
	chatJID = chatJID.ToNonAD()
	if chatJID.Server == "lid" {
		if pn := a.ResolveLIDToPhone(chatJID.User); pn != "" {
			chatJID = types.NewJID(pn, types.DefaultUserServer)
			chatID = chatJID.String()
		}
	}

	senderJID, err := types.ParseJID(sender)
	if err == nil {
		senderJID = senderJID.ToNonAD()
		if senderJID.Server == "lid" {
			if pn := a.ResolveLIDToPhone(senderJID.User); pn != "" {
				sender = types.NewJID(pn, types.DefaultUserServer).String()
			}
		}
	}

	chatName := a.resolveChatName(chatJID)
	if chatName == "" && !strings.Contains(chatID, "@g.us") {
		chatName = senderName
	}

	ts := time.Unix(int64(webMsg.GetMessageTimestamp()), 0)
	msgID := ""
	if webMsg.Key != nil && webMsg.Key.ID != nil {
		msgID = *webMsg.Key.ID
	}

	return domain.Message{
		ID:         msgID,
		ChatID:     chatID,
		ChatName:   chatName,
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

// DownloadMedia downloads the media attachment for a message on demand and returns the local file path.
func (a *Adapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	if a.client == nil {
		return "", fmt.Errorf("client not connected")
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "watui-media")
	} else {
		cacheDir = filepath.Join(cacheDir, "watui", "media")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create media cache dir: %w", err)
	}

	ext := ".bin"
	switch msg.Type {
	case domain.MessageTypeImage:
		ext = ".jpg"
	case domain.MessageTypeVideo:
		ext = ".mp4"
	case domain.MessageTypeSticker:
		ext = ".webp"
	case domain.MessageTypeAudio:
		ext = ".ogg"
	}

	safeID := strings.ReplaceAll(msg.ID, "/", "_")
	filePath := filepath.Join(cacheDir, fmt.Sprintf("%s%s", safeID, ext))

	// Return cached file if already on disk and non-empty
	if fi, err := os.Stat(filePath); err == nil && fi.Size() > 0 {
		return filePath, nil
	}

	rawMsg := a.getCachedMediaMessage(msg.ID)
	if rawMsg == nil && a.localDB != nil {
		var rawBytes []byte
		row := a.localDB.QueryRowContext(ctx, "SELECT raw_message FROM watui_unread_messages WHERE id = ?", msg.ID)
		if err := row.Scan(&rawBytes); err == nil && len(rawBytes) > 0 {
			var parsed waE2E.Message
			if err := proto.Unmarshal(rawBytes, &parsed); err == nil {
				rawMsg = &parsed
				a.cacheMediaMessage(msg.ID, &parsed)
			}
		}
	}

	if rawMsg == nil {
		return "", fmt.Errorf("media metadata not found for message %s", msg.ID)
	}

	var downloadable whatsmeow.DownloadableMessage
	if rawMsg.ImageMessage != nil {
		downloadable = rawMsg.ImageMessage
		if rawMsg.ImageMessage.GetMimetype() == "image/png" {
			filePath = filepath.Join(cacheDir, fmt.Sprintf("%s.png", safeID))
		}
	} else if rawMsg.VideoMessage != nil {
		downloadable = rawMsg.VideoMessage
	} else if rawMsg.StickerMessage != nil {
		downloadable = rawMsg.StickerMessage
		filePath = filepath.Join(cacheDir, fmt.Sprintf("%s.webp", safeID))
	} else if rawMsg.AudioMessage != nil {
		downloadable = rawMsg.AudioMessage
	} else if rawMsg.DocumentMessage != nil {
		downloadable = rawMsg.DocumentMessage
		fileName := rawMsg.DocumentMessage.GetFileName()
		if fileName != "" {
			safeFileName := filepath.Base(fileName)
			filePath = filepath.Join(cacheDir, fmt.Sprintf("%s_%s", safeID, safeFileName))
		}
	}

	if downloadable == nil {
		return "", fmt.Errorf("message %s does not contain downloadable attachment", msg.ID)
	}

	data, err := a.client.Download(ctx, downloadable)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write media file to disk: %w", err)
	}

	return filePath, nil
}
