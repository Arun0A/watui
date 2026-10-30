package whatsapp

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image/gif"
	"image/jpeg"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"watui/internal/config"
	"watui/internal/domain"
	"watui/internal/media"
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
	AppConfig  *config.Config
}

// Adapter implements domain.WhatsAppAdapter using whatsmeow.
// It completely encapsulates all WhatsApp-specific protocol, crypto, and wire details.
type Adapter struct {
	client    *whatsmeow.Client
	container *sqlstore.Container
	localDB   *sql.DB
	config    Config
	appConfig *config.Config

	mu              sync.RWMutex
	messageHandlers []domain.MessageHandler
	statusHandlers  []domain.StatusHandler
	dismissHandlers []func(chatID string)
	currentStatus   domain.ConnectionStatus

	contactsMu       sync.RWMutex
	cachedContacts   []domain.Contact
	contactsByJID    map[string]string
	contactsLoaded   bool
	contactsHandlers []func([]domain.Contact)
	refreshMu        sync.Mutex
	isRefreshing     bool

	mediaMu    sync.RWMutex
	mediaCache map[string]*waE2E.Message

	archivedMu       sync.RWMutex
	archivedChats    map[string]bool
	archivedHandlers []func(chatID string, archived bool)

	mutedMu    sync.RWMutex
	mutedChats map[string]time.Time

	lidMapMu        sync.RWMutex
	lidMapCache     map[string]string
	phoneToLIDCache map[string]string
	lidMapCachedAt  time.Time

	groupParticipantsMu sync.RWMutex
	groupParticipants   map[string][]domain.Contact

	ephemeralMu        sync.RWMutex
	ephemeralTimers    map[string]uint32
	ephemeralSettingTS map[string]int64
	ephemeralHandlers  []func(chatID string, timer uint32)

	presenceMu       sync.RWMutex
	presences        map[string]domain.UserPresence
	presenceHandlers []domain.PresenceHandler

	receiptMu       sync.RWMutex
	receiptHandlers []domain.ReceiptHandler

	resetMu       sync.Mutex
	lastResetTime time.Time
	resuming      atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

func unwrapMessage(m *waE2E.Message) *waE2E.Message {
	if m == nil {
		return nil
	}
	for {
		if m.EphemeralMessage != nil && m.EphemeralMessage.Message != nil {
			m = m.EphemeralMessage.Message
			continue
		}
		if m.ViewOnceMessage != nil && m.ViewOnceMessage.Message != nil {
			m = m.ViewOnceMessage.Message
			continue
		}
		if m.ViewOnceMessageV2 != nil && m.ViewOnceMessageV2.Message != nil {
			m = m.ViewOnceMessageV2.Message
			continue
		}
		if m.ViewOnceMessageV2Extension != nil && m.ViewOnceMessageV2Extension.Message != nil {
			m = m.ViewOnceMessageV2Extension.Message
			continue
		}
		if m.DocumentWithCaptionMessage != nil && m.DocumentWithCaptionMessage.Message != nil {
			m = m.DocumentWithCaptionMessage.Message
			continue
		}
		if m.BotInvokeMessage != nil && m.BotInvokeMessage.Message != nil {
			m = m.BotInvokeMessage.Message
			continue
		}
		if m.DeviceSentMessage != nil && m.DeviceSentMessage.Message != nil {
			m = m.DeviceSentMessage.Message
			continue
		}
		if m.EditedMessage != nil && m.EditedMessage.Message != nil {
			m = m.EditedMessage.Message
			continue
		}
		if m.ProtocolMessage != nil && m.ProtocolMessage.EditedMessage != nil {
			m = m.ProtocolMessage.EditedMessage
			continue
		}
		break
	}
	return m
}

func extractRevokeProtocolInfo(evt *events.Message) (string, bool) {
	if evt == nil {
		return "", false
	}
	findInMsg := func(m *waE2E.Message) (string, bool) {
		curr := m
		for curr != nil {
			if curr.ProtocolMessage != nil && curr.ProtocolMessage.GetType() == waE2E.ProtocolMessage_REVOKE {
				if key := curr.ProtocolMessage.GetKey(); key != nil && key.GetID() != "" {
					return key.GetID(), true
				}
			}
			if curr.EphemeralMessage != nil && curr.EphemeralMessage.GetMessage() != nil {
				curr = curr.EphemeralMessage.GetMessage()
				continue
			}
			if curr.DeviceSentMessage != nil && curr.DeviceSentMessage.GetMessage() != nil {
				curr = curr.DeviceSentMessage.GetMessage()
				continue
			}
			break
		}
		return "", false
	}
	if evt.Message != nil {
		if id, ok := findInMsg(evt.Message); ok {
			return id, true
		}
	}
	if evt.RawMessage != nil {
		if id, ok := findInMsg(evt.RawMessage); ok {
			return id, true
		}
	}
	return "", false
}

func extractEditProtocolInfo(evt *events.Message) (targetID string, targetChat string, newMsg *waE2E.Message, isEdit bool) {
	if evt == nil {
		return "", "", nil, false
	}

	findInMsg := func(m *waE2E.Message) (string, string, *waE2E.Message, bool) {
		if m == nil {
			return "", "", nil, false
		}
		curr := m
		for curr != nil {
			if curr.ProtocolMessage != nil && curr.ProtocolMessage.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
				pm := curr.ProtocolMessage
				tid := ""
				tchat := ""
				if pm.GetKey() != nil {
					tid = pm.GetKey().GetID()
					tchat = pm.GetKey().GetRemoteJID()
				}
				em := pm.GetEditedMessage()
				if em == nil && curr.EditedMessage != nil {
					em = curr.EditedMessage.GetMessage()
				}
				return tid, tchat, em, true
			}
			if curr.EditedMessage != nil && curr.EditedMessage.GetMessage() != nil {
				curr = curr.EditedMessage.GetMessage()
				continue
			}
			if curr.DeviceSentMessage != nil && curr.DeviceSentMessage.GetMessage() != nil {
				curr = curr.DeviceSentMessage.GetMessage()
				continue
			}
			if curr.EphemeralMessage != nil && curr.EphemeralMessage.GetMessage() != nil {
				curr = curr.EphemeralMessage.GetMessage()
				continue
			}
			if curr.ViewOnceMessage != nil && curr.ViewOnceMessage.GetMessage() != nil {
				curr = curr.ViewOnceMessage.GetMessage()
				continue
			}
			if curr.ViewOnceMessageV2 != nil && curr.ViewOnceMessageV2.GetMessage() != nil {
				curr = curr.ViewOnceMessageV2.GetMessage()
				continue
			}
			if curr.ViewOnceMessageV2Extension != nil && curr.ViewOnceMessageV2Extension.GetMessage() != nil {
				curr = curr.ViewOnceMessageV2Extension.GetMessage()
				continue
			}
			if curr.DocumentWithCaptionMessage != nil && curr.DocumentWithCaptionMessage.GetMessage() != nil {
				curr = curr.DocumentWithCaptionMessage.GetMessage()
				continue
			}
			if curr.BotInvokeMessage != nil && curr.BotInvokeMessage.GetMessage() != nil {
				curr = curr.BotInvokeMessage.GetMessage()
				continue
			}
			break
		}
		return "", "", nil, false
	}

	if evt.Message != nil {
		if tid, tchat, em, ok := findInMsg(evt.Message); ok {
			return tid, tchat, em, true
		}
	}
	if evt.RawMessage != nil {
		if tid, tchat, em, ok := findInMsg(evt.RawMessage); ok {
			return tid, tchat, em, true
		}
	}
	if evt.IsEdit {
		targetID := string(evt.Info.MsgBotInfo.EditTargetID)
		if targetID == "" {
			targetID = evt.Info.ID
		}
		tchat := ""
		if evt.Info.Chat.String() != "" {
			tchat = evt.Info.Chat.String()
		}
		return targetID, tchat, evt.Message, true
	}
	return "", "", nil, false
}

func findSecretEncryptedMessage(m *waE2E.Message) *waE2E.SecretEncryptedMessage {
	curr := m
	for curr != nil {
		if curr.SecretEncryptedMessage != nil {
			return curr.SecretEncryptedMessage
		}
		if curr.EphemeralMessage != nil && curr.EphemeralMessage.GetMessage() != nil {
			curr = curr.EphemeralMessage.GetMessage()
			continue
		}
		if curr.DeviceSentMessage != nil && curr.DeviceSentMessage.GetMessage() != nil {
			curr = curr.DeviceSentMessage.GetMessage()
			continue
		}
		if curr.ViewOnceMessage != nil && curr.ViewOnceMessage.GetMessage() != nil {
			curr = curr.ViewOnceMessage.GetMessage()
			continue
		}
		if curr.ViewOnceMessageV2 != nil && curr.ViewOnceMessageV2.GetMessage() != nil {
			curr = curr.ViewOnceMessageV2.GetMessage()
			continue
		}
		if curr.ViewOnceMessageV2Extension != nil && curr.ViewOnceMessageV2Extension.GetMessage() != nil {
			curr = curr.ViewOnceMessageV2Extension.GetMessage()
			continue
		}
		if curr.DocumentWithCaptionMessage != nil && curr.DocumentWithCaptionMessage.GetMessage() != nil {
			curr = curr.DocumentWithCaptionMessage.GetMessage()
			continue
		}
		if curr.BotInvokeMessage != nil && curr.BotInvokeMessage.GetMessage() != nil {
			curr = curr.BotInvokeMessage.GetMessage()
			continue
		}
		if curr.EditedMessage != nil && curr.EditedMessage.GetMessage() != nil {
			curr = curr.EditedMessage.GetMessage()
			continue
		}
		break
	}
	return nil
}

func (a *Adapter) decryptSecretEditMessage(evt *events.Message, secEncMsg *waE2E.SecretEncryptedMessage, targetID string) *waE2E.Message {
	if secEncMsg == nil {
		return nil
	}

	// 1. Try standard whatsmeow client decryption first
	if a != nil && a.client != nil {
		origMsg := evt.Message
		tempMsg := &waE2E.Message{
			SecretEncryptedMessage: secEncMsg,
		}
		if origMsg != nil && origMsg.MessageContextInfo != nil {
			tempMsg.MessageContextInfo = origMsg.MessageContextInfo
		}
		evt.Message = tempMsg
		decrypted, err := a.client.DecryptSecretEncryptedMessage(a.ctx, evt)
		evt.Message = origMsg
		if err == nil && decrypted != nil {
			return decrypted
		}
	}

	// 2. Fallback: Direct decryption from whatsmeow_message_secrets
	if a == nil || a.localDB == nil || targetID == "" {
		return nil
	}

	var baseEncKey []byte
	var storedSender, storedChat string
	err := a.localDB.QueryRow("SELECT key, sender_jid, chat_jid FROM whatsmeow_message_secrets WHERE message_id = ? LIMIT 1", targetID).Scan(&baseEncKey, &storedSender, &storedChat)
	if err != nil || len(baseEncKey) == 0 {
		return nil
	}

	iv := secEncMsg.GetEncIV()
	payload := secEncMsg.GetEncPayload()
	if len(iv) == 0 || len(payload) == 0 {
		return nil
	}

	candidateOrigSenders := []string{storedSender}
	candidateModSenders := []string{evt.Info.Sender.ToNonAD().String()}
	if secEncMsg.GetTargetMessageKey() != nil {
		if p := secEncMsg.GetTargetMessageKey().GetParticipant(); p != "" {
			candidateOrigSenders = append(candidateOrigSenders, p)
		}
		if r := secEncMsg.GetTargetMessageKey().GetRemoteJID(); r != "" && !strings.Contains(r, "@g.us") {
			candidateOrigSenders = append(candidateOrigSenders, r)
		}
	}

	expandJID := func(j string) []string {
		if j == "" {
			return nil
		}
		res := []string{j}
		clean := strings.Split(j, ":")[0]
		if clean != j {
			res = append(res, clean)
		}
		for _, s := range []string{j, clean} {
			if strings.HasSuffix(s, "@lid") {
				lid := strings.TrimSuffix(s, "@lid")
				if pn := a.ResolveLIDToPhone(lid); pn != "" {
					res = append(res, pn+"@s.whatsapp.net")
				}
			} else if strings.HasSuffix(s, "@s.whatsapp.net") {
				pn := strings.TrimSuffix(s, "@s.whatsapp.net")
				if lid := a.ResolvePhoneToLID(pn); lid != "" {
					res = append(res, lid+"@lid")
				}
			}
		}
		return res
	}

	var allOrig []string
	seenOrig := make(map[string]bool)
	for _, o := range candidateOrigSenders {
		for _, exp := range expandJID(o) {
			if !seenOrig[exp] {
				seenOrig[exp] = true
				allOrig = append(allOrig, exp)
			}
		}
	}

	var allMod []string
	seenMod := make(map[string]bool)
	for _, m := range candidateModSenders {
		for _, exp := range expandJID(m) {
			if !seenMod[exp] {
				seenMod[exp] = true
				allMod = append(allMod, exp)
			}
		}
	}

	useCases := []string{"Message Edit", ""}

	for _, origSender := range allOrig {
		for _, modSender := range allMod {
			for _, uc := range useCases {
				useCaseSecret := make([]byte, 0, len(targetID)+len(origSender)+len(modSender)+len(uc))
				useCaseSecret = append(useCaseSecret, targetID...)
				useCaseSecret = append(useCaseSecret, origSender...)
				useCaseSecret = append(useCaseSecret, modSender...)
				useCaseSecret = append(useCaseSecret, uc...)

				secretKey := hkdfutil.SHA256(baseEncKey, nil, useCaseSecret, 32)
				var additionalData []byte
				if uc == "" {
					additionalData = []byte(fmt.Sprintf("%s\x00%s", targetID, modSender))
				}

				plaintext, err := gcmutil.Decrypt(secretKey, iv, payload, additionalData)
				if err == nil && len(plaintext) > 0 {
					var decrypted waE2E.Message
					if proto.Unmarshal(plaintext, &decrypted) == nil {
						return &decrypted
					}
				}
			}
		}
	}

	return nil
}

func (a *Adapter) extractEditDetails(evt *events.Message) (targetID string, targetChat string, newMsg *waE2E.Message, isEdit bool) {
	if evt == nil {
		return "", "", nil, false
	}

	// 1. Check for ProtocolMessage_MESSAGE_EDIT first (either in Message or RawMessage)
	tid, tchat, protoMsg, ok := extractEditProtocolInfo(evt)
	if ok && tid != "" {
		return tid, tchat, protoMsg, true
	}

	// 2. Check for SecretEncryptedMessage (1-on-1 or modern groups)
	var secEncMsg *waE2E.SecretEncryptedMessage
	if evt.Message != nil {
		secEncMsg = findSecretEncryptedMessage(evt.Message)
	}
	if secEncMsg == nil && evt.RawMessage != nil {
		secEncMsg = findSecretEncryptedMessage(evt.RawMessage)
	}
	if secEncMsg != nil && secEncMsg.GetSecretEncType() == waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
		targetKey := secEncMsg.GetTargetMessageKey()
		tid := ""
		tchat := ""
		if targetKey != nil {
			tid = targetKey.GetID()
			tchat = targetKey.GetRemoteJID()
		}
		if tid == "" && evt.Info.MsgBotInfo.EditTargetID != "" {
			tid = string(evt.Info.MsgBotInfo.EditTargetID)
		}
		decrypted := a.decryptSecretEditMessage(evt, secEncMsg, tid)
		if tid != "" {
			return tid, tchat, decrypted, true
		}
	}

	return "", "", nil, false
}

func hasMedia(m *waE2E.Message) bool {
	m = unwrapMessage(m)
	if m == nil {
		return false
	}
	return m.ImageMessage != nil || m.VideoMessage != nil || m.AudioMessage != nil || m.DocumentMessage != nil || m.StickerMessage != nil
}

func (a *Adapter) cacheMediaMessage(id string, chatID string, raw *waE2E.Message, ts ...time.Time) {
	if raw == nil || id == "" {
		return
	}
	a.mediaMu.Lock()
	if a.mediaCache == nil {
		a.mediaCache = make(map[string]*waE2E.Message)
	}
	a.mediaCache[id] = raw
	a.mediaMu.Unlock()

	if a.localDB != nil && hasMedia(raw) {
		rawBytes, err := proto.Marshal(raw)
		if err == nil && len(rawBytes) > 0 {
			var msgTs int64
			if len(ts) > 0 && !ts[0].IsZero() {
				msgTs = ts[0].Unix()
			} else {
				msgTs = time.Now().Unix()
			}
			_, _ = a.localDB.Exec(`INSERT OR REPLACE INTO watui_media_cache (id, chat_id, raw_message, timestamp) VALUES (?, ?, ?, ?)`,
				id, chatID, rawBytes, msgTs)
			_, _ = a.localDB.Exec(`DELETE FROM watui_media_cache WHERE id NOT IN (SELECT id FROM watui_media_cache ORDER BY timestamp DESC LIMIT 1000)`)
		}
	}
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
	dbKey, err := security.ResolveDatabaseKey(cfg.DBPath, dbDir)
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

	sharedDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	sharedDB.SetMaxOpenConns(10)
	sharedDB.SetMaxIdleConns(5)
	sharedDB.SetConnMaxLifetime(10 * time.Minute)

	// Explicitly configure SQLite pragmas on the shared connection
	_, _ = sharedDB.Exec(`
		PRAGMA busy_timeout = 30000;
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
	`)

	container := sqlstore.NewWithDB(sharedDB, "sqlite3", dbLog)
	if err := container.Upgrade(ctx); err != nil {
		_ = sharedDB.Close()
		if strings.Contains(err.Error(), "file is not a database") {
			return nil, fmt.Errorf("failed to open encrypted database %q: encryption key mismatch or file corrupted (is this database from another machine or user? If your machine-id or hostname changed across reboot, set WATUI_MACHINE_ID or check %s): %w", cfg.DBPath, filepath.Join(dbDir, security.MachineIDFileName), err)
		}
		return nil, fmt.Errorf("failed to upgrade session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	whatsmeow.KeepAliveMaxFailTime = 15 * time.Second
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.InitialAutoReconnect = true
	client.EnableAutoReconnect = true
	client.EmitAppStateEventsOnFullSync = true
	localDB := sharedDB

	security.EnsureSecurePermissions(cfg.DBPath)
	if localDB != nil {
		_, _ = localDB.Exec(`CREATE TABLE IF NOT EXISTS watui_groups (
			jid TEXT PRIMARY KEY,
			name TEXT
		);
		CREATE TABLE IF NOT EXISTS watui_chat_ephemeral (
			chat_id TEXT PRIMARY KEY,
			timer INTEGER NOT NULL,
			setting_timestamp INTEGER NOT NULL
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
			quoted_id TEXT,
			quoted_text TEXT,
			quoted_sender TEXT,
			is_edit BOOLEAN,
			raw_message BLOB
		);
		CREATE INDEX IF NOT EXISTS idx_watui_unread_chat_id ON watui_unread_messages(chat_id);
		CREATE INDEX IF NOT EXISTS idx_watui_unread_chat_ts ON watui_unread_messages(chat_id, timestamp DESC);
		CREATE TABLE IF NOT EXISTS watui_media_cache (
			id TEXT PRIMARY KEY,
			chat_id TEXT NOT NULL,
			raw_message BLOB NOT NULL,
			timestamp INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_watui_media_cache_ts ON watui_media_cache(timestamp DESC);
		CREATE TABLE IF NOT EXISTS watui_messages (
			id TEXT PRIMARY KEY,
			chat_id TEXT NOT NULL,
			chat_name TEXT,
			sender TEXT NOT NULL,
			sender_name TEXT,
			timestamp INTEGER NOT NULL,
			body TEXT,
			type TEXT,
			is_from_me BOOLEAN,
			quoted_id TEXT,
			quoted_text TEXT,
			quoted_sender TEXT,
			is_edit BOOLEAN,
			is_revoke BOOLEAN,
			raw_message BLOB,
			status TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_watui_messages_chat_ts ON watui_messages(chat_id, timestamp DESC);
		CREATE TABLE IF NOT EXISTS watui_message_receipts (
			message_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			user_jid TEXT NOT NULL,
			user_name TEXT,
			receipt_type TEXT NOT NULL,
			timestamp INTEGER NOT NULL,
			PRIMARY KEY (message_id, user_jid, receipt_type)
		);
		CREATE INDEX IF NOT EXISTS idx_watui_receipts_msg ON watui_message_receipts(message_id);`)
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN chat_name TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN raw_message BLOB;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN quoted_id TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN quoted_text TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN quoted_sender TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN is_edit BOOLEAN;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN is_revoke BOOLEAN;")
		_, _ = localDB.Exec("ALTER TABLE watui_unread_messages ADD COLUMN status TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN quoted_id TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN quoted_text TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN quoted_sender TEXT;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN is_edit BOOLEAN;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN is_revoke BOOLEAN;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN raw_message BLOB;")
		_, _ = localDB.Exec("ALTER TABLE watui_messages ADD COLUMN status TEXT;")

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

		// Prune any massive historical backlog from chats with > 100 messages to keep DB lean and fast
		_, _ = localDB.Exec(`
			DELETE FROM watui_unread_messages 
			WHERE id NOT IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (PARTITION BY chat_id ORDER BY timestamp DESC) as rn 
					FROM watui_unread_messages
				) WHERE rn <= 100
			);
		`)
	}

	adapterCtx, cancel := context.WithCancel(ctx)
	adapter := &Adapter{
		client:             client,
		container:          container,
		localDB:            localDB,
		config:             cfg,
		appConfig:          cfg.AppConfig,
		currentStatus:      domain.StatusDisconnected,
		mediaCache:         make(map[string]*waE2E.Message),
		archivedChats:      make(map[string]bool),
		groupParticipants:  make(map[string][]domain.Contact),
		ephemeralTimers:    make(map[string]uint32),
		ephemeralSettingTS: make(map[string]int64),
		presences:          make(map[string]domain.UserPresence),
		ctx:                adapterCtx,
		cancel:             cancel,
	}

	if localDB != nil {
		adapter.loadEphemeralChats()
	}

	client.AddEventHandler(adapter.handleEvent)

	// Preload local SQLite contacts, cached groups, and archived chats immediately
	if adapter.IsLoggedIn() {
		adapter.ensureLIDMap()
		initial := adapter.fetchLocalContacts(adapterCtx)
		if len(initial) > 0 {
			adapter.contactsMu.Lock()
			adapter.updateContactsIndexLocked(initial)
			adapter.contactsMu.Unlock()
		}
		adapter.loadArchivedChats()
		adapter.loadMutedChats()
		adapter.loadEphemeralChats()
	}

	go adapter.startSleepWakeMonitor(adapterCtx)
	go adapter.startAppStateSyncMonitor(adapterCtx)

	return adapter, nil
}

// IsLoggedIn checks whether valid session credentials exist in SQLite.
func (a *Adapter) IsLoggedIn() bool {
	return a.client != nil && a.client.Store != nil && a.client.Store.ID != nil
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
	a.statusHandlers = append(a.statusHandlers, handler)
	st := a.currentStatus
	a.mu.Unlock()

	if st != "" {
		handler(st)
	}
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

// Disconnect gracefully shuts down the connection.
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
		if a.container != nil {
			_ = a.container.Close()
		} else if a.localDB != nil {
			_ = a.localDB.Close()
		}
	}()

	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		if a.container != nil {
			_ = a.container.Close()
		} else if a.localDB != nil {
			_ = a.localDB.Close()
		}
	}

	a.setStatus(domain.StatusDisconnected)
}

// Logout unlinks the device session with WhatsApp servers, disconnects, and clears local session data.
func (a *Adapter) Logout(ctx context.Context) error {
	var logoutErr error
	if a.client != nil && a.IsLoggedIn() {
		if !a.client.IsConnected() {
			_ = a.client.Connect()
			_ = a.client.WaitForConnection(5 * time.Second)
		}
		if a.client.IsConnected() {
			logoutErr = a.client.Logout(ctx)
		}
		if logoutErr != nil || !a.client.IsConnected() {
			// If server-side unlink failed or client is offline, force delete the device store
			if a.client.Store != nil {
				_ = a.client.Store.Delete(ctx)
			}
			a.client.Disconnect()
		}
	}

	a.Disconnect()
	a.setStatus(domain.StatusLoggedOut)
	return logoutErr
}

// ResetConnection disconnects the current socket and forces an immediate reconnection.
func (a *Adapter) ResetConnection() {
	if a.client == nil || !a.IsLoggedIn() {
		return
	}
	a.resetMu.Lock()
	now := time.Now()
	if now.Sub(a.lastResetTime) < 3*time.Second {
		a.resetMu.Unlock()
		return
	}
	a.lastResetTime = now
	a.resetMu.Unlock()

	a.setStatus(domain.StatusConnecting)
	if a.client.IsConnected() {
		a.client.ResetConnection()
	} else {
		go func() {
			_ = a.client.Connect()
		}()
	}
}

func (a *Adapter) startSleepWakeMonitor(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastWall := time.Now().UnixMilli()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nowWall := time.Now().UnixMilli()
			elapsed := nowWall - lastWall
			lastWall = nowWall
			// If more than 5 seconds elapsed between 2-second ticks, system was suspended (slept).
			if elapsed > 5000 {
				a.handleSystemResume(elapsed)
			}
		}
	}
}

func (a *Adapter) startAppStateSyncMonitor(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.client != nil && a.client.IsConnected() && a.IsLoggedIn() {
				_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularLow)
			}
		}
	}
}

func (a *Adapter) handleSystemResume(elapsedMs int64) {
	if a.client == nil || !a.IsLoggedIn() {
		return
	}
	if !a.resuming.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.resuming.Store(false)
		a.setStatus(domain.StatusConnecting)
		// Give local OS network stack a moment (1.5s) to re-establish Wi-Fi / DHCP routes
		time.Sleep(1500 * time.Millisecond)
		a.ResetConnection()
	}()
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

// ResolveLIDToPhone maps an internal WhatsApp LID to a phone number using SQLite/in-memory cache.
func (a *Adapter) ResolveLIDToPhone(lidUser string) string {
	if a.localDB == nil || lidUser == "" {
		return ""
	}
	if idx := strings.Index(lidUser, ":"); idx != -1 {
		lidUser = lidUser[:idx]
	}
	a.ensureLIDMap()
	a.lidMapMu.RLock()
	var pn string
	if a.lidMapCache != nil {
		pn = a.lidMapCache[lidUser]
	}
	a.lidMapMu.RUnlock()
	return pn
}

// ResolvePhoneToLID maps a phone number user to its internal WhatsApp LID using SQLite/in-memory cache.
func (a *Adapter) ResolvePhoneToLID(phoneUser string) string {
	if a.localDB == nil || phoneUser == "" {
		return ""
	}
	if idx := strings.Index(phoneUser, ":"); idx != -1 {
		phoneUser = phoneUser[:idx]
	}
	a.ensureLIDMap()
	a.lidMapMu.RLock()
	var lid string
	if a.phoneToLIDCache != nil {
		lid = a.phoneToLIDCache[phoneUser]
	}
	a.lidMapMu.RUnlock()
	return lid
}

func (a *Adapter) loadArchivedChats() {
	if a.localDB == nil {
		return
	}
	rows, err := a.localDB.Query("SELECT chat_jid FROM whatsmeow_chat_settings WHERE archived = 1")
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	a.ensureLIDMap()
	a.lidMapMu.RLock()
	lidMap := a.lidMapCache
	phoneMap := a.phoneToLIDCache

	archived := make(map[string]bool)
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err == nil && jid != "" {
			archived[jid] = true
			clean := jid
			if idx := strings.Index(clean, ":"); idx != -1 {
				if atIdx := strings.Index(clean, "@"); atIdx != -1 {
					clean = clean[:idx] + clean[atIdx:]
				}
			}
			archived[clean] = true
			if strings.HasSuffix(clean, "@lid") {
				lidUser := strings.TrimSuffix(clean, "@lid")
				if pn, ok := lidMap[lidUser]; ok && pn != "" {
					archived[pn+"@s.whatsapp.net"] = true
				}
			} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
				phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
				if lid, ok := phoneMap[phoneUser]; ok && lid != "" {
					archived[lid+"@lid"] = true
				}
			}
		}
	}
	a.lidMapMu.RUnlock()
	_ = rows.Err()

	a.archivedMu.Lock()
	oldArchived := a.archivedChats
	a.archivedChats = archived
	a.archivedMu.Unlock()

	// Notify any status changes discovered during load/sync
	if oldArchived != nil {
		for cID := range oldArchived {
			if !archived[cID] {
				a.notifyChatArchived(cID, false)
			}
		}
		for cID := range archived {
			if !oldArchived[cID] {
				a.notifyChatArchived(cID, true)
			}
		}
	}
}

// OnChatArchived registers a listener triggered when a chat's archive status changes.
func (a *Adapter) OnChatArchived(handler func(chatID string, archived bool)) {
	a.archivedMu.Lock()
	defer a.archivedMu.Unlock()
	a.archivedHandlers = append(a.archivedHandlers, handler)
}

func (a *Adapter) notifyChatArchived(chatID string, archived bool) {
	if chatID == "" {
		return
	}
	a.archivedMu.RLock()
	handlers := make([]func(chatID string, archived bool), len(a.archivedHandlers))
	copy(handlers, a.archivedHandlers)
	a.archivedMu.RUnlock()

	for _, h := range handlers {
		h(chatID, archived)
	}
}

// IsChatArchived checks whether the specified chat JID is archived in WhatsApp.
func (a *Adapter) IsChatArchived(chatID string) bool {
	if chatID == "" {
		return false
	}
	clean := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 {
			clean = clean[:idx] + clean[atIdx:]
		}
	}

	a.archivedMu.RLock()
	defer a.archivedMu.RUnlock()
	if a.archivedChats != nil {
		if a.archivedChats[chatID] || a.archivedChats[clean] {
			return true
		}
		if strings.HasSuffix(clean, "@lid") {
			lidUser := strings.TrimSuffix(clean, "@lid")
			lidMap := a.getLIDMap()
			if pn, ok := lidMap[lidUser]; ok && pn != "" {
				if a.archivedChats[pn+"@s.whatsapp.net"] {
					return true
				}
			}
		} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
			phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
			lid := a.ResolvePhoneToLID(phoneUser)
			if lid != "" {
				if a.archivedChats[lid+"@lid"] {
					return true
				}
			}
		}
	}
	return false
}

// GetArchivedChats returns a copy of all currently known archived chat JIDs.
func (a *Adapter) GetArchivedChats() map[string]bool {
	a.archivedMu.RLock()
	defer a.archivedMu.RUnlock()
	res := make(map[string]bool, len(a.archivedChats))
	for k, v := range a.archivedChats {
		res[k] = v
	}
	return res
}

// SetChatArchived archives or unarchives the specified chat JID in WhatsApp.
func (a *Adapter) SetChatArchived(ctx context.Context, chatID string, archived bool) error {
	jid, err := NormalizeJID(chatID)
	if err != nil {
		return err
	}
	jid = jid.ToNonAD()
	clean := jid.String()
	rawJID := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 {
			clean = clean[:idx] + clean[atIdx:]
		}
	}

	var altJID types.JID
	if strings.HasSuffix(clean, "@lid") {
		lidUser := strings.TrimSuffix(clean, "@lid")
		if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
			altJID = types.NewJID(pn, types.DefaultUserServer)
		}
	} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
		phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
		if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
			altJID = types.NewJID(lid, types.HiddenUserServer)
		}
	}

	a.archivedMu.Lock()
	if a.archivedChats == nil {
		a.archivedChats = make(map[string]bool)
	}
	if archived {
		a.archivedChats[chatID] = true
		a.archivedChats[clean] = true
		if !altJID.IsEmpty() {
			a.archivedChats[altJID.String()] = true
		}
	} else {
		delete(a.archivedChats, chatID)
		delete(a.archivedChats, clean)
		if !altJID.IsEmpty() {
			delete(a.archivedChats, altJID.String())
		}
	}
	a.archivedMu.Unlock()

	if a.client != nil && a.client.Store != nil && a.client.Store.ChatSettings != nil {
		_ = a.client.Store.ChatSettings.PutArchived(ctx, jid, archived)
		if !altJID.IsEmpty() {
			_ = a.client.Store.ChatSettings.PutArchived(ctx, altJID, archived)
		}
	}
	if a.localDB != nil {
		archivedVal := 0
		if archived {
			archivedVal = 1
		}
		_, _ = a.localDB.Exec(`UPDATE whatsmeow_chat_settings SET archived = ? WHERE chat_jid = ? OR chat_jid = ?`, archivedVal, clean, chatID)
		if !altJID.IsEmpty() {
			_, _ = a.localDB.Exec(`UPDATE whatsmeow_chat_settings SET archived = ? WHERE chat_jid = ?`, archivedVal, altJID.String())
		}
		if archived {
			var count int
			_ = a.localDB.QueryRow(`SELECT COUNT(*) FROM whatsmeow_chat_settings WHERE chat_jid = ?`, clean).Scan(&count)
			if count == 0 {
				var ourJID string
				if a.client != nil && a.client.Store != nil && a.client.Store.ID != nil {
					ourJID = a.client.Store.ID.ToNonAD().String()
				} else {
					_ = a.localDB.QueryRow(`SELECT our_jid FROM whatsmeow_chat_settings LIMIT 1`).Scan(&ourJID)
				}
				if ourJID != "" {
					_, _ = a.localDB.Exec(`INSERT OR IGNORE INTO whatsmeow_chat_settings (our_jid, chat_jid, archived) VALUES (?, ?, 1)`, ourJID, clean)
				}
			}
		}
	}

	a.notifyChatArchived(clean, archived)
	if rawJID != clean {
		a.notifyChatArchived(rawJID, archived)
	}
	if !altJID.IsEmpty() {
		a.notifyChatArchived(altJID.String(), archived)
	}

	if a.client == nil {
		return errors.New("whatsapp client not initialized")
	}

	patch := appstate.BuildArchive(jid, archived, time.Time{}, nil)
	sendErr := a.client.SendAppState(ctx, patch)
	if sendErr != nil {
		// If SendAppState failed (e.g. 409 conflict, version mismatch, or LTHash desync),
		// resync app state to catch up on latest version/hash and retry sending the patch.
		_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularLow)
		sendErr = a.client.SendAppState(ctx, patch)
	}
	if sendErr != nil && strings.Contains(sendErr.Error(), "failed to fetch app state after sending update") {
		// The patch was accepted by WhatsApp server, post-send fetch failed
		_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularLow)
		sendErr = nil
	}
	if !altJID.IsEmpty() {
		patchAlt := appstate.BuildArchive(altJID, archived, time.Time{}, nil)
		altErr := a.client.SendAppState(ctx, patchAlt)
		if altErr != nil {
			_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularLow)
			_ = a.client.SendAppState(ctx, patchAlt)
		}
	}
	return sendErr
}

func (a *Adapter) loadMutedChats() {
	if a.localDB == nil {
		return
	}
	rows, err := a.localDB.Query("SELECT chat_jid, muted_until FROM whatsmeow_chat_settings WHERE muted_until < 0 OR muted_until > ?", time.Now().Unix())
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	a.ensureLIDMap()
	a.lidMapMu.RLock()
	lidMap := a.lidMapCache
	muted := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var mutedUntil int64
		if err := rows.Scan(&jid, &mutedUntil); err == nil && jid != "" {
			var until time.Time
			if mutedUntil < 0 {
				until = store.MutedForever
			} else if mutedUntil > 0 {
				until = time.Unix(mutedUntil, 0)
			}
			muted[jid] = until
			clean := jid
			if idx := strings.Index(clean, ":"); idx != -1 {
				if atIdx := strings.Index(clean, "@"); atIdx != -1 {
					clean = clean[:idx] + clean[atIdx:]
				}
			}
			muted[clean] = until
			if strings.HasSuffix(clean, "@lid") && lidMap != nil {
				lidUser := strings.TrimSuffix(clean, "@lid")
				if pn, ok := lidMap[lidUser]; ok && pn != "" {
					muted[pn+"@s.whatsapp.net"] = until
				}
			}
		}
	}
	a.lidMapMu.RUnlock()
	a.mutedMu.Lock()
	a.mutedChats = muted
	a.mutedMu.Unlock()
}

// IsChatMuted checks whether the specified chat JID is muted globally in WhatsApp.
func (a *Adapter) IsChatMuted(chatID string) bool {
	if chatID == "" {
		return false
	}
	a.mutedMu.RLock()
	defer a.mutedMu.RUnlock()
	if a.mutedChats == nil {
		return false
	}
	until, exists := a.mutedChats[chatID]
	if !exists {
		clean := chatID
		if idx := strings.Index(clean, ":"); idx != -1 {
			if atIdx := strings.Index(clean, "@"); atIdx != -1 {
				clean = clean[:idx] + clean[atIdx:]
			}
		}
		until, exists = a.mutedChats[clean]
		if !exists && strings.HasSuffix(clean, "@lid") {
			lidUser := strings.TrimSuffix(clean, "@lid")
			lidMap := a.getLIDMap()
			if pn, ok := lidMap[lidUser]; ok && pn != "" {
				until, exists = a.mutedChats[pn+"@s.whatsapp.net"]
			}
		} else if !exists && strings.HasSuffix(clean, "@s.whatsapp.net") {
			pn := strings.TrimSuffix(clean, "@s.whatsapp.net")
			if lid := a.ResolvePhoneToLID(pn); lid != "" {
				until, exists = a.mutedChats[lid+"@lid"]
			}
		}
	}
	if !exists {
		if jid, err := NormalizeJID(chatID); err == nil && a.client != nil && a.client.Store.ChatSettings != nil {
			if settings, err := a.client.Store.ChatSettings.GetChatSettings(context.Background(), jid); err == nil && settings.Found {
				until = settings.MutedUntil
				exists = true
			}
		}
	}
	if !exists {
		return false
	}
	return until.After(time.Now())
}

// GetMutedChats returns a copy of all currently globally muted chat JIDs.
func (a *Adapter) GetMutedChats() map[string]bool {
	a.mutedMu.RLock()
	defer a.mutedMu.RUnlock()
	res := make(map[string]bool, len(a.mutedChats))
	now := time.Now()
	for k, until := range a.mutedChats {
		if until.After(now) {
			res[k] = true
		}
	}
	return res
}

// SetChatMuted mutes or unmutes the specified chat JID globally in WhatsApp.
func (a *Adapter) SetChatMuted(ctx context.Context, chatID string, muted bool, duration time.Duration) error {
	jid, err := NormalizeJID(chatID)
	if err != nil {
		return err
	}
	clean := jid.String()
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 {
			clean = clean[:idx] + clean[atIdx:]
		}
	}

	var until time.Time
	if muted {
		if duration <= 0 {
			until = store.MutedForever
		} else {
			until = time.Now().Add(duration)
		}
	}

	a.mutedMu.Lock()
	if a.mutedChats == nil {
		a.mutedChats = make(map[string]time.Time)
	}
	if muted {
		a.mutedChats[chatID] = until
		a.mutedChats[clean] = until
	} else {
		delete(a.mutedChats, chatID)
		delete(a.mutedChats, clean)
	}
	a.mutedMu.Unlock()

	if a.client != nil && a.client.Store.ChatSettings != nil {
		_ = a.client.Store.ChatSettings.PutMutedUntil(ctx, jid, until)
	}

	if a.client == nil {
		return errors.New("whatsapp client not initialized")
	}

	patch := appstate.BuildMute(jid, muted, duration)
	return a.client.SendAppState(ctx, patch)
}

func (a *Adapter) loadEphemeralChats() {
	if a.localDB == nil {
		return
	}
	// 1. Load from watui_chat_ephemeral
	rows, err := a.localDB.Query("SELECT chat_id, timer, setting_timestamp FROM watui_chat_ephemeral WHERE timer > 0")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cid string
			var timer uint32
			var ts int64
			if err := rows.Scan(&cid, &timer, &ts); err == nil && cid != "" && timer > 0 {
				a.updateChatEphemeralTimer(cid, timer, ts)
			}
		}
	}

	// 2. Scan recent messages from watui_messages to automatically adopt active disappearing timers for all existing chats
	mRows, err := a.localDB.Query(`
		SELECT chat_id, raw_message FROM watui_messages 
		WHERE raw_message IS NOT NULL 
		ORDER BY timestamp DESC
	`)
	if err == nil {
		defer mRows.Close()
		seen := make(map[string]bool)
		for mRows.Next() {
			var cid string
			var rawBytes []byte
			if err := mRows.Scan(&cid, &rawBytes); err == nil && len(rawBytes) > 0 {
				if seen[cid] {
					continue
				}
				seen[cid] = true
				var m waE2E.Message
				if err := proto.Unmarshal(rawBytes, &m); err == nil {
					ci := extractContextInfo(&m)
					if ci != nil && ci.GetExpiration() > 0 {
						ts := ci.GetEphemeralSettingTimestamp()
						if ts <= 0 {
							ts = time.Now().Unix()
						}
						a.updateChatEphemeralTimer(cid, ci.GetExpiration(), ts)
					}
				}
			}
		}
	}
}

// GetChatEphemeralTimer returns the disappearing message timer (in seconds) for a chat, or 0 if disabled.
func (a *Adapter) GetChatEphemeralTimer(chatID string) uint32 {
	if chatID == "" {
		return 0
	}
	t, _ := a.getChatEphemeralTimer(context.Background(), chatID)
	return t
}

// GetEphemeralChats returns a map of all chats with disappearing messages enabled along with their timer in seconds.
func (a *Adapter) GetEphemeralChats() map[string]uint32 {
	a.ephemeralMu.RLock()
	defer a.ephemeralMu.RUnlock()
	res := make(map[string]uint32, len(a.ephemeralTimers))
	for k, timer := range a.ephemeralTimers {
		if timer > 0 {
			res[k] = timer
		}
	}
	return res
}

// OnChatEphemeral registers a listener triggered when a chat's disappearing timer changes.
func (a *Adapter) OnChatEphemeral(handler func(chatID string, timer uint32)) {
	a.ephemeralMu.Lock()
	defer a.ephemeralMu.Unlock()
	a.ephemeralHandlers = append(a.ephemeralHandlers, handler)
}

func (a *Adapter) notifyChatEphemeral(chatID string, timer uint32) {
	a.ephemeralMu.RLock()
	handlers := make([]func(string, uint32), len(a.ephemeralHandlers))
	copy(handlers, a.ephemeralHandlers)
	a.ephemeralMu.RUnlock()
	for _, h := range handlers {
		h(chatID, timer)
	}
}

// SetChatDisappearingTimer changes the disappearing messages timer on WhatsApp for a chat.
func (a *Adapter) SetChatDisappearingTimer(ctx context.Context, chatID string, timer time.Duration) error {
	if a.client == nil {
		return errors.New("whatsapp client not initialized")
	}
	chatJID, err := NormalizeJID(chatID)
	if err != nil {
		return err
	}
	clean := chatJID.ToNonAD().String()
	err = a.client.SetDisappearingTimer(ctx, chatJID, timer, time.Now())
	if err != nil {
		return err
	}
	a.updateChatEphemeralTimer(clean, uint32(timer.Seconds()), time.Now().Unix())
	return nil
}

func (a *Adapter) updateChatEphemeralTimer(chatID string, timer uint32, settingTS int64) {
	if chatID == "" {
		return
	}
	clean := chatID
	if parsed, err := NormalizeJID(chatID); err == nil {
		clean = parsed.ToNonAD().String()
	} else if parsed, err := types.ParseJID(chatID); err == nil {
		clean = parsed.ToNonAD().String()
	}

	var allIDs []string
	allIDs = append(allIDs, clean)
	if chatID != clean {
		allIDs = append(allIDs, chatID)
	}

	// Bidirectional LID <-> Phone resolution so DMs match regardless of representation
	if strings.HasSuffix(clean, "@lid") {
		lidUser := strings.TrimSuffix(clean, "@lid")
		if idx := strings.Index(lidUser, ":"); idx != -1 {
			lidUser = lidUser[:idx]
		}
		if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
			allIDs = append(allIDs, pn+"@s.whatsapp.net")
		}
	} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
		pn := strings.TrimSuffix(clean, "@s.whatsapp.net")
		if idx := strings.Index(pn, ":"); idx != -1 {
			pn = pn[:idx]
		}
		if lid := a.ResolvePhoneToLID(pn); lid != "" {
			allIDs = append(allIDs, lid+"@lid")
		}
	}

	a.ephemeralMu.Lock()
	if a.ephemeralTimers == nil {
		a.ephemeralTimers = make(map[string]uint32)
	}
	if a.ephemeralSettingTS == nil {
		a.ephemeralSettingTS = make(map[string]int64)
	}
	prev := a.ephemeralTimers[clean]
	for _, id := range allIDs {
		if timer > 0 {
			a.ephemeralTimers[id] = timer
			a.ephemeralSettingTS[id] = settingTS
		} else {
			delete(a.ephemeralTimers, id)
			delete(a.ephemeralSettingTS, id)
		}
	}
	a.ephemeralMu.Unlock()

	if a.localDB != nil {
		for _, id := range allIDs {
			if timer > 0 {
				_, _ = a.localDB.Exec(`
					INSERT OR REPLACE INTO watui_chat_ephemeral (chat_id, timer, setting_timestamp)
					VALUES (?, ?, ?)
				`, id, timer, settingTS)
			} else {
				_, _ = a.localDB.Exec(`DELETE FROM watui_chat_ephemeral WHERE chat_id = ?`, id)
			}
		}
	}

	if prev != timer {
		for _, id := range allIDs {
			a.notifyChatEphemeral(id, timer)
		}
	}
}

func (a *Adapter) getChatEphemeralTimer(ctx context.Context, chatID string) (uint32, int64) {
	if chatID == "" {
		return 0, 0
	}
	var chatJID types.JID
	clean := chatID
	if parsed, err := NormalizeJID(chatID); err == nil {
		chatJID = parsed.ToNonAD()
		clean = chatJID.String()
	} else if parsed, err := types.ParseJID(chatID); err == nil {
		chatJID = parsed.ToNonAD()
		clean = chatJID.String()
	}

	var altID string
	if strings.HasSuffix(clean, "@lid") {
		lidUser := strings.TrimSuffix(clean, "@lid")
		if idx := strings.Index(lidUser, ":"); idx != -1 {
			lidUser = lidUser[:idx]
		}
		if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
			altID = pn + "@s.whatsapp.net"
		}
	} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
		pn := strings.TrimSuffix(clean, "@s.whatsapp.net")
		if idx := strings.Index(pn, ":"); idx != -1 {
			pn = pn[:idx]
		}
		if lid := a.ResolvePhoneToLID(pn); lid != "" {
			altID = lid + "@lid"
		}
	}

	a.ephemeralMu.RLock()
	timer, ok := a.ephemeralTimers[clean]
	if !ok {
		timer, ok = a.ephemeralTimers[chatID]
	}
	if !ok && altID != "" {
		timer, ok = a.ephemeralTimers[altID]
	}
	settingTS := a.ephemeralSettingTS[clean]
	if settingTS == 0 {
		settingTS = a.ephemeralSettingTS[chatID]
	}
	if settingTS == 0 && altID != "" {
		settingTS = a.ephemeralSettingTS[altID]
	}
	a.ephemeralMu.RUnlock()

	if ok {
		return timer, settingTS
	}

	// Check local database
	if a.localDB != nil {
		var dbTimer uint32
		var dbTS int64
		err := a.localDB.QueryRowContext(ctx, "SELECT timer, setting_timestamp FROM watui_chat_ephemeral WHERE chat_id = ? OR chat_id = ? OR chat_id = ?", clean, chatID, altID).Scan(&dbTimer, &dbTS)
		if err == nil && dbTimer > 0 {
			a.updateChatEphemeralTimer(clean, dbTimer, dbTS)
			return dbTimer, dbTS
		}
	}

	// Chat history fallback: scan recent messages in this chat to detect if other clients have disappearing mode on
	if a.localDB != nil {
		var rawBytes []byte
		row := a.localDB.QueryRowContext(ctx, `
			SELECT raw_message FROM watui_messages 
			WHERE (chat_id = ? OR chat_id = ? OR chat_id = ?) AND raw_message IS NOT NULL 
			ORDER BY timestamp DESC LIMIT 1
		`, clean, chatID, altID)
		if err := row.Scan(&rawBytes); err == nil && len(rawBytes) > 0 {
			var m waE2E.Message
			if err := proto.Unmarshal(rawBytes, &m); err == nil {
				ci := extractContextInfo(&m)
				if ci != nil && ci.GetExpiration() > 0 {
					exp := ci.GetExpiration()
					ts := ci.GetEphemeralSettingTimestamp()
					if ts <= 0 {
						ts = time.Now().Unix()
					}
					a.updateChatEphemeralTimer(clean, exp, ts)
					return exp, ts
				}
			}
		}
	}

	// Cache zero timer so subsequent outgoing messages do not re-query
	a.updateChatEphemeralTimer(clean, 0, 0)
	return 0, 0
}

func (a *Adapter) prepareOutgoingMessage(ctx context.Context, recipientJID types.JID, waMsg *waE2E.Message) *waE2E.Message {
	if waMsg == nil {
		return nil
	}
	if waMsg.EphemeralMessage != nil {
		return waMsg
	}

	timer, settingTS := a.getChatEphemeralTimer(ctx, recipientJID.ToNonAD().String())
	if timer == 0 {
		return waMsg
	}
	if settingTS <= 0 {
		settingTS = time.Now().Unix()
	}

	mode := &waE2E.DisappearingMode{
		Initiator:     waE2E.DisappearingMode_CHANGED_IN_CHAT.Enum(),
		Trigger:       waE2E.DisappearingMode_CHAT_SETTING.Enum(),
		InitiatedByMe: proto.Bool(true),
	}

	innerMsg := waMsg

	if innerMsg.Conversation != nil {
		ctxInfo := &waE2E.ContextInfo{
			Expiration:                proto.Uint32(timer),
			EphemeralSettingTimestamp: proto.Int64(settingTS),
			DisappearingMode:          mode,
		}
		innerMsg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        innerMsg.Conversation,
				ContextInfo: ctxInfo,
			},
		}
	} else if innerMsg.ExtendedTextMessage != nil {
		if innerMsg.ExtendedTextMessage.ContextInfo == nil {
			innerMsg.ExtendedTextMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.ExtendedTextMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.ExtendedTextMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.ExtendedTextMessage.ContextInfo.DisappearingMode = mode
	} else if innerMsg.ImageMessage != nil {
		if innerMsg.ImageMessage.ContextInfo == nil {
			innerMsg.ImageMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.ImageMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.ImageMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.ImageMessage.ContextInfo.DisappearingMode = mode
	} else if innerMsg.VideoMessage != nil {
		if innerMsg.VideoMessage.ContextInfo == nil {
			innerMsg.VideoMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.VideoMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.VideoMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.VideoMessage.ContextInfo.DisappearingMode = mode
	} else if innerMsg.AudioMessage != nil {
		if innerMsg.AudioMessage.ContextInfo == nil {
			innerMsg.AudioMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.AudioMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.AudioMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.AudioMessage.ContextInfo.DisappearingMode = mode
	} else if innerMsg.DocumentMessage != nil {
		if innerMsg.DocumentMessage.ContextInfo == nil {
			innerMsg.DocumentMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.DocumentMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.DocumentMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.DocumentMessage.ContextInfo.DisappearingMode = mode
	} else if innerMsg.StickerMessage != nil {
		if innerMsg.StickerMessage.ContextInfo == nil {
			innerMsg.StickerMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		innerMsg.StickerMessage.ContextInfo.Expiration = proto.Uint32(timer)
		innerMsg.StickerMessage.ContextInfo.EphemeralSettingTimestamp = proto.Int64(settingTS)
		innerMsg.StickerMessage.ContextInfo.DisappearingMode = mode
	}

	return &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: innerMsg,
		},
	}
}

// DeleteMessage deletes a message locally ("delete for me") or revokes it on WhatsApp servers for everyone ("delete for all").
func (a *Adapter) DeleteMessage(ctx context.Context, chatID string, messageID string, deleteForEveryone bool, sender ...string) error {
	if deleteForEveryone {
		if a.client == nil {
			return errors.New("whatsapp client not initialized")
		}
		if !a.client.IsConnected() {
			if !a.client.WaitForConnection(4 * time.Second) {
				go func() { _ = a.client.Connect() }()
				return errors.New("connection lost: reconnecting to WhatsApp, please retry in a moment")
			}
		}

		chatJID, err := NormalizeJID(chatID)
		if err != nil {
			return fmt.Errorf("invalid recipient %q: %w", chatID, err)
		}

		var senderJID types.JID
		if len(sender) > 0 && sender[0] != "" {
			sJID, err := NormalizeJID(sender[0])
			if err == nil {
				senderJID = sJID
			}
		}

		revokeMsg := a.client.BuildRevoke(chatJID, senderJID, types.MessageID(messageID))
		_, err = a.sendMessageWithRetry(ctx, chatJID, revokeMsg)
		if err != nil {
			return fmt.Errorf("failed to delete for everyone: %w", err)
		}
	}

	if a.localDB != nil {
		if deleteForEveryone {
			var origBody, origSender, origSenderName, origChatName string
			var origTimestamp int64
			var origIsFromMe bool
			row := a.localDB.QueryRow("SELECT chat_name, body, sender, sender_name, timestamp, is_from_me FROM watui_messages WHERE id = ?", messageID)
			_ = row.Scan(&origChatName, &origBody, &origSender, &origSenderName, &origTimestamp, &origIsFromMe)
			if origBody == "" {
				row2 := a.localDB.QueryRow("SELECT chat_name, body, sender, sender_name, timestamp, is_from_me FROM watui_unread_messages WHERE id = ?", messageID)
				_ = row2.Scan(&origChatName, &origBody, &origSender, &origSenderName, &origTimestamp, &origIsFromMe)
			}
			_, _ = a.localDB.Exec("UPDATE watui_messages SET is_revoke = 1 WHERE id = ?", messageID)
			_, _ = a.localDB.Exec("UPDATE watui_unread_messages SET is_revoke = 1 WHERE id = ?", messageID)

			ts := time.Now()
			if origTimestamp > 0 {
				ts = time.Unix(origTimestamp, 0)
			}
			revokeDMsg := domain.Message{
				ID:           messageID,
				ChatID:       chatID,
				ChatName:     origChatName,
				Sender:       origSender,
				SenderName:   origSenderName,
				Body:         origBody,
				Timestamp:    ts,
				IsFromMe:     origIsFromMe,
				Type:         domain.MessageTypeText,
				QuotedID:     messageID,
				QuotedText:   origBody,
				QuotedSender: origSender,
				IsRevoke:     true,
			}
			a.notifyMessage(revokeDMsg)
		} else {
			_, _ = a.localDB.Exec("DELETE FROM watui_messages WHERE id = ?", messageID)
			_, _ = a.localDB.Exec("DELETE FROM watui_unread_messages WHERE id = ?", messageID)
		}
	}

	return nil
}

// EditMessage edits an already sent message on WhatsApp servers and updates local cache.
func (a *Adapter) EditMessage(ctx context.Context, chatID string, messageID string, newText string) error {
	if a.client == nil {
		return errors.New("whatsapp client not initialized")
	}
	if !a.client.IsConnected() {
		if !a.client.WaitForConnection(4 * time.Second) {
			go func() { _ = a.client.Connect() }()
			return errors.New("connection lost: reconnecting to WhatsApp, please retry in a moment")
		}
	}

	if strings.HasPrefix(strings.TrimSpace(newText), "file://") {
		return errors.New("cannot attach files while editing a message")
	}
	if a.localDB != nil {
		var msgType string
		err := a.localDB.QueryRowContext(ctx, "SELECT type FROM watui_messages WHERE id = ?", messageID).Scan(&msgType)
		if err == nil && msgType != "" && msgType != string(domain.MessageTypeText) {
			return errors.New("cannot edit media or attachment messages")
		}
	}

	chatJID, err := NormalizeJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	editMsg := a.client.BuildEdit(chatJID, types.MessageID(messageID), &waE2E.Message{
		Conversation: proto.String(newText),
	})
	_, err = a.sendMessageWithRetry(ctx, chatJID, editMsg)
	if err != nil {
		return fmt.Errorf("failed to send edited message: %w", err)
	}

	if a.localDB != nil {
		_, _ = a.localDB.ExecContext(ctx, "UPDATE watui_messages SET body = ? WHERE id = ?", newText, messageID)
		_, _ = a.localDB.ExecContext(ctx, "UPDATE watui_unread_messages SET body = ? WHERE id = ?", newText, messageID)
	}

	return nil
}

// ReactMessage adds or removes an emoji reaction on a message in WhatsApp.
// An empty reactionEmoji removes the reaction.
func (a *Adapter) ReactMessage(ctx context.Context, chatID string, messageID string, reactionEmoji string, sender ...string) error {
	if a.client == nil {
		return errors.New("whatsapp client not initialized")
	}
	if !a.client.IsConnected() {
		if !a.client.WaitForConnection(4 * time.Second) {
			go func() { _ = a.client.Connect() }()
			return errors.New("connection lost: reconnecting to WhatsApp, please retry in a moment")
		}
	}

	chatJID, err := NormalizeJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	var senderJID types.JID
	if len(sender) > 0 && sender[0] != "" && sender[0] != "You" {
		sJID, err := NormalizeJID(sender[0])
		if err == nil {
			senderJID = sJID
		}
	}

	reactMsg := a.client.BuildReaction(chatJID, senderJID, types.MessageID(messageID), reactionEmoji)
	_, err = a.sendMessageWithRetry(ctx, chatJID, reactMsg)
	if err != nil {
		return fmt.Errorf("failed to send reaction: %w", err)
	}

	a.updateLocalReaction(ctx, chatID, messageID, reactionEmoji)
	return nil
}

func (a *Adapter) updateLocalReaction(ctx context.Context, chatID string, messageID string, reactionEmoji string) {
	if a.localDB == nil {
		return
	}
	nonAD := chatID
	if parsed, err := NormalizeJID(chatID); err == nil {
		nonAD = parsed.ToNonAD().String()
	}
	if reactionEmoji == "" {
		// Remove reaction from me for this message in history
		_, _ = a.localDB.ExecContext(ctx, `DELETE FROM watui_messages 
			WHERE (chat_id = ? OR chat_id = ?) AND quoted_id = ? AND is_from_me = 1 AND type = 'reaction'`,
			chatID, nonAD, messageID,
		)
		_, _ = a.localDB.ExecContext(ctx, `DELETE FROM watui_unread_messages 
			WHERE (chat_id = ? OR chat_id = ?) AND quoted_id = ? AND is_from_me = 1 AND type = 'reaction'`,
			chatID, nonAD, messageID,
		)
	} else {
		// Remove any previous reaction from me for this message to prevent duplicate reaction rows
		_, _ = a.localDB.ExecContext(ctx, `DELETE FROM watui_messages 
			WHERE (chat_id = ? OR chat_id = ?) AND quoted_id = ? AND is_from_me = 1 AND type = 'reaction'`,
			chatID, nonAD, messageID,
		)
		var targetBody, targetSender, targetChatName sql.NullString
		_ = a.localDB.QueryRowContext(ctx, `SELECT body, sender_name, chat_name FROM watui_messages WHERE id = ?`, messageID).Scan(&targetBody, &targetSender, &targetChatName)

		reactMsgID := fmt.Sprintf("react-%s-%d", messageID, time.Now().UnixNano())
		bodyText := fmt.Sprintf("[Reaction: %s]", reactionEmoji)
		_, _ = a.localDB.ExecContext(ctx, `INSERT INTO watui_messages
			(id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, status)
			VALUES (?, ?, ?, 'You', 'You', ?, ?, 'reaction', 1, ?, ?, ?, 'sent')`,
			reactMsgID, chatID, targetChatName.String, time.Now().Unix(), bodyText, messageID, targetBody.String, targetSender.String,
		)
	}
}

func isConnectionOrTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, whatsmeow.ErrMessageTimedOut) ||
		errors.Is(err, whatsmeow.ErrNotConnected) ||
		errors.Is(err, socket.ErrSocketClosed) ||
		errors.Is(err, socket.ErrDialFailed) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection lost") ||
		strings.Contains(msg, "not connected") ||
		strings.Contains(msg, "closed network connection") ||
		strings.Contains(msg, "socket closed") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable")
}

func (a *Adapter) sendMessageWithRetry(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	if a.client == nil {
		return whatsmeow.SendResponse{}, errors.New("whatsapp client not initialized")
	}

	var reqExtra whatsmeow.SendRequestExtra
	if len(extra) > 0 {
		reqExtra = extra[0]
	}
	if reqExtra.Timeout == 0 {
		reqExtra.Timeout = 15 * time.Second
	}

	// If client is not currently connected, wait briefly for connection or auto-reconnect
	if !a.client.IsConnected() {
		if !a.client.WaitForConnection(4 * time.Second) {
			a.ResetConnection()
			if !a.client.WaitForConnection(4 * time.Second) {
				return whatsmeow.SendResponse{}, errors.New("connection lost: reconnecting to WhatsApp, please retry in a moment")
			}
		}
	}

	msg = a.prepareOutgoingMessage(ctx, to, msg)
	resp, err := a.client.SendMessage(ctx, to, msg, reqExtra)
	if err == nil {
		return resp, nil
	}

	// If sending failed due to a dead socket, timeout, or dropped connection, reset connection and retry ONCE
	if isConnectionOrTimeoutError(err) {
		a.ResetConnection()
		if a.client.WaitForConnection(5 * time.Second) {
			resp, retryErr := a.client.SendMessage(ctx, to, msg, reqExtra)
			if retryErr == nil {
				return resp, nil
			}
			err = retryErr
		}
	}

	return whatsmeow.SendResponse{}, err
}

// SendTextMessage sends a basic text message to a WhatsApp chat JID or raw phone number, optionally quoting a message.
func (a *Adapter) SendTextMessage(ctx context.Context, chatID string, text string, quotedMsg ...string) (domain.Message, error) {
	recipientJID, err := NormalizeJID(chatID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	var quotedID, quotedBody, quotedSender string
	if len(quotedMsg) > 0 {
		quotedID = quotedMsg[0]
	}
	if len(quotedMsg) > 1 {
		quotedBody = quotedMsg[1]
	}
	if len(quotedMsg) > 2 {
		quotedSender = quotedMsg[2]
	}

	wireText := text
	var mentionedJIDs []string

	if strings.Contains(chatID, "@g.us") && strings.Contains(wireText, "@") {
		participants, _ := a.GetGroupParticipants(ctx, chatID)
		// Sort participants by Name length descending so longer matches take precedence
		sort.Slice(participants, func(i, j int) bool {
			return len(participants[i].Name) > len(participants[j].Name)
		})
		seenJID := make(map[string]bool)
		for _, p := range participants {
			if p.Name == "" || p.Name == "You" {
				continue
			}
			tag := "@" + p.Name
			if strings.Contains(wireText, tag) {
				pJID, err := types.ParseJID(p.JID)
				if err == nil {
					user := pJID.User
					if idx := strings.Index(user, ":"); idx != -1 {
						user = user[:idx]
					}
					if pJID.Server == "lid" {
						if pn := a.ResolveLIDToPhone(user); pn != "" {
							pJID = types.NewJID(pn, types.DefaultUserServer)
							user = pn
						}
					}
					cleanJID := pJID.ToNonAD().String()
					if !seenJID[cleanJID] {
						seenJID[cleanJID] = true
						mentionedJIDs = append(mentionedJIDs, cleanJID)
					}
					wireText = strings.ReplaceAll(wireText, tag, "@"+user)
				}
			}
		}

		matches := mentionRegex.FindAllStringSubmatchIndex(wireText, -1)
		for _, sub := range matches {
			startDigit, endDigit := sub[2], sub[3]
			digitStr := wireText[startDigit:endDigit]
			pJID := types.NewJID(digitStr, types.DefaultUserServer)
			cleanJID := pJID.String()
			if !seenJID[cleanJID] {
				seenJID[cleanJID] = true
				mentionedJIDs = append(mentionedJIDs, cleanJID)
			}
		}
	}

	var msg *waE2E.Message
	if quotedID != "" || len(mentionedJIDs) > 0 {
		ctxInfo := &waE2E.ContextInfo{}
		if quotedID != "" {
			if (quotedSender == "" || quotedBody == "") && a.localDB != nil {
				var s, b string
				if err := a.localDB.QueryRow("SELECT sender, body FROM watui_messages WHERE id = ?", quotedID).Scan(&s, &b); err == nil {
					if quotedSender == "" {
						quotedSender = s
					}
					if quotedBody == "" {
						quotedBody = b
					}
				}
			}

			ctxInfo.StanzaID = proto.String(quotedID)
			if quotedSender != "" {
				ctxInfo.Participant = proto.String(quotedSender)
			}
			if quotedBody != "" {
				ctxInfo.QuotedMessage = &waE2E.Message{
					Conversation: proto.String(quotedBody),
				}
			}
		}
		if len(mentionedJIDs) > 0 {
			ctxInfo.MentionedJID = mentionedJIDs
		}

		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String(wireText),
				ContextInfo: ctxInfo,
			},
		}
	} else {
		msg = &waE2E.Message{
			Conversation: proto.String(wireText),
		}
	}

	resp, err := a.sendMessageWithRetry(ctx, recipientJID, msg)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to send message: %w", err)
	}

	senderID := ""
	if a.client.Store.ID != nil {
		senderID = a.client.Store.ID.ToNonAD().String()
	}

	var quotedSenderName string
	if quotedSender != "" {
		quotedSenderName = a.resolveParticipantName(quotedSender)
	}

	domainMsg := domain.Message{
		ID:           resp.ID,
		ChatID:       recipientJID.String(),
		Sender:       senderID,
		SenderName:   "Me",
		Timestamp:    resp.Timestamp,
		IsFromMe:     true,
		Type:         domain.MessageTypeText,
		Body:         text,
		Status:       domain.MessageStatusSent,
		QuotedID:     quotedID,
		QuotedText:   quotedBody,
		QuotedSender: quotedSenderName,
	}
	a.saveHistoryMessage(domainMsg)
	return domainMsg, nil
}

// convertGifToMp4 converts raw GIF image bytes into an MP4 video (H.264 / yuv420p)
// suitable for WhatsApp's native looping GIF playback.
func convertGifToMp4(ctx context.Context, gifData []byte) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg not found in PATH")
	}

	tmpDir, err := os.MkdirTemp("", "watui-gif-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	inputPath := filepath.Join(tmpDir, "input.gif")
	if err := os.WriteFile(inputPath, gifData, 0600); err != nil {
		return nil, err
	}

	outputPath := filepath.Join(tmpDir, "output.mp4")
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-y",
		"-i", inputPath,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-an",
		outputPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg gif conversion failed: %w (output: %s)", err, string(out))
	}

	return os.ReadFile(outputPath)
}

// SendFileMessage uploads and sends a file attachment to a WhatsApp chat JID or raw phone number, optionally quoting a message.
func (a *Adapter) SendFileMessage(ctx context.Context, chatID string, filePath string, caption string, quotedMsg ...string) (domain.Message, error) {
	if a.client == nil {
		return domain.Message{}, fmt.Errorf("client not connected")
	}

	if !a.client.IsConnected() {
		if !a.client.WaitForConnection(4 * time.Second) {
			a.ResetConnection()
			if !a.client.WaitForConnection(4 * time.Second) {
				return domain.Message{}, errors.New("connection lost: reconnecting to WhatsApp, please retry in a moment")
			}
		}
	}

	recipientJID, err := NormalizeJID(chatID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("invalid recipient %q: %w", chatID, err)
	}

	var quotedID, quotedBody, quotedSender string
	if len(quotedMsg) > 0 {
		quotedID = quotedMsg[0]
	}
	if len(quotedMsg) > 1 {
		quotedBody = quotedMsg[1]
	}
	if len(quotedMsg) > 2 {
		quotedSender = quotedMsg[2]
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

	var isGif bool
	var gifThumb []byte
	var gifWidth, gifHeight, gifSeconds uint32

	if ext == ".gif" {
		if g, err := gif.DecodeAll(bytes.NewReader(fileData)); err == nil && len(g.Image) > 0 {
			firstFrame := g.Image[0]
			bounds := firstFrame.Bounds()
			gifWidth = uint32(bounds.Dx())
			gifHeight = uint32(bounds.Dy())

			var totalDelay100ths int
			for _, d := range g.Delay {
				totalDelay100ths += d
			}
			gifSeconds = uint32(totalDelay100ths / 100)
			if gifSeconds == 0 {
				gifSeconds = 1
			}

			var thumbBuf bytes.Buffer
			if err := jpeg.Encode(&thumbBuf, firstFrame, &jpeg.Options{Quality: 60}); err == nil {
				if len(thumbBuf.Bytes()) <= 32768 {
					gifThumb = thumbBuf.Bytes()
				}
			}
		}

		mp4Data, err := convertGifToMp4(ctx, fileData)
		if err == nil && len(mp4Data) > 0 {
			fileData = mp4Data
			mimeType = "video/mp4"
			isGif = true
		}
	}

	var appInfo whatsmeow.MediaType
	var msgType domain.MessageType
	if isGif {
		appInfo = whatsmeow.MediaVideo
		msgType = domain.MessageTypeVideo
	} else {
		switch ext {
		case ".jpg", ".jpeg", ".png", ".bmp":
			appInfo = whatsmeow.MediaImage
			msgType = domain.MessageTypeImage
		case ".mp4", ".mov", ".avi", ".mkv", ".webm":
			appInfo = whatsmeow.MediaVideo
			msgType = domain.MessageTypeVideo
		case ".mp3", ".ogg", ".wav", ".m4a", ".aac", ".flac":
			appInfo = whatsmeow.MediaAudio
			msgType = domain.MessageTypeAudio
		default:
			// Non-video, non-image files, or GIFs without ffmpeg installed,
			// are safely uploaded as DocumentMessage so WhatsApp doesn't drop them.
			appInfo = whatsmeow.MediaDocument
			msgType = domain.MessageTypeDocument
		}
	}

	resp, err := a.client.Upload(ctx, fileData, appInfo)
	if err != nil && isConnectionOrTimeoutError(err) {
		a.ResetConnection()
		if a.client.WaitForConnection(5 * time.Second) {
			resp, err = a.client.Upload(ctx, fileData, appInfo)
		}
	}
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to upload media: %w", err)
	}

	wireCaption := caption
	var mentionedJIDs []string
	if caption != "" && strings.Contains(chatID, "@g.us") && strings.Contains(caption, "@") {
		participants, _ := a.GetGroupParticipants(ctx, chatID)
		sort.Slice(participants, func(i, j int) bool {
			return len(participants[i].Name) > len(participants[j].Name)
		})
		seenJID := make(map[string]bool)
		for _, p := range participants {
			if p.Name == "" || p.Name == "You" {
				continue
			}
			tag := "@" + p.Name
			if strings.Contains(wireCaption, tag) {
				pJID, err := types.ParseJID(p.JID)
				if err == nil {
					user := pJID.User
					if idx := strings.Index(user, ":"); idx != -1 {
						user = user[:idx]
					}
					if pJID.Server == "lid" {
						if pn := a.ResolveLIDToPhone(user); pn != "" {
							pJID = types.NewJID(pn, types.DefaultUserServer)
							user = pn
						}
					}
					cleanJID := pJID.ToNonAD().String()
					if !seenJID[cleanJID] {
						seenJID[cleanJID] = true
						mentionedJIDs = append(mentionedJIDs, cleanJID)
					}
					wireCaption = strings.ReplaceAll(wireCaption, tag, "@"+user)
				}
			}
		}

		matches := mentionRegex.FindAllStringSubmatchIndex(wireCaption, -1)
		for _, sub := range matches {
			startDigit, endDigit := sub[2], sub[3]
			digitStr := wireCaption[startDigit:endDigit]
			pJID := types.NewJID(digitStr, types.DefaultUserServer)
			cleanJID := pJID.String()
			if !seenJID[cleanJID] {
				seenJID[cleanJID] = true
				mentionedJIDs = append(mentionedJIDs, cleanJID)
			}
		}
	}

	var ctxInfo *waE2E.ContextInfo
	if quotedID != "" || len(mentionedJIDs) > 0 {
		ctxInfo = &waE2E.ContextInfo{}
		if quotedID != "" {
			if (quotedSender == "" || quotedBody == "") && a.localDB != nil {
				var s, b string
				if err := a.localDB.QueryRow("SELECT sender, body FROM watui_messages WHERE id = ?", quotedID).Scan(&s, &b); err == nil {
					if quotedSender == "" {
						quotedSender = s
					}
					if quotedBody == "" {
						quotedBody = b
					}
				}
			}

			ctxInfo.StanzaID = proto.String(quotedID)
			if quotedSender != "" {
				ctxInfo.Participant = proto.String(quotedSender)
			}
			if quotedBody != "" {
				ctxInfo.QuotedMessage = &waE2E.Message{
					Conversation: proto.String(quotedBody),
				}
			}
		}
		if len(mentionedJIDs) > 0 {
			ctxInfo.MentionedJID = mentionedJIDs
		}
	}

	var waMsg *waE2E.Message
	var label string
	if isGif {
		label = "GIF"
	} else {
		switch msgType {
		case domain.MessageTypeImage:
			label = "Image"
		case domain.MessageTypeVideo:
			label = "Video"
		case domain.MessageTypeAudio:
			label = "Audio"
		default:
			label = "Document"
		}
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
		if wireCaption != "" {
			imgMsg.Caption = proto.String(wireCaption)
		}
		if ctxInfo != nil {
			imgMsg.ContextInfo = ctxInfo
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
		if isGif {
			vidMsg.GifPlayback = proto.Bool(true)
			if len(gifThumb) > 0 {
				vidMsg.JPEGThumbnail = gifThumb
			}
			if gifWidth > 0 && gifHeight > 0 {
				vidMsg.Width = proto.Uint32(gifWidth)
				vidMsg.Height = proto.Uint32(gifHeight)
			}
			if gifSeconds > 0 {
				vidMsg.Seconds = proto.Uint32(gifSeconds)
			}
		}
		if wireCaption != "" {
			vidMsg.Caption = proto.String(wireCaption)
		}
		if ctxInfo != nil {
			vidMsg.ContextInfo = ctxInfo
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
		if ctxInfo != nil {
			audioMsg.ContextInfo = ctxInfo
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
		if wireCaption != "" {
			docMsg.Caption = proto.String(wireCaption)
		}
		if ctxInfo != nil {
			docMsg.ContextInfo = ctxInfo
		}
		waMsg = &waE2E.Message{DocumentMessage: docMsg}
	}

	waMsg = a.prepareOutgoingMessage(ctx, recipientJID, waMsg)
	sendResp, err := a.sendMessageWithRetry(ctx, recipientJID, waMsg)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to send media message: %w", err)
	}

	a.cacheMediaMessage(sendResp.ID, recipientJID.String(), waMsg, sendResp.Timestamp)

	senderID := ""
	if a.client.Store.ID != nil {
		senderID = a.client.Store.ID.ToNonAD().String()
	}

	var quotedSenderName string
	if quotedSender != "" {
		quotedSenderName = a.resolveParticipantName(quotedSender)
	}

	domainMsg := domain.Message{
		ID:           sendResp.ID,
		ChatID:       recipientJID.String(),
		Sender:       senderID,
		SenderName:   "Me",
		Timestamp:    sendResp.Timestamp,
		IsFromMe:     true,
		Type:         msgType,
		Body:         bodyText,
		Status:       domain.MessageStatusSent,
		QuotedID:     quotedID,
		QuotedText:   quotedBody,
		QuotedSender: quotedSenderName,
	}
	a.saveHistoryMessage(domainMsg, waMsg)
	return domainMsg, nil
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

func isGenericName(name, jid string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if isRawJID(name) {
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
	if isDigitsAndHyphens && (strings.Contains(name, "-") || len(strings.ReplaceAll(name, " ", "")) >= 10 || strings.HasPrefix(name, "120363")) {
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

func (a *Adapter) ensureLIDMap() {
	a.lidMapMu.RLock()
	if a.lidMapCache != nil && time.Since(a.lidMapCachedAt) < 2*time.Minute {
		a.lidMapMu.RUnlock()
		return
	}
	a.lidMapMu.RUnlock()

	a.lidMapMu.Lock()
	defer a.lidMapMu.Unlock()
	if a.lidMapCache != nil && time.Since(a.lidMapCachedAt) < 2*time.Minute {
		return
	}
	if a.localDB == nil {
		return
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := a.localDB.QueryContext(queryCtx, "SELECT lid, pn FROM whatsmeow_lid_map")
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	lidToPhone := make(map[string]string)
	phoneToLID := make(map[string]string)
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err == nil && lid != "" && pn != "" {
			lidToPhone[lid] = pn
			phoneToLID[pn] = lid
		}
	}
	a.lidMapCache = lidToPhone
	a.phoneToLIDCache = phoneToLID
	a.lidMapCachedAt = time.Now()
}

func (a *Adapter) getLIDMap() map[string]string {
	a.ensureLIDMap()
	a.lidMapMu.RLock()
	defer a.lidMapMu.RUnlock()
	res := make(map[string]string, len(a.lidMapCache))
	for k, v := range a.lidMapCache {
		res[k] = v
	}
	return res
}

func (a *Adapter) fetchLocalContacts(ctx context.Context) []domain.Contact {
	var list []domain.Contact

	// 1. Instantly read cached groups from local SQLite
	list = append(list, a.getLocalGroups()...)

	// 2. Read contacts from whatsmeow SQLite store
	if a.client != nil && a.client.Store != nil {
		if a.client.Store.ID != nil {
			myJID := a.client.Store.ID.ToNonAD()
			list = append(list, domain.Contact{
				JID:  myJID.String(),
				Name: "You",
			})
			if myLID := a.ResolvePhoneToLID(myJID.User); myLID != "" {
				list = append(list, domain.Contact{
					JID:  types.NewJID(myLID, "lid").String(),
					Name: "You",
				})
			}
		}
		if a.client.Store.LID.User != "" {
			list = append(list, domain.Contact{
				JID:  a.client.Store.LID.ToNonAD().String(),
				Name: "You",
			})
		}
	}
	if a.client != nil && a.client.Store != nil && a.client.Store.Contacts != nil {
		rawMap, err := a.client.Store.Contacts.GetAllContacts(ctx)
		if err == nil {
			lidMap := a.getLIDMap()
			phoneToLID := make(map[string]string, len(lidMap))
			for lid, pn := range lidMap {
				phoneToLID[pn] = lid
			}
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
				if actualJID != jid {
					list = append(list, domain.Contact{
						JID:          jid.String(),
						Name:         name,
						PushName:     info.PushName,
						BusinessName: info.BusinessName,
						IsGroup:      false,
					})
				}
				if jid.Server == types.DefaultUserServer && len(phoneToLID) > 0 {
					if lid, ok := phoneToLID[jid.User]; ok && lid != "" {
						list = append(list, domain.Contact{
							JID:          types.NewJID(lid, "lid").String(),
							Name:         name,
							PushName:     info.PushName,
							BusinessName: info.BusinessName,
							IsGroup:      false,
						})
					}
				}
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

	if len(needed) > 25 {
		needed = needed[:25]
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

// GetGroupParticipants retrieves the list of participants in a group chat,
// resolving human names and filtering out the current user.
func (a *Adapter) GetGroupParticipants(ctx context.Context, groupJID string) ([]domain.Contact, error) {
	if groupJID == "" {
		return nil, nil
	}

	a.groupParticipantsMu.RLock()
	if cached, ok := a.groupParticipants[groupJID]; ok && len(cached) > 0 {
		a.groupParticipantsMu.RUnlock()
		return cached, nil
	}
	a.groupParticipantsMu.RUnlock()

	parsedJID, err := NormalizeJID(groupJID)
	if err != nil {
		return nil, err
	}

	var result []domain.Contact
	seen := make(map[string]bool)

	if a.client != nil {
		reqCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		info, err := a.client.GetGroupInfo(reqCtx, parsedJID.ToNonAD())
		cancel()
		if err == nil && info != nil {
			for _, p := range info.Participants {
				pNonAD := p.JID.ToNonAD()
				name := a.resolveParticipantName(pNonAD.String(), p.DisplayName)
				if name == "You" {
					continue
				}
				user := pNonAD.User
				if idx := strings.Index(user, ":"); idx != -1 {
					user = user[:idx]
				}
				if seen[user] {
					continue
				}
				seen[user] = true
				result = append(result, domain.Contact{
					JID:      pNonAD.String(),
					Name:     name,
					PushName: p.DisplayName,
					IsGroup:  false,
				})
			}
		}
	}

	// Also supplement from recent message senders in this group
	if a.localDB != nil {
		rows, err := a.localDB.QueryContext(ctx,
			"SELECT DISTINCT sender, sender_name FROM watui_messages WHERE chat_id = ? AND sender != '' ORDER BY timestamp DESC LIMIT 100",
			groupJID,
		)
		if err == nil {
			for rows.Next() {
				var s, sn string
				if err := rows.Scan(&s, &sn); err == nil && s != "" {
					pJID, err := types.ParseJID(s)
					if err == nil {
						user := pJID.ToNonAD().User
						if idx := strings.Index(user, ":"); idx != -1 {
							user = user[:idx]
						}
						if !seen[user] {
							name := a.resolveParticipantName(s, sn)
							if name != "You" {
								seen[user] = true
								result = append(result, domain.Contact{
									JID:      pJID.ToNonAD().String(),
									Name:     name,
									PushName: sn,
									IsGroup:  false,
								})
							}
						}
					}
				}
			}
			_ = rows.Close()
		}
	}

	if len(result) > 0 {
		a.groupParticipantsMu.Lock()
		if a.groupParticipants == nil {
			a.groupParticipants = make(map[string][]domain.Contact)
		}
		a.groupParticipants[groupJID] = result
		a.groupParticipantsMu.Unlock()
	}

	return result, nil
}

func (a *Adapter) refreshContactsCache(ctx context.Context) {
	if a.client == nil || !a.IsLoggedIn() {
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
	a.updateContactsIndexLocked(combined)
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

func (a *Adapter) updateContactsIndexLocked(list []domain.Contact) {
	a.cachedContacts = list
	a.contactsByJID = make(map[string]string, len(list)*2)
	for _, c := range list {
		if c.Name != "" {
			a.contactsByJID[c.JID] = c.Name
			user := strings.Split(c.JID, "@")[0]
			if idx := strings.Index(user, ":"); idx != -1 {
				user = user[:idx]
			}
			a.contactsByJID[user] = c.Name
		}
	}
	a.contactsLoaded = true
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
	if a.client == nil || !a.IsLoggedIn() {
		return nil
	}
	if !a.client.IsConnected() {
		if !a.client.WaitForConnection(3 * time.Second) {
			go func() { _ = a.client.Connect() }()
			return errors.New("client is reconnecting")
		}
	}
	// Fetch regular_low (contains read/unread statuses and archive status) and regular_high
	_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularLow)
	_ = a.SyncAppStateRelaxed(ctx, appstate.WAPatchRegularHigh)
	a.loadArchivedChats()
	return nil
}

// FetchAppStateCollection fetches a specific app state patch collection from WhatsApp.
func (a *Adapter) FetchAppStateCollection(ctx context.Context, name appstate.WAPatchName, fullSync bool) error {
	if a.client == nil || !a.IsLoggedIn() {
		return errors.New("client not initialized or not logged in")
	}
	err := a.client.FetchAppState(ctx, name, fullSync, false)
	a.loadArchivedChats()
	return err
}

func (a *Adapter) GetRawClient() *whatsmeow.Client {
	return a.client
}

// SyncAppStateRelaxed attempts standard FetchAppState, and if it fails due to LTHash verification failure or mismatch,
// decodes both snapshot and delta patch mutations directly without aborting on LTHash mismatch so that chat archives,
// unarchives, and read states are reconciled across clients.
func (a *Adapter) SyncAppStateRelaxed(ctx context.Context, name appstate.WAPatchName) error {
	if a.client == nil || !a.IsLoggedIn() {
		return errors.New("client not initialized or not logged in")
	}
	err := a.client.FetchAppState(ctx, name, false, false)
	if err == nil {
		a.loadArchivedChats()
		return nil
	}

	//lint:ignore SA1019 DangerousInternals is needed for low-level app state patch recovery on LTHash mismatch
	dangerous := a.client.DangerousInternals()
	proc := appstate.NewProcessor(a.client.Store, a.client.Log)

	version, hash, _ := a.client.Store.AppState.GetAppStateVersion(ctx, string(name))
	state := appstate.HashState{Version: version, Hash: hash}
	wantSnapshot := (version == 0)

	hasMore := true
	for hasMore {
		patches, err := dangerous.FetchAppStatePatches(ctx, name, state.Version, wantSnapshot)
		if err != nil {
			if !wantSnapshot && version > 0 {
				// Delta fetch from current version failed, fallback to full snapshot
				wantSnapshot = true
				state = appstate.HashState{}
				continue
			}
			return fmt.Errorf("failed to fetch patches for %s: %w", name, err)
		}
		if patches == nil {
			break
		}
		wantSnapshot = false
		hasMore = patches.HasMorePatches

		mutations, newState, err := proc.DecodePatches(ctx, patches, state, false)
		if err != nil {
			if !wantSnapshot && version > 0 {
				// Delta decode failed, fallback to snapshot
				wantSnapshot = true
				state = appstate.HashState{}
				continue
			}
			return fmt.Errorf("failed to decode patches for %s: %w", name, err)
		}
		state = newState

		for _, mut := range mutations {
			evt := dangerous.DispatchAppState(ctx, name, mut, true)
			if evt != nil {
				a.handleEvent(evt)
			}
		}
	}

	_ = a.client.Store.AppState.PutAppStateVersion(ctx, string(name), state.Version, state.Hash)
	a.loadArchivedChats()
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
		(id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke, raw_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChatID, msg.ChatName, msg.Sender, msg.SenderName, msg.Timestamp.Unix(), msg.Body, string(msg.Type), msg.IsFromMe, msg.QuotedID, msg.QuotedText, msg.QuotedSender, msg.IsEdit, msg.IsRevoke, rawBytes,
	)

	// Keep unread messages table lean by pruning older messages beyond 100 per chat
	_, _ = a.localDB.Exec(`DELETE FROM watui_unread_messages 
		WHERE chat_id = ? AND id NOT IN (
			SELECT id FROM watui_unread_messages WHERE chat_id = ? ORDER BY timestamp DESC LIMIT 100
		)`, msg.ChatID, msg.ChatID)
}

// SetAppConfig updates the declarative application configuration.
func (a *Adapter) SetAppConfig(cfg *config.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appConfig = cfg
}

// ReloadConfig dynamically reloads configuration from disk if a config file is configured.
func (a *Adapter) ReloadConfig(ctx context.Context) error {
	a.mu.Lock()
	cfg := a.appConfig
	a.mu.Unlock()
	if cfg == nil || cfg.SourcePath == "" {
		return nil
	}
	newCfg, err := config.Load(cfg.SourcePath)
	if err != nil {
		return err
	}
	a.SetAppConfig(newCfg)
	return nil
}

func (a *Adapter) saveHistoryMessage(msg domain.Message, raw ...*waE2E.Message) {
	if a.localDB == nil || a.appConfig == nil || !a.appConfig.IsHistoryPersistEnabled() {
		return
	}
	chatID := msg.ChatID
	if parsed, err := NormalizeJID(chatID); err == nil {
		chatID = parsed.ToNonAD().String()
	}

	var rawBytes []byte
	if len(raw) > 0 && raw[0] != nil {
		rawBytes, _ = proto.Marshal(raw[0])
	}

	statusVal := string(msg.Status)
	if statusVal == "" {
		if msg.IsFromMe {
			statusVal = string(domain.MessageStatusSent)
		} else {
			statusVal = string(domain.MessageStatusDelivered)
		}
	}

	// Preserve existing higher status (e.g. read / delivered) if already present in DB
	var existingStatus sql.NullString
	if err := a.localDB.QueryRow("SELECT status FROM watui_messages WHERE id = ?", msg.ID).Scan(&existingStatus); err == nil && existingStatus.Valid && existingStatus.String != "" {
		if existingStatus.String == string(domain.MessageStatusRead) {
			statusVal = string(domain.MessageStatusRead)
		} else if existingStatus.String == string(domain.MessageStatusDelivered) && statusVal == string(domain.MessageStatusSent) {
			statusVal = string(domain.MessageStatusDelivered)
		}
	}

	_, _ = a.localDB.Exec(`INSERT OR REPLACE INTO watui_messages
		(id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke, raw_message, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, chatID, msg.ChatName, msg.Sender, msg.SenderName, msg.Timestamp.Unix(), msg.Body, string(msg.Type), msg.IsFromMe, msg.QuotedID, msg.QuotedText, msg.QuotedSender, msg.IsEdit, msg.IsRevoke, rawBytes, statusVal,
	)

	// Keep history table strictly bounded per chat according to cyclic limit (default: 30)
	maxKeep := a.appConfig.GetCycleMsgCountPerChat()
	if maxKeep <= 0 {
		maxKeep = 30
	}
	_, _ = a.localDB.Exec(`DELETE FROM watui_messages 
		WHERE chat_id = ? AND id NOT IN (
			SELECT id FROM watui_messages WHERE chat_id = ? ORDER BY timestamp DESC LIMIT ?
		)`, chatID, chatID, maxKeep)
}

// GetChatHistory returns up to limit historical messages for the given chat from local SQLite.
func (a *Adapter) GetChatHistory(ctx context.Context, chatID string, limit int, beforeTimestamp time.Time) ([]domain.Message, error) {
	if a.localDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
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

	var query string
	var args []interface{}
	if beforeTimestamp.IsZero() {
		query = `SELECT id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke, status 
			FROM watui_messages 
			WHERE chat_id = ? OR chat_id = ? OR chat_id = ? 
			ORDER BY timestamp DESC LIMIT ?`
		args = []interface{}{chatID, nonAD, altTarget, limit}
	} else {
		query = `SELECT id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke, status 
			FROM watui_messages 
			WHERE (chat_id = ? OR chat_id = ? OR chat_id = ?) AND timestamp < ? 
			ORDER BY timestamp DESC LIMIT ?`
		args = []interface{}{chatID, nonAD, altTarget, beforeTimestamp.Unix(), limit}
	}

	rows, err := a.localDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []domain.Message
	for rows.Next() {
		var m domain.Message
		var ts int64
		var chatName, senderName, body, qID, qText, qSender, status sql.NullString
		var msgType string
		var isFromMe bool
		var isEdit, isRevoke sql.NullBool
		if err := rows.Scan(&m.ID, &m.ChatID, &chatName, &m.Sender, &senderName, &ts, &body, &msgType, &isFromMe, &qID, &qText, &qSender, &isEdit, &isRevoke, &status); err != nil {
			continue
		}
		if status.Valid && status.String != "" {
			m.Status = domain.MessageStatus(status.String)
		} else if isFromMe {
			m.Status = domain.MessageStatusSent
		} else {
			m.Status = domain.MessageStatusDelivered
		}
		m.ChatName = chatName.String
		m.SenderName = senderName.String
		if !m.IsFromMe && isGenericName(m.SenderName, m.Sender) {
			if resolved := a.resolveParticipantName(m.Sender); resolved != "" && !isGenericName(resolved, m.Sender) {
				m.SenderName = resolved
			}
		}
		m.Body = body.String
		m.Type = domain.MessageType(msgType)
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = isFromMe
		m.QuotedID = qID.String
		m.QuotedText = qText.String
		m.QuotedSender = qSender.String
		m.IsEdit = isEdit.Valid && isEdit.Bool
		m.IsRevoke = isRevoke.Valid && isRevoke.Bool
		result = append(result, m)
	}
	_ = rows.Close()

	if a.localDB != nil && len(result) > 0 {
		var missingIDs []interface{}
		seen := make(map[string]bool)
		for _, m := range result {
			if m.QuotedText == "" && m.QuotedID != "" && !seen[m.QuotedID] {
				missingIDs = append(missingIDs, m.QuotedID)
				seen[m.QuotedID] = true
			}
		}
		if len(missingIDs) > 0 {
			placeholders := strings.Repeat("?,", len(missingIDs))
			placeholders = placeholders[:len(placeholders)-1]
			qQuery := fmt.Sprintf("SELECT id, body, sender_name FROM watui_messages WHERE id IN (%s)", placeholders)
			qRows, err := a.localDB.QueryContext(ctx, qQuery, missingIDs...)
			if err == nil {
				defer qRows.Close()
				type qInfo struct{ body, sender string }
				qMap := make(map[string]qInfo)
				for qRows.Next() {
					var id string
					var b, s sql.NullString
					if err := qRows.Scan(&id, &b, &s); err == nil {
						qMap[id] = qInfo{body: b.String, sender: s.String}
					}
				}
				for i := range result {
					if result[i].QuotedText == "" && result[i].QuotedID != "" {
						if info, ok := qMap[result[i].QuotedID]; ok && info.body != "" {
							result[i].QuotedText = info.body
							if result[i].QuotedSender == "" {
								result[i].QuotedSender = info.sender
							}
						}
					}
				}
			}
		}
	}

	// Reverse so messages are returned in chronological order (oldest first)
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return result, nil
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

// GetUnreadMessages returns all unread messages persisted in local SQLite (capped to latest 50 per chat for instant startup).
func (a *Adapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	if a.localDB == nil {
		return nil, nil
	}
	lidMap := a.getLIDMap()

	// Use partitioned window query to only pull the latest 50 unreads per chat, avoiding megabytes of stale backlog
	rows, err := a.localDB.QueryContext(ctx, `
		SELECT id, chat_id, chat_name, sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke
		FROM (
			SELECT id, chat_id, COALESCE(chat_name, '') AS chat_name, sender, sender_name, timestamp, body, type, is_from_me,
			       quoted_id, quoted_text, quoted_sender, is_edit, is_revoke,
			       ROW_NUMBER() OVER (PARTITION BY chat_id ORDER BY timestamp DESC) as rn
			FROM watui_unread_messages
		) WHERE rn <= 50
		ORDER BY timestamp ASC`)
	if err != nil {
		rows, err = a.localDB.QueryContext(ctx, `SELECT id, chat_id, COALESCE(chat_name, ''), sender, sender_name, timestamp, body, type, is_from_me, quoted_id, quoted_text, quoted_sender, is_edit, is_revoke
			FROM watui_unread_messages ORDER BY timestamp ASC`)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	senderNameCache := make(map[string]string)
	for rows.Next() {
		var m domain.Message
		var ts int64
		var tStr string
		var qID, qText, qSender sql.NullString
		var isEdit, isRevoke sql.NullBool
		if err := rows.Scan(&m.ID, &m.ChatID, &m.ChatName, &m.Sender, &m.SenderName, &ts, &m.Body, &tStr, &m.IsFromMe, &qID, &qText, &qSender, &isEdit, &isRevoke); err == nil {
			m.Timestamp = time.Unix(ts, 0)
			m.Type = domain.MessageType(tStr)
			m.Status = domain.MessageStatusDelivered
			m.QuotedID = qID.String
			m.QuotedText = qText.String
			m.QuotedSender = qSender.String
			m.IsEdit = isEdit.Valid && isEdit.Bool
			m.IsRevoke = isRevoke.Valid && isRevoke.Bool
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
			if !m.IsFromMe && isGenericName(m.SenderName, m.Sender) {
				if cached, ok := senderNameCache[m.Sender]; ok {
					if cached != "" {
						m.SenderName = cached
					}
				} else {
					if resolved := a.resolveParticipantName(m.Sender); resolved != "" && !isGenericName(resolved, m.Sender) {
						senderNameCache[m.Sender] = resolved
						m.SenderName = resolved
					} else {
						senderNameCache[m.Sender] = ""
					}
				}
			}
			msgs = append(msgs, m)
		}
	}
	_ = rows.Close()

	if a.localDB != nil && len(msgs) > 0 {
		var missingIDs []interface{}
		seen := make(map[string]bool)
		for _, m := range msgs {
			if m.QuotedText == "" && m.QuotedID != "" && !seen[m.QuotedID] {
				missingIDs = append(missingIDs, m.QuotedID)
				seen[m.QuotedID] = true
			}
		}
		if len(missingIDs) > 0 {
			placeholders := strings.Repeat("?,", len(missingIDs))
			placeholders = placeholders[:len(placeholders)-1]
			qQuery := fmt.Sprintf("SELECT id, body, sender_name FROM watui_messages WHERE id IN (%s)", placeholders)
			qRows, err := a.localDB.QueryContext(ctx, qQuery, missingIDs...)
			if err == nil {
				defer qRows.Close()
				type qInfo struct{ body, sender string }
				qMap := make(map[string]qInfo)
				for qRows.Next() {
					var id string
					var b, s sql.NullString
					if err := qRows.Scan(&id, &b, &s); err == nil {
						qMap[id] = qInfo{body: b.String, sender: s.String}
					}
				}
				for i := range msgs {
					if msgs[i].QuotedText == "" && msgs[i].QuotedID != "" {
						if info, ok := qMap[msgs[i].QuotedID]; ok && info.body != "" {
							msgs[i].QuotedText = info.body
							if msgs[i].QuotedSender == "" {
								msgs[i].QuotedSender = info.sender
							}
						}
					}
				}
			}
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
		if !a.IsLoggedIn() {
			// Websocket connected for QR pairing, but device is not authenticated yet.
			return
		}
		a.setStatus(domain.StatusConnected)
		go a.refreshContactsCache(a.ctx)
		go func() {
			// Catch up on any app state mutations (e.g. chats read on other devices)
			_ = a.SyncAppStateRelaxed(a.ctx, appstate.WAPatchRegularLow)
			_ = a.SyncAppStateRelaxed(a.ctx, appstate.WAPatchRegularHigh)
			a.loadArchivedChats()
			a.loadMutedChats()
			a.loadEphemeralChats()
			go a.refreshContactsCache(a.ctx)
			_ = a.client.SendPresence(a.ctx, types.PresenceAvailable)
		}()

	case *events.PairSuccess:
		a.setStatus(domain.StatusConnected)
		go func() {
			a.loadArchivedChats()
			a.loadMutedChats()
			a.loadEphemeralChats()
			go a.refreshContactsCache(a.ctx)
		}()

	case *events.Disconnected:
		a.setStatus(domain.StatusDisconnected)

	case *events.AppState:
		if !a.IsLoggedIn() {
			return
		}
		go a.loadArchivedChats()
		go a.loadMutedChats()
		go a.loadEphemeralChats()
		go a.refreshContactsCache(a.ctx)

	case *events.AppStateSyncComplete:
		if !a.IsLoggedIn() {
			return
		}
		go a.loadArchivedChats()
		go a.loadMutedChats()
		go a.loadEphemeralChats()
		go a.refreshContactsCache(a.ctx)

	case *events.AppStateSyncError:
		if !a.IsLoggedIn() {
			return
		}
		if evt.Name == appstate.WAPatchRegularLow || evt.Name == appstate.WAPatchRegularHigh {
			go func() {
				_ = a.SyncAppStateRelaxed(a.ctx, evt.Name)
			}()
		}

	case *events.Archive:
		if evt.Action != nil {
			isArchived := evt.Action.GetArchived()
			rawJID := evt.JID.String()
			chatID := evt.JID.ToNonAD().String()
			clean := chatID
			if idx := strings.Index(clean, ":"); idx != -1 {
				if atIdx := strings.Index(clean, "@"); atIdx != -1 {
					clean = clean[:idx] + clean[atIdx:]
				}
			}
			a.archivedMu.Lock()
			if a.archivedChats == nil {
				a.archivedChats = make(map[string]bool)
			}
			prev := a.archivedChats[clean] || a.archivedChats[chatID]
			statusChanged := (prev != isArchived)
			if isArchived {
				a.archivedChats[chatID] = true
				a.archivedChats[rawJID] = true
				a.archivedChats[clean] = true
				if strings.HasSuffix(clean, "@lid") {
					lidUser := strings.TrimSuffix(clean, "@lid")
					if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
						a.archivedChats[pn+"@s.whatsapp.net"] = true
					}
				} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
					phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
					if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
						a.archivedChats[lid+"@lid"] = true
					}
				}
			} else {
				delete(a.archivedChats, chatID)
				delete(a.archivedChats, rawJID)
				delete(a.archivedChats, clean)
				if strings.HasSuffix(clean, "@lid") {
					lidUser := strings.TrimSuffix(clean, "@lid")
					if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
						delete(a.archivedChats, pn+"@s.whatsapp.net")
					}
				} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
					phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
					if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
						delete(a.archivedChats, lid+"@lid")
					}
				}
			}
			a.archivedMu.Unlock()

			if a.client != nil && a.client.Store != nil && a.client.Store.ChatSettings != nil {
				_ = a.client.Store.ChatSettings.PutArchived(a.ctx, evt.JID, isArchived)
			}
			if a.localDB != nil {
				archivedVal := 0
				if isArchived {
					archivedVal = 1
				}
				_, _ = a.localDB.Exec(`UPDATE whatsmeow_chat_settings SET archived = ? WHERE chat_jid = ? OR chat_jid = ?`, archivedVal, chatID, rawJID)
				if isArchived {
					var count int
					_ = a.localDB.QueryRow(`SELECT COUNT(*) FROM whatsmeow_chat_settings WHERE chat_jid = ?`, chatID).Scan(&count)
					if count == 0 {
						var ourJID string
						if a.client != nil && a.client.Store != nil && a.client.Store.ID != nil {
							ourJID = a.client.Store.ID.ToNonAD().String()
						} else {
							_ = a.localDB.QueryRow(`SELECT our_jid FROM whatsmeow_chat_settings LIMIT 1`).Scan(&ourJID)
						}
						if ourJID != "" {
							_, _ = a.localDB.Exec(`INSERT OR IGNORE INTO whatsmeow_chat_settings (our_jid, chat_jid, archived) VALUES (?, ?, 1)`, ourJID, chatID)
						}
					}
				}
			}

			if statusChanged {
				a.notifyChatArchived(clean, isArchived)
				if rawJID != clean {
					a.notifyChatArchived(rawJID, isArchived)
				}
				if strings.HasSuffix(clean, "@lid") {
					lidUser := strings.TrimSuffix(clean, "@lid")
					if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
						a.notifyChatArchived(pn+"@s.whatsapp.net", isArchived)
					}
				} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
					phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
					if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
						a.notifyChatArchived(lid+"@lid", isArchived)
					}
				}
			}

			go a.refreshContactsCache(a.ctx)
		}

	case *events.Mute:
		if evt.Action != nil {
			isMuted := evt.Action.GetMuted()
			var until time.Time
			if isMuted {
				if evt.Action.GetMuteEndTimestamp() < 0 {
					until = store.MutedForever
				} else if evt.Action.GetMuteEndTimestamp() > 0 {
					until = time.UnixMilli(evt.Action.GetMuteEndTimestamp())
				}
			}
			chatID := evt.JID.ToNonAD().String()
			a.mutedMu.Lock()
			if a.mutedChats == nil {
				a.mutedChats = make(map[string]time.Time)
			}
			if isMuted {
				a.mutedChats[chatID] = until
				a.mutedChats[evt.JID.String()] = until
			} else {
				delete(a.mutedChats, chatID)
				delete(a.mutedChats, evt.JID.String())
			}
			a.mutedMu.Unlock()
		}

	case *events.LoggedOut:
		a.setStatus(domain.StatusLoggedOut)

	case *events.Presence:
		nonAD := evt.From.ToNonAD().String()
		p := domain.UserPresence{
			JID:      nonAD,
			IsOnline: !evt.Unavailable,
			LastSeen: evt.LastSeen,
		}
		a.presenceMu.Lock()
		if a.presences == nil {
			a.presences = make(map[string]domain.UserPresence)
		}
		a.presences[nonAD] = p
		a.presences[evt.From.String()] = p
		var aliases []string
		if strings.HasSuffix(nonAD, "@s.whatsapp.net") {
			user := strings.TrimSuffix(nonAD, "@s.whatsapp.net")
			if lid := a.ResolvePhoneToLID(user); lid != "" {
				a.presences[lid+"@lid"] = p
				aliases = append(aliases, lid+"@lid")
			}
		} else if strings.HasSuffix(nonAD, "@lid") {
			user := strings.TrimSuffix(nonAD, "@lid")
			if pn := a.ResolveLIDToPhone(user); pn != "" {
				a.presences[pn+"@s.whatsapp.net"] = p
				aliases = append(aliases, pn+"@s.whatsapp.net")
			}
		}
		a.presenceMu.Unlock()
		a.notifyPresence(p)
		for _, aliasJID := range aliases {
			aliasP := p
			aliasP.JID = aliasJID
			a.notifyPresence(aliasP)
		}

	case *events.ChatPresence:
		chatUser := evt.Sender.ToNonAD().String()
		if chatUser == "" {
			chatUser = evt.Chat.ToNonAD().String()
		}
		if chatUser != "" && !strings.Contains(chatUser, "@g.us") {
			p := domain.UserPresence{
				JID:      chatUser,
				IsOnline: true,
			}
			a.presenceMu.Lock()
			if a.presences == nil {
				a.presences = make(map[string]domain.UserPresence)
			}
			a.presences[chatUser] = p
			var aliases []string
			if strings.HasSuffix(chatUser, "@s.whatsapp.net") {
				user := strings.TrimSuffix(chatUser, "@s.whatsapp.net")
				if lid := a.ResolvePhoneToLID(user); lid != "" {
					a.presences[lid+"@lid"] = p
					aliases = append(aliases, lid+"@lid")
				}
			} else if strings.HasSuffix(chatUser, "@lid") {
				user := strings.TrimSuffix(chatUser, "@lid")
				if pn := a.ResolveLIDToPhone(user); pn != "" {
					a.presences[pn+"@s.whatsapp.net"] = p
					aliases = append(aliases, pn+"@s.whatsapp.net")
				}
			}
			a.presenceMu.Unlock()
			a.notifyPresence(p)
			for _, aliasJID := range aliases {
				aliasP := p
				aliasP.JID = aliasJID
				a.notifyPresence(aliasP)
			}
		}

	case *events.Receipt:
		// 1. Live receipt persistence and notification
		var rType string
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf ||
			evt.Type == types.ReceiptTypePlayed || evt.Type == types.ReceiptTypePlayedSelf {
			rType = "read"
		} else if evt.Type == types.ReceiptTypeDelivered || evt.Type == types.ReceiptTypeSender || evt.Type == "" {
			rType = "delivered"
		}

		chatID := evt.Chat.ToNonAD().String()
		userJID := evt.Sender.ToNonAD().String()
		if userJID == "" && !evt.SenderAlt.IsEmpty() {
			userJID = evt.SenderAlt.ToNonAD().String()
		}
		if userJID == "" && !evt.MessageSender.IsEmpty() {
			userJID = evt.MessageSender.ToNonAD().String()
		}

		if (chatID == "" || chatID == "<nil>") && len(evt.MessageIDs) > 0 && a.localDB != nil {
			var dbChat string
			_ = a.localDB.QueryRow("SELECT chat_id FROM watui_messages WHERE id = ?", string(evt.MessageIDs[0])).Scan(&dbChat)
			if dbChat != "" {
				chatID = dbChat
			}
		}

		if (userJID == "" || userJID == "<nil>") && chatID != "" && !strings.Contains(chatID, "@g.us") {
			userJID = chatID
		}

		userName := a.ResolveContactName(userJID)
		if userName == "" {
			userName = userJID
		}
		ts := evt.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}

		if rType != "" && len(evt.MessageIDs) > 0 {
			var strIDs []string
			for _, mID := range evt.MessageIDs {
				idStr := string(mID)
				if idStr == "" {
					continue
				}
				strIDs = append(strIDs, idStr)
				a.recordMessageReceipt(idStr, chatID, userJID, userName, rType, ts)
			}
			if len(strIDs) > 0 {
				a.notifyReceipt(chatID, strIDs, rType, ts)
			}
		}

		// When read on phone or companion, delete those messages from unread persistence
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf {
			affectedChats := a.deleteUnreadMessageIDs(evt.MessageIDs)
			for _, cID := range affectedChats {
				if !a.isChatUnread(cID) {
					a.notifyChatDismissed(cID)
				}
			}
			if !evt.Chat.IsEmpty() {
				cID := evt.Chat.ToNonAD().String()
				if !a.isChatUnread(cID) {
					a.notifyChatDismissed(cID)
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

			clean := chatID
			if idx := strings.Index(clean, ":"); idx != -1 {
				if atIdx := strings.Index(clean, "@"); atIdx != -1 {
					clean = clean[:idx] + clean[atIdx:]
				}
			}

			// Capture archive status from HistorySync conversation
			if conv.Archived != nil {
				isArchived := conv.GetArchived()
				a.archivedMu.Lock()
				if a.archivedChats == nil {
					a.archivedChats = make(map[string]bool)
				}
				if isArchived {
					a.archivedChats[chatID] = true
					a.archivedChats[rawJID] = true
					a.archivedChats[clean] = true
					if conv.GetPnJID() != "" {
						a.archivedChats[conv.GetPnJID()] = true
					}
					if conv.GetLidJID() != "" {
						a.archivedChats[conv.GetLidJID()] = true
					}
					if strings.HasSuffix(clean, "@lid") {
						lidUser := strings.TrimSuffix(clean, "@lid")
						if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
							a.archivedChats[pn+"@s.whatsapp.net"] = true
						}
					} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
						phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
						if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
							a.archivedChats[lid+"@lid"] = true
						}
					}
				} else {
					delete(a.archivedChats, chatID)
					delete(a.archivedChats, rawJID)
					delete(a.archivedChats, clean)
					if conv.GetPnJID() != "" {
						delete(a.archivedChats, conv.GetPnJID())
					}
					if conv.GetLidJID() != "" {
						delete(a.archivedChats, conv.GetLidJID())
					}
					if strings.HasSuffix(clean, "@lid") {
						lidUser := strings.TrimSuffix(clean, "@lid")
						if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
							delete(a.archivedChats, pn+"@s.whatsapp.net")
						}
					} else if strings.HasSuffix(clean, "@s.whatsapp.net") {
						phoneUser := strings.TrimSuffix(clean, "@s.whatsapp.net")
						if lid := a.ResolvePhoneToLID(phoneUser); lid != "" {
							delete(a.archivedChats, lid+"@lid")
						}
					}
				}
				a.archivedMu.Unlock()

				if a.client != nil && a.client.Store != nil && a.client.Store.ChatSettings != nil && err == nil {
					_ = a.client.Store.ChatSettings.PutArchived(a.ctx, parsedJID, isArchived)
					if conv.GetPnJID() != "" {
						if pnJID, pErr := types.ParseJID(conv.GetPnJID()); pErr == nil {
							_ = a.client.Store.ChatSettings.PutArchived(a.ctx, pnJID, isArchived)
						}
					}
					if conv.GetLidJID() != "" {
						if lidJID, lErr := types.ParseJID(conv.GetLidJID()); lErr == nil {
							_ = a.client.Store.ChatSettings.PutArchived(a.ctx, lidJID, isArchived)
						}
					}
				}
				if a.localDB != nil {
					archivedVal := 0
					if isArchived {
						archivedVal = 1
					}
					_, _ = a.localDB.Exec(`UPDATE whatsmeow_chat_settings SET archived = ? WHERE chat_jid = ? OR chat_jid = ?`, archivedVal, chatID, rawJID)
					if isArchived {
						var count int
						_ = a.localDB.QueryRow(`SELECT COUNT(*) FROM whatsmeow_chat_settings WHERE chat_jid = ?`, chatID).Scan(&count)
						if count == 0 {
							var ourJID string
							if a.client != nil && a.client.Store != nil && a.client.Store.ID != nil {
								ourJID = a.client.Store.ID.ToNonAD().String()
							} else {
								_ = a.localDB.QueryRow(`SELECT our_jid FROM whatsmeow_chat_settings LIMIT 1`).Scan(&ourJID)
							}
							if ourJID != "" {
								_, _ = a.localDB.Exec(`INSERT OR IGNORE INTO whatsmeow_chat_settings (our_jid, chat_jid, archived) VALUES (?, ?, 1)`, ourJID, chatID)
							}
						}
					}
				}
				a.notifyChatArchived(clean, isArchived)
				if rawJID != clean {
					a.notifyChatArchived(rawJID, isArchived)
				}
				if conv.GetPnJID() != "" {
					a.notifyChatArchived(conv.GetPnJID(), isArchived)
				}
				if conv.GetLidJID() != "" {
					a.notifyChatArchived(conv.GetLidJID(), isArchived)
				}
			}

			if conv.EphemeralExpiration != nil {
				var ts int64
				if conv.EphemeralSettingTimestamp != nil {
					ts = *conv.EphemeralSettingTimestamp
				}
				if ts <= 0 {
					ts = time.Now().Unix()
				}
				a.updateChatEphemeralTimer(chatID, *conv.EphemeralExpiration, ts)
			}

			if conv.GetUnreadCount() == 0 {
				_ = a.DismissUnread(context.Background(), chatID)
				_ = a.DismissUnread(context.Background(), rawJID)
				a.notifyChatDismissed(chatID)
				a.notifyChatDismissed(rawJID)
				for _, hMsg := range conv.GetMessages() {
					if hMsg == nil || hMsg.Message == nil {
						continue
					}
					webMsg := hMsg.Message
					if domainMsg, ok := a.extractWebMessage(chatID, webMsg); ok {
						if webMsg.Message != nil {
							a.cacheMediaMessage(domainMsg.ID, chatID, webMsg.Message, domainMsg.Timestamp)
						}
						a.saveHistoryMessage(domainMsg, webMsg.Message)
						a.recordWebMessageReceipts(domainMsg.ID, chatID, webMsg)
					}
				}
				continue
			}

			// If unread, process messages
			for _, hMsg := range conv.GetMessages() {
				if hMsg == nil || hMsg.Message == nil {
					continue
				}
				webMsg := hMsg.Message
				isFromMe := webMsg.Key != nil && webMsg.Key.GetFromMe()
				domainMsg, ok := a.extractWebMessage(chatID, webMsg)
				if ok {
					if webMsg.Message != nil {
						a.cacheMediaMessage(domainMsg.ID, chatID, webMsg.Message, domainMsg.Timestamp)
					}
					if !isFromMe {
						a.saveUnreadMessage(domainMsg, webMsg.Message)
					}
					a.saveHistoryMessage(domainMsg, webMsg.Message)
					a.recordWebMessageReceipts(domainMsg.ID, chatID, webMsg)
					if !isFromMe {
						a.notifyMessage(domainMsg)
					}
				}
			}
		}
		go a.refreshContactsCache(a.ctx)

	case *events.Message:
		if targetID, isRevoke := extractRevokeProtocolInfo(evt); isRevoke && targetID != "" {
			var origBody, origSender, origSenderName, origChatID, origChatName, origType string
			var origTimestamp int64
			var origIsFromMe bool
			if a.localDB != nil {
				row := a.localDB.QueryRow("SELECT chat_id, chat_name, body, sender, sender_name, timestamp, type, is_from_me FROM watui_messages WHERE id = ?", targetID)
				_ = row.Scan(&origChatID, &origChatName, &origBody, &origSender, &origSenderName, &origTimestamp, &origType, &origIsFromMe)
				if origBody == "" {
					row2 := a.localDB.QueryRow("SELECT chat_id, chat_name, body, sender, sender_name, timestamp, type, is_from_me FROM watui_unread_messages WHERE id = ?", targetID)
					_ = row2.Scan(&origChatID, &origChatName, &origBody, &origSender, &origSenderName, &origTimestamp, &origType, &origIsFromMe)
				}
				_, _ = a.localDB.Exec("UPDATE watui_messages SET is_revoke = 1 WHERE id = ?", targetID)
				_, _ = a.localDB.Exec("UPDATE watui_unread_messages SET is_revoke = 1 WHERE id = ?", targetID)
			}
			if origBody == "" {
				if origType != "" && origType != "text" {
					origBody = fmt.Sprintf("[%s]", strings.ToUpper(origType))
				} else {
					origBody = "[Message]"
				}
			}

			finalChatID := origChatID
			if finalChatID == "" {
				chatJID := evt.Info.Chat.ToNonAD()
				if chatJID.Server == "lid" {
					if pn := a.ResolveLIDToPhone(chatJID.User); pn != "" {
						chatJID = types.NewJID(pn, types.DefaultUserServer)
					}
				}
				finalChatID = chatJID.String()
			}
			if strings.HasSuffix(finalChatID, "@lid") {
				lidUser := strings.TrimSuffix(finalChatID, "@lid")
				if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
					finalChatID = pn + "@s.whatsapp.net"
				}
			}

			sender := origSender
			if sender == "" {
				senderJID := evt.Info.Sender.ToNonAD()
				if senderJID.Server == "lid" {
					if pn := a.ResolveLIDToPhone(senderJID.User); pn != "" {
						senderJID = types.NewJID(pn, types.DefaultUserServer)
					}
				}
				sender = senderJID.String()
				if evt.Info.IsFromMe && (sender == "" || sender == "You") {
					sender = "You"
				}
			}

			senderName := origSenderName
			if senderName == "" {
				senderName = a.resolveParticipantName(evt.Info.Sender.String(), evt.Info.PushName)
				if senderName == "" {
					if evt.Info.PushName != "" {
						senderName = evt.Info.PushName
					} else if evt.Info.IsFromMe {
						senderName = "You"
					}
				}
			}

			chatName := origChatName
			if chatName == "" {
				chatJIDParsed, _ := types.ParseJID(finalChatID)
				chatName = a.resolveChatName(chatJIDParsed)
			}

			ts := evt.Info.Timestamp
			if ts.IsZero() && origTimestamp > 0 {
				ts = time.Unix(origTimestamp, 0)
			} else if ts.IsZero() {
				ts = time.Now()
			}

			revokeDMsg := domain.Message{
				ID:           targetID,
				ChatID:       finalChatID,
				ChatName:     chatName,
				Sender:       sender,
				SenderName:   senderName,
				Body:         origBody,
				Timestamp:    ts,
				IsFromMe:     origIsFromMe || evt.Info.IsFromMe,
				Type:         domain.MessageTypeText,
				QuotedID:     targetID,
				QuotedText:   origBody,
				QuotedSender: sender,
				IsRevoke:     true,
			}

			a.notifyMessage(revokeDMsg)
			return
		}

		var protoMsg *waE2E.ProtocolMessage
		unwrapped := unwrapMessage(evt.Message)
		if unwrapped != nil && unwrapped.ProtocolMessage != nil {
			protoMsg = unwrapped.ProtocolMessage
		}
		if protoMsg == nil && evt.RawMessage != nil {
			rawUnwrapped := unwrapMessage(evt.RawMessage)
			if rawUnwrapped != nil && rawUnwrapped.ProtocolMessage != nil {
				protoMsg = rawUnwrapped.ProtocolMessage
			}
		}
		if protoMsg != nil && protoMsg.Type != nil && *protoMsg.Type == waE2E.ProtocolMessage_EPHEMERAL_SETTING {
			timer := protoMsg.GetEphemeralExpiration()
			ts := protoMsg.GetEphemeralSettingTimestamp()
			if ts <= 0 {
				ts = evt.Info.Timestamp.Unix()
			}
			a.updateChatEphemeralTimer(evt.Info.Chat.ToNonAD().String(), timer, ts)
		} else if ci := extractContextInfo(evt.Message); ci != nil && ci.GetExpiration() > 0 {
			ts := ci.GetEphemeralSettingTimestamp()
			if ts <= 0 {
				ts = evt.Info.Timestamp.Unix()
			}
			a.updateChatEphemeralTimer(evt.Info.Chat.ToNonAD().String(), ci.GetExpiration(), ts)
		} else if ci := extractContextInfo(evt.RawMessage); ci != nil && ci.GetExpiration() > 0 {
			ts := ci.GetEphemeralSettingTimestamp()
			if ts <= 0 {
				ts = evt.Info.Timestamp.Unix()
			}
			a.updateChatEphemeralTimer(evt.Info.Chat.ToNonAD().String(), ci.GetExpiration(), ts)
		}

		if targetID, targetChat, newMsg, isEdit := a.extractEditDetails(evt); isEdit && targetID != "" {
			var newBody string
			if newMsg != nil {
				newBody, _, _ = extractMessageContent(newMsg)
			}
			if newBody == "" && evt.Message != nil {
				newBody, _, _ = extractMessageContent(evt.Message)
			}
			if newBody == "" && evt.RawMessage != nil {
				newBody, _, _ = extractMessageContent(evt.RawMessage)
			}
			if newBody != "" {
				var origBody, origSender, origSenderName, origChatID, origChatName string
				if a.localDB != nil {
					row := a.localDB.QueryRow("SELECT chat_id, chat_name, body, sender, sender_name FROM watui_messages WHERE id = ?", targetID)
					_ = row.Scan(&origChatID, &origChatName, &origBody, &origSender, &origSenderName)
					if origBody == "" {
						row2 := a.localDB.QueryRow("SELECT chat_id, chat_name, body, sender, sender_name FROM watui_unread_messages WHERE id = ?", targetID)
						_ = row2.Scan(&origChatID, &origChatName, &origBody, &origSender, &origSenderName)
					}
					_, _ = a.localDB.Exec("UPDATE watui_messages SET body = ? WHERE id = ?", newBody, targetID)
					_, _ = a.localDB.Exec("UPDATE watui_unread_messages SET body = ? WHERE id = ?", newBody, targetID)
				}
				if origBody == "" {
					origBody = "[Original message not cached]"
				}

				editMsgID := evt.Info.ID
				if editMsgID == "" || editMsgID == targetID {
					editMsgID = fmt.Sprintf("edit_%s_%d", targetID, evt.Info.Timestamp.Unix())
				}

				// Resolve target ChatID
				var finalChatID string
				if origChatID != "" {
					finalChatID = origChatID
				} else if targetChat != "" {
					if parsed, err := NormalizeJID(targetChat); err == nil {
						finalChatID = parsed.ToNonAD().String()
					} else {
						finalChatID = targetChat
					}
				} else if evt.Info.DeviceSentMeta != nil && evt.Info.DeviceSentMeta.DestinationJID != "" {
					if parsed, err := NormalizeJID(evt.Info.DeviceSentMeta.DestinationJID); err == nil {
						finalChatID = parsed.ToNonAD().String()
					}
				} else {
					chatJID := evt.Info.Chat.ToNonAD()
					if chatJID.Server == "lid" {
						if pn := a.ResolveLIDToPhone(chatJID.User); pn != "" {
							chatJID = types.NewJID(pn, types.DefaultUserServer)
						}
					}
					finalChatID = chatJID.String()
				}
				if strings.HasSuffix(finalChatID, "@lid") {
					lidUser := strings.TrimSuffix(finalChatID, "@lid")
					if pn := a.ResolveLIDToPhone(lidUser); pn != "" {
						finalChatID = pn + "@s.whatsapp.net"
					}
				}

				// Resolve Sender
				senderJID := evt.Info.Sender.ToNonAD()
				if senderJID.Server == "lid" {
					if pn := a.ResolveLIDToPhone(senderJID.User); pn != "" {
						senderJID = types.NewJID(pn, types.DefaultUserServer)
					}
				}
				sender := senderJID.String()
				if sender == "" && origSender != "" {
					sender = origSender
				}
				if evt.Info.IsFromMe && (sender == "" || sender == "You") {
					sender = "You"
				}

				senderName := a.resolveParticipantName(evt.Info.Sender.String(), evt.Info.PushName)
				if senderName == "" {
					if origSenderName != "" {
						senderName = origSenderName
					} else if evt.Info.PushName != "" {
						senderName = evt.Info.PushName
					} else if evt.Info.IsFromMe {
						senderName = "You"
					} else {
						senderName = senderJID.User
					}
				}

				chatJIDParsed, _ := types.ParseJID(finalChatID)
				chatName := a.resolveChatName(chatJIDParsed)
				if chatName == "" {
					if origChatName != "" {
						chatName = origChatName
					} else if !evt.Info.IsGroup {
						chatName = senderName
					}
				}

				editDMsg := domain.Message{
					ID:           editMsgID,
					ChatID:       finalChatID,
					ChatName:     chatName,
					Sender:       sender,
					SenderName:   senderName,
					Body:         newBody,
					Timestamp:    evt.Info.Timestamp,
					IsFromMe:     evt.Info.IsFromMe,
					Type:         domain.MessageTypeText,
					QuotedID:     targetID,
					QuotedText:   origBody,
					QuotedSender: sender,
					IsEdit:       true,
				}

				if !editDMsg.IsFromMe {
					a.saveUnreadMessage(editDMsg, newMsg)
				}
				a.saveHistoryMessage(editDMsg, newMsg)
				a.notifyMessage(editDMsg)
				return
			}
		}

		domainMsg, ok := a.extractDomainMessage(evt)
		if !ok {
			// Internal protocol, sender-key-distribution, or empty control message; do not leak to UI
			return
		}

		if evt.Message != nil {
			a.cacheMediaMessage(domainMsg.ID, domainMsg.ChatID, evt.Message, domainMsg.Timestamp)
		}

		if !domainMsg.IsFromMe {
			a.saveUnreadMessage(domainMsg, evt.Message)

			// If this incoming message quotes or reacts to an earlier message,
			// record verified read and delivery receipts for the quoted message.
			if domainMsg.QuotedID != "" {
				a.recordMessageReceipt(domainMsg.QuotedID, domainMsg.ChatID, domainMsg.Sender, domainMsg.SenderName, "read", domainMsg.Timestamp)
				a.recordMessageReceipt(domainMsg.QuotedID, domainMsg.ChatID, domainMsg.Sender, domainMsg.SenderName, "delivered", domainMsg.Timestamp)
				a.notifyReceipt(domainMsg.ChatID, []string{domainMsg.QuotedID}, "read", domainMsg.Timestamp)
			}

			// Any prior message sent by 'Me' in this chat was definitely delivered before this incoming message.
			if a.localDB != nil {
				_, _ = a.localDB.Exec(`UPDATE watui_messages SET status = 'delivered' 
					WHERE chat_id = ? AND is_from_me = 1 AND timestamp <= ? AND status = 'sent'`,
					domainMsg.ChatID, domainMsg.Timestamp.Unix())
			}
		}
		a.saveHistoryMessage(domainMsg, evt.Message)

		a.notifyMessage(domainMsg)

	case *events.DeleteForMe:
		if evt.MessageID != "" && a.localDB != nil {
			_, _ = a.localDB.Exec("DELETE FROM watui_messages WHERE id = ?", evt.MessageID)
			_, _ = a.localDB.Exec("DELETE FROM watui_unread_messages WHERE id = ?", evt.MessageID)
		}

	case *events.GroupInfo:
		if evt.Ephemeral != nil {
			var timer uint32
			if evt.Ephemeral.IsEphemeral {
				timer = evt.Ephemeral.DisappearingTimer
			}
			ts := evt.Timestamp.Unix()
			if ts <= 0 {
				ts = time.Now().Unix()
			}
			a.updateChatEphemeralTimer(evt.JID.ToNonAD().String(), timer, ts)
		}
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
	m = unwrapMessage(m)
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
		if m.VideoMessage.GetGifPlayback() {
			if m.VideoMessage.Caption != nil && *m.VideoMessage.Caption != "" {
				return *m.VideoMessage.Caption, domain.MessageTypeVideo, true
			}
			return "[GIF]", domain.MessageTypeVideo, true
		}
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
		return "Group (" + chatNonAD.User + ")"
	}

	// 1-on-1 contact: check fast in-memory cache first
	a.contactsMu.RLock()
	if a.contactsByJID != nil {
		if name, ok := a.contactsByJID[chatStr]; ok && name != "" {
			a.contactsMu.RUnlock()
			return name
		}
		if name, ok := a.contactsByJID[chatNonAD.User]; ok && name != "" {
			a.contactsMu.RUnlock()
			return name
		}
	}
	a.contactsMu.RUnlock()

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

var mentionRegex = regexp.MustCompile(`(?:^|[^\w@])@(\d{5,20})\b`)

func (a *Adapter) resolveParticipantName(participant string, pushName ...string) string {
	if participant == "" {
		return ""
	}
	if a.client != nil && a.client.Store != nil {
		if a.client.Store.ID != nil {
			myJID := a.client.Store.ID.ToNonAD()
			myUser := myJID.User
			if participant == myUser || strings.HasPrefix(participant, myUser+":") || strings.HasPrefix(participant, myUser+"@") {
				return "You"
			}
			if myLID := a.ResolvePhoneToLID(myUser); myLID != "" {
				if participant == myLID || strings.HasPrefix(participant, myLID+":") || strings.HasPrefix(participant, myLID+"@") {
					return "You"
				}
			}
		}
		if a.client.Store.LID.User != "" {
			myLIDUser := a.client.Store.LID.ToNonAD().User
			if participant == myLIDUser || strings.HasPrefix(participant, myLIDUser+":") || strings.HasPrefix(participant, myLIDUser+"@") {
				return "You"
			}
		}
	}
	pJID, err := types.ParseJID(participant)
	if err != nil {
		if pn := a.ResolveLIDToPhone(participant); pn != "" {
			pJID = types.NewJID(pn, types.DefaultUserServer)
		} else {
			testJID := types.NewJID(participant, types.DefaultUserServer)
			if name := a.resolveChatName(testJID); name != "" {
				return name
			}
			testLID := types.NewJID(participant, "lid")
			if name := a.resolveChatName(testLID); name != "" {
				return name
			}
			for _, p := range pushName {
				p = strings.TrimSpace(p)
				if p != "" {
					return p
				}
			}
			return participant
		}
	}
	pJID = pJID.ToNonAD()
	if pJID.Server == "lid" {
		if pn := a.ResolveLIDToPhone(pJID.User); pn != "" {
			pJID = types.NewJID(pn, types.DefaultUserServer)
		}
	}
	name := a.resolveChatName(pJID)
	if name != "" {
		return name
	}
	for _, p := range pushName {
		p = strings.TrimSpace(p)
		if p != "" {
			return p
		}
	}
	return pJID.User
}

// ResolveContactName resolves a user or participant JID into a friendly contact name if known.
func (a *Adapter) ResolveContactName(jidStr string) string {
	if jidStr == "" {
		return ""
	}
	name := a.resolveParticipantName(jidStr)
	if name != "" && name != jidStr {
		return name
	}
	parsed, err := types.ParseJID(jidStr)
	if err == nil {
		if resolved := a.resolveChatName(parsed); resolved != "" {
			return resolved
		}
	}
	return jidStr
}

func extractContextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	m = unwrapMessage(m)
	if m == nil {
		return nil
	}
	if m.ExtendedTextMessage != nil {
		return m.ExtendedTextMessage.ContextInfo
	} else if m.ImageMessage != nil {
		return m.ImageMessage.ContextInfo
	} else if m.VideoMessage != nil {
		return m.VideoMessage.ContextInfo
	} else if m.AudioMessage != nil {
		return m.AudioMessage.ContextInfo
	} else if m.DocumentMessage != nil {
		return m.DocumentMessage.ContextInfo
	} else if m.StickerMessage != nil {
		return m.StickerMessage.ContextInfo
	}
	return nil
}

func (a *Adapter) resolveMentionsInText(text string, mentionedJIDs ...string) string {
	if text == "" {
		return text
	}
	for _, jidStr := range mentionedJIDs {
		if jidStr == "" {
			continue
		}
		name := a.resolveParticipantName(jidStr)
		if name == "" {
			continue
		}
		pJID, err := types.ParseJID(jidStr)
		if err == nil {
			user := pJID.User
			if idx := strings.Index(user, ":"); idx != -1 {
				user = user[:idx]
			}
			text = strings.ReplaceAll(text, "@"+user, "@"+name)
			if pn := a.ResolveLIDToPhone(user); pn != "" {
				text = strings.ReplaceAll(text, "@"+pn, "@"+name)
			}
			if lid := a.ResolvePhoneToLID(user); lid != "" {
				text = strings.ReplaceAll(text, "@"+lid, "@"+name)
			}
		}
	}

	matches := mentionRegex.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	for i := len(matches) - 1; i >= 0; i-- {
		sub := matches[i]
		startDigit, endDigit := sub[2], sub[3]
		digitStr := text[startDigit:endDigit]
		resolved := a.resolveParticipantName(digitStr)
		if resolved != "" && resolved != digitStr && !strings.Contains(resolved, "@") {
			text = text[:startDigit] + resolved + text[endDigit:]
		}
	}
	return text
}

func extractQuotedInfo(m *waE2E.Message) (string, string, string) {
	m = unwrapMessage(m)
	if m == nil {
		return "", "", ""
	}

	if m.ReactionMessage != nil && m.ReactionMessage.Key != nil {
		var qID, qSender string
		if m.ReactionMessage.Key.ID != nil {
			qID = *m.ReactionMessage.Key.ID
		}
		if m.ReactionMessage.Key.FromMe != nil && *m.ReactionMessage.Key.FromMe {
			qSender = "You"
		} else if m.ReactionMessage.Key.Participant != nil && *m.ReactionMessage.Key.Participant != "" {
			qSender = *m.ReactionMessage.Key.Participant
		} else if m.ReactionMessage.Key.RemoteJID != nil && *m.ReactionMessage.Key.RemoteJID != "" && !strings.Contains(*m.ReactionMessage.Key.RemoteJID, "@g.us") {
			qSender = *m.ReactionMessage.Key.RemoteJID
		}
		return qID, "", qSender
	}

	ctxInfo := extractContextInfo(m)
	if ctxInfo != nil {
		var qID, qText, qSender string
		if ctxInfo.StanzaID != nil {
			qID = *ctxInfo.StanzaID
		}
		if ctxInfo.Participant != nil {
			qSender = *ctxInfo.Participant
		}
		if ctxInfo.QuotedMessage != nil {
			qText, _, _ = extractMessageContent(ctxInfo.QuotedMessage)
		}
		return qID, qText, qSender
	}
	return "", "", ""
}

func (a *Adapter) extractDomainMessage(evt *events.Message) (domain.Message, bool) {
	body, msgType, ok := extractMessageContent(evt.Message)
	if !ok {
		return domain.Message{}, false
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

	senderName := a.resolveParticipantName(evt.Info.Sender.String(), evt.Info.PushName)
	if senderName == "" {
		if evt.Info.PushName != "" {
			senderName = evt.Info.PushName
		} else {
			senderName = senderJID.User
		}
	}

	chatName := a.resolveChatName(chatJID)
	if chatName == "" && !evt.Info.IsGroup {
		chatName = senderName
	}

	qID, qText, qSender := extractQuotedInfo(evt.Message)
	var qSenderName string
	if qSender != "" {
		if qSender == "You" {
			qSenderName = "You"
		} else {
			qSenderName = a.resolveParticipantName(qSender)
		}
	}
	if qID != "" {
		if qText == "" && a.localDB != nil {
			var b, s sql.NullString
			if err := a.localDB.QueryRow("SELECT body, sender_name FROM watui_messages WHERE id = ?", qID).Scan(&b, &s); err == nil && b.String != "" {
				qText = b.String
				if qSenderName == "" && s.String != "" {
					qSenderName = s.String
				}
			} else if err := a.localDB.QueryRow("SELECT body, sender_name FROM watui_unread_messages WHERE id = ?", qID).Scan(&b, &s); err == nil && b.String != "" {
				qText = b.String
				if qSenderName == "" && s.String != "" {
					qSenderName = s.String
				}
			}
		}
	}

	var mentionedJIDs []string
	if ctxInfo := extractContextInfo(evt.Message); ctxInfo != nil {
		mentionedJIDs = ctxInfo.GetMentionedJID()
	}
	isMentioned := false
	for _, j := range mentionedJIDs {
		if a.resolveParticipantName(j) == "You" {
			isMentioned = true
			break
		}
	}
	body = a.resolveMentionsInText(body, mentionedJIDs...)
	qText = a.resolveMentionsInText(qText)
	if !isMentioned && strings.Contains(body, "@You") {
		isMentioned = true
	}

	msgStatus := domain.MessageStatusDelivered
	if evt.Info.IsFromMe {
		msgStatus = domain.MessageStatusSent
	}

	return domain.Message{
		ID:            evt.Info.ID,
		ChatID:        chatJID.String(),
		ChatName:      chatName,
		Sender:        senderJID.String(),
		SenderName:    senderName,
		Timestamp:     evt.Info.Timestamp,
		IsFromMe:      evt.Info.IsFromMe,
		Type:          msgType,
		Body:          body,
		Status:        msgStatus,
		QuotedID:      qID,
		QuotedText:    qText,
		QuotedSender:  qSenderName,
		MentionedJIDs: mentionedJIDs,
		IsMentioned:   isMentioned,
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
				senderJID = types.NewJID(pn, types.DefaultUserServer)
			}
		}
	}

	senderName := a.resolveParticipantName(sender, webMsg.GetPushName())
	if senderName == "" {
		if webMsg.GetPushName() != "" {
			senderName = webMsg.GetPushName()
		} else {
			senderName = sender
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

	qID, qText, qSender := extractQuotedInfo(webMsg.Message)
	var qSenderName string
	if qSender != "" {
		if qSender == "You" {
			qSenderName = "You"
		} else {
			qSenderName = a.resolveParticipantName(qSender)
		}
	}
	if qID != "" {
		if qText == "" && a.localDB != nil {
			var b, s sql.NullString
			if err := a.localDB.QueryRow("SELECT body, sender_name FROM watui_messages WHERE id = ?", qID).Scan(&b, &s); err == nil && b.String != "" {
				qText = b.String
				if qSenderName == "" && s.String != "" {
					qSenderName = s.String
				}
			} else if err := a.localDB.QueryRow("SELECT body, sender_name FROM watui_unread_messages WHERE id = ?", qID).Scan(&b, &s); err == nil && b.String != "" {
				qText = b.String
				if qSenderName == "" && s.String != "" {
					qSenderName = s.String
				}
			}
		}
	}

	var mentionedJIDs []string
	if ctxInfo := extractContextInfo(webMsg.Message); ctxInfo != nil {
		mentionedJIDs = ctxInfo.GetMentionedJID()
	}
	isMentioned := false
	for _, j := range mentionedJIDs {
		if a.resolveParticipantName(j) == "You" {
			isMentioned = true
			break
		}
	}
	body = a.resolveMentionsInText(body, mentionedJIDs...)
	qText = a.resolveMentionsInText(qText)
	if !isMentioned && strings.Contains(body, "@You") {
		isMentioned = true
	}

	fromMe := false
	if webMsg.Key != nil && webMsg.Key.GetFromMe() {
		fromMe = true
	}
	webStatus := domain.MessageStatusDelivered
	if fromMe {
		webStatus = domain.MessageStatusSent
		if webMsg.Status != nil {
			switch *webMsg.Status {
			case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
				webStatus = domain.MessageStatusRead
			case waWeb.WebMessageInfo_DELIVERY_ACK:
				webStatus = domain.MessageStatusDelivered
			case waWeb.WebMessageInfo_SERVER_ACK:
				webStatus = domain.MessageStatusSent
			}
		}
	}

	return domain.Message{
		ID:            msgID,
		ChatID:        chatID,
		ChatName:      chatName,
		Sender:        sender,
		SenderName:    senderName,
		Timestamp:     ts,
		IsFromMe:      fromMe,
		Type:          msgType,
		Body:          body,
		Status:        webStatus,
		QuotedID:      qID,
		QuotedText:    qText,
		QuotedSender:  qSenderName,
		MentionedJIDs: mentionedJIDs,
		IsMentioned:   isMentioned,
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

	cacheDir, err := media.GetMediaCacheDir()
	if err != nil {
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

	// Return cached file if already on disk and non-empty (check exact or any matching prefix)
	if fi, err := os.Stat(filePath); err == nil && fi.Size() > 0 {
		_ = media.TouchMediaFile(filePath)
		return filePath, nil
	}
	if matches, err := filepath.Glob(filepath.Join(cacheDir, safeID+"*")); err == nil {
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Size() > 0 {
				_ = media.TouchMediaFile(m)
				return m, nil
			}
		}
	}

	rawMsg := a.getCachedMediaMessage(msg.ID)
	if rawMsg == nil && a.localDB != nil {
		var rawBytes []byte
		// 1. Check dedicated media cache table
		row := a.localDB.QueryRowContext(ctx, "SELECT raw_message FROM watui_media_cache WHERE id = ?", msg.ID)
		if err := row.Scan(&rawBytes); err != nil || len(rawBytes) == 0 {
			// 2. Check history messages table
			row = a.localDB.QueryRowContext(ctx, "SELECT raw_message FROM watui_messages WHERE id = ? AND raw_message IS NOT NULL", msg.ID)
			_ = row.Scan(&rawBytes)
		}
		if len(rawBytes) == 0 {
			// 3. Check unread messages table
			row = a.localDB.QueryRowContext(ctx, "SELECT raw_message FROM watui_unread_messages WHERE id = ? AND raw_message IS NOT NULL", msg.ID)
			_ = row.Scan(&rawBytes)
		}
		if len(rawBytes) > 0 {
			var parsed waE2E.Message
			if err := proto.Unmarshal(rawBytes, &parsed); err == nil {
				rawMsg = &parsed
				a.cacheMediaMessage(msg.ID, msg.ChatID, &parsed)
			}
		}
	}

	if rawMsg == nil {
		return "", fmt.Errorf("media metadata not found for message %s (attachment was not cached or has expired)", msg.ID)
	}

	rawMsg = unwrapMessage(rawMsg)

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
	_ = media.TouchMediaFile(filePath)

	return filePath, nil
}

func (a *Adapter) recordMessageReceipt(messageID, chatID, userJID, userName, receiptType string, ts time.Time) {
	if a.localDB == nil || messageID == "" {
		return
	}
	tsUnix := ts.Unix()
	if tsUnix <= 0 {
		tsUnix = time.Now().Unix()
	}

	if chatID == "" || chatID == "<nil>" {
		var dbChat string
		_ = a.localDB.QueryRow("SELECT chat_id FROM watui_messages WHERE id = ?", messageID).Scan(&dbChat)
		if dbChat != "" {
			chatID = dbChat
		}
	}

	if (userJID == "" || userJID == "<nil>") && chatID != "" && !strings.Contains(chatID, "@g.us") {
		userJID = chatID
	}

	if userJID == "" || userJID == "<nil>" {
		if receiptType == "read" {
			_, _ = a.localDB.Exec(`UPDATE watui_messages SET status = 'read' WHERE id = ?`, messageID)
		} else if receiptType == "delivered" {
			_, _ = a.localDB.Exec(`UPDATE watui_messages SET status = 'delivered' WHERE id = ? AND (status IS NULL OR status != 'read')`, messageID)
		}
		return
	}

	if userName == "" || userName == userJID {
		userName = a.ResolveContactName(userJID)
	}

	_, _ = a.localDB.Exec(`INSERT OR REPLACE INTO watui_message_receipts 
		(message_id, chat_id, user_jid, user_name, receipt_type, timestamp)
		VALUES (?, ?, ?, ?, ?, ?)`,
		messageID, chatID, userJID, userName, receiptType, tsUnix,
	)

	// If read, also ensure delivered is recorded
	if receiptType == "read" {
		_, _ = a.localDB.Exec(`INSERT OR IGNORE INTO watui_message_receipts
			(message_id, chat_id, user_jid, user_name, receipt_type, timestamp)
			VALUES (?, ?, ?, ?, 'delivered', ?)`,
			messageID, chatID, userJID, userName, tsUnix,
		)
		_, _ = a.localDB.Exec(`UPDATE watui_messages SET status = 'read' WHERE id = ?`, messageID)
	} else if receiptType == "delivered" {
		_, _ = a.localDB.Exec(`UPDATE watui_messages SET status = 'delivered' WHERE id = ? AND (status IS NULL OR status != 'read')`, messageID)
	}
}

func (a *Adapter) recordWebMessageReceipts(messageID, chatID string, webMsg *waWeb.WebMessageInfo) {
	if a.localDB == nil || messageID == "" || webMsg == nil {
		return
	}
	for _, ur := range webMsg.GetUserReceipt() {
		if ur == nil {
			continue
		}
		uJID := ur.GetUserJID()
		if uJID == "" {
			continue
		}
		uName := a.ResolveContactName(uJID)
		if readTS := ur.GetReadTimestamp(); readTS > 0 {
			a.recordMessageReceipt(messageID, chatID, uJID, uName, "read", time.Unix(readTS, 0))
		}
		if delTS := ur.GetReceiptTimestamp(); delTS > 0 {
			a.recordMessageReceipt(messageID, chatID, uJID, uName, "delivered", time.Unix(delTS, 0))
		}
		if playedTS := ur.GetPlayedTimestamp(); playedTS > 0 {
			a.recordMessageReceipt(messageID, chatID, uJID, uName, "read", time.Unix(playedTS, 0))
		}
	}
}

// GetMessageReceipts retrieves read and delivery receipts for a message.
func (a *Adapter) GetMessageReceipts(ctx context.Context, chatID string, messageID string) (*domain.MessageReceiptDetails, error) {
	details := &domain.MessageReceiptDetails{
		MessageID: messageID,
		ChatID:    chatID,
		IsGroup:   strings.Contains(chatID, "@g.us"),
		SentAt:    time.Now(),
		Status:    domain.MessageStatusSent,
	}

	if a.localDB == nil {
		return details, nil
	}

	// 1. Check message details from watui_messages
	var ts int64
	var isFromMe bool
	var status sql.NullString
	err := a.localDB.QueryRowContext(ctx, `SELECT timestamp, is_from_me, status FROM watui_messages WHERE id = ?`, messageID).Scan(&ts, &isFromMe, &status)
	if err == nil {
		details.SentAt = time.Unix(ts, 0)
		details.IsFromMe = isFromMe
		if status.Valid && status.String != "" {
			details.Status = domain.MessageStatus(status.String)
		}
	} else {
		// Fallback: check unread messages table
		var uTS int64
		var uFromMe bool
		if uErr := a.localDB.QueryRowContext(ctx, `SELECT timestamp, is_from_me FROM watui_unread_messages WHERE id = ?`, messageID).Scan(&uTS, &uFromMe); uErr == nil {
			details.SentAt = time.Unix(uTS, 0)
			details.IsFromMe = uFromMe
		}
	}

	myJID := ""
	myLID := ""
	if a.client != nil && a.client.Store != nil {
		if a.client.Store.ID != nil {
			myJID = a.client.Store.ID.ToNonAD().String()
		}
		if a.client.Store.LID.User != "" {
			myLID = a.client.Store.LID.ToNonAD().String()
		}
	}

	// 2. Fetch all recorded receipts for this message
	rows, err := a.localDB.QueryContext(ctx, `SELECT user_jid, user_name, receipt_type, timestamp FROM watui_message_receipts WHERE message_id = ? ORDER BY timestamp ASC`, messageID)
	if err == nil {
		defer rows.Close()
		readMap := make(map[string]domain.MessageReceipt)
		deliveredMap := make(map[string]domain.MessageReceipt)

		for rows.Next() {
			var uJID, uName, rType string
			var rTS int64
			if err := rows.Scan(&uJID, &uName, &rType, &rTS); err != nil {
				continue
			}
			t := time.Unix(rTS, 0)
			name := uName
			if name == "" || name == uJID {
				if resolved := a.ResolveContactName(uJID); resolved != "" && resolved != uJID {
					name = resolved
				}
			}

			// In group chats, skip self receipts on messages sent by me
			if details.IsGroup && details.IsFromMe {
				if name == "You" || (myJID != "" && (uJID == myJID || strings.HasPrefix(uJID, myJID+":"))) ||
					(myLID != "" && (uJID == myLID || strings.HasPrefix(uJID, myLID+":"))) {
					continue
				}
			}

			rcpt := domain.MessageReceipt{
				UserJID:   uJID,
				UserName:  name,
				Status:    domain.MessageStatus(rType),
				Timestamp: t,
			}

			if rType == "read" {
				readMap[uJID] = rcpt
				if details.ReadAt.IsZero() || t.Before(details.ReadAt) {
					details.ReadAt = t
				}
			} else if rType == "delivered" {
				deliveredMap[uJID] = rcpt
				if details.DeliveredAt.IsZero() || t.Before(details.DeliveredAt) {
					details.DeliveredAt = t
				}
			}
		}

		for _, r := range readMap {
			details.Read = append(details.Read, r)
		}
		sort.Slice(details.Read, func(i, j int) bool {
			return details.Read[i].Timestamp.Before(details.Read[j].Timestamp)
		})

		for _, r := range deliveredMap {
			details.Delivered = append(details.Delivered, r)
		}
		sort.Slice(details.Delivered, func(i, j int) bool {
			return details.Delivered[i].Timestamp.Before(details.Delivered[j].Timestamp)
		})

		// 3. Incorporate reactions and quote-replies to this message from other participants
		qRows, qErr := a.localDB.QueryContext(ctx, `SELECT sender, sender_name, timestamp FROM watui_messages WHERE quoted_id = ? AND sender != '' AND is_from_me = 0`, messageID)
		if qErr == nil {
			for qRows.Next() {
				var qSender, qSenderName string
				var qTS int64
				if err := qRows.Scan(&qSender, &qSenderName, &qTS); err != nil {
					continue
				}
				name := qSenderName
				if name == "" || name == qSender {
					if resolved := a.ResolveContactName(qSender); resolved != "" && resolved != qSender {
						name = resolved
					}
				}
				if details.IsGroup && details.IsFromMe {
					if name == "You" || (myJID != "" && (qSender == myJID || strings.HasPrefix(qSender, myJID+":"))) ||
						(myLID != "" && (qSender == myLID || strings.HasPrefix(qSender, myLID+":"))) {
						continue
					}
				}
				t := time.Unix(qTS, 0)
				if existing, ok := readMap[qSender]; !ok || t.Before(existing.Timestamp) {
					rcpt := domain.MessageReceipt{
						UserJID:   qSender,
						UserName:  name,
						Status:    domain.MessageStatusRead,
						Timestamp: t,
					}
					readMap[qSender] = rcpt
					details.Read = append(details.Read, rcpt)
					if details.ReadAt.IsZero() || t.Before(details.ReadAt) {
						details.ReadAt = t
					}
				}
				if existing, ok := deliveredMap[qSender]; !ok || t.Before(existing.Timestamp) {
					rcpt := domain.MessageReceipt{
						UserJID:   qSender,
						UserName:  name,
						Status:    domain.MessageStatusDelivered,
						Timestamp: t,
					}
					deliveredMap[qSender] = rcpt
					details.Delivered = append(details.Delivered, rcpt)
					if details.DeliveredAt.IsZero() || t.Before(details.DeliveredAt) {
						details.DeliveredAt = t
					}
				}
			}
			qRows.Close()

			sort.Slice(details.Read, func(i, j int) bool {
				return details.Read[i].Timestamp.Before(details.Read[j].Timestamp)
			})
			sort.Slice(details.Delivered, func(i, j int) bool {
				return details.Delivered[i].Timestamp.Before(details.Delivered[j].Timestamp)
			})
		}

		// 4. If this is a sent message in a chat with newer incoming activity from others, it was definitely delivered
		if details.IsFromMe && !details.SentAt.IsZero() {
			var newerIncoming int
			_ = a.localDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM watui_messages WHERE chat_id = ? AND timestamp >= ? AND is_from_me = 0`, details.ChatID, details.SentAt.Unix()).Scan(&newerIncoming)
			if newerIncoming > 0 && details.Status == domain.MessageStatusSent {
				details.Status = domain.MessageStatusDelivered
				if details.DeliveredAt.IsZero() {
					details.DeliveredAt = details.SentAt
				}
				_, _ = a.localDB.ExecContext(ctx, `UPDATE watui_messages SET status = 'delivered' WHERE id = ? AND status = 'sent'`, messageID)
			}
		}
	}

	// For 1-on-1 chats:
	if !details.IsGroup {
		if !details.ReadAt.IsZero() {
			details.Status = domain.MessageStatusRead
			if details.DeliveredAt.IsZero() {
				details.DeliveredAt = details.ReadAt
			}
		} else if !details.DeliveredAt.IsZero() {
			details.Status = domain.MessageStatusDelivered
		} else if details.Status == domain.MessageStatusRead {
			details.ReadAt = details.SentAt
			details.DeliveredAt = details.SentAt
		} else if details.Status == domain.MessageStatusDelivered {
			details.DeliveredAt = details.SentAt
		}
	} else {
		// Group chats:
		if len(details.Read) > 0 {
			details.Status = domain.MessageStatusRead
		} else if len(details.Delivered) > 0 {
			if details.Status != domain.MessageStatusRead {
				details.Status = domain.MessageStatusDelivered
			}
		}
		if details.Status == domain.MessageStatusRead {
			if details.ReadAt.IsZero() {
				details.ReadAt = details.SentAt
			}
			if details.DeliveredAt.IsZero() {
				details.DeliveredAt = details.SentAt
			}
		} else if details.Status == domain.MessageStatusDelivered {
			if details.DeliveredAt.IsZero() {
				details.DeliveredAt = details.SentAt
			}
		}
	}

	return details, nil
}

// SubscribePresence requests presence updates from WhatsApp for a specific user JID.
func (a *Adapter) SubscribePresence(ctx context.Context, jid string) error {
	if a.client == nil || !a.client.IsConnected() {
		return errors.New("client not connected")
	}
	parsed, err := NormalizeJID(jid)
	if err != nil {
		return err
	}
	if strings.Contains(parsed.String(), "@g.us") {
		return nil
	}
	go func() {
		subCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.client.SendPresence(subCtx, types.PresenceAvailable)
		err := a.client.SubscribePresence(subCtx, parsed)
		if err != nil {
			_ = a.client.SubscribePresence(subCtx, parsed.ToNonAD())
		}
		nonAD := parsed.ToNonAD().String()
		if strings.HasSuffix(nonAD, "@s.whatsapp.net") {
			user := strings.TrimSuffix(nonAD, "@s.whatsapp.net")
			if lid := a.ResolvePhoneToLID(user); lid != "" {
				if lidJID, lErr := NormalizeJID(lid + "@lid"); lErr == nil {
					_ = a.client.SubscribePresence(subCtx, lidJID)
				}
			}
		} else if strings.HasSuffix(nonAD, "@lid") {
			user := strings.TrimSuffix(nonAD, "@lid")
			if pn := a.ResolveLIDToPhone(user); pn != "" {
				if pnJID, pErr := NormalizeJID(pn + "@s.whatsapp.net"); pErr == nil {
					_ = a.client.SubscribePresence(subCtx, pnJID)
				}
			}
		}
	}()
	return nil
}

// GetUserPresence retrieves the cached presence information for a given user JID.
func (a *Adapter) GetUserPresence(jid string) (domain.UserPresence, bool) {
	a.presenceMu.RLock()
	defer a.presenceMu.RUnlock()
	if a.presences == nil {
		return domain.UserPresence{}, false
	}
	parsed, err := NormalizeJID(jid)
	if err == nil {
		nonAD := parsed.ToNonAD().String()
		if p, ok := a.presences[nonAD]; ok {
			return p, true
		}
		if p, ok := a.presences[parsed.String()]; ok {
			return p, true
		}
		if strings.HasSuffix(nonAD, "@s.whatsapp.net") {
			user := strings.TrimSuffix(nonAD, "@s.whatsapp.net")
			if lid := a.ResolvePhoneToLID(user); lid != "" {
				if p, ok := a.presences[lid+"@lid"]; ok {
					return p, true
				}
			}
		} else if strings.HasSuffix(nonAD, "@lid") {
			user := strings.TrimSuffix(nonAD, "@lid")
			if pn := a.ResolveLIDToPhone(user); pn != "" {
				if p, ok := a.presences[pn+"@s.whatsapp.net"]; ok {
					return p, true
				}
			}
		}
	}
	if p, ok := a.presences[jid]; ok {
		return p, true
	}
	return domain.UserPresence{}, false
}

// OnPresence registers a listener for live presence updates.
func (a *Adapter) OnPresence(handler domain.PresenceHandler) {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	a.presenceHandlers = append(a.presenceHandlers, handler)
}

// OnReceipt registers a listener for incoming live receipts.
func (a *Adapter) OnReceipt(handler domain.ReceiptHandler) {
	a.receiptMu.Lock()
	defer a.receiptMu.Unlock()
	a.receiptHandlers = append(a.receiptHandlers, handler)
}

func (a *Adapter) notifyPresence(p domain.UserPresence) {
	a.presenceMu.Lock()
	if a.presences == nil {
		a.presences = make(map[string]domain.UserPresence)
	}
	a.presences[p.JID] = p
	handlers := append([]domain.PresenceHandler(nil), a.presenceHandlers...)
	a.presenceMu.Unlock()
	for _, h := range handlers {
		h(p)
	}
}

func (a *Adapter) notifyReceipt(chatID string, msgIDs []string, receiptType string, ts time.Time) {
	a.receiptMu.RLock()
	handlers := append([]domain.ReceiptHandler(nil), a.receiptHandlers...)
	a.receiptMu.RUnlock()
	for _, h := range handlers {
		h(chatID, msgIDs, receiptType, ts)
	}
}
