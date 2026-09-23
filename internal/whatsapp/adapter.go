package whatsapp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"watui/internal/domain"
)

// Config holds configuration for the WhatsApp adapter.
type Config struct {
	DBPath   string
	LogLevel string
}

// Adapter implements domain.WhatsAppAdapter using whatsmeow.
// It completely encapsulates all WhatsApp-specific protocol, crypto, and wire details.
type Adapter struct {
	client    *whatsmeow.Client
	container *sqlstore.Container
	config    Config

	mu              sync.RWMutex
	messageHandlers []domain.MessageHandler
	statusHandlers  []domain.StatusHandler
	currentStatus   domain.ConnectionStatus
}

// NewAdapter creates and initializes a WhatsApp adapter instance.
func NewAdapter(ctx context.Context, cfg Config) (*Adapter, error) {
	if cfg.DBPath == "" {
		cfg.DBPath = "watui.db"
	}

	dbLog := waLog.Stdout("Database", cfg.LogLevel, true)
	clientLog := waLog.Stdout("Client", cfg.LogLevel, true)

	container, err := sqlstore.New(ctx, "sqlite3", "file:"+cfg.DBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get device store: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, clientLog)

	adapter := &Adapter{
		client:        client,
		container:     container,
		config:        cfg,
		currentStatus: domain.StatusDisconnected,
	}

	client.AddEventHandler(adapter.handleEvent)

	return adapter, nil
}

// IsLoggedIn checks whether valid session credentials exist in SQLite.
func (a *Adapter) IsLoggedIn() bool {
	return a.client.Store.ID != nil
}

// OnMessage registers an incoming message handler.
func (a *Adapter) OnMessage(handler domain.MessageHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messageHandlers = append(a.messageHandlers, handler)
}

// OnStatus registers a connection status handler.
func (a *Adapter) OnStatus(handler domain.StatusHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statusHandlers = append(a.statusHandlers, handler)
}

func (a *Adapter) setStatus(status domain.ConnectionStatus) {
	a.mu.Lock()
	a.currentStatus = status
	handlers := append([]domain.StatusHandler(nil), a.statusHandlers...)
	a.mu.Unlock()

	for _, h := range handlers {
		h(status)
	}
}

// Connect starts the WebSocket and completes authentication or QR pairing.
func (a *Adapter) Connect(ctx context.Context) error {
	a.setStatus(domain.StatusConnecting)

	if a.client.Store.ID == nil {
		// Device is not paired yet. Request QR channel.
		qrChan, err := a.client.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("failed to get QR channel: %w", err)
		}

		err = a.client.Connect()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		a.setStatus(domain.StatusWaitingQR)

		// Handle pairing loop in background
		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					a.renderQRInTerminal(evt.Code)
				case "success":
					fmt.Println("\n Successfully authenticated with WhatsApp!")
					a.setStatus(domain.StatusConnected)
				case "timeout":
					fmt.Println("\n QR code pairing timed out. Restart watui to try again.")
					a.setStatus(domain.StatusDisconnected)
				}
			}
		}()
	} else {
		// Session already exists in SQLite, connect directly
		err := a.client.Connect()
		if err != nil {
			return fmt.Errorf("failed to connect with existing session: %w", err)
		}
	}

	return nil
}

// Disconnect gracefully shuts down the connection.
func (a *Adapter) Disconnect() {
	if a.client != nil {
		a.client.Disconnect()
	}
	a.setStatus(domain.StatusDisconnected)
}

// SendTextMessage sends a basic text message to a WhatsApp chat JID.
func (a *Adapter) SendTextMessage(ctx context.Context, chatID string, text string) (domain.Message, error) {
	recipientJID, err := types.ParseJID(chatID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("invalid recipient JID %q: %w", chatID, err)
	}

	msg := &waE2E.Message{
		Conversation: proto.String(text),
	}

	resp, err := a.client.SendMessage(ctx, recipientJID, msg)
	if err != nil {
		return domain.Message{}, fmt.Errorf("failed to send message: %w", err)
	}

	senderID := ""
	if a.client.Store.ID != nil {
		senderID = a.client.Store.ID.ToNonAD().String()
	}

	return domain.Message{
		ID:         resp.ID,
		ChatID:     chatID,
		Sender:     senderID,
		SenderName: "Me",
		Timestamp:  resp.Timestamp,
		IsFromMe:   true,
		Type:       domain.MessageTypeText,
		Body:       text,
		Status:     domain.MessageStatusSent,
	}, nil
}

// GetContacts retrieves all known contacts from the local session database.
func (a *Adapter) GetContacts(ctx context.Context) ([]domain.Contact, error) {
	if a.client == nil || a.client.Store == nil || a.client.Store.Contacts == nil {
		return nil, nil
	}

	rawMap, err := a.client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get contacts: %w", err)
	}

	var list []domain.Contact
	for jid, info := range rawMap {
		if jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer {
			continue
		}

		name := strings.TrimSpace(info.FullName)
		if name == "" {
			name = strings.TrimSpace(info.BusinessName)
		}
		if name == "" {
			name = strings.TrimSpace(info.PushName)
		}
		if name == "" {
			name = jid.User
		}

		list = append(list, domain.Contact{
			JID:          jid.String(),
			Name:         name,
			PushName:     info.PushName,
			BusinessName: info.BusinessName,
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})

	return list, nil
}

// MarkRead sends a read receipt to WhatsApp for the given message IDs.
func (a *Adapter) MarkRead(ctx context.Context, chatID string, senderID string, messageIDs []string) error {
	if len(messageIDs) == 0 || a.client == nil {
		return nil
	}

	cJID, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid chat JID %q: %w", chatID, err)
	}

	sJID := cJID
	if senderID != "" && senderID != chatID {
		if parsed, err := types.ParseJID(senderID); err == nil {
			sJID = parsed
		}
	}

	waMsgIDs := make([]types.MessageID, len(messageIDs))
	for i, id := range messageIDs {
		waMsgIDs[i] = types.MessageID(id)
	}

	return a.client.MarkRead(ctx, waMsgIDs, time.Now(), cJID, sJID)
}

// handleEvent processes internal whatsmeow events and maps them to clean domain models.
func (a *Adapter) handleEvent(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		a.setStatus(domain.StatusConnected)

	case *events.LoggedOut:
		a.setStatus(domain.StatusLoggedOut)

	case *events.Message:
		domainMsg := a.extractDomainMessage(evt)

		a.mu.RLock()
		handlers := append([]domain.MessageHandler(nil), a.messageHandlers...)
		a.mu.RUnlock()

		for _, h := range handlers {
			h(domainMsg)
		}
	}
}

// extractDomainMessage extracts semantic fields from the raw wire message event.
func (a *Adapter) extractDomainMessage(evt *events.Message) domain.Message {
	msgType := domain.MessageTypeText
	body := ""

	m := evt.Message
	if m != nil {
		if m.Conversation != nil && *m.Conversation != "" {
			body = *m.Conversation
			msgType = domain.MessageTypeText
		} else if m.ExtendedTextMessage != nil && m.ExtendedTextMessage.Text != nil {
			body = *m.ExtendedTextMessage.Text
			msgType = domain.MessageTypeText
		} else if m.ImageMessage != nil {
			msgType = domain.MessageTypeImage
			if m.ImageMessage.Caption != nil {
				body = *m.ImageMessage.Caption
			} else {
				body = "[Image]"
			}
		} else if m.AudioMessage != nil {
			msgType = domain.MessageTypeAudio
			body = "[Audio / Voice Note]"
		} else if m.VideoMessage != nil {
			msgType = domain.MessageTypeVideo
			if m.VideoMessage.Caption != nil {
				body = *m.VideoMessage.Caption
			} else {
				body = "[Video]"
			}
		} else if m.DocumentMessage != nil {
			msgType = domain.MessageTypeDocument
			if m.DocumentMessage.FileName != nil {
				body = fmt.Sprintf("[Document: %s]", *m.DocumentMessage.FileName)
			} else {
				body = "[Document]"
			}
		} else if m.ReactionMessage != nil {
			msgType = domain.MessageTypeReaction
			if m.ReactionMessage.Text != nil {
				body = *m.ReactionMessage.Text
			}
		} else {
			msgType = domain.MessageTypeUnknown
			body = "[Unsupported or Service Message]"
		}
	}

	senderName := evt.Info.PushName
	if senderName == "" {
		senderName = evt.Info.Sender.User
	}

	return domain.Message{
		ID:         evt.Info.ID,
		ChatID:     evt.Info.Chat.String(),
		Sender:     evt.Info.Sender.String(),
		SenderName: senderName,
		Timestamp:  evt.Info.Timestamp,
		IsFromMe:   evt.Info.IsFromMe,
		Type:       msgType,
		Body:       body,
		Status:     domain.MessageStatusDelivered,
	}
}

// renderQRInTerminal prints a clear ANSI/UTF-8 QR code for WhatsApp companion linking.
func (a *Adapter) renderQRInTerminal(code string) {
	qr, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		fmt.Printf("Error generating QR code: %v\n", err)
		return
	}

	fmt.Println("\n=======================================================")
	fmt.Println(" Scan this QR code in WhatsApp on your phone:")
	fmt.Println(" Settings -> Linked Devices -> Link a Device")
	fmt.Println("=======================================================")
	fmt.Print(qr.ToSmallString(false))
	fmt.Println("=======================================================")
}

