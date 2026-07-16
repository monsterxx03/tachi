// Package chrome implements the channel.Channel interface for Chrome
// Extension communication over HTTP/WebSocket (localhost).
package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
	"golang.org/x/net/websocket"
)

// DefaultPort is the default TCP port for the HTTP/WebSocket server.
const DefaultPort = 18520

// Server is an HTTP+WebSocket server that bridges Chrome Extension
// WebSocket connections to the channel.MessageHandler pipeline.
//
// Architecture:
//
//	┌──────────────┐  WebSocket   ┌──────────┐  handler   ┌──────────┐
//	│  Sidepanel    │ ←─────────→ │  Server   │ ────────→ │  Manager  │
//	│  (extension)  │   /ws       │  :18520   │ ←──────── │  (agent)  │
//	└──────────────┘              └──────────┘            └──────────┘
//
// Each browser tab opens one WebSocket connection, identified by its
// ThreadID (e.g., "tab_17"). The Server tracks threadID → conn mappings
// so proactive messages (cron, notifications) can be pushed to the
// correct browser tab.
type Server struct {
	server   *http.Server
	port     int
	handler  channel.MessageHandler

	// clients maps threadID → WebSocket connection for proactive Send().
	clients map[string]*websocket.Conn
	mu      sync.RWMutex

	logger *logger.Logger
}

// NewServer creates a Server.
func NewServer(port int) *Server {
	return &Server{
		port:    port,
		clients: make(map[string]*websocket.Conn),
		logger:  logger.New("channel.chrome"),
	}
}

// Start begins listening on 127.0.0.1:<port>. Blocking — call in a goroutine.
// Returns an error if the server fails to start.
func (s *Server) Start(handler channel.MessageHandler) error {
	s.handler = handler

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/ws", websocket.Handler(s.handleWS))

	s.server = &http.Server{
		Addr:    addr,
		Handler: withCORS(mux),
	}

	// Verify the port is free before entering ListenAndServe.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("chrome: cannot listen on %s: %w", addr, err)
	}

	s.logger.Info(context.Background(), "chrome: HTTP+WebSocket server listening", "addr", addr)

	if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("chrome: serve error: %w", err)
	}
	return nil
}

// Close gracefully shuts down the HTTP server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s.mu.Lock()
	for tid, conn := range s.clients {
		conn.Close()
		delete(s.clients, tid)
	}
	s.mu.Unlock()

	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

// Send delivers a proactive message to the WebSocket connection associated
// with the given threadID. Returns an error if the thread is unknown or
// the connection is dead.
func (s *Server) Send(threadID string, content string) error {
	s.mu.RLock()
	conn, ok := s.clients[threadID]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("chrome: no WebSocket client for thread %s", threadID)
	}

	resp := ChromeResponse{
		ID:       "", // proactive messages have no request ID
		Type:     "result",
		ThreadID: threadID,
		Content:  content,
	}

	s.mu.RLock()
	// Double-check under read lock.
	conn, ok = s.clients[threadID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("chrome: client disconnected for thread %s", threadID)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("chrome: marshal proactive message: %w", err)
	}

	s.mu.RLock()
	// Acquire write lock to send, preventing concurrent writes.
	// We re-check conn under the lock.
	conn, ok = s.clients[threadID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("chrome: client disconnected for thread %s", threadID)
	}

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(data); err != nil {
		// Connection is probably dead — remove it.
		s.mu.Lock()
		delete(s.clients, threadID)
		s.mu.Unlock()
		return fmt.Errorf("chrome: write to thread %s: %w", threadID, err)
	}
	return nil
}

// handleWS handles an incoming WebSocket connection from the extension.
func (s *Server) handleWS(conn *websocket.Conn) {
	addr := conn.RemoteAddr()
	s.logger.Info(context.Background(), "chrome: WebSocket connected", "addr", addr)

	// Read loop: one goroutine per connection.
	for {
		// Read a ChromeRequest message.
		var data []byte
		if err := websocket.Message.Receive(conn, &data); err != nil {
			s.logger.Error(context.Background(), "chrome: WebSocket read error", err, "addr", addr)
			s.removeClient(conn)
			return
		}

		if len(data) == 0 {
			continue
		}

		// Parse the request.
		var req ChromeRequest
		if err := json.Unmarshal(data, &req); err != nil {
			s.logger.Error(context.Background(), "chrome: invalid JSON", err, "addr", addr)
			s.writeError(conn, req.ID, req.ThreadID, fmt.Sprintf("invalid JSON: %v", err))
			continue
		}

		// Handle ping — respond immediately.
		if req.Action == "ping" {
			s.writeJSON(conn, ChromeResponse{
				ID:       req.ID,
				Type:     "result",
				ThreadID: req.ThreadID,
				Content:  "pong",
			})
			continue
		}

		s.logger.Info(context.Background(), "chrome: recv", "action", req.Action, "thread", req.ThreadID, "id", req.ID)

		// Track the connection by threadID so Send() can find it.
		if req.ThreadID != "" {
			s.setClient(req.ThreadID, conn)
		}

		// Build the IncomingMessage and call the handler.
		incoming := s.toIncoming(req)

		// Run handler in a separate goroutine so we can keep reading
		// (e.g., for steer/ambient messages while the agent is processing).
		// We capture the request's ID and threadID for the response.
		go s.handleMessage(conn, req.ID, req.ThreadID, incoming)
	}
}

// handleMessage calls the handler and writes the response back.
// Runs in a separate goroutine per message.
func (s *Server) handleMessage(conn *websocket.Conn, reqID, threadID string, incoming channel.IncomingMessage) {
	result := s.handler(context.Background(), incoming)

	if result.Err != nil {
		s.logger.Error(context.Background(), "chrome: handler error", result.Err, "thread", threadID)
		s.writeError(conn, reqID, threadID, fmt.Sprintf("❌ %v", result.Err))
		return
	}

	if result.Steered {
		s.logger.Info(context.Background(), "chrome: message steered", "thread", threadID)
		return
	}

	// Send the reply back.
	s.writeJSON(conn, ChromeResponse{
		ID:       reqID,
		Type:     "result",
		ThreadID: threadID,
		Content:  result.Reply.Content,
	})
}

// toIncoming converts a ChromeRequest to a channel.IncomingMessage.
func (s *Server) toIncoming(req ChromeRequest) channel.IncomingMessage {
	content := req.Content
	if content == "" {
		content = req.Selection.Text
	}
	return channel.IncomingMessage{
		ThreadID:  req.ThreadID,
		MessageID: req.ID,
		Content:   content,
	}
}

// writeJSON marshals and sends a ChromeResponse over WebSocket.
func (s *Server) writeJSON(conn *websocket.Conn, resp ChromeResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error(context.Background(), "chrome: marshal response", err)
		return
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := websocket.Message.Send(conn, string(data)); err != nil {
		s.logger.Error(context.Background(), "chrome: write error", err)
	}
}

// writeError sends an error response.
func (s *Server) writeError(conn *websocket.Conn, reqID, threadID, msg string) {
	s.writeJSON(conn, ChromeResponse{
		ID:       reqID,
		Type:     "error",
		ThreadID: threadID,
		Content:  msg,
	})
}

// setClient registers a WebSocket connection for a threadID.
// Closes any previous connection for the same threadID.
func (s *Server) setClient(threadID string, conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.clients[threadID]; ok && old != conn {
		old.Close()
	}
	s.clients[threadID] = conn
}

// removeClient removes a WebSocket connection from the tracking map.
func (s *Server) removeClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove all entries for the given connection (a connection might
	// have been registered under multiple threadIDs across reconnections).
	for tid, c := range s.clients {
		if c == conn {
			delete(s.clients, tid)
			s.logger.Info(context.Background(), "chrome: removed client", "thread", tid)
		}
	}
}

// handleHealthz provides a simple health check endpoint.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

// withCORS wraps an http.Handler with CORS headers for Chrome extension access.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
