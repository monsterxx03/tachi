// Package hooks provides a general-purpose event hook system for Tachi.
//
// It supports two types of handlers:
//   - Callback: Go functions registered programmatically (used by built-in
//     integrations like Herdr)
//   - Command: external executables configured via config.yaml (user-defined
//     scripts for notifications, logging, CI triggers, etc.)
//
// Both handler types run concurrently for the same event. Callbacks execute
// first (inline), then commands are scheduled. Panics from callbacks are
// recovered; command timeouts are enforced. Neither can block the agent loop
// (async by default).
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// HandlerType identifies whether a handler is a Go callback or an external command.
type HandlerType int

const (
	// HandlerCallback is a Go function invoked directly in the Dispatch goroutine.
	HandlerCallback HandlerType = iota
	// HandlerCommand is an external command executed via os/exec.
	HandlerCommand
)

// Handler unifies both callback and command handlers under one type.
type Handler struct {
	Type HandlerType
	Name string // e.g. "herdr", "my-notify-script"

	// Callback mode
	Callback func(ctx context.Context, event string, payload []byte)

	// Command mode
	Command string
	Timeout time.Duration
	Async   bool
	Env     map[string]string
}

// Dispatcher manages event handlers and dispatches events to all registered
// handlers. It is safe for concurrent use.
type Dispatcher struct {
	mu       sync.RWMutex
	handlers map[string][]Handler // event name → handlers
	logger   *logger.Logger       // optional; nil = no logging
}

// NewDispatcher creates an empty Dispatcher. The logger is optional (nil
// disables hook-related logging).
func NewDispatcher(l *logger.Logger) *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string][]Handler),
		logger:   l,
	}
}

// RegisterCallback adds a Go function handler for the given event.
// The function receives the event name and the raw JSON payload bytes.
// It is called synchronously during Dispatch; panics are recovered.
func (d *Dispatcher) RegisterCallback(event string, name string, fn func(ctx context.Context, event string, payload []byte)) {
	if fn == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[event] = append(d.handlers[event], Handler{
		Type:     HandlerCallback,
		Name:     name,
		Callback: fn,
	})
}

// RegisterCommand adds an external command handler for the given event.
// The command receives the event payload as JSON on stdin.
func (d *Dispatcher) RegisterCommand(event string, cmd Handler) {
	if cmd.Command == "" {
		return
	}
	cmd.Type = HandlerCommand
	if cmd.Timeout <= 0 {
		cmd.Timeout = 5 * time.Second
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[event] = append(d.handlers[event], cmd)
}

// Payload is the structured data passed to handlers for each event.
type Payload struct {
	Event     string `json:"event"`
	SessionID string `json:"session_id,omitempty"`

	// Optional fields populated per event type
	UserMessage  string `json:"user_message,omitempty"`
	TurnCount    int    `json:"turn_count,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolID       string `json:"tool_id,omitempty"`
	ToolArgs     string `json:"tool_args,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	WorkspaceDir string `json:"workspace_dir,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Approved     bool   `json:"approved,omitempty"` // permission_result: true=allowed, false=denied
}

// Dispatch sends an event to all registered handlers. Callbacks run first
// (inline, with panic recovery). Commands are then scheduled according to
// their Async flag.
//
// Dispatch returns immediately; it does not wait for async command handlers.
// The ctx parameter is used for cancellation of sync command handlers.
func (d *Dispatcher) Dispatch(ctx context.Context, event string, payload Payload) {
	d.mu.RLock()
	handlers := d.handlers[event]
	d.mu.RUnlock()

	if len(handlers) == 0 {
		return
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return
	}

	for _, h := range handlers {
		switch h.Type {
		case HandlerCallback:
			d.runCallback(ctx, h, event, jsonBytes)
		case HandlerCommand:
			d.runCommand(ctx, h, jsonBytes, payload)
		}
	}
}

func (d *Dispatcher) runCallback(ctx context.Context, h Handler, event string, payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			if d.logger != nil {
				d.logger.Error(ctx, "Hooks: callback panicked", fmt.Errorf("%v", r), "name", h.Name, "event", event)
			}
		}
	}()
	h.Callback(ctx, event, payload)
}

func (d *Dispatcher) runCommand(ctx context.Context, h Handler, stdinData []byte, payload Payload) {
	if h.Async {
		go d.execCommand(context.Background(), h, stdinData, payload)
		return
	}
	d.execCommand(ctx, h, stdinData, payload)
}

func (d *Dispatcher) execCommand(ctx context.Context, h Handler, stdinData []byte, payload Payload) {
	// Expand template variables in the command string before execution.
	expanded := expandVars(h.Command, payload.SessionID, payload.WorkspaceDir)
	parts := strings.Fields(expanded)
	if len(parts) == 0 {
		return
	}

	cmdCtx := ctx
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		cmdCtx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(cmdCtx, parts[0], parts[1:]...)
	c.Stdin = bytes.NewReader(stdinData)

	// Merge extra env vars
	if len(h.Env) > 0 {
		c.Env = append(c.Environ(), mapToEnv(h.Env)...)
	}

	out, err := c.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return // context cancelled/timeout, expected
		}
		if d.logger != nil {
			d.logger.Error(ctx, "Hooks: command failed", err, "name", h.Name, "command", h.Command, "output", string(out))
		}
	}
}

// Events returns the set of event names that have at least one handler registered.
func (d *Dispatcher) Events() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	events := make([]string, 0, len(d.handlers))
	for e := range d.handlers {
		events = append(events, e)
	}
	return events
}

// Close releases all handlers. After calling Close, the dispatcher should not
// be used.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers = nil
}

// mapToEnv converts a map to "KEY=VALUE" strings suitable for os.Environ.
func mapToEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}
