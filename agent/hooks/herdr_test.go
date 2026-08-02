package hooks

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// herdrTestMsg is the subset of a Herdr API request the test server decodes.
type herdrTestMsg struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// startHerdrTestServer listens on a temp Unix socket and forwards every
// received request to the returned channel. The handler's send() never reads
// a response, so the server only needs to accept + decode.
// A short dir name is required: macOS sun_path is capped at ~104 bytes and
// t.TempDir() paths exceed it.
func startHerdrTestServer(t *testing.T) (string, chan herdrTestMsg) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make(chan herdrTestMsg, 64)
	go func() {
		// Serial accept+read per connection: the handler opens a fresh socket
		// per report, and concurrent per-connection reader goroutines would
		// race on the msgs channel, defeating the order assertions.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			dec := json.NewDecoder(conn)
			for {
				var m herdrTestMsg
				if err := dec.Decode(&m); err != nil {
					break
				}
				msgs <- m
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return sockPath, msgs
}

func newTestHerdrHandler(sockPath string) *HerdrHandler {
	h := &HerdrHandler{
		sockPath: sockPath,
		paneID:   "w1:p1",
		source:   "herdr:tachi",
		agent:    "tachi",
		sendCh:   make(chan map[string]any, 64),
	}
	go h.sendLoop()
	return h
}

func drainMsgs(t *testing.T, msgs chan herdrTestMsg, n int) []herdrTestMsg {
	t.Helper()
	got := make([]herdrTestMsg, 0, n)
	timeout := time.After(3 * time.Second)
	for len(got) < n {
		select {
		case m := <-msgs:
			got = append(got, m)
		case <-timeout:
			t.Fatalf("timed out waiting for %d reports, got %d: %v", n, len(got), got)
		}
	}
	return got
}

// TestHerdrHandlerPreservesOrder verifies that state reports reach the socket
// in dispatch order even though Handle is non-blocking: a single FIFO worker
// must deliver stream_start → tool_call → turn_complete as
// working → working → idle.
func TestHerdrHandlerPreservesOrder(t *testing.T) {
	sockPath, msgs := startHerdrTestServer(t)
	h := newTestHerdrHandler(sockPath)

	h.Handle(context.Background(), EventStreamStart, []byte(`{"session_id":"s1"}`))
	h.Handle(context.Background(), EventToolCall, []byte(`{"session_id":"s1"}`))
	h.Handle(context.Background(), EventTurnComplete, []byte(`{"session_id":"s1"}`))

	got := drainMsgs(t, msgs, 3)
	want := []string{"working", "working", "idle"}
	for i, m := range got {
		if m.Method != "pane.report_agent" {
			t.Fatalf("report %d method = %q, want pane.report_agent", i, m.Method)
		}
		if state, _ := m.Params["state"].(string); state != want[i] {
			t.Errorf("report %d state = %q, want %q (order must be preserved)", i, state, want[i])
		}
	}
}

// TestHerdrHandlerSessionEndSendsSynchronously verifies that EventSessionEnd
// sends release_agent synchronously (not queued): the process may exit right
// after Close, so the release must reach Herdr even if the worker never gets
// to drain the queue again.
func TestHerdrHandlerSessionEndSendsSynchronously(t *testing.T) {
	sockPath, msgs := startHerdrTestServer(t)
	h := newTestHerdrHandler(sockPath)

	// Queue a couple of state reports, then release synchronously. The
	// release must be observed regardless of what the worker manages to
	// deliver (the synchronous send cannot be lost).
	h.Handle(context.Background(), EventStreamStart, []byte(`{}`))
	h.Handle(context.Background(), EventToolCall, []byte(`{}`))
	h.Handle(context.Background(), EventSessionEnd, []byte(`{}`))

	timeout := time.After(3 * time.Second)
	for {
		select {
		case m := <-msgs:
			if m.Method == "pane.release_agent" {
				return // release arrived — good enough
			}
		case <-timeout:
			t.Fatal("release_agent never arrived after synchronous session_end")
		}
	}
}

// TestHerdrHandlerMappings covers the state vocabulary for every mapped
// event: blocked while waiting on the user, working otherwise, idle only on
// turn completion and errors.
func TestHerdrHandlerMappings(t *testing.T) {
	cases := []struct {
		event string
		state string // "" = no state report (session/release actions)
	}{
		{EventStreamStart, "working"},
		{EventTurnComplete, "idle"},
		{EventTurnTruncated, "working"},
		{EventToolCall, "working"},
		{EventToolResult, "working"},
		{EventPermissionRequest, "blocked"},
		{EventPermissionResult, "working"},
		{EventAskUserQuestion, "blocked"},
		{EventAskUserResponse, "working"},
		{EventError, "idle"},
		{EventSessionStart, ""},
		{EventSessionEnd, ""},
	}
	for _, c := range cases {
		ea, ok := EventActions[c.event]
		if !ok {
			t.Errorf("event %s missing from EventActions", c.event)
			continue
		}
		if ea.state != c.state {
			t.Errorf("event %s maps to state %q, want %q", c.event, ea.state, c.state)
		}
	}
}
