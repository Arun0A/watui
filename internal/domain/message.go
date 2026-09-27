package domain

import (
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

// Message is the core protocol-agnostic representation of a WhatsApp message.
type Message struct {
	ID         string        `json:"id"`
	ChatID     string        `json:"chat_id"`
	ChatName   string        `json:"chat_name,omitempty"`
	Sender     string        `json:"sender"`
	SenderName string        `json:"sender_name,omitempty"`
	Timestamp  time.Time     `json:"timestamp"`
	IsFromMe   bool          `json:"is_from_me"`
	Type       MessageType   `json:"type"`
	Body       string        `json:"body"`
	Status     MessageStatus `json:"status"`
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
