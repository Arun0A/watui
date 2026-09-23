package domain

import "time"

// Chat represents a 1-on-1 or group conversation thread.
type Chat struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	IsGroup         bool      `json:"is_group"`
	UnreadCount     int       `json:"unread_count"`
	LastMessageTime time.Time `json:"last_message_time"`
	LastMessageText string    `json:"last_message_text"`
}
