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
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
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
		is_edit BOOLEAN,
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
		is_edit BOOLEAN,
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
		is_edit BOOLEAN,
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
		is_edit BOOLEAN,
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

func TestExtractEditProtocolInfo(t *testing.T) {
	newText := "Edited text content"
	innerContent := &waE2E.Message{
		Conversation: proto.String(newText),
	}

	// 1. Direct ProtocolMessage in evt.Message
	evt1 := &events.Message{
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Key: &waCommon.MessageKey{
					ID:        proto.String("ORIG_1"),
					RemoteJID: proto.String("12345@s.whatsapp.net"),
				},
				Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				EditedMessage: innerContent,
			},
		},
	}
	tid, tchat, newMsg, isEdit := extractEditProtocolInfo(evt1)
	if !isEdit || tid != "ORIG_1" || tchat != "12345@s.whatsapp.net" || newMsg == nil {
		t.Fatalf("Direct edit extraction failed: got isEdit=%v, tid=%s, tchat=%s", isEdit, tid, tchat)
	}

	// 2. Wrapped in EditedMessage inside evt.Message
	evt2 := &events.Message{
		Message: &waE2E.Message{
			EditedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ProtocolMessage: &waE2E.ProtocolMessage{
						Key: &waCommon.MessageKey{
							ID:        proto.String("ORIG_2"),
							RemoteJID: proto.String("group1@g.us"),
						},
						Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
						EditedMessage: innerContent,
					},
				},
			},
		},
	}
	tid, tchat, newMsg, isEdit = extractEditProtocolInfo(evt2)
	if !isEdit || tid != "ORIG_2" || tchat != "group1@g.us" || newMsg == nil {
		t.Fatalf("EditedMessage extraction failed: got isEdit=%v, tid=%s, tchat=%s", isEdit, tid, tchat)
	}

	// 3. Wrapped in DeviceSentMessage -> EditedMessage in RawMessage
	evt3 := &events.Message{
		RawMessage: &waE2E.Message{
			DeviceSentMessage: &waE2E.DeviceSentMessage{
				DestinationJID: proto.String("dest@s.whatsapp.net"),
				Message: &waE2E.Message{
					EditedMessage: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{
							ProtocolMessage: &waE2E.ProtocolMessage{
								Key: &waCommon.MessageKey{
									ID:        proto.String("ORIG_3"),
									RemoteJID: proto.String("dest@s.whatsapp.net"),
								},
								Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
								EditedMessage: innerContent,
							},
						},
					},
				},
			},
		},
	}
	tid, tchat, newMsg, isEdit = extractEditProtocolInfo(evt3)
	if !isEdit || tid != "ORIG_3" || tchat != "dest@s.whatsapp.net" || newMsg == nil {
		t.Fatalf("DeviceSentMessage edit extraction failed: got isEdit=%v, tid=%s, tchat=%s", isEdit, tid, tchat)
	}

	// 4. Fallback when whatsmeow already set IsEdit = true and unpacked Message
	evt4 := &events.Message{
		Info: types.MessageInfo{
			ID: "ORIG_4",
			MessageSource: types.MessageSource{
				Chat: types.NewJID("user4", "s.whatsapp.net"),
			},
		},
		IsEdit:  true,
		Message: innerContent,
	}
	tid, tchat, newMsg, isEdit = extractEditProtocolInfo(evt4)
	if !isEdit || tid != "ORIG_4" || newMsg != innerContent {
		t.Fatalf("IsEdit fallback failed: got isEdit=%v, tid=%s, tchat=%s", isEdit, tid, tchat)
	}
}

func TestExtractRevokeProtocolInfo(t *testing.T) {
	// 1. Direct Revoke
	evt1 := &events.Message{
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Key: &waCommon.MessageKey{
					ID: proto.String("REVOKE_1"),
				},
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			},
		},
	}
	id, isRevoke := extractRevokeProtocolInfo(evt1)
	if !isRevoke || id != "REVOKE_1" {
		t.Fatalf("Direct revoke failed: got isRevoke=%v, id=%s", isRevoke, id)
	}

	// 2. Wrapped in EphemeralMessage
	evt2 := &events.Message{
		Message: &waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ProtocolMessage: &waE2E.ProtocolMessage{
						Key: &waCommon.MessageKey{
							ID: proto.String("REVOKE_2"),
						},
						Type: waE2E.ProtocolMessage_REVOKE.Enum(),
					},
				},
			},
		},
	}
	id, isRevoke = extractRevokeProtocolInfo(evt2)
	if !isRevoke || id != "REVOKE_2" {
		t.Fatalf("Ephemeral revoke failed: got isRevoke=%v, id=%s", isRevoke, id)
	}
}

func TestExtractEditDetailsSecretEncryptedMessage(t *testing.T) {
	adapter := &Adapter{}

	// SecretEncryptedMessage for a 1-on-1 message edit
	sem := &waE2E.SecretEncryptedMessage{
		TargetMessageKey: &waCommon.MessageKey{
			ID:        proto.String("ORIG_1ON1"),
			RemoteJID: proto.String("123456789@s.whatsapp.net"),
		},
		SecretEncType: waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(),
	}

	evt := &events.Message{
		Message: &waE2E.Message{
			SecretEncryptedMessage: sem,
		},
	}

	tid, tchat, _, isEdit := adapter.extractEditDetails(evt)
	if !isEdit {
		t.Fatalf("Expected isEdit=true for SecretEncryptedMessage")
	}
	if tid != "ORIG_1ON1" {
		t.Fatalf("Expected targetID=ORIG_1ON1, got %s", tid)
	}
	if tchat != "123456789@s.whatsapp.net" {
		t.Fatalf("Expected targetChat=123456789@s.whatsapp.net, got %s", tchat)
	}
}

func TestDecryptSecretEditMessageFallback(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open err: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE whatsmeow_message_secrets (
			our_jid TEXT,
			chat_jid TEXT,
			sender_jid TEXT,
			message_id TEXT,
			key BLOB,
			PRIMARY KEY (our_jid, chat_jid, sender_jid, message_id)
		);
	`)
	if err != nil {
		t.Fatalf("create table err: %v", err)
	}

	targetID := "TARGET_123"
	chatJID := "120363430194759319@g.us"
	senderLID := "202808741634244@lid"
	baseKey := []byte("01234567890123456789012345678901") // 32 bytes

	_, err = db.Exec("INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key) VALUES (?, ?, ?, ?, ?)",
		"our_device@s.whatsapp.net", chatJID, senderLID, targetID, baseKey)
	if err != nil {
		t.Fatalf("insert err: %v", err)
	}

	// Prepare encrypted payload
	inner := &waE2E.Message{
		Conversation: proto.String("Decrypted fallback text!"),
	}
	plaintext, _ := proto.Marshal(inner)

	useCaseSecret := make([]byte, 0, len(targetID)+len(senderLID)+len(senderLID)+len("Message Edit"))
	useCaseSecret = append(useCaseSecret, targetID...)
	useCaseSecret = append(useCaseSecret, senderLID...)
	useCaseSecret = append(useCaseSecret, senderLID...)
	useCaseSecret = append(useCaseSecret, "Message Edit"...)
	derivedKey := hkdfutil.SHA256(baseKey, nil, useCaseSecret, 32)

	iv := []byte("123456789012") // 12 bytes
	ciphertext, err := gcmutil.Encrypt(derivedKey, iv, plaintext, nil)
	if err != nil {
		t.Fatalf("encrypt err: %v", err)
	}

	sem := &waE2E.SecretEncryptedMessage{
		TargetMessageKey: &waCommon.MessageKey{
			ID:        proto.String(targetID),
			RemoteJID: proto.String(chatJID),
		},
		EncIV:         iv,
		EncPayload:    ciphertext,
		SecretEncType: waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(),
	}

	evt := &events.Message{
		Info: types.MessageInfo{
			ID: "EDIT_STANZA_999",
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("120363430194759319", "g.us"),
				Sender: types.NewJID("202808741634244", "lid"),
			},
		},
		Message: &waE2E.Message{
			SecretEncryptedMessage: sem,
		},
	}

	adapter := &Adapter{
		localDB: db,
	}

	decrypted := adapter.decryptSecretEditMessage(evt, sem, targetID)
	if decrypted == nil {
		t.Fatalf("Expected decrypted message, got nil")
	}

	body, _, ok := extractMessageContent(decrypted)
	if !ok || body != "Decrypted fallback text!" {
		t.Fatalf("Expected 'Decrypted fallback text!', got %q (ok=%v)", body, ok)
	}
}

func TestAdapterMuteLifecycle(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open err: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE whatsmeow_chat_settings (
			our_jid TEXT,
			chat_jid TEXT,
			muted_until INTEGER,
			pinned BOOLEAN,
			archived BOOLEAN,
			PRIMARY KEY (our_jid, chat_jid)
		);
	`)
	if err != nil {
		t.Fatalf("create table err: %v", err)
	}

	futureMute := time.Now().Add(1 * time.Hour).Unix()
	_, err = db.Exec(`
		INSERT INTO whatsmeow_chat_settings (our_jid, chat_jid, muted_until) VALUES
		('our@s.whatsapp.net', '120363001@g.us', -1),
		('our@s.whatsapp.net', '120363002@g.us', ?),
		('our@s.whatsapp.net', '120363003@g.us', 1000);
	`, futureMute)
	if err != nil {
		t.Fatalf("insert err: %v", err)
	}

	adapter := &Adapter{
		localDB: db,
	}

	if adapter.IsChatMuted("120363001@g.us") {
		t.Errorf("Before loadMutedChats, chat should not be loaded")
	}

	adapter.loadMutedChats()

	if !adapter.IsChatMuted("120363001@g.us") {
		t.Errorf("Expected 120363001@g.us to be muted forever")
	}
	if !adapter.IsChatMuted("120363002@g.us") {
		t.Errorf("Expected 120363002@g.us to be muted for 1 hour")
	}
	if adapter.IsChatMuted("120363003@g.us") {
		t.Errorf("Expected 120363003@g.us to be expired mute (timestamp 1000)")
	}

	mutedMap := adapter.GetMutedChats()
	if !mutedMap["120363001@g.us"] || !mutedMap["120363002@g.us"] {
		t.Errorf("Expected mutedMap to contain active mutes, got: %v", mutedMap)
	}
}

func TestPrepareOutgoingMessage(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE watui_chat_ephemeral (
		chat_id TEXT PRIMARY KEY,
		timer INTEGER NOT NULL DEFAULT 0,
		setting_timestamp INTEGER NOT NULL DEFAULT 0
	);`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	adapter := &Adapter{
		localDB:            db,
		ephemeralTimers:    make(map[string]uint32),
		ephemeralSettingTS: make(map[string]int64),
	}

	ephemeralJID := types.NewJID("111222333", types.DefaultUserServer)
	adapter.updateChatEphemeralTimer(ephemeralJID.String(), 86400, 1700000000)

	normalJID := types.NewJID("999888777", types.DefaultUserServer)

	ctx := context.Background()

	// 1. Plain Conversation should be wrapped and converted to ExtendedTextMessage with Expiration
	plainMsg := &waE2E.Message{
		Conversation: proto.String("Disappearing message text"),
	}
	preparedPlain := adapter.prepareOutgoingMessage(ctx, ephemeralJID, plainMsg)
	if preparedPlain == nil || preparedPlain.EphemeralMessage == nil {
		t.Fatalf("Expected EphemeralMessage wrapper for plain message")
	}
	innerPlain := preparedPlain.EphemeralMessage.Message
	if innerPlain.ExtendedTextMessage == nil {
		t.Fatalf("Expected Conversation to be converted to ExtendedTextMessage")
	}
	if innerPlain.ExtendedTextMessage.GetText() != "Disappearing message text" {
		t.Errorf("Expected text 'Disappearing message text', got %q", innerPlain.ExtendedTextMessage.GetText())
	}
	ci := innerPlain.ExtendedTextMessage.ContextInfo
	if ci == nil || ci.GetExpiration() != 86400 {
		t.Errorf("Expected Expiration 86400, got %v", ci)
	}
	if ci.GetEphemeralSettingTimestamp() != 1700000000 {
		t.Errorf("Expected EphemeralSettingTimestamp 1700000000, got %v", ci.GetEphemeralSettingTimestamp())
	}
	if ci.DisappearingMode == nil || ci.DisappearingMode.GetInitiator() != waE2E.DisappearingMode_CHANGED_IN_CHAT {
		t.Errorf("Expected DisappearingMode CHANGED_IN_CHAT, got %v", ci.DisappearingMode)
	}
	if !ci.DisappearingMode.GetInitiatedByMe() {
		t.Errorf("Expected DisappearingMode InitiatedByMe true")
	}

	// 2. ExtendedTextMessage with quote/mention should preserve fields and add Expiration
	extMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("Quoting a message"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID: proto.String("ORIG_STANZA_123"),
			},
		},
	}
	preparedExt := adapter.prepareOutgoingMessage(ctx, ephemeralJID, extMsg)
	if preparedExt == nil || preparedExt.EphemeralMessage == nil {
		t.Fatalf("Expected EphemeralMessage wrapper for extended message")
	}
	innerExt := preparedExt.EphemeralMessage.Message
	if innerExt.ExtendedTextMessage.ContextInfo.GetStanzaID() != "ORIG_STANZA_123" {
		t.Errorf("Expected StanzaID preserved, got %q", innerExt.ExtendedTextMessage.ContextInfo.GetStanzaID())
	}
	if innerExt.ExtendedTextMessage.ContextInfo.GetExpiration() != 86400 {
		t.Errorf("Expected Expiration 86400, got %d", innerExt.ExtendedTextMessage.ContextInfo.GetExpiration())
	}

	// 3. Media message (ImageMessage) should have ContextInfo.Expiration and Ephemeral wrapper
	imgMsg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:  proto.String("Photo caption"),
			Mimetype: proto.String("image/jpeg"),
		},
	}
	preparedImg := adapter.prepareOutgoingMessage(ctx, ephemeralJID, imgMsg)
	if preparedImg == nil || preparedImg.EphemeralMessage == nil {
		t.Fatalf("Expected EphemeralMessage wrapper for image message")
	}
	innerImg := preparedImg.EphemeralMessage.Message
	if innerImg.ImageMessage.ContextInfo == nil || innerImg.ImageMessage.ContextInfo.GetExpiration() != 86400 {
		t.Errorf("Expected ImageMessage ContextInfo.Expiration 86400, got %v", innerImg.ImageMessage.ContextInfo)
	}

	// 4. Non-ephemeral chat should return unmodified message without Ephemeral wrapper
	normalMsg := &waE2E.Message{
		Conversation: proto.String("Regular non-ephemeral message"),
	}
	preparedNormal := adapter.prepareOutgoingMessage(ctx, normalJID, normalMsg)
	if preparedNormal.EphemeralMessage != nil {
		t.Errorf("Did not expect EphemeralMessage for normal chat")
	}
	if preparedNormal.GetConversation() != "Regular non-ephemeral message" {
		t.Errorf("Expected unmodified message for normal chat")
	}

	// 5. Already wrapped EphemeralMessage should not be double-wrapped
	alreadyWrapped := &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				Conversation: proto.String("Already wrapped"),
			},
		},
	}
	preparedWrapped := adapter.prepareOutgoingMessage(ctx, ephemeralJID, alreadyWrapped)
	if preparedWrapped != alreadyWrapped {
		t.Errorf("Expected already wrapped message to be returned as-is")
	}
}

func TestEphemeralTimerManagementAndEvents(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE watui_chat_ephemeral (
		chat_id TEXT PRIMARY KEY,
		timer INTEGER NOT NULL DEFAULT 0,
		setting_timestamp INTEGER NOT NULL DEFAULT 0
	);`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	adapter := &Adapter{
		localDB:            db,
		ephemeralTimers:    make(map[string]uint32),
		ephemeralSettingTS: make(map[string]int64),
	}

	var notifiedChat string
	var notifiedTimer uint32
	adapter.OnChatEphemeral(func(chatID string, timer uint32) {
		notifiedChat = chatID
		notifiedTimer = timer
	})

	// 1. Update timer to 24h
	testChat := "447123456789@s.whatsapp.net"
	adapter.updateChatEphemeralTimer(testChat, 86400, 1690000000)

	if notifiedChat != testChat || notifiedTimer != 86400 {
		t.Errorf("Expected OnChatEphemeral notification (chat: %s, timer: 86400), got (%s, %d)", testChat, notifiedChat, notifiedTimer)
	}

	if tSec := adapter.GetChatEphemeralTimer(testChat); tSec != 86400 {
		t.Errorf("Expected GetChatEphemeralTimer %d, got %d", 86400, tSec)
	}

	// AD / device-specific JID should resolve to the same timer
	testChatAD := "447123456789:12@s.whatsapp.net"
	if tSec := adapter.GetChatEphemeralTimer(testChatAD); tSec != 86400 {
		t.Errorf("Expected AD JID to resolve to 86400, got %d", tSec)
	}

	ephMap := adapter.GetEphemeralChats()
	if ephMap[testChat] != 86400 {
		t.Errorf("Expected GetEphemeralChats to contain %s: 86400, got %v", testChat, ephMap)
	}

	// Verify persistence in SQLite
	var dbTimer uint32
	var dbTS int64
	err = db.QueryRow("SELECT timer, setting_timestamp FROM watui_chat_ephemeral WHERE chat_id = ?", testChat).Scan(&dbTimer, &dbTS)
	if err != nil || dbTimer != 86400 || dbTS != 1690000000 {
		t.Errorf("Expected SQLite row (86400, 1690000000), got (%d, %d, err: %v)", dbTimer, dbTS, err)
	}

	// 2. Turning off ephemeral (timer = 0)
	adapter.updateChatEphemeralTimer(testChat, 0, 1690000001)
	if notifiedTimer != 0 {
		t.Errorf("Expected OnChatEphemeral notification with timer 0, got %d", notifiedTimer)
	}
	if tSec := adapter.GetChatEphemeralTimer(testChat); tSec != 0 {
		t.Errorf("Expected GetChatEphemeralTimer 0 after turning off, got %d", tSec)
	}

	// 3. Test handleEvent with ProtocolMessage EPHEMERAL_SETTING
	protoEvt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: types.NewJID("group123", types.GroupServer),
			},
			Timestamp: time.Unix(1705000000, 0),
		},
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type:                      waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(),
				EphemeralExpiration:       proto.Uint32(604800), // 7 days
				EphemeralSettingTimestamp: proto.Int64(1705000000),
			},
		},
	}
	adapter.handleEvent(protoEvt)
	if tSec := adapter.GetChatEphemeralTimer("group123@g.us"); tSec != 604800 {
		t.Errorf("Expected Group ephemeral timer 604800 from ProtocolMessage, got %d", tSec)
	}

	// 4. Test handleEvent with GroupInfo
	groupInfoEvt := &events.GroupInfo{
		JID: types.NewJID("group456", types.GroupServer),
		Ephemeral: &types.GroupEphemeral{
			IsEphemeral:       true,
			DisappearingTimer: 7776000, // 90 days
		},
		Timestamp: time.Unix(1706000000, 0),
	}
	adapter.handleEvent(groupInfoEvt)
	if tSec := adapter.GetChatEphemeralTimer("group456@g.us"); tSec != 7776000 {
		t.Errorf("Expected Group ephemeral timer 7776000 from GroupInfo, got %d", tSec)
	}
}

func TestEphemeralLIDPhoneResolutionAndHistoryFallback(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE watui_chat_ephemeral (
			chat_id TEXT PRIMARY KEY,
			timer INTEGER NOT NULL DEFAULT 0,
			setting_timestamp INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE whatsmeow_lid_map (
			lid TEXT PRIMARY KEY,
			pn TEXT NOT NULL
		);
		CREATE TABLE watui_messages (
			id TEXT PRIMARY KEY,
			chat_id TEXT,
			sender TEXT,
			timestamp INTEGER,
			raw_message BLOB
		);
	`)
	if err != nil {
		t.Fatalf("failed to create tables: %v", err)
	}

	_, _ = db.Exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('202808741634244', '919635706699')`)

	adapter := &Adapter{
		localDB:            db,
		ephemeralTimers:    make(map[string]uint32),
		ephemeralSettingTS: make(map[string]int64),
	}

	// 1. Test updating via LID resolves to Phone
	adapter.updateChatEphemeralTimer("202808741634244@lid", 86400, 1791359305)

	phoneJID := "919635706699@s.whatsapp.net"
	lidJID := "202808741634244@lid"

	if timer := adapter.GetChatEphemeralTimer(phoneJID); timer != 86400 {
		t.Errorf("Expected Phone JID %s to resolve to timer 86400, got %d", phoneJID, timer)
	}
	if timer := adapter.GetChatEphemeralTimer(lidJID); timer != 86400 {
		t.Errorf("Expected LID JID %s to resolve to timer 86400, got %d", lidJID, timer)
	}

	// Verify both stored in SQLite
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM watui_chat_ephemeral WHERE timer = 86400").Scan(&count)
	if count < 2 {
		t.Errorf("Expected at least 2 rows in watui_chat_ephemeral, got %d", count)
	}

	// 2. Test chat history fallback
	// Clear memory and DB ephemeral tables
	adapter.ephemeralMu.Lock()
	adapter.ephemeralTimers = make(map[string]uint32)
	adapter.ephemeralSettingTS = make(map[string]int64)
	adapter.ephemeralMu.Unlock()
	_, _ = db.Exec("DELETE FROM watui_chat_ephemeral")

	// Store an incoming message in history with ephemeral context info under LID
	historyMsg := &waE2E.Message{
		Conversation: proto.String("Hello with disappearing mode"),
	}
	ctxInfo := &waE2E.ContextInfo{
		Expiration:                proto.Uint32(86400),
		EphemeralSettingTimestamp: proto.Int64(1791359305),
	}
	historyMsg = &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        historyMsg.Conversation,
			ContextInfo: ctxInfo,
		},
	}
	rawBytes, _ := proto.Marshal(historyMsg)
	_, err = db.Exec("INSERT INTO watui_messages (id, chat_id, sender, timestamp, raw_message) VALUES (?, ?, ?, ?, ?)",
		"HIST_MSG_1", lidJID, lidJID, 1791359305, rawBytes)
	if err != nil {
		t.Fatalf("failed to insert history message: %v", err)
	}

	// Query via Phone JID - should scan history via LID mapping and find the timer
	timerFound := adapter.GetChatEphemeralTimer(phoneJID)
	if timerFound != 86400 {
		t.Errorf("Expected history fallback to find timer 86400 for %s, got %d", phoneJID, timerFound)
	}

	// Verify it auto-populated memory and SQLite
	if timerMem := adapter.ephemeralTimers[phoneJID]; timerMem != 86400 {
		t.Errorf("Expected timer to be cached in memory, got %d", timerMem)
	}
}
