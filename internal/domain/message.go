package domain

import (
	"regexp"
	"strings"
	"time"
)

// MessageType indicates the type of payload contained in a message.
type MessageType string

const (
	MessageTypeText     MessageType = "text"
	MessageTypeImage    MessageType = "image"
	MessageTypeAudio    MessageType = "audio"
	MessageTypeVideo    MessageType = "video"
	MessageTypeDocument MessageType = "document"
	MessageTypeReaction MessageType = "reaction"
	MessageTypeSticker  MessageType = "sticker"
	MessageTypeUnknown  MessageType = "unknown"
)

// MessageStatus represents the delivery lifecycle of a message.
type MessageStatus string

const (
	MessageStatusPending   MessageStatus = "pending"
	MessageStatusSent      MessageStatus = "sent"
	MessageStatusDelivered MessageStatus = "delivered"
	MessageStatusRead      MessageStatus = "read"
	MessageStatusFailed    MessageStatus = "failed"
)

var mentionYouRegex = regexp.MustCompile(`(?:^|[^\w@])(@You)\b`)

// Message is the core protocol-agnostic representation of a WhatsApp message.
type Message struct {
	ID            string        `json:"id"`
	ChatID        string        `json:"chat_id"`
	ChatName      string        `json:"chat_name,omitempty"`
	Sender        string        `json:"sender"`
	SenderName    string        `json:"sender_name,omitempty"`
	Timestamp     time.Time     `json:"timestamp"`
	IsFromMe      bool          `json:"is_from_me"`
	Type          MessageType   `json:"type"`
	Body          string        `json:"body"`
	Status        MessageStatus `json:"status"`
	QuotedID      string        `json:"quoted_id,omitempty"`
	QuotedText    string        `json:"quoted_text,omitempty"`
	QuotedSender  string        `json:"quoted_sender,omitempty"`
	MentionedJIDs []string      `json:"mentioned_jids,omitempty"`
	IsMentioned   bool          `json:"is_mentioned,omitempty"`
	IsEdit        bool          `json:"is_edit,omitempty"`
}

// IsMedia returns true if the message represents a media attachment that can be previewed.
func (m Message) IsMedia() bool {
	switch m.Type {
	case MessageTypeImage, MessageTypeVideo, MessageTypeSticker, MessageTypeAudio, MessageTypeDocument:
		return true
	default:
		return false
	}
}

// MentionsMe checks whether the incoming message explicitly mentions or tags the logged-in user.
func (m Message) MentionsMe() bool {
	if m.IsFromMe {
		return false
	}
	if m.IsMentioned {
		return true
	}
	if mentionYouRegex.MatchString(m.Body) || strings.Contains(m.Body, "@You") {
		return true
	}
	return false
}
