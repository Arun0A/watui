package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"watui/internal/config"
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
						Text: proto.String("+1"),
					},
				},
			},
			expectedType: domain.MessageTypeReaction,
			expectedBody: "[Reaction: +1]",
		},
	}

	for _, tc := range tests {
		msg, ok := adapter.extractDomainMessage(tc.evt)
		if !ok {
			t.Fatalf("[%s] Expected ok=true, got false", tc.name)
		}
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

	// Test protocol / sender key distribution message is dropped
	protocolEvt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   testChatJID,
				Sender: testSenderJID,
			},
			ID: "PROTO1",
		},
		Message: &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
		},
	}
	_, ok := adapter.extractDomainMessage(protocolEvt)
	if ok {
		t.Errorf("Expected SenderKeyDistributionMessage to be filtered out (ok=false), but got ok=true")
	}
}

func TestChatHistoryStorageAndCyclicPruning(t *testing.T) {
	// Create an in-memory SQLite database
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open in-memory db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS watui_messages (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		chat_name TEXT,
		sender TEXT NOT NULL,
		sender_name TEXT,
		timestamp INTEGER NOT NULL,
		body TEXT,
		type TEXT,
		is_from_me BOOLEAN
	);
	CREATE INDEX IF NOT EXISTS idx_watui_messages_chat_ts ON watui_messages(chat_id, timestamp DESC);`)
	if err != nil {
		t.Fatalf("Failed to create tables: %v", err)
	}

	persistTrue := true
	cfg := &config.Config{
		PersistChatHistory:   &persistTrue,
		CycleMsgCountPerChat: 5, // Small cap of 5 messages for test
	}

	adapter := &Adapter{
		localDB:   db,
		appConfig: cfg,
	}

	chatID := "testuser@s.whatsapp.net"
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Insert 8 messages for chatID
	for i := 1; i <= 8; i++ {
		adapter.saveHistoryMessage(domain.Message{
			ID:        fmt.Sprintf("MSG%d", i),
			ChatID:    chatID,
			Sender:    chatID,
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Body:      fmt.Sprintf("Body %d", i),
			Type:      domain.MessageTypeText,
		})
	}

	// 1. Verify cyclic pruning: only 5 newest messages should remain (MSG4 to MSG8)
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM watui_messages WHERE chat_id = ?", chatID).Scan(&count)
	if count != 5 {
		t.Fatalf("Expected 5 messages after cyclic pruning, got %d", count)
	}

	// 2. Fetch latest 3 messages
	msgs, err := adapter.GetChatHistory(context.Background(), chatID, 3, time.Time{})
	if err != nil {
		t.Fatalf("Failed to get chat history: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("Expected 3 messages, got %d", len(msgs))
	}
	// Should be MSG6, MSG7, MSG8 in chronological order
	if msgs[0].ID != "MSG6" || msgs[1].ID != "MSG7" || msgs[2].ID != "MSG8" {
		t.Errorf("Expected [MSG6, MSG7, MSG8], got [%s, %s, %s]", msgs[0].ID, msgs[1].ID, msgs[2].ID)
	}

	// 3. Paginate older messages before MSG6
	olderMsgs, err := adapter.GetChatHistory(context.Background(), chatID, 3, msgs[0].Timestamp)
	if err != nil {
		t.Fatalf("Failed to paginate older chat history: %v", err)
	}
	if len(olderMsgs) != 2 {
		t.Fatalf("Expected 2 older messages (MSG4, MSG5), got %d", len(olderMsgs))
	}
	if olderMsgs[0].ID != "MSG4" || olderMsgs[1].ID != "MSG5" {
		t.Errorf("Expected [MSG4, MSG5], got [%s, %s]", olderMsgs[0].ID, olderMsgs[1].ID)
	}
}
