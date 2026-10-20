package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// 移动接口：手机是同一批会话的第三块屏，不是第二个 agent 宿主。
//
// 它住在桌面进程里（package main）而不是一个公共包里，只因为它的工作就是够到 desktopApp 的
// 内部（d.asks、d.runs、事件出口）—— 那些不该被导出成一套新的公共 API。它不是一个新的入口
// 模式：不碰 main.go 的 command，也不进 channel manager；随桌面进程启动，随它退出。
//
// 设计：docs/2026-10-18-mobile-client-design.md §4；线上协议：docs/agents/mobile-protocol.md。

const (
	// mobileMaxConns bounds how many phones one desktop serves. The seventeenth is refused at
	// handshake: the point of the bound is that no phone can cost the desktop anything, and a
	// connection table with no ceiling is exactly that cost.
	mobileMaxConns = 16
	// mobileQueue bounds one connection's outbound queue. Full means the phone cannot keep up,
	// and the answer is "drop it and let it reread" — never block the goroutine that emits
	// events (a tool result, a state change) behind a phone's socket.
	mobileQueue = 256
	// mobileWriteTimeout bounds a single write, so a stalled socket dies instead of wedging its
	// writer goroutine forever.
	mobileWriteTimeout = 10 * time.Second
	// mobileIdleTimeout is the heartbeat rule from the protocol: no ping for two minutes closes
	// the connection (the phone reconnects when it comes back to the foreground).
	mobileIdleTimeout = 2 * time.Minute
)

// mobileServer is the HTTP + WebSocket listener. One per process, present only while the
// config says it is enabled.
type mobileServer struct {
	desk *desktopApp
	srv  *http.Server
	ln   net.Listener

	mu    sync.Mutex
	token string
	conns map[*mobileConn]struct{}
	// listenErr is why the listener is not up (port taken, bad address). It is NOT fatal: the
	// settings page reports it, and the desktop keeps running.
	listenErr string
}

// mobileServer returns the live server, or nil when the interface is off. The pointer is read
// under the app lock because start/stop replace it.
func (d *desktopApp) mobileServer() *mobileServer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mobile
}

func (d *desktopApp) setMobileServer(m *mobileServer) {
	d.mu.Lock()
	d.mobile = m
	d.mu.Unlock()
}

// startMobile brings the interface up when the config asks for it. A failure to listen is
// reported and swallowed — an interface that cannot bind must not cost the user their desktop
// (same posture as the node service).
func (d *desktopApp) startMobile(cfg *config.Config) {
	d.stopMobile()
	if cfg == nil || !cfg.Mobile.Enabled {
		return
	}
	token, err := loadOrCreateMobileToken()
	if err != nil {
		logger.New("desktop").Error(context.Background(), "mobile: no token file", err)
		return
	}
	m := newMobileServer(d, token)
	addr := cfg.Mobile.MobileAddress()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.listenErr = err.Error()
		d.setMobileServer(m)
		logger.New("desktop").Error(context.Background(), "mobile: listener unavailable at "+addr, err)
		return
	}
	m.ln = ln
	d.setMobileServer(m)
	go m.serve()
}

// stopMobile closes the listener and every live connection. Idempotent.
func (d *desktopApp) stopMobile() {
	m := d.mobileServer()
	d.setMobileServer(nil)
	if m != nil {
		m.close()
	}
}

func newMobileServer(d *desktopApp, token string) *mobileServer {
	return &mobileServer{desk: d, token: token, conns: make(map[*mobileConn]struct{})}
}

// setToken replaces the credential and drops every live connection: a regenerated token that
// left old sockets working would not have invalidated anything (see RegenerateMobileToken).
func (m *mobileServer) setToken(token string) {
	m.mu.Lock()
	m.token = token
	conns := make([]*mobileConn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (m *mobileServer) listenError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listenErr
}

func (m *mobileServer) serve() {
	server := &http.Server{Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second}
	m.srv = server
	err := server.Serve(m.ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.New("desktop").Error(context.Background(), "mobile: server stopped", err)
	}
}

func (m *mobileServer) close() {
	m.mu.Lock()
	conns := make([]*mobileConn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
	if m.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = m.srv.Shutdown(ctx)
		cancel()
	} else if m.ln != nil {
		_ = m.ln.Close()
	}
}

// handler wires the protocol's routes. Every route except ping and the WebSocket needs the
// bearer token; the WebSocket takes it in the query string because React Native cannot set
// handshake headers (protocol).
func (m *mobileServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", m.handlePing)
	mux.HandleFunc("GET /v1/ws", m.handleWS)
	mux.HandleFunc("GET /v1/sessions", m.guard(m.handleSessions))
	mux.HandleFunc("POST /v1/sessions", m.guard(m.handleCreateSession))
	mux.HandleFunc("GET /v1/projects", m.guard(m.handleProjects))
	mux.HandleFunc("GET /v1/providers", m.guard(m.handleProviders))
	mux.HandleFunc("GET /v1/sessions/{id}/page", m.guard(m.handlePage))
	mux.HandleFunc("GET /v1/sessions/{id}/files/{attachmentId}", m.guard(m.handleFile))
	mux.HandleFunc("POST /v1/sessions/{id}/messages", m.guard(m.handleMessage))
	mux.HandleFunc("POST /v1/sessions/{id}/stop", m.guard(m.handleStop))
	mux.HandleFunc("DELETE /v1/sessions/{id}", m.guard(m.handleDeleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/pin", m.guard(m.handlePin))
	mux.HandleFunc("POST /v1/sessions/{id}/provider", m.guard(m.handleProvider))
	mux.HandleFunc("POST /v1/sessions/{id}/skip-approval", m.guard(m.handleSkipApproval))
	mux.HandleFunc("POST /v1/sessions/{id}/permissions/{toolId}", m.guard(m.handlePermission))
	mux.HandleFunc("POST /v1/sessions/{id}/questions", m.guard(m.handleQuestions))
	return mux
}

// guard is the bearer check for HTTP. The comparison is constant time: the token IS the
// credential, and a byte-by-byte early exit would leak it one request at a time.
func (m *mobileServer) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		m.mu.Lock()
		want := m.token
		m.mu.Unlock()
		if want == "" || !mobileTokenEqual(got, want) {
			mobileFail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func mobileFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func mobileWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ── connections ──

// mobileConn is one phone. The event queue is the backpressure boundary: a full queue closes
// the connection instead of slowing the emitter down.
type mobileConn struct {
	srv *mobileServer
	ws  *websocket.Conn

	out    chan []byte
	lagged chan struct{}
	gone   chan struct{}

	mu        sync.Mutex
	sessionID string
	closed    bool
	once      sync.Once
}

func newMobileConn(srv *mobileServer, ws *websocket.Conn) *mobileConn {
	return &mobileConn{
		srv:    srv,
		ws:     ws,
		out:    make(chan []byte, mobileQueue),
		lagged: make(chan struct{}, 1),
		gone:   make(chan struct{}),
	}
}

// subscribe replaces this connection's subscription. One session at a time (protocol): the
// second `open` replaces the first.
func (c *mobileConn) subscribe(sessionID string) {
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
}

// wants reports whether this connection should receive an event about sessionID. An empty
// sessionID is a broadcast (ask_answered is the protocol's one exception: it is small and may
// belong to any conversation).
func (c *mobileConn) wants(sessionID string) bool {
	if sessionID == "" {
		return true // broadcast: ask_answered may belong to any conversation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID == sessionID
}

// send queues one already-marshalled event. NEVER blocks: a phone that cannot keep up gets a
// `lagged` and a closed socket, and rereads a snapshot on reconnect.
func (c *mobileConn) send(b []byte) {
	if c.isClosed() {
		return
	}
	select {
	case c.out <- b:
	default:
		select {
		case c.lagged <- struct{}{}:
		default:
		}
	}
}

func (c *mobileConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *mobileConn) close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.gone)
		// A connection that never finished its handshake has no socket yet (tests build one
		// to exercise the queue); there is nothing to close then.
		if c.ws != nil {
			_ = c.ws.CloseNow()
		}
	})
}

// writeLoop is the connection's ONLY writer, so two half-written frames can never interleave.
func (c *mobileConn) writeLoop() {
	for {
		select {
		case <-c.gone:
			return
		case <-c.lagged:
			// The phone fell behind: say so, then drop it. It reconnects and rereads a
			// snapshot — the record is authoritative, the stream only extends the tail.
			c.write(mobileMarshal(map[string]any{"type": mobileEvLagged}))
			c.close()
			return
		case b := <-c.out:
			if !c.write(b) {
				c.close()
				return
			}
		}
	}
}

func (c *mobileConn) write(b []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), mobileWriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b) == nil
}

// notify sends one event to the phones that should see it. Called from the app's event paths;
// it takes the server pointer under the app lock only long enough to read it.
func (d *desktopApp) mobileNotify(sessionID string, ev map[string]any) {
	m := d.mobileServer()
	if m == nil {
		return
	}
	m.broadcast(sessionID, ev)
}

// mobileBroadcast sends one event to EVERY phone, subscribed or not. It exists for ask_answered
// (see the protocol's exception) and nothing else: a session's stream must not be sprayed at
// connections looking at another conversation.
func (d *desktopApp) mobileBroadcast(ev map[string]any) {
	m := d.mobileServer()
	if m == nil {
		return
	}
	m.broadcast("", ev)
}

func (m *mobileServer) broadcast(sessionID string, ev map[string]any) {
	b := mobileMarshal(ev)
	m.mu.Lock()
	conns := make([]*mobileConn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		if c.wants(sessionID) {
			c.send(b)
		}
	}
}

func mobileMarshal(ev map[string]any) []byte {
	b, err := json.Marshal(ev)
	if err != nil {
		// A payload that cannot marshal is a programming error, not a runtime condition: log
		// it and send nothing rather than a half-shaped event the phone would misread.
		logger.New("desktop").Error(context.Background(), "mobile: event marshal failed", err)
		return []byte(`{"type":"lagged"}`)
	}
	return b
}

// ── WebSocket endpoint ──

func (m *mobileServer) handleWS(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	token := m.token
	n := len(m.conns)
	m.mu.Unlock()
	if token == "" || !mobileTokenEqual(r.URL.Query().Get("token"), token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if n >= mobileMaxConns {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The origin check exists to stop a browser page on another site from opening this
		// socket. There is no browser here: the client is a phone that presents a bearer
		// token in the query string, and the traffic is LAN plaintext by design (protocol).
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := newMobileConn(m, ws)
	m.mu.Lock()
	if len(m.conns) >= mobileMaxConns { // re-check: two handshakes can race the first count
		m.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "too many connections")
		return
	}
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	go c.writeLoop()
	defer func() {
		m.mu.Lock()
		delete(m.conns, c)
		m.mu.Unlock()
		c.close()
	}()
	c.readLoop(r.Context())
}

// readLoop serves the client's commands until it goes away or stops sending heartbeats.
func (c *mobileConn) readLoop(parent context.Context) {
	for {
		ctx, cancel := context.WithTimeout(parent, mobileIdleTimeout)
		_, data, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var msg struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // one unparseable frame is not a broken connection
		}
		switch msg.Type {
		case mobileCmdPing:
			c.send(mobileMarshal(map[string]any{"type": "pong"}))
		case mobileCmdOpen:
			c.open(msg.SessionID)
		}
	}
}

// open subscribes and answers with a snapshot. It is a READ: the phone looking at a
// conversation must not build an agent, must not touch the desktop's displayed session, and
// must not emit a state event (see loadSessionForRemote for the write path, which does restore
// the history before a message).
func (c *mobileConn) open(sessionID string) {
	c.subscribe(sessionID)
	snap, err := c.srv.desk.mobileSnapshot(sessionID)
	if err != nil {
		c.send(mobileMarshal(map[string]any{
			"type": mobileEvError, "message": err.Error(), "interrupted": false,
		}))
		return
	}
	c.send(mobileMarshal(map[string]any{"type": mobileEvSnapshot, "snapshot": snap}))
}
