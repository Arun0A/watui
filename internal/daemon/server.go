package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"watui/internal/domain"
)

// Server coordinates IPC connections between the background daemon and attached TUI clients.
type Server struct {
	adapter  domain.WhatsAppAdapter
	dbPath   string
	listener net.Listener

	clientsMu sync.Mutex
	clients   map[net.Conn]*json.Encoder

	activeClients int32

	ctx    context.Context
	cancel context.CancelFunc

	statusMu      sync.RWMutex
	currentStatus domain.ConnectionStatus
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
		clients:       make(map[net.Conn]*json.Encoder),
		ctx:           ctx,
		cancel:        cancel,
		currentStatus: domain.StatusConnected,
	}

	// Register listeners with underlying WhatsApp adapter to forward live events to all clients
	adapter.OnMessage(func(msg domain.Message) {
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
		s.BroadcastEvent("dismiss", chatID)
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
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	s.clientsMu.Lock()
	s.clients[conn] = enc
	s.clientsMu.Unlock()
	atomic.AddInt32(&s.activeClients, 1)

	defer func() {
		_ = conn.Close()
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
		atomic.AddInt32(&s.activeClients, -1)
	}()

	// 1. Send initial snapshot immediately to make client launch snappy quick (<5ms)
	initCtx, initCancel := context.WithTimeout(s.ctx, 2*time.Second)
	unreads, _ := s.adapter.GetUnreadMessages(initCtx)
	initCancel()

	s.statusMu.RLock()
	currentStatus := s.currentStatus
	s.statusMu.RUnlock()

	archivedMap := s.adapter.GetArchivedChats()

	snapshot := InitialSnapshot{
		Status:         currentStatus,
		UnreadMessages: unreads,
		ArchivedChats:  archivedMap,
	}

	snapData, _ := json.Marshal(snapshot)
	_ = enc.Encode(RPCResponse{
		Event:  "snapshot",
		Result: snapData,
	})

	// Stream contacts in the background after the fast handshake completes
	go func() {
		contactCtx, contactCancel := context.WithTimeout(s.ctx, 5*time.Second)
		contacts, err := s.adapter.GetContacts(contactCtx)
		contactCancel()
		if err == nil && len(contacts) > 0 {
			cData, err := json.Marshal(contacts)
			if err == nil {
				s.clientsMu.Lock()
				_ = enc.Encode(RPCResponse{
					Event:  "contacts",
					Result: cData,
				})
				s.clientsMu.Unlock()
			}
		}
	}()

	// 2. Request / Response loop
	for {
		var req RPCRequest
		if err := dec.Decode(&req); err != nil {
			if err != io.EOF {
				log.Printf("[daemon-ipc] client decode error: %v\n", err)
			}
			return
		}

		resp := s.executeRequest(req)
		s.clientsMu.Lock()
		err := enc.Encode(resp)
		s.clientsMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *Server) executeRequest(req RPCRequest) RPCResponse {
	resp := RPCResponse{ID: req.ID}
	reqCtx, reqCancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer reqCancel()

	switch req.Method {
	case "get_unread":
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
			msg, err := s.adapter.SendTextMessage(reqCtx, p.ChatID, p.Text)
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
			msg, err := s.adapter.SendFileMessage(reqCtx, p.ChatID, p.FilePath, p.Caption)
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

	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	for conn, enc := range s.clients {
		_ = enc.Encode(msg)
		_ = conn // in case of failure, handleClient will clean it up on read
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
