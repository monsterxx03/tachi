package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event 是服务端推给手机的一条增量。字段与 docs/agents/mobile-protocol.md
// 的事件表一一对应；用 omitempty，一个结构体覆盖所有类型。
type Event struct {
	Type string `json:"type"`
	// ID 是 cron_changed 变的那一条（定时任务不属于任何会话，所以走广播）
	ID string `json:"id,omitempty"`

	Text        string        `json:"text,omitempty"`
	DisplayText string        `json:"displayText,omitempty"`
	Origin      string        `json:"origin,omitempty"`
	Steer       bool          `json:"steer,omitempty"`
	Snapshot    *Snapshot     `json:"snapshot,omitempty"`
	State       *SessionState `json:"state,omitempty"`
	Ask         any           `json:"ask,omitempty"`
	ToolID      string        `json:"toolId,omitempty"`
	ToolName    string        `json:"toolName,omitempty"`
	Title       string        `json:"title,omitempty"`
	Args        string        `json:"args,omitempty"`
	Result      string        `json:"result,omitempty"`
	IsError     bool          `json:"isError,omitempty"`
	Truncated   bool          `json:"truncated,omitempty"`
	End         string        `json:"end,omitempty"`
	DurationMs  int64         `json:"durationMs,omitempty"`
	Iterations  int           `json:"iterations,omitempty"`
	Message     string        `json:"message,omitempty"`
	Interrupted bool          `json:"interrupted,omitempty"`
	// From / To 是 session_moved 的两个 id：这段对话从哪搬到哪（压缩写进子会话）
	From    string     `json:"from,omitempty"`
	To      string     `json:"to,omitempty"`
	Tps     float64    `json:"tps,omitempty"`
	LastTps float64    `json:"lastTps,omitempty"`
	Usage   *UsageInfo `json:"usage,omitempty"`
	Cost    float64    `json:"cost,omitempty"`
}

// 协议里的不变量：每条连接 256 条事件的缓冲，满了发 lagged 并断开
const clientQueueSize = 256

var upgrader = websocket.Upgrader{
	// 局域网明文，来源不设限；鉴权靠查询串里的令牌
	CheckOrigin: func(r *http.Request) bool { return true },
}

type client struct {
	srv       *Server
	conn      *websocket.Conn
	sessionID string

	send   chan []byte
	closed chan struct{}
	once   sync.Once
}

func (c *client) enqueue(data []byte) {
	select {
	case c.send <- data:
	case <-c.closed:
	default:
		// 跟不上就断开，让客户端重连重读 —— 绝不阻塞发事件的那个 goroutine
		c.srv.log.Info(context.Background(), "mobile client lagged, dropping connection")
		c.close()
	}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// React Native 的 WebSocket 设不了握手头，所以令牌在查询串里
	if !constTimeEqual(r.URL.Query().Get("token"), s.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	c := &client{
		srv:    s,
		conn:   conn,
		send:   make(chan []byte, clientQueueSize),
		closed: make(chan struct{}),
	}

	s.mu.Lock()
	if len(s.clients) >= 16 {
		// 第 17 条在握手时拒绝（同样是「手机不能拖住桌面」）
		s.mu.Unlock()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "too many connections"),
			time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	s.clients[c] = struct{}{}
	s.mu.Unlock()

	go c.writeLoop()
	c.readLoop()

	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.close()
}

// writeLoop 是唯一写这条连接的地方
func (c *client) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.closed:
			return
		case data := <-c.send:
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *client) readLoop() {
	// 两分钟没有心跳就关（协议里那条）
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "open":
			// 一次只订阅一个会话，第二次 open 替换订阅
			c.sessionID = msg.SessionID
			if snap := c.srv.snapshot(msg.SessionID); snap != nil {
				c.enqueueEvent(Event{Type: "snapshot", Snapshot: snap})
			}
		case "ping":
			c.enqueueEvent(map[string]string{"type": "pong"})
		}
	}
}

func (c *client) enqueueEvent(v any) {
	if data, err := json.Marshal(v); err == nil {
		c.enqueue(data)
	}
}

// broadcast 只发订阅了该会话的连接；ask_answered 例外（它可能属于任何会话）
func (s *Server) broadcast(sessionID string, ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	targets := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		// ask_answered / sessions_changed / cron_changed 是广播：它们不属于某一段会话的流
		if ev.Type == "ask_answered" || ev.Type == "sessions_changed" || ev.Type == "cron_changed" || c.sessionID == sessionID {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()

	for _, c := range targets {
		c.enqueue(data)
	}
}

// pushSnapshot 在会话级的设置变了之后，让手机重读一份
func (s *Server) pushSnapshot(sessionID string) {
	if snap := s.snapshot(sessionID); snap != nil {
		s.broadcast(sessionID, Event{Type: "snapshot", Snapshot: snap})
	}
}

func (s *Server) snapshot(sessionID string) *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[sessionID]
	if !ok {
		return nil
	}
	cp := sess.Snapshot
	// 快照只带最近 snapshotLimit 条（和真服务端同一个上限），更早的由 page 端点翻 ——
	// 手机「往上滑加载更早」的路才有东西可翻。
	msgs := sess.Messages
	if len(msgs) > snapshotLimit {
		msgs = msgs[len(msgs)-snapshotLimit:]
		cp.HasMore = true
	}
	// 空就是空数组，不是 null —— Go 的 nil 切片会序列化成 null，客户端会当数组用
	cp.Messages = append([]Message{}, msgs...)
	cp.Asks = append([]any{}, sess.Asks...)
	cp.Attachments = append([]AttachmentRef{}, sess.Attachments...)
	cp.Context.Parts = append([]ContextPart{}, sess.Context.Parts...)
	cp.Workspace.Additional = append([]string{}, sess.Workspace.Additional...)
	return &cp
}
