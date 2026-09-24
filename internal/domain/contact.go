package domain

// Contact represents an address book contact, participant, or group.
type Contact struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	PushName     string `json:"push_name"`
	BusinessName string `json:"business_name,omitempty"`
	IsGroup      bool   `json:"is_group,omitempty"`
}
