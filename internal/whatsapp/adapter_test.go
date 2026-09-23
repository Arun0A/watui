package whatsapp

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"watui/internal/domain"
)

func TestExtractDomainMessage(t *testing.T) {
	// Adapter instance with dummy fields for testing extraction
	adapter := &Adapter{}

	testChatJID := types.NewJID("1234567890", "s.whatsapp.net")
	testSenderJID := types.NewJID("1234567890", "s.whatsapp.net")
	testTimestamp := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		evt          *events.Message
		expectedType domain.MessageType
		expectedBody string
	}{
		{
			name: "plain conversation text",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:     testChatJID,
						Sender:   testSenderJID,
						IsFromMe: false,
					},
					ID:        "MSG123",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					Conversation: proto.String("Hello from Linux!"),
				},
			},
			expectedType: domain.MessageTypeText,
			expectedBody: "Hello from Linux!",
		},
		{
			name: "extended text message",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:     testChatJID,
						Sender:   testSenderJID,
						IsFromMe: true,
					},
					ID:        "MSG124",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					ExtendedTextMessage: &waE2E.ExtendedTextMessage{
						Text: proto.String("Check this link: https://example.com"),
					},
				},
			},
			expectedType: domain.MessageTypeText,
			expectedBody: "Check this link: https://example.com",
		},
		{
			name: "image message with caption",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:   testChatJID,
						Sender: testSenderJID,
					},
					ID:        "MSG125",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					ImageMessage: &waE2E.ImageMessage{
						Caption: proto.String("Look at this screenshot"),
					},
				},
			},
			expectedType: domain.MessageTypeImage,
			expectedBody: "Look at this screenshot",
		},
		{
			name: "reaction message",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:   testChatJID,
						Sender: testSenderJID,
					},
					ID:        "MSG126",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					ReactionMessage: &waE2E.ReactionMessage{
						Text: proto.String("🚀"),
					},
				},
			},
			expectedType: domain.MessageTypeReaction,
			expectedBody: "🚀",
		},
	}

	for _, tc := range tests {
		msg := adapter.extractDomainMessage(tc.evt)
		if msg.Type != tc.expectedType {
			t.Errorf("[%s] Expected type %q, got %q", tc.name, tc.expectedType, msg.Type)
		}
		if msg.Body != tc.expectedBody {
			t.Errorf("[%s] Expected body %q, got %q", tc.name, tc.expectedBody, msg.Body)
		}
		if msg.ID != tc.evt.Info.ID {
			t.Errorf("[%s] Expected ID %q, got %q", tc.name, tc.evt.Info.ID, msg.ID)
		}
	}
}
