package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/channel"
	"golang.org/x/net/websocket"
)

// wsDial connects to the test server's /ws endpoint and returns a WebSocket
// connection configured for text messages.
func wsDial(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	origin := "chrome-extension://test-extension-id"
	url := fmt.Sprintf("ws://%s/ws", addr)
	conn, err := websocket.Dial(url, "", origin)
	if err != nil {
		t.Fatalf("wsDial: %v", err)
	}
	return conn
}

// wsSend marshals a ChromeRequest and sends it over WebSocket.
func wsSend(t *testing.T, conn *websocket.Conn, req ChromeRequest) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("wsSend marshal: %v", err)
	}
	if err := websocket.Message.Send(conn, string(data)); err != nil {
		t.Fatalf("wsSend: %v", err)
	}
}

// wsRecv reads a ChromeResponse from WebSocket.
func wsRecv(t *testing.T, conn *websocket.Conn) ChromeResponse {
	t.Helper()
	var data string
	if err := websocket.Message.Receive(conn, &data); err != nil {
		t.Fatalf("wsRecv: %v", err)
	}
	var resp ChromeResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("wsRecv unmarshal: %v", err)
	}
	return resp
}

// startTestServer creates a ChromeChannel on a random port and starts it.
// Returns (channel, addr, cleanupFn).
func startTestServer(t *testing.T, handler channel.MessageHandler) (*ChromeChannel, string, func()) {
	t.Helper()

	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	ch := NewChromeChannel("chrome", port)
	ch.server = NewServer(port)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)
		// Start the server in a goroutine.
		errCh := make(chan error, 1)
		go func() {
			errCh <- ch.server.Start(handler)
		}()
		<-ctx.Done()
		ch.server.Close()
		<-errCh
	}()

	// Wait for server to be ready.
	time.Sleep(50 * time.Millisecond)

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cleanup := func() {
		cancel()
		<-done
	}

	return ch, addr, cleanup
}

// ── Tests ──────────────────────────────────────────────────────────────────

func TestPingPong(t *testing.T) {
	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		t.Errorf("handler should not be called for ping")
		return channel.HandlerResult{}
	}

	_, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	conn := wsDial(t, addr)
	defer conn.Close()

	wsSend(t, conn, ChromeRequest{
		ID:       "ping-1",
		Action:   "ping",
		ThreadID: "global",
	})

	resp := wsRecv(t, conn)
	if resp.ID != "ping-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "ping-1")
	}
	if resp.Content != "pong" {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "pong")
	}
}

func TestHandlerInvocation(t *testing.T) {
	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		if msg.ThreadID != "tab_456" {
			t.Errorf("ThreadID = %q, want %q", msg.ThreadID, "tab_456")
		}
		if msg.MessageID != "req-1" {
			t.Errorf("MessageID = %q, want %q", msg.MessageID, "req-1")
		}
		if !strings.Contains(msg.Content, "ReAct") {
			t.Errorf("Content should contain 'ReAct', got: %s", msg.Content)
		}
		return channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  "Here's the explanation of ReAct...",
			},
		}
	}

	_, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	conn := wsDial(t, addr)
	defer conn.Close()

	wsSend(t, conn, ChromeRequest{
		ID:       "req-1",
		Action:   "ask_tachi",
		ThreadID: "tab_456",
		Selection: struct {
			Text  string `json:"text"`
			URL   string `json:"url,omitempty"`
			Title string `json:"title,omitempty"`
		}{
			Text:  "ReAct",
			URL:   "https://react.dev",
			Title: "ReAct Framework",
		},
	})

	resp := wsRecv(t, conn)
	if resp.ID != "req-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "req-1")
	}
	if resp.Type != "result" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "result")
	}
	if resp.Content != "Here's the explanation of ReAct..." {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "Here's the explanation of ReAct...")
	}
}

func TestToIncoming(t *testing.T) {
	server := NewServer(DefaultPort)

	t.Run("content takes priority", func(t *testing.T) {
		req := ChromeRequest{
			Action:  "summarize",
			Content: "请总结此页面",
		}
		req.Selection.Text = "fallback text"

		msg := server.toIncoming(req)
		if msg.Content != "请总结此页面" {
			t.Errorf("Content = %q, want content to take priority", msg.Content)
		}
	})

	t.Run("fallback to selection text", func(t *testing.T) {
		req := ChromeRequest{
			Action: "explain",
		}
		req.Selection.Text = "selected text"

		msg := server.toIncoming(req)
		if msg.Content != "selected text" {
			t.Errorf("Content = %q, want %q", msg.Content, "selected text")
		}
	})

	t.Run("empty when nothing provided", func(t *testing.T) {
		req := ChromeRequest{
			Action: "explain",
		}

		msg := server.toIncoming(req)
		if msg.Content != "" {
			t.Errorf("Content = %q, want empty", msg.Content)
		}
	})
}

func TestSend(t *testing.T) {
	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		return channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  "reply to: " + msg.Content,
			},
		}
	}

	ch, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	// First, establish a WebSocket connection so the server tracks this thread.
	conn := wsDial(t, addr)
	defer conn.Close()

	// Send a request to register the threadID -> conn mapping.
	wsSend(t, conn, ChromeRequest{
		ID:       "init-1",
		Action:   "ask_tachi",
		ThreadID: "tab_123",
		Content:  "hello",
	})

	// Drain the response.
	wsRecv(t, conn)

	// Now use channel.Send() to send a proactive message.
	if err := ch.Send(t.Context(), channel.OutgoingMessage{
		ThreadID: "tab_123",
		Content:  "proactive notification",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp := wsRecv(t, conn)
	if resp.Type != "result" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "result")
	}
	if resp.Content != "proactive notification" {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "proactive notification")
	}
}

func TestHandlerError(t *testing.T) {
	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		return channel.HandlerResult{
			Err: fmt.Errorf("something went wrong"),
		}
	}

	_, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	conn := wsDial(t, addr)
	defer conn.Close()

	wsSend(t, conn, ChromeRequest{
		ID:       "err-1",
		Action:   "explain",
		ThreadID: "tab_789",
		Content:  "test",
	})

	resp := wsRecv(t, conn)
	if resp.Type != "error" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "error")
	}
	if resp.ID != "err-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "err-1")
	}
	if !strings.Contains(resp.Content, "something went wrong") {
		t.Errorf("resp.Content should contain error, got: %s", resp.Content)
	}
}

func TestSteeredResult(t *testing.T) {
	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		return channel.HandlerResult{
			Steered: true,
		}
	}

	_, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	conn := wsDial(t, addr)
	defer conn.Close()

	wsSend(t, conn, ChromeRequest{
		ID:       "steer-1",
		Action:   "ask_tachi",
		ThreadID: "tab_999",
		Content:  "follow up",
	})

	// Steered results produce no response — verify by waiting briefly
	// and checking no data arrives.
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var data string
	err := websocket.Message.Receive(conn, &data)
	if err == nil {
		t.Errorf("expected timeout (no response for steered), got data: %s", data)
	}
}

func TestConcurrentThreads(t *testing.T) {
	var mu sync.Mutex
	received := make(map[string]bool)

	handler := func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		mu.Lock()
		received[msg.ThreadID] = true
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // simulate work
		return channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  "done",
			},
		}
	}

	_, addr, cleanup := startTestServer(t, handler)
	defer cleanup()

	// Two connections, different threads.
	conn1 := wsDial(t, addr)
	defer conn1.Close()
	conn2 := wsDial(t, addr)
	defer conn2.Close()

	wsSend(t, conn1, ChromeRequest{ID: "a1", Action: "ask_tachi", ThreadID: "tab_1", Content: "msg1"})
	wsSend(t, conn2, ChromeRequest{ID: "b1", Action: "ask_tachi", ThreadID: "tab_2", Content: "msg2"})

	resp1 := wsRecv(t, conn1)
	resp2 := wsRecv(t, conn2)

	if resp1.ThreadID != "tab_1" || resp2.ThreadID != "tab_2" {
		t.Errorf("threads mixed up: got %s and %s", resp1.ThreadID, resp2.ThreadID)
	}
}

func TestHealthz(t *testing.T) {
	_, addr, cleanup := startTestServer(t, func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		return channel.HandlerResult{}
	})
	defer cleanup()

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
