package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"watui/internal/domain"
)

type clientWriter struct {
	enc *json.Encoder
	mu  sync.Mutex
}

// Server coordinates IPC connections between the background daemon and attached TUI clients.
type Server struct {
	adapter  domain.WhatsAppAdapter
	dbPath   string
	listener net.Listener

	clientsMu sync.RWMutex
	clients   map[net.Conn]*clientWriter

	activeClients int32

	ctx    context.Context
	cancel context.CancelFunc

	statusMu      sync.RWMutex
	currentStatus domain.ConnectionStatus

	unreadsMu     sync.RWMutex
	cachedUnreads []domain.Message
}

// NewServer creates a new daemon IPC server for the given adapter.
func NewServer(adapter domain.WhatsAppAdapter, dbPath string) (*Server, error) {
	l, err := ListenIPC(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPC listener: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		adapter:       adapter,
		dbPath:        dbPath,
		listener:      l,
		clients:       make(map[net.Conn]*clientWriter),
		ctx:           ctx,
		cancel:        cancel,
		currentStatus: domain.StatusConnected,
	}

	unreads, err := adapter.GetUnreadMessages(ctx)
	if err == nil {
		s.cachedUnreads = unreads
	}

	// Register listeners with underlying WhatsApp adapter to forward live events to all clients
	adapter.OnMessage(func(msg domain.Message) {
		s.unreadsMu.Lock()
		s.cachedUnreads = append(s.cachedUnreads, msg)
		s.unreadsMu.Unlock()
		s.BroadcastEvent("message", msg)
	})

	adapter.OnStatus(func(status domain.ConnectionStatus) {
		s.statusMu.Lock()
		s.currentStatus = status
		s.statusMu.Unlock()
		s.BroadcastEvent("status", status)
	})

	adapter.OnContactsUpdated(func(contacts []domain.Contact) {
		s.BroadcastEvent("contacts", contacts)
	})

	adapter.OnChatDismissed(func(chatID string) {
		s.unreadsMu.Lock()
		if s.cachedUnreads != nil {
			var filtered []domain.Message
			for _, m := range s.cachedUnreads {
				if m.ChatID != chatID {
					filtered = append(filtered, m)
				}
			}
			s.cachedUnreads = filtered
		}
		s.unreadsMu.Unlock()
		s.BroadcastEvent("dismiss", chatID)
	})

	adapter.OnChatEphemeral(func(chatID string, timer uint32) {
		s.BroadcastEvent("ephemeral", SetChatEphemeralParams{ChatID: chatID, Timer: timer})
	})

	go s.acceptLoop()

	return s, nil
}

// HasActiveClients returns true if one or more TUI clients are currently attached.
func (s *Server) HasActiveClients() bool {
	return atomic.LoadInt32(&s.activeClients) > 0
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}

		go s.handleClient(conn)
	}
}

func (s *Server) handleClient(conn net.Conn) {
	cw := &clientWriter{enc: json.NewEncoder(conn)}
	dec := json.NewDecoder(conn)

	s.clientsMu.Lock()
	s.clients[conn] = cw
	s.clientsMu.Unlock()
	atomic.AddInt32(&s.activeClients, 1)

	defer func() {
		_ = conn.Close()
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
		atomic.AddInt32(&s.activeClients, -1)
	}()

	// 1. Send initial snapshot immediately to make client launch snappy quick (<1ms)
	s.unreadsMu.RLock()
	var unreads []domain.Message
	if s.cachedUnreads != nil {
		unreads = make([]domain.Message, len(s.cachedUnreads))
		copy(unreads, s.cachedUnreads)
	}
	s.unreadsMu.RUnlock()

	if unreads == nil {
		initCtx, initCancel := context.WithTimeout(s.ctx, 2*time.Second)
		unreads, _ = s.adapter.GetUnreadMessages(initCtx)
		initCancel()
	}

	s.statusMu.RLock()
	currentStatus := s.currentStatus
	s.statusMu.RUnlock()

	archivedMap := s.adapter.GetArchivedChats()
	mutedMap := s.adapter.GetMutedChats()

	snapshot := InitialSnapshot{
		Status:         currentStatus,
		UnreadMessages: unreads,
		ArchivedChats:  archivedMap,
		MutedChats:     mutedMap,
		EphemeralChats: s.adapter.GetEphemeralChats(),
	}

	snapData, _ := json.Marshal(snapshot)
	cw.mu.Lock()
	_ = cw.enc.Encode(RPCResponse{
		Event:  "snapshot",
		Result: snapData,
	})
	cw.mu.Unlock()

	// Stream contacts in the background after the fast handshake completes
	go func() {
		contactCtx, contactCancel := context.WithTimeout(s.ctx, 5*time.Second)
		contacts, err := s.adapter.GetContacts(contactCtx)
		contactCancel()
		if err == nil && len(contacts) > 0 {
			cData, err := json.Marshal(contacts)
			if err == nil {
				cw.mu.Lock()
				_ = cw.enc.Encode(RPCResponse{
					Event:  "contacts",
					Result: cData,
				})
				cw.mu.Unlock()
			}
		}
	}()

	// 2. Request / Response loop: execute requests concurrently to prevent head-of-line blocking
	for {
		var req RPCRequest
		if err := dec.Decode(&req); err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				log.Printf("[daemon-ipc] client decode error: %v\n", err)
			}
			return
		}

		go func(r RPCRequest) {
			resp := s.executeRequest(r)
			cw.mu.Lock()
			_ = cw.enc.Encode(resp)
			cw.mu.Unlock()
		}(req)
	}
}

func (s *Server) executeRequest(req RPCRequest) RPCResponse {
	resp := RPCResponse{ID: req.ID}
	reqCtx, reqCancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer reqCancel()

	switch req.Method {
	case "get_unread":
		s.unreadsMu.RLock()
		if s.cachedUnreads != nil {
			snap := make([]domain.Message, len(s.cachedUnreads))
			copy(snap, s.cachedUnreads)
			s.unreadsMu.RUnlock()
			resp.Result, _ = json.Marshal(snap)
			return resp
		}
		s.unreadsMu.RUnlock()
		unreads, err := s.adapter.GetUnreadMessages(reqCtx)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Result, _ = json.Marshal(unreads)
		}

	case "get_contacts":
		contacts, err := s.adapter.GetContacts(reqCtx)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Result, _ = json.Marshal(contacts)
		}

	case "send_text":
		var p SendTextParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			msg, err := s.adapter.SendTextMessage(reqCtx, p.ChatID, p.Text, p.QuotedID, p.QuotedBody, p.QuotedSender)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result, _ = json.Marshal(msg)
			}
		}

	case "send_file":
		var p SendFileParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			var msg domain.Message
			var err error
			if p.QuotedID != "" || p.QuotedBody != "" || p.QuotedSender != "" {
				msg, err = s.adapter.SendFileMessage(reqCtx, p.ChatID, p.FilePath, p.Caption, p.QuotedID, p.QuotedBody, p.QuotedSender)
			} else {
				msg, err = s.adapter.SendFileMessage(reqCtx, p.ChatID, p.FilePath, p.Caption)
			}
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result, _ = json.Marshal(msg)
			}
		}

	case "mark_read":
		var p MarkReadParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.MarkRead(reqCtx, p.ChatID, p.SenderID, p.MessageIDs)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.unreadsMu.Lock()
				if s.cachedUnreads != nil {
					var filtered []domain.Message
					for _, m := range s.cachedUnreads {
						if m.ChatID != p.ChatID {
							filtered = append(filtered, m)
						}
					}
					s.cachedUnreads = filtered
				}
				s.unreadsMu.Unlock()
			}
		}

	case "dismiss_unread":
		var p DismissParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.DismissUnread(reqCtx, p.ChatID)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.unreadsMu.Lock()
				if s.cachedUnreads != nil {
					var filtered []domain.Message
					for _, m := range s.cachedUnreads {
						if m.ChatID != p.ChatID {
							filtered = append(filtered, m)
						}
					}
					s.cachedUnreads = filtered
				}
				s.unreadsMu.Unlock()
			}
		}

	case "delete_message":
		var p DeleteMessageParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.DeleteMessage(reqCtx, p.ChatID, p.MessageID, p.DeleteForEveryone, p.Sender)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.unreadsMu.Lock()
				if s.cachedUnreads != nil {
					var filtered []domain.Message
					for _, m := range s.cachedUnreads {
						if m.ID != p.MessageID {
							filtered = append(filtered, m)
						}
					}
					s.cachedUnreads = filtered
				}
				s.unreadsMu.Unlock()
			}
		}

	case "edit_message":
		var p EditMessageParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.EditMessage(reqCtx, p.ChatID, p.MessageID, p.NewText)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.unreadsMu.Lock()
				if s.cachedUnreads != nil {
					for i, m := range s.cachedUnreads {
						if m.ID == p.MessageID {
							s.cachedUnreads[i].Body = p.NewText
							break
						}
					}
				}
				s.unreadsMu.Unlock()
			}
		}

	case "download_media":
		var p DownloadMediaParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			path, err := s.adapter.DownloadMedia(reqCtx, p.Message)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result, _ = json.Marshal(path)
			}
		}

	case "ensure_group_names":
		var p EnsureGroupsParams
		if err := json.Unmarshal(req.Params, &p); err == nil {
			s.adapter.EnsureGroupNames(reqCtx, p.JIDs)
		}

	case "is_chat_archived":
		var p DismissParams
		if err := json.Unmarshal(req.Params, &p); err == nil {
			archived := s.adapter.IsChatArchived(p.ChatID)
			resp.Result, _ = json.Marshal(archived)
		}

	case "set_chat_archived":
		var p SetChatArchivedParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.SetChatArchived(reqCtx, p.ChatID, p.Archived)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.BroadcastEvent("archived", p)
			}
		}

	case "is_chat_muted":
		var p DismissParams
		if err := json.Unmarshal(req.Params, &p); err == nil {
			muted := s.adapter.IsChatMuted(p.ChatID)
			resp.Result, _ = json.Marshal(muted)
		}

	case "set_chat_muted":
		var p SetChatMutedParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.SetChatMuted(reqCtx, p.ChatID, p.Muted, p.Duration)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.BroadcastEvent("muted", p)
			}
		}

	case "get_chat_ephemeral":
		var p DismissParams
		if err := json.Unmarshal(req.Params, &p); err == nil {
			timer := s.adapter.GetChatEphemeralTimer(p.ChatID)
			resp.Result, _ = json.Marshal(timer)
		}

	case "set_chat_ephemeral":
		var p SetChatEphemeralParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			err := s.adapter.SetChatDisappearingTimer(reqCtx, p.ChatID, time.Duration(p.Timer)*time.Second)
			if err != nil {
				resp.Error = err.Error()
			} else {
				s.BroadcastEvent("ephemeral", p)
			}
		}

	case "sync":
		if err := s.adapter.Sync(reqCtx); err != nil {
			resp.Error = err.Error()
		}

	case "get_chat_history":
		var p GetChatHistoryParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = err.Error()
		} else {
			msgs, err := s.adapter.GetChatHistory(reqCtx, p.ChatID, p.Limit, p.BeforeTimestamp)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result, _ = json.Marshal(msgs)
			}
		}

	case "get_group_participants":
		var groupJID string
		if err := json.Unmarshal(req.Params, &groupJID); err != nil {
			resp.Error = err.Error()
		} else {
			participants, err := s.adapter.GetGroupParticipants(reqCtx, groupJID)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result, _ = json.Marshal(participants)
			}
		}

	default:
		resp.Error = fmt.Sprintf("unknown method: %s", req.Method)
	}

	return resp
}

// BroadcastEvent sends a push event to all currently connected clients.
func (s *Server) BroadcastEvent(event string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	msg := RPCResponse{
		Event:  event,
		Result: data,
	}

	s.clientsMu.RLock()
	writers := make([]*clientWriter, 0, len(s.clients))
	for _, cw := range s.clients {
		writers = append(writers, cw)
	}
	s.clientsMu.RUnlock()

	for _, cw := range writers {
		go func(w *clientWriter) {
			w.mu.Lock()
			_ = w.enc.Encode(msg)
			w.mu.Unlock()
		}(cw)
	}
}

// Close gracefully stops the IPC server and cleans up socket/port files.
func (s *Server) Close() {
	s.cancel()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	CleanupIPC(s.dbPath)
}
