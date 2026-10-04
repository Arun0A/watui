package domain

import (
	"context"
	"time"
)

// ConnectionStatus describes the state of the connection to WhatsApp.
type ConnectionStatus string

const (
	StatusDisconnected ConnectionStatus = "disconnected"
	StatusConnecting   ConnectionStatus = "connecting"
	StatusWaitingQR    ConnectionStatus = "waiting_qr"
	StatusConnected    ConnectionStatus = "connected"
	StatusLoggedOut    ConnectionStatus = "logged_out"
)

// MessageHandler is a callback function for newly arrived or decrypted messages.
type MessageHandler func(msg Message)

// StatusHandler is a callback function for connection status changes.
type StatusHandler func(status ConnectionStatus)

// WhatsAppAdapter defines the clean contract between WhatsApp's wire protocol
// and the rest of our application (local store, TUI, CLI).
// The TUI or repository code NEVER touches protobufs, sockets, or crypto keys.
type WhatsAppAdapter interface {
	// Connect establishes the connection. If pairing is required, QR code is emitted.
	Connect(ctx context.Context) error

	// Disconnect gracefully closes the connection.
	Disconnect()

	// IsLoggedIn returns true if a valid session exists in local storage.
	IsLoggedIn() bool

	// OnMessage registers a listener for incoming messages.
	OnMessage(handler MessageHandler)

	// OnStatus registers a listener for connection status changes.
	OnStatus(handler StatusHandler)

	// SendTextMessage sends a plain text message to a given chat JID, optionally quoting a message ID/content.
	SendTextMessage(ctx context.Context, chatID string, text string, quotedMsg ...string) (Message, error)

	// SendFileMessage uploads and sends a local media or document file to a chat, optionally quoting a message.
	SendFileMessage(ctx context.Context, chatID string, filePath string, caption string, quotedMsg ...string) (Message, error)

	// GetContacts retrieves all known contacts from local store.
	GetContacts(ctx context.Context) ([]Contact, error)

	// MarkRead sends a read receipt to WhatsApp for the given message IDs.
	MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error

	// OnContactsUpdated registers a listener for when contacts/groups are updated in background.
	OnContactsUpdated(handler func([]Contact))

	// GetUnreadMessages retrieves all unread messages persisted in local SQLite across restarts.
	GetUnreadMessages(ctx context.Context) ([]Message, error)

	// DismissUnread deletes unread messages for the given chat from local persistence.
	DismissUnread(ctx context.Context, chatID string) error

	// Sync explicitly pulls server updates (such as read state mutations) from WhatsApp.
	Sync(ctx context.Context) error

	// OnChatDismissed registers a listener triggered when a chat has been marked read remotely.
	OnChatDismissed(handler func(chatID string))

	// DownloadMedia downloads the media attachment for a message on demand and returns the local file path.
	DownloadMedia(ctx context.Context, msg Message) (string, error)

	// EnsureGroupNames ensures that metadata for the provided group JIDs is fetched and cached.
	EnsureGroupNames(ctx context.Context, jids []string)

	// IsChatArchived checks whether the specified chat JID is archived in WhatsApp.
	IsChatArchived(chatID string) bool

	// GetArchivedChats returns a map of all currently archived chat JIDs.
	GetArchivedChats() map[string]bool

	// SetChatArchived archives or unarchives the specified chat JID in WhatsApp.
	SetChatArchived(ctx context.Context, chatID string, archived bool) error

	// DeleteMessage deletes a message locally ("delete for me") or revokes it on WhatsApp servers for everyone ("delete for all").
	DeleteMessage(ctx context.Context, chatID string, messageID string, deleteForEveryone bool, sender ...string) error

	// GetChatHistory fetches up to limit historical messages for the given chat from local storage,
	// returning messages strictly older than beforeTimestamp (if non-zero) or the latest if beforeTimestamp is zero.
	GetChatHistory(ctx context.Context, chatID string, limit int, beforeTimestamp time.Time) ([]Message, error)

	// GetGroupParticipants retrieves the list of participants in a group chat.
	GetGroupParticipants(ctx context.Context, groupJID string) ([]Contact, error)
}
