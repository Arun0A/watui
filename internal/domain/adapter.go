package domain

import (
	"context"
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

	// SendTextMessage sends a plain text message to a given chat JID.
	SendTextMessage(ctx context.Context, chatID string, text string) (Message, error)

	// GetContacts retrieves all known contacts from local store.
	GetContacts(ctx context.Context) ([]Contact, error)

	// MarkRead sends a read receipt to WhatsApp for the given message IDs.
	MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error

	// OnContactsUpdated registers a listener for when contacts/groups are updated in background.
	OnContactsUpdated(handler func([]Contact))
}
