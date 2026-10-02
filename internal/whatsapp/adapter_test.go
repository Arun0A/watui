package whatsapp

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
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
		{
			name: "video message with gif playback",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:   testChatJID,
						Sender: testSenderJID,
					},
					ID:        "GIF123",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					VideoMessage: &waE2E.VideoMessage{
						GifPlayback: proto.Bool(true),
					},
				},
			},
			expectedType: domain.MessageTypeVideo,
			expectedBody: "[GIF]",
		},
		{
			name: "video message with gif playback and caption",
			evt: &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:   testChatJID,
						Sender: testSenderJID,
					},
					ID:        "GIF124",
					Timestamp: testTimestamp,
				},
				Message: &waE2E.Message{
					VideoMessage: &waE2E.VideoMessage{
						GifPlayback: proto.Bool(true),
						Caption:     proto.String("Dancing cat"),
					},
				},
			},
			expectedType: domain.MessageTypeVideo,
			expectedBody: "Dancing cat",
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
		is_from_me BOOLEAN,
		quoted_id TEXT,
		quoted_text TEXT,
		quoted_sender TEXT,
		raw_message BLOB
	);
	CREATE INDEX IF NOT EXISTS idx_watui_messages_chat_ts ON watui_messages(chat_id, timestamp DESC);
	CREATE TABLE IF NOT EXISTS watui_media_cache (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		raw_message BLOB NOT NULL,
		timestamp INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_watui_media_cache_ts ON watui_media_cache(timestamp DESC);`)
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

func TestMediaCachePersistence(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open in-memory db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS watui_media_cache (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		raw_message BLOB NOT NULL,
		timestamp INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_watui_media_cache_ts ON watui_media_cache(timestamp DESC);
	CREATE TABLE IF NOT EXISTS watui_messages (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		chat_name TEXT,
		sender TEXT NOT NULL,
		sender_name TEXT,
		timestamp INTEGER NOT NULL,
		body TEXT,
		type TEXT,
		is_from_me BOOLEAN,
		quoted_id TEXT,
		quoted_text TEXT,
		quoted_sender TEXT,
		raw_message BLOB
	);
	CREATE TABLE IF NOT EXISTS watui_unread_messages (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		raw_message BLOB
	);`)
	if err != nil {
		t.Fatalf("Failed to create tables: %v", err)
	}

	persistTrue := true
	cfg := &config.Config{
		PersistChatHistory:   &persistTrue,
		CycleMsgCountPerChat: 30,
	}

	adapter := &Adapter{
		localDB:    db,
		appConfig:  cfg,
		mediaCache: make(map[string]*waE2E.Message),
	}

	chatID := "123456789-987654@g.us"
	msgID := "MEDIA123"
	caption := "Test Image"
	rawMsg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption: &caption,
		},
	}

	// 1. Cache media message
	adapter.cacheMediaMessage(msgID, chatID, rawMsg, time.Now())

	// 2. Also save into history
	adapter.saveHistoryMessage(domain.Message{
		ID:        msgID,
		ChatID:    chatID,
		Sender:    chatID,
		Timestamp: time.Now(),
		Body:      "[Image]",
		Type:      domain.MessageTypeImage,
	}, rawMsg)

	// Verify it was saved to watui_media_cache table
	var dbCount int
	err = db.QueryRow("SELECT COUNT(*) FROM watui_media_cache WHERE id = ?", msgID).Scan(&dbCount)
	if err != nil || dbCount != 1 {
		t.Fatalf("Expected 1 row in watui_media_cache, got %d (err: %v)", dbCount, err)
	}

	// Verify raw_message in watui_messages
	var rawBlob []byte
	err = db.QueryRow("SELECT raw_message FROM watui_messages WHERE id = ?", msgID).Scan(&rawBlob)
	if err != nil || len(rawBlob) == 0 {
		t.Fatalf("Expected non-empty raw_message in watui_messages, got len=%d (err: %v)", len(rawBlob), err)
	}

	// 3. Dismiss unread on chat should NOT delete watui_media_cache
	_ = adapter.DismissUnread(context.Background(), chatID)
	err = db.QueryRow("SELECT COUNT(*) FROM watui_media_cache WHERE id = ?", msgID).Scan(&dbCount)
	if err != nil || dbCount != 1 {
		t.Fatalf("Expected watui_media_cache to survive DismissUnread, got %d", dbCount)
	}

	// 4. Clear in-memory cache to simulate daemon restart
	adapter.mediaMu.Lock()
	adapter.mediaCache = make(map[string]*waE2E.Message)
	adapter.mediaMu.Unlock()

	if adapter.getCachedMediaMessage(msgID) != nil {
		t.Fatal("Expected memory cache to be cleared")
	}

	// 5. Test DownloadMedia fallback queries SQLite watui_media_cache
	// (Will fail at a.client.Download because client is nil, but should NOT fail with "media metadata not found")
	_, dlErr := adapter.DownloadMedia(context.Background(), domain.Message{
		ID:     msgID,
		ChatID: chatID,
		Type:   domain.MessageTypeImage,
	})
	if dlErr == nil || strings.Contains(dlErr.Error(), "media metadata not found") {
		t.Fatalf("Expected DownloadMedia to find metadata and fail on client nil, got: %v", dlErr)
	}
}

func TestUnreadReplyRetention(t *testing.T) {
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
		is_from_me BOOLEAN,
		quoted_id TEXT,
		quoted_text TEXT,
		quoted_sender TEXT,
		raw_message BLOB
	);
	CREATE TABLE IF NOT EXISTS watui_unread_messages (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		chat_name TEXT,
		sender TEXT NOT NULL,
		sender_name TEXT,
		timestamp INTEGER NOT NULL,
		body TEXT,
		type TEXT,
		is_from_me BOOLEAN,
		quoted_id TEXT,
		quoted_text TEXT,
		quoted_sender TEXT,
		raw_message BLOB
	);`)
	if err != nil {
		t.Fatalf("Failed to create tables: %v", err)
	}

	persistTrue := true
	cfg := &config.Config{
		PersistChatHistory:   &persistTrue,
		CycleMsgCountPerChat: 30,
	}

	adapter := &Adapter{
		localDB:   db,
		appConfig: cfg,
	}

	chatID := "group123@g.us"

	// 1. Save an earlier message to watui_messages (representing a message in cyclic cache)
	adapter.saveHistoryMessage(domain.Message{
		ID:         "MSG_EARLIER",
		ChatID:     chatID,
		Sender:     "alice@s.whatsapp.net",
		SenderName: "Alice",
		Timestamp:  time.Now().Add(-10 * time.Minute),
		Body:       "Original question?",
		Type:       domain.MessageTypeText,
	})

	// 2. Save an unread message that replies to MSG_EARLIER with explicit QuotedText & QuotedSender
	replyMsg1 := domain.Message{
		ID:           "REPLY_1",
		ChatID:       chatID,
		Sender:       "bob@s.whatsapp.net",
		SenderName:   "Bob",
		Timestamp:    time.Now().Add(-5 * time.Minute),
		Body:         "Yes, here is the answer",
		Type:         domain.MessageTypeText,
		QuotedID:     "MSG_EARLIER",
		QuotedText:   "Original question?",
		QuotedSender: "Alice",
	}
	adapter.saveUnreadMessage(replyMsg1)

	// 3. Save another unread message that replies to MSG_EARLIER but QuotedText was empty in the wire event
	replyMsg2 := domain.Message{
		ID:         "REPLY_2",
		ChatID:     chatID,
		Sender:     "charlie@s.whatsapp.net",
		SenderName: "Charlie",
		Timestamp:  time.Now().Add(-2 * time.Minute),
		Body:       "Me too",
		Type:       domain.MessageTypeText,
		QuotedID:   "MSG_EARLIER", // QuotedText is empty
	}
	adapter.saveUnreadMessage(replyMsg2)

	// 4. Retrieve unread messages as if opening a new TUI session
	unreads, err := adapter.GetUnreadMessages(context.Background())
	if err != nil {
		t.Fatalf("GetUnreadMessages failed: %v", err)
	}
	if len(unreads) != 2 {
		t.Fatalf("Expected 2 unread messages, got %d", len(unreads))
	}

	// Verify reply 1 retained QuotedID, QuotedText, QuotedSender
	if unreads[0].QuotedID != "MSG_EARLIER" {
		t.Errorf("Expected QuotedID MSG_EARLIER, got %q", unreads[0].QuotedID)
	}
	if unreads[0].QuotedText != "Original question?" {
		t.Errorf("Expected QuotedText 'Original question?', got %q", unreads[0].QuotedText)
	}
	if unreads[0].QuotedSender != "Alice" {
		t.Errorf("Expected QuotedSender 'Alice', got %q", unreads[0].QuotedSender)
	}

	// Verify reply 2 had its QuotedText and QuotedSender automatically resolved from watui_messages!
	if unreads[1].QuotedID != "MSG_EARLIER" {
		t.Errorf("Expected QuotedID MSG_EARLIER, got %q", unreads[1].QuotedID)
	}
	if unreads[1].QuotedText != "Original question?" {
		t.Errorf("Expected QuotedText resolved to 'Original question?', got %q", unreads[1].QuotedText)
	}
	if unreads[1].QuotedSender != "Alice" {
		t.Errorf("Expected QuotedSender resolved to 'Alice', got %q", unreads[1].QuotedSender)
	}
}

func TestResolveMentionsInText(t *testing.T) {
	adapter := &Adapter{}

	// Test 1: no mentions
	text := "Hello world"
	if res := adapter.resolveMentionsInText(text); res != text {
		t.Errorf("Expected %q, got %q", text, res)
	}

	// Test 2: email addresses should not be mangled
	emailText := "hello user@example.com or user@12345.com"
	if res := adapter.resolveMentionsInText(emailText); res != emailText {
		t.Errorf("Expected %q, got %q", emailText, res)
	}
}

func TestGetGroupParticipants(t *testing.T) {
	adapter := &Adapter{
		groupParticipants: map[string][]domain.Contact{
			"group1@g.us": {
				{JID: "919876543210@s.whatsapp.net", Name: "Alice"},
				{JID: "919876543211@s.whatsapp.net", Name: "Bob"},
			},
		},
	}

	participants, err := adapter.GetGroupParticipants(context.Background(), "group1@g.us")
	if err != nil {
		t.Fatalf("GetGroupParticipants failed: %v", err)
	}
	if len(participants) != 2 {
		t.Fatalf("Expected 2 participants, got %d", len(participants))
	}
	if participants[0].Name != "Alice" || participants[1].Name != "Bob" {
		t.Errorf("Unexpected participants: %v", participants)
	}
}

func TestReactionQuotedAssociation(t *testing.T) {
	adapter := &Adapter{}
	chatJID := types.NewJID("120363430194759319", "g.us")
	senderJID := types.NewJID("919876543210", "s.whatsapp.net")
	targetSender := "919876543211@s.whatsapp.net"

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			ID:        "REACT_MSG_1",
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{
					RemoteJID:   proto.String(chatJID.String()),
					ID:          proto.String("ORIG_MSG_42"),
					Participant: proto.String(targetSender),
				},
				Text: proto.String("❤️"),
			},
		},
	}

	msg, ok := adapter.extractDomainMessage(evt)
	if !ok {
		t.Fatalf("Expected extractDomainMessage ok=true, got false")
	}
	if msg.Type != domain.MessageTypeReaction {
		t.Errorf("Expected MessageTypeReaction, got %s", msg.Type)
	}
	if msg.Body != "[Reaction: ❤️]" {
		t.Errorf("Expected [Reaction: ❤️], got %s", msg.Body)
	}
	if msg.QuotedID != "ORIG_MSG_42" {
		t.Errorf("Expected QuotedID ORIG_MSG_42, got %q", msg.QuotedID)
	}
	if msg.QuotedSender != "919876543211" {
		t.Errorf("Expected QuotedSender 919876543211, got %q", msg.QuotedSender)
	}
}

func TestConvertGifToMp4(t *testing.T) {
	// Create sample animated GIF in memory (odd dimensions to test padding scale)
	rect := image.Rect(0, 0, 61, 61)
	pal := color.Palette{color.Black, color.White}
	img1 := image.NewPaletted(rect, pal)
	img2 := image.NewPaletted(rect, pal)
	img2.Set(10, 10, color.White)
	g := &gif.GIF{
		Image: []*image.Paletted{img1, img2},
		Delay: []int{10, 10},
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("Failed to encode test GIF: %v", err)
	}

	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed, skipping GIF conversion test")
	}

	mp4Data, err := convertGifToMp4(context.Background(), buf.Bytes())
	if err != nil {
		t.Fatalf("convertGifToMp4 failed: %v", err)
	}
	if len(mp4Data) == 0 {
		t.Fatalf("convertGifToMp4 returned empty data")
	}
	// Verify MP4 signature (ftyp)
	if !bytes.Contains(mp4Data[:min(len(mp4Data), 32)], []byte("ftyp")) {
		t.Errorf("Converted data does not have MP4 ftyp box signature")
	}
}
