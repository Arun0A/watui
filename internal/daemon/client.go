package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"watui/internal/domain"
)

// RemoteAdapter implements domain.WhatsAppAdapter by communicating over IPC with a running daemon.
type RemoteAdapter struct {
	conn    net.Conn
	enc     *json.Encoder
	dec     *json.Decoder
	writeMu sync.Mutex

	reqMu   sync.Mutex
	reqID   uint64
	pending map[uint64]chan RPCResponse

	handlersMu       sync.RWMutex
	messageHandlers  []domain.MessageHandler
	statusHandlers   []domain.StatusHandler
	contactsHandlers []func([]domain.Contact)
	dismissHandlers  []func(string)

	statusMu      sync.RWMutex
	currentStatus domain.ConnectionStatus

	archivedMu    sync.RWMutex
	archivedChats map[string]bool

	mutedMu    sync.RWMutex
	mutedChats map[string]bool

	ephemeralMu       sync.RWMutex
	ephemeralChats    map[string]uint32
	ephemeralHandlers []func(string, uint32)

	cachedUnread   []domain.Message
	cachedContacts []domain.Contact
	cachedMu       sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
}

// ConnectRemote dials the background daemon IPC and returns a fully initialized RemoteAdapter.
// Returns an error if the daemon is not running or unreachable.
func ConnectRemote(dbPath string) (*RemoteAdapter, error) {
	conn, err := DialIPC(dbPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &RemoteAdapter{
		conn:           conn,
		enc:            json.NewEncoder(conn),
		dec:            json.NewDecoder(conn),
		pending:        make(map[uint64]chan RPCResponse),
		archivedChats:  make(map[string]bool),
		mutedChats:     make(map[string]bool),
		ephemeralChats: make(map[string]uint32),
		currentStatus:  domain.StatusConnected,
		ctx:            ctx,
		cancel:         cancel,
	}

	// 1. Wait for initial snapshot from daemon
	var initResp RPCResponse
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	if err := r.dec.Decode(&initResp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to receive daemon handshake: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	if initResp.Event == "snapshot" {
		var snap InitialSnapshot
		if err := json.Unmarshal(initResp.Result, &snap); err == nil {
			r.currentStatus = snap.Status
			r.cachedUnread = snap.UnreadMessages
			r.cachedContacts = snap.Contacts
			r.archivedChats = snap.ArchivedChats
			if snap.MutedChats != nil {
				r.mutedChats = snap.MutedChats
			}
			if snap.EphemeralChats != nil {
				r.ephemeralChats = snap.EphemeralChats
			}
		}
	}

	// 2. Start background reader for events and replies
	go r.readLoop()

	return r, nil
}

func (r *RemoteAdapter) readLoop() {
	defer func() {
		r.cancel()
		_ = r.conn.Close()
		r.statusMu.Lock()
		r.currentStatus = domain.StatusDisconnected
		r.statusMu.Unlock()
		r.dispatchStatus(domain.StatusDisconnected)
	}()

	for {
		var resp RPCResponse
		if err := r.dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}

		if resp.Event != "" {
			r.handleEvent(resp)
			continue
		}

		if resp.ID > 0 {
			r.reqMu.Lock()
			ch, ok := r.pending[resp.ID]
			delete(r.pending, resp.ID)
			r.reqMu.Unlock()
			if ok {
				ch <- resp
			}
		}
	}
}

func (r *RemoteAdapter) handleEvent(resp RPCResponse) {
	switch resp.Event {
	case "message":
		var msg domain.Message
		if err := json.Unmarshal(resp.Result, &msg); err == nil {
			r.handlersMu.RLock()
			handlers := append([]domain.MessageHandler(nil), r.messageHandlers...)
			r.handlersMu.RUnlock()
			for _, h := range handlers {
				h(msg)
			}
		}

	case "status":
		var status domain.ConnectionStatus
		if err := json.Unmarshal(resp.Result, &status); err == nil {
			r.statusMu.Lock()
			r.currentStatus = status
			r.statusMu.Unlock()
			r.dispatchStatus(status)
		}

	case "contacts":
		var contacts []domain.Contact
		if err := json.Unmarshal(resp.Result, &contacts); err == nil {
			r.cachedMu.Lock()
			r.cachedContacts = contacts
			r.cachedMu.Unlock()
			r.handlersMu.RLock()
			handlers := make([]func([]domain.Contact), len(r.contactsHandlers))
			copy(handlers, r.contactsHandlers)
			r.handlersMu.RUnlock()
			for _, h := range handlers {
				h(contacts)
			}
		}

	case "dismiss":
		var chatID string
		if err := json.Unmarshal(resp.Result, &chatID); err == nil {
			r.handlersMu.RLock()
			handlers := make([]func(string), len(r.dismissHandlers))
			copy(handlers, r.dismissHandlers)
			r.handlersMu.RUnlock()
			for _, h := range handlers {
				h(chatID)
			}
		}

	case "archived":
		var arch map[string]bool
		if err := json.Unmarshal(resp.Result, &arch); err == nil && len(arch) > 0 {
			r.archivedMu.Lock()
			r.archivedChats = arch
			r.archivedMu.Unlock()
		} else {
			var p SetChatArchivedParams
			if err := json.Unmarshal(resp.Result, &p); err == nil && p.ChatID != "" {
				r.archivedMu.Lock()
				if r.archivedChats == nil {
					r.archivedChats = make(map[string]bool)
				}
				if p.Archived {
					r.archivedChats[p.ChatID] = true
				} else {
					delete(r.archivedChats, p.ChatID)
				}
				r.archivedMu.Unlock()
			}
		}

	case "muted":
		var m map[string]bool
		if err := json.Unmarshal(resp.Result, &m); err == nil && len(m) > 0 {
			r.mutedMu.Lock()
			r.mutedChats = m
			r.mutedMu.Unlock()
		} else {
			var p SetChatMutedParams
			if err := json.Unmarshal(resp.Result, &p); err == nil && p.ChatID != "" {
				r.mutedMu.Lock()
				if r.mutedChats == nil {
					r.mutedChats = make(map[string]bool)
				}
				if p.Muted {
					r.mutedChats[p.ChatID] = true
				} else {
					delete(r.mutedChats, p.ChatID)
				}
				r.mutedMu.Unlock()
			}
		}

	case "ephemeral":
		var p SetChatEphemeralParams
		if err := json.Unmarshal(resp.Result, &p); err == nil && p.ChatID != "" {
			r.ephemeralMu.Lock()
			if r.ephemeralChats == nil {
				r.ephemeralChats = make(map[string]uint32)
			}
			if p.Timer > 0 {
				r.ephemeralChats[p.ChatID] = p.Timer
			} else {
				delete(r.ephemeralChats, p.ChatID)
			}
			r.ephemeralMu.Unlock()

			r.handlersMu.RLock()
			handlers := make([]func(string, uint32), len(r.ephemeralHandlers))
			copy(handlers, r.ephemeralHandlers)
			r.handlersMu.RUnlock()
			for _, h := range handlers {
				h(p.ChatID, p.Timer)
			}
		}
	}
}

func (r *RemoteAdapter) dispatchStatus(status domain.ConnectionStatus) {
	r.handlersMu.RLock()
	handlers := append([]domain.StatusHandler(nil), r.statusHandlers...)
	r.handlersMu.RUnlock()
	for _, h := range handlers {
		h(status)
	}
}

func (r *RemoteAdapter) call(ctx context.Context, method string, params interface{}, result interface{}) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}

	id := atomic.AddUint64(&r.reqID, 1)
	ch := make(chan RPCResponse, 1)

	r.reqMu.Lock()
	r.pending[id] = ch
	r.reqMu.Unlock()

	defer func() {
		r.reqMu.Lock()
		delete(r.pending, id)
		r.reqMu.Unlock()
	}()

	var pData []byte
	if params != nil {
		var err error
		pData, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}

	req := RPCRequest{
		ID:     id,
		Method: method,
		Params: pData,
	}

	r.writeMu.Lock()
	err := r.enc.Encode(req)
	r.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send RPC request: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.ctx.Done():
		return errors.New("daemon connection closed")
	case resp := <-ch:
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// -------------------------------------------------------------
// domain.WhatsAppAdapter Interface Implementation
// -------------------------------------------------------------

func (r *RemoteAdapter) Connect(ctx context.Context) error {
	// Daemon is already connected! Notify listeners of current status immediately.
	r.statusMu.RLock()
	st := r.currentStatus
	r.statusMu.RUnlock()
	r.dispatchStatus(st)
	return nil
}

func (r *RemoteAdapter) Disconnect() {
	r.cancel()
	_ = r.conn.Close()
}

func (r *RemoteAdapter) IsLoggedIn() bool {
	return true
}

func (r *RemoteAdapter) OnMessage(handler domain.MessageHandler) {
	r.handlersMu.Lock()
	r.messageHandlers = append(r.messageHandlers, handler)
	r.handlersMu.Unlock()
}

func (r *RemoteAdapter) OnStatus(handler domain.StatusHandler) {
	r.handlersMu.Lock()
	r.statusHandlers = append(r.statusHandlers, handler)
	r.handlersMu.Unlock()

	r.statusMu.RLock()
	st := r.currentStatus
	r.statusMu.RUnlock()
	if st != "" {
		handler(st)
	}
}

func (r *RemoteAdapter) OnContactsUpdated(handler func([]domain.Contact)) {
	r.handlersMu.Lock()
	r.contactsHandlers = append(r.contactsHandlers, handler)
	r.handlersMu.Unlock()
}

func (r *RemoteAdapter) OnChatDismissed(handler func(chatID string)) {
	r.handlersMu.Lock()
	r.dismissHandlers = append(r.dismissHandlers, handler)
	r.handlersMu.Unlock()
}

func (r *RemoteAdapter) SendTextMessage(ctx context.Context, chatID string, text string, quotedMsg ...string) (domain.Message, error) {
	params := SendTextParams{
		ChatID: chatID,
		Text:   text,
	}
	if len(quotedMsg) > 0 {
		params.QuotedID = quotedMsg[0]
	}
	if len(quotedMsg) > 1 {
		params.QuotedBody = quotedMsg[1]
	}
	if len(quotedMsg) > 2 {
		params.QuotedSender = quotedMsg[2]
	}
	var msg domain.Message
	err := r.call(ctx, "send_text", params, &msg)
	return msg, err
}

func (r *RemoteAdapter) SendFileMessage(ctx context.Context, chatID string, filePath string, caption string, quotedMsg ...string) (domain.Message, error) {
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
	var msg domain.Message
	err := r.call(ctx, "send_file", SendFileParams{
		ChatID:       chatID,
		FilePath:     filePath,
		Caption:      caption,
		QuotedID:     quotedID,
		QuotedBody:   quotedBody,
		QuotedSender: quotedSender,
	}, &msg)
	return msg, err
}

func (r *RemoteAdapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	r.cachedMu.RLock()
	cached := r.cachedContacts
	r.cachedMu.RUnlock()
	if len(cached) > 0 {
		return cached, nil
	}

	var contacts []domain.Contact
	err := r.call(ctx, "get_contacts", nil, &contacts)
	if err == nil {
		r.cachedMu.Lock()
		r.cachedContacts = contacts
		r.cachedMu.Unlock()
	}
	return contacts, err
}

func (r *RemoteAdapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
	return r.call(ctx, "mark_read", MarkReadParams{
		ChatID:     chatID,
		SenderID:   senderID,
		MessageIDs: messageIDs,
	}, nil)
}

func (r *RemoteAdapter) GetUnreadMessages(ctx context.Context) ([]domain.Message, error) {
	r.cachedMu.RLock()
	cached := r.cachedUnread
	r.cachedMu.RUnlock()
	if cached != nil {
		// Return snapshot once, subsequent calls can query fresh
		r.cachedMu.Lock()
		r.cachedUnread = nil
		r.cachedMu.Unlock()
		return cached, nil
	}

	var msgs []domain.Message
	err := r.call(ctx, "get_unread", nil, &msgs)
	return msgs, err
}

func (r *RemoteAdapter) DismissUnread(ctx context.Context, chatID string) error {
	return r.call(ctx, "dismiss_unread", DismissParams{ChatID: chatID}, nil)
}

func (r *RemoteAdapter) Sync(ctx context.Context) error {
	return r.call(ctx, "sync", nil, nil)
}

func (r *RemoteAdapter) DownloadMedia(ctx context.Context, msg domain.Message) (string, error) {
	var path string
	err := r.call(ctx, "download_media", DownloadMediaParams{Message: msg}, &path)
	return path, err
}

func (r *RemoteAdapter) EnsureGroupNames(ctx context.Context, jids []string) {
	go func() {
		_ = r.call(ctx, "ensure_group_names", EnsureGroupsParams{JIDs: jids}, nil)
	}()
}

func (r *RemoteAdapter) IsChatArchived(chatID string) bool {
	if chatID == "" {
		return false
	}
	clean := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 {
			clean = clean[:idx] + clean[atIdx:]
		}
	}
	r.archivedMu.RLock()
	defer r.archivedMu.RUnlock()
	if r.archivedChats != nil {
		return r.archivedChats[chatID] || r.archivedChats[clean]
	}
	return false
}

func (r *RemoteAdapter) GetArchivedChats() map[string]bool {
	r.archivedMu.RLock()
	defer r.archivedMu.RUnlock()
	res := make(map[string]bool, len(r.archivedChats))
	for k, v := range r.archivedChats {
		res[k] = v
	}
	return res
}

func (r *RemoteAdapter) SetChatArchived(ctx context.Context, chatID string, archived bool) error {
	r.archivedMu.Lock()
	if r.archivedChats == nil {
		r.archivedChats = make(map[string]bool)
	}
	if archived {
		r.archivedChats[chatID] = true
	} else {
		delete(r.archivedChats, chatID)
	}
	r.archivedMu.Unlock()

	return r.call(ctx, "set_chat_archived", SetChatArchivedParams{ChatID: chatID, Archived: archived}, nil)
}

func (r *RemoteAdapter) DeleteMessage(ctx context.Context, chatID string, messageID string, deleteForEveryone bool, sender ...string) error {
	var s string
	if len(sender) > 0 {
		s = sender[0]
	}
	return r.call(ctx, "delete_message", DeleteMessageParams{
		ChatID:            chatID,
		MessageID:         messageID,
		DeleteForEveryone: deleteForEveryone,
		Sender:            s,
	}, nil)
}

func (r *RemoteAdapter) EditMessage(ctx context.Context, chatID string, messageID string, newText string) error {
	return r.call(ctx, "edit_message", EditMessageParams{
		ChatID:    chatID,
		MessageID: messageID,
		NewText:   newText,
	}, nil)
}

func (r *RemoteAdapter) GetChatHistory(ctx context.Context, chatID string, limit int, beforeTimestamp time.Time) ([]domain.Message, error) {
	var msgs []domain.Message
	err := r.call(ctx, "get_chat_history", GetChatHistoryParams{
		ChatID:          chatID,
		Limit:           limit,
		BeforeTimestamp: beforeTimestamp,
	}, &msgs)
	return msgs, err
}

func (r *RemoteAdapter) GetGroupParticipants(ctx context.Context, groupJID string) ([]domain.Contact, error) {
	var participants []domain.Contact
	err := r.call(ctx, "get_group_participants", groupJID, &participants)
	return participants, err
}

func (r *RemoteAdapter) IsChatMuted(chatID string) bool {
	r.mutedMu.RLock()
	defer r.mutedMu.RUnlock()
	clean := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 && atIdx > idx {
			clean = clean[:idx] + clean[atIdx:]
		}
	}
	if r.mutedChats != nil {
		return r.mutedChats[chatID] || r.mutedChats[clean]
	}
	return false
}

func (r *RemoteAdapter) GetMutedChats() map[string]bool {
	r.mutedMu.RLock()
	defer r.mutedMu.RUnlock()
	res := make(map[string]bool, len(r.mutedChats))
	for k, v := range r.mutedChats {
		res[k] = v
	}
	return res
}

func (r *RemoteAdapter) SetChatMuted(ctx context.Context, chatID string, muted bool, duration time.Duration) error {
	r.mutedMu.Lock()
	if r.mutedChats == nil {
		r.mutedChats = make(map[string]bool)
	}
	if muted {
		r.mutedChats[chatID] = true
	} else {
		delete(r.mutedChats, chatID)
	}
	r.mutedMu.Unlock()

	return r.call(ctx, "set_chat_muted", SetChatMutedParams{ChatID: chatID, Muted: muted, Duration: duration}, nil)
}

func (r *RemoteAdapter) GetChatEphemeralTimer(chatID string) uint32 {
	r.ephemeralMu.RLock()
	defer r.ephemeralMu.RUnlock()
	clean := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 && atIdx > idx {
			clean = clean[:idx] + clean[atIdx:]
		}
	}
	if r.ephemeralChats != nil {
		if t, ok := r.ephemeralChats[clean]; ok {
			return t
		}
		return r.ephemeralChats[chatID]
	}
	return 0
}

func (r *RemoteAdapter) GetEphemeralChats() map[string]uint32 {
	r.ephemeralMu.RLock()
	defer r.ephemeralMu.RUnlock()
	res := make(map[string]uint32, len(r.ephemeralChats))
	for k, v := range r.ephemeralChats {
		if v > 0 {
			res[k] = v
		}
	}
	return res
}

func (r *RemoteAdapter) OnChatEphemeral(handler func(chatID string, timer uint32)) {
	r.handlersMu.Lock()
	defer r.handlersMu.Unlock()
	r.ephemeralHandlers = append(r.ephemeralHandlers, handler)
}

func (r *RemoteAdapter) SetChatDisappearingTimer(ctx context.Context, chatID string, timer time.Duration) error {
	timerSec := uint32(timer.Seconds())
	clean := chatID
	if idx := strings.Index(clean, ":"); idx != -1 {
		if atIdx := strings.Index(clean, "@"); atIdx != -1 && atIdx > idx {
			clean = clean[:idx] + clean[atIdx:]
		}
	}
	r.ephemeralMu.Lock()
	if r.ephemeralChats == nil {
		r.ephemeralChats = make(map[string]uint32)
	}
	if timerSec > 0 {
		r.ephemeralChats[clean] = timerSec
		r.ephemeralChats[chatID] = timerSec
	} else {
		delete(r.ephemeralChats, clean)
		delete(r.ephemeralChats, chatID)
	}
	r.ephemeralMu.Unlock()

	return r.call(ctx, "set_chat_ephemeral", SetChatEphemeralParams{ChatID: chatID, Timer: timerSec}, nil)
}
