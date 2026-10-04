package daemon

import (
	"encoding/json"
	"time"

	"watui/internal/domain"
)

// RPCRequest represents a call from a client (TUI) to the daemon.
type RPCRequest struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCResponse represents a reply or an event from the daemon to the client.
type RPCResponse struct {
	ID     uint64          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"` // "message", "status", "contacts", "dismiss", "archived"
}

// InitialSnapshot is pushed to a connecting client upon successful handshake.
type InitialSnapshot struct {
	Status         domain.ConnectionStatus `json:"status"`
	UnreadMessages []domain.Message        `json:"unread_messages"`
	Contacts       []domain.Contact        `json:"contacts"`
	ArchivedChats  map[string]bool         `json:"archived_chats"`
}

type SendTextParams struct {
	ChatID       string `json:"chat_id"`
	Text         string `json:"text"`
	QuotedID     string `json:"quoted_id,omitempty"`
	QuotedBody   string `json:"quoted_body,omitempty"`
	QuotedSender string `json:"quoted_sender,omitempty"`
}

type SendFileParams struct {
	ChatID       string `json:"chat_id"`
	FilePath     string `json:"file_path"`
	Caption      string `json:"caption"`
	QuotedID     string `json:"quoted_id,omitempty"`
	QuotedBody   string `json:"quoted_body,omitempty"`
	QuotedSender string `json:"quoted_sender,omitempty"`
}

type MarkReadParams struct {
	ChatID     string   `json:"chat_id"`
	SenderID   string   `json:"sender_id"`
	MessageIDs []string `json:"message_ids"`
}

type DismissParams struct {
	ChatID string `json:"chat_id"`
}

type DeleteMessageParams struct {
	ChatID            string `json:"chat_id"`
	MessageID         string `json:"message_id"`
	DeleteForEveryone bool   `json:"delete_for_everyone"`
	Sender            string `json:"sender,omitempty"`
}

type EditMessageParams struct {
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	NewText   string `json:"new_text"`
}

type DownloadMediaParams struct {
	Message domain.Message `json:"message"`
}

type EnsureGroupsParams struct {
	JIDs []string `json:"jids"`
}

type SetChatArchivedParams struct {
	ChatID   string `json:"chat_id"`
	Archived bool   `json:"archived"`
}

type GetChatHistoryParams struct {
	ChatID          string    `json:"chat_id"`
	Limit           int       `json:"limit"`
	BeforeTimestamp time.Time `json:"before_timestamp"`
}
