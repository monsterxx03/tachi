package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// HerdrHandler reports Tachi lifecycle events to a local Herdr server via its
// Unix domain socket API. It is registered as a Callback handler on the
// Dispatcher when HERDR_ENV=1 is detected at startup.
//
// The handler connects to the Herdr socket, sends a JSON-RPC-style request,
// and closes the connection. It does not wait for a response.
//
// State reports funnel through a single FIFO queue drained by one worker
// goroutine, so they reach Herdr in dispatch order. This matters: Herdr
// tracks a per-source monotonic seq and DROPS any report whose seq is not
// strictly increasing (out-of-order delivery). Fire-and-forget goroutines
// per event would race on the socket and could deliver e.g. a blocked report
// ahead of its working predecessor, leaving the pane stuck on a stale state.
// The queue is deliberately the only concurrency control: it is lock-free
// and cannot block the agent loop (dropping the newest report when full).
type HerdrHandler struct {
	sockPath string // HERDR_SOCKET_PATH
	paneID   string // HERDR_PANE_ID
	source   string // "herdr:tachi"
	agent    string // "tachi"
	logger   *logger.Logger

	// sendCh is the ordered send queue; sendLoop drains it one request at a
	// time. Buffered so a burst of events (stream_start + tool_call) never
	// blocks the agent loop; when full, the newest report is dropped rather
	// than stalling dispatch.
	sendCh chan map[string]any
}

// DetectHerdr reports whether Tachi is running inside a Herdr-managed pane.
func DetectHerdr() bool {
	return os.Getenv("HERDR_ENV") == "1" &&
		os.Getenv("HERDR_SOCKET_PATH") != "" &&
		os.Getenv("HERDR_PANE_ID") != ""
}

// NewHerdrHandler creates a handler from the current process environment.
func NewHerdrHandler() *HerdrHandler {
	h := &HerdrHandler{
		sockPath: os.Getenv("HERDR_SOCKET_PATH"),
		paneID:   os.Getenv("HERDR_PANE_ID"),
		source:   "herdr:tachi",
		agent:    "tachi",
		sendCh:   make(chan map[string]any, 64),
	}
	go h.sendLoop()
	return h
}

// SetLogger attaches a logger for reporting socket send outcomes (optional).
func (h *HerdrHandler) SetLogger(l *logger.Logger) { h.logger = l }

// herdrAction distinguishes between session identity, lifecycle state reports,
// and agent release.
type herdrAction string

const (
	actionSession = herdrAction("session") // → pane.report_agent_session
	actionState   = herdrAction("state")   // → pane.report_agent
	actionRelease = herdrAction("release") // → pane.release_agent
)

// eventAction maps a Tachi hook event to a Herdr socket API call.
type eventAction struct {
	action herdrAction
	state  string // "working" / "blocked" / "idle"; empty for session actions
}

// EventActions maps Tachi events to Herdr actions. Exported so the agent can
// iterate over them when registering handlers.
var EventActions = map[string]eventAction{
	EventSessionStart: {action: actionSession},
	EventSessionEnd:   {action: actionRelease},
	EventStreamStart:  {action: actionState, state: "working"},
	EventTurnComplete: {action: actionState, state: "idle"},
	// A truncated turn keeps working — the loop continues with a
	// continuation prompt rather than finishing.
	EventTurnTruncated: {action: actionState, state: "working"},
	EventToolCall:      {action: actionState, state: "working"},
	// tool_result re-asserts working: after the last tool of a turn the
	// loop goes straight back to the LLM, and if the tool_call working
	// report was ever dropped this keeps the pane from drifting stale.
	EventToolResult:        {action: actionState, state: "working"},
	EventPermissionRequest: {action: actionState, state: "blocked"},
	// Permission approval returns to "working" so the tool execution phase
	// (which doesn't re-fire tool_call because the bash ask was handled by
	// the policy path) doesn't leave Herdr stuck in "blocked".
	EventPermissionResult: {action: actionState, state: "working"},
	EventAskUserQuestion:  {action: actionState, state: "blocked"},
	// Answering the form ends the blocked phase; the loop resumes with the
	// tool result and the next LLM call.
	EventAskUserResponse: {action: actionState, state: "working"},
	EventError:           {action: actionState, state: "idle"},
}

// Handle implements the callback signature for hooks.Dispatcher.
// It receives the event name and raw JSON payload from the dispatcher,
// and queues the appropriate Herdr socket API request.
func (h *HerdrHandler) Handle(ctx context.Context, event string, payload []byte) {
	ea, ok := EventActions[event]
	if !ok {
		return
	}

	// Extract session_id from payload if present
	var data struct {
		SessionID string `json:"session_id,omitempty"`
	}
	// Ignore parse errors; session_id is optional
	_ = json.Unmarshal(payload, &data)

	req := h.buildRequest(ea, data.SessionID)

	if event == EventSessionEnd {
		// The process may exit immediately after Close, so the release must
		// be sent synchronously — a queued report could be lost with the
		// worker goroutine. Any state reports still in the queue arrive
		// before it (normal order) or are dropped with the process (nobody
		// observes them at exit).
		h.send(req)
		return
	}

	select {
	case h.sendCh <- req:
	default:
		// Queue full (Herdr slow/unreachable + a burst of events): drop the
		// newest report rather than stall the agent loop. The next event
		// (usually the turn-ending one) still reaches Herdr.
		if h.logger != nil {
			h.logger.Debug(ctx, "Hooks: herdr send queue full, dropping report", "event", event)
		}
	}
}

// sendLoop drains the FIFO queue in dispatch order, one request at a time.
func (h *HerdrHandler) sendLoop() {
	for req := range h.sendCh {
		h.send(req)
	}
}

func (h *HerdrHandler) buildRequest(ea eventAction, sessionID string) map[string]any {
	id := fmt.Sprintf("tachi:%d:%06d", time.Now().UnixMilli(), rand.Intn(1_000_000))
	seq := time.Now().UnixNano()

	params := map[string]any{
		"pane_id": h.paneID,
		"source":  h.source,
		"agent":   h.agent,
		"seq":     seq,
	}

	var method string
	switch ea.action {
	case actionSession:
		method = "pane.report_agent_session"
		params["agent_session_id"] = sessionID
		params["session_start_source"] = "startup"
	case actionRelease:
		method = "pane.release_agent"
	case actionState:
		method = "pane.report_agent"
		params["state"] = ea.state
		if sessionID != "" {
			params["agent_session_id"] = sessionID
		}
	}

	return map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	}
}

func (h *HerdrHandler) send(req map[string]any) {
	conn, err := net.DialTimeout("unix", h.sockPath, 500*time.Millisecond)
	if err != nil {
		// Herdr not running; silent
		if h.logger != nil {
			h.logger.Debug(context.Background(), "Hooks: herdr socket dial failed", "method", req["method"], "error", err)
		}
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		if h.logger != nil {
			h.logger.Debug(context.Background(), "Hooks: herdr socket send failed", "method", req["method"], "error", err)
		}
	}
}
