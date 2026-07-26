package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"
)

// HerdrHandler reports Tachi lifecycle events to a local Herdr server via its
// Unix domain socket API. It is registered as a Callback handler on the
// Dispatcher when HERDR_ENV=1 is detected at startup.
//
// The handler connects to the Herdr socket, sends a JSON-RPC-style request,
// and closes the connection. It does not wait for a response (fire-and-forget).
type HerdrHandler struct {
	sockPath string // HERDR_SOCKET_PATH
	paneID   string // HERDR_PANE_ID
	source   string // "herdr:tachi"
	agent    string // "tachi"
}

// DetectHerdr reports whether Tachi is running inside a Herdr-managed pane.
func DetectHerdr() bool {
	return os.Getenv("HERDR_ENV") == "1" &&
		os.Getenv("HERDR_SOCKET_PATH") != "" &&
		os.Getenv("HERDR_PANE_ID") != ""
}

// NewHerdrHandler creates a handler from the current process environment.
func NewHerdrHandler() *HerdrHandler {
	return &HerdrHandler{
		sockPath: os.Getenv("HERDR_SOCKET_PATH"),
		paneID:   os.Getenv("HERDR_PANE_ID"),
		source:   "herdr:tachi",
		agent:    "tachi",
	}
}

// herdrAction distinguishes between session identity, lifecycle state reports,
// and agent release.
type herdrAction string

const (
	actionSession = herdrAction("session")  // → pane.report_agent_session
	actionState   = herdrAction("state")    // → pane.report_agent
	actionRelease = herdrAction("release")  // → pane.release_agent
)

// eventAction maps a Tachi hook event to a Herdr socket API call.
type eventAction struct {
	action herdrAction
	state  string // "working" / "blocked" / "idle"; empty for session actions
}

// EventActions maps Tachi events to Herdr actions. Exported so the agent can
// iterate over them when registering handlers.
var EventActions = map[string]eventAction{
	"session_start":      {action: actionSession},
	"session_end":        {action: actionRelease},
	"turn_complete":      {action: actionState, state: "idle"},
	"turn_truncated":     {action: actionState, state: "working"},
	"tool_call":          {action: actionState, state: "working"},
	"permission_request": {action: actionState, state: "blocked"},
	"ask_user_question":  {action: actionState, state: "blocked"},
	"error":              {action: actionState, state: "idle"},
}

// Handle implements the callback signature for hooks.Dispatcher.
// It receives the event name and raw JSON payload from the dispatcher,
// and sends the appropriate Herdr socket API request.
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

	// session_end is dispatched synchronously so the process doesn't exit
	// before the Herdr socket write completes. All other events are
	// fire-and-forget (best-effort, no ordering guarantees).
	if event == "session_end" {
		h.send(req)
	} else {
		go h.send(req)
	}
}

func (h *HerdrHandler) buildRequest(ea eventAction, sessionID string) map[string]interface{} {
	id := fmt.Sprintf("tachi:%d:%06d", time.Now().UnixMilli(), rand.Intn(1_000_000))
	seq := time.Now().UnixNano()

	params := map[string]interface{}{
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

	return map[string]interface{}{
		"id":     id,
		"method": method,
		"params": params,
	}
}

func (h *HerdrHandler) send(req map[string]interface{}) {
	conn, err := net.DialTimeout("unix", h.sockPath, 500*time.Millisecond)
	if err != nil {
		return // Herdr not running; silent
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_ = json.NewEncoder(conn).Encode(req)
	// No response read — fire-and-forget
}
