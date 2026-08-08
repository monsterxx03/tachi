//go:build integration

// Package acp provides the itest/acp driver: it launches the REAL tachi
// binary in ACP mode and drives it through the REAL ACP client SDK
// (acp-go-sdk ClientSideConnection) — exercising the true process boundary,
// the newline-delimited JSON-RPC wire format, and the bidirectional
// client↔agent callbacks (SessionUpdate notifications, RequestPermission).
//
// The client implementation records every SessionUpdate and answers
// permission requests per a configurable policy (allow / reject / cancel),
// so scenarios can assert on the streamed event sequence like they would in
// an editor.
package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
)

// EventRecorder collects SessionUpdate notifications received by the client.
// Prompt is synchronous (the agent streams updates during the turn and only
// then responds), so after Prompt returns every notification is recorded.
type EventRecorder struct {
	mu     sync.Mutex
	events []acpapi.SessionNotification
}

func (r *EventRecorder) add(n acpapi.SessionNotification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, n)
}

// All returns a copy of all recorded notifications, in arrival order.
func (r *EventRecorder) All() []acpapi.SessionNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]acpapi.SessionNotification, len(r.events))
	copy(out, r.events)
	return out
}

// ForSession returns the notifications for one session.
func (r *EventRecorder) ForSession(id acpapi.SessionId) []acpapi.SessionNotification {
	var out []acpapi.SessionNotification
	for _, n := range r.All() {
		if n.SessionId == id {
			out = append(out, n)
		}
	}
	return out
}

// Text concatenates all AgentMessageChunk text deltas across sessions —
// the streamed assistant output.
func (r *EventRecorder) Text() string {
	var sb strings.Builder
	for _, n := range r.All() {
		if u := n.Update.AgentMessageChunk; u != nil {
			if t := u.Content.Text; t != nil {
				sb.WriteString(t.Text)
			}
		}
	}
	return sb.String()
}

// Thoughts concatenates all AgentThoughtChunk text deltas.
func (r *EventRecorder) Thoughts() string {
	var sb strings.Builder
	for _, n := range r.All() {
		if u := n.Update.AgentThoughtChunk; u != nil {
			if t := u.Content.Text; t != nil {
				sb.WriteString(t.Text)
			}
		}
	}
	return sb.String()
}

// ToolCallIDs returns the tool call IDs announced via StartToolCall updates.
func (r *EventRecorder) ToolCallIDs() []acpapi.ToolCallId {
	var out []acpapi.ToolCallId
	for _, n := range r.All() {
		if u := n.Update.ToolCall; u != nil {
			out = append(out, u.ToolCallId)
		}
	}
	return out
}

// PermissionPolicy controls how the client answers RequestPermission.
type PermissionPolicy int

const (
	// PermissionAllowAll selects the first option (Zed's allow-once) —
	// the default for scenarios that must let tools execute.
	PermissionAllowAll PermissionPolicy = iota
	// PermissionReject rejects every permission request.
	PermissionReject
	// PermissionCancel cancels every permission request.
	PermissionCancel
)

// recordingClient implements the acpapi.Client interface. Only the two
// callbacks the agent actually uses are implemented meaningfully; the fs /
// terminal methods return "unsupported" because the test client advertises
// no such capabilities.
type recordingClient struct {
	rec  *EventRecorder
	perm PermissionPolicy

	mu         sync.Mutex
	permission []acpapi.RequestPermissionRequest
}

func (c *recordingClient) SessionUpdate(_ context.Context, n acpapi.SessionNotification) error {
	c.rec.add(n)
	return nil
}

func (c *recordingClient) RequestPermission(_ context.Context, req acpapi.RequestPermissionRequest) (acpapi.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permission = append(c.permission, req)
	c.mu.Unlock()

	selectOption := func(id string) (acpapi.RequestPermissionResponse, bool) {
		for _, opt := range req.Options {
			if string(opt.OptionId) == id {
				return acpapi.RequestPermissionResponse{
					Outcome: acpapi.RequestPermissionOutcome{
						Selected: &acpapi.RequestPermissionOutcomeSelected{OptionId: opt.OptionId},
					},
				}, true
			}
		}
		return acpapi.RequestPermissionResponse{}, false
	}
	cancelled := func() acpapi.RequestPermissionResponse {
		return acpapi.RequestPermissionResponse{
			Outcome: acpapi.RequestPermissionOutcome{Cancelled: &acpapi.RequestPermissionOutcomeCancelled{}},
		}
	}

	switch c.perm {
	case PermissionReject:
		// The agent offers a "reject" option (permission.go); selecting it
		// denies this one call. Fall back to cancelled if absent.
		if resp, ok := selectOption("reject"); ok {
			return resp, nil
		}
		return cancelled(), nil
	case PermissionCancel:
		return cancelled(), nil
	default: // PermissionAllowAll: pick "allow" (the agent's first option)
		if resp, ok := selectOption("allow"); ok {
			return resp, nil
		}
		if len(req.Options) == 0 {
			return cancelled(), nil
		}
		return acpapi.RequestPermissionResponse{
			Outcome: acpapi.RequestPermissionOutcome{
				Selected: &acpapi.RequestPermissionOutcomeSelected{OptionId: req.Options[0].OptionId},
			},
		}, nil
	}
}

// PermissionRequests returns the permission requests received so far.
func (c *recordingClient) PermissionRequests() []acpapi.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]acpapi.RequestPermissionRequest, len(c.permission))
	copy(out, c.permission)
	return out
}

func unsupported(name string) error {
	return fmt.Errorf("unsupported: %s capability not advertised", name)
}

func (c *recordingClient) ReadTextFile(context.Context, acpapi.ReadTextFileRequest) (acpapi.ReadTextFileResponse, error) {
	return acpapi.ReadTextFileResponse{}, unsupported("readTextFile")
}
func (c *recordingClient) WriteTextFile(context.Context, acpapi.WriteTextFileRequest) (acpapi.WriteTextFileResponse, error) {
	return acpapi.WriteTextFileResponse{}, unsupported("writeTextFile")
}
func (c *recordingClient) CreateTerminal(context.Context, acpapi.CreateTerminalRequest) (acpapi.CreateTerminalResponse, error) {
	return acpapi.CreateTerminalResponse{}, unsupported("terminal")
}
func (c *recordingClient) KillTerminal(context.Context, acpapi.KillTerminalRequest) (acpapi.KillTerminalResponse, error) {
	return acpapi.KillTerminalResponse{}, unsupported("terminal")
}
func (c *recordingClient) TerminalOutput(context.Context, acpapi.TerminalOutputRequest) (acpapi.TerminalOutputResponse, error) {
	return acpapi.TerminalOutputResponse{}, unsupported("terminal")
}
func (c *recordingClient) ReleaseTerminal(context.Context, acpapi.ReleaseTerminalRequest) (acpapi.ReleaseTerminalResponse, error) {
	return acpapi.ReleaseTerminalResponse{}, unsupported("terminal")
}
func (c *recordingClient) WaitForTerminalExit(context.Context, acpapi.WaitForTerminalExitRequest) (acpapi.WaitForTerminalExitResponse, error) {
	return acpapi.WaitForTerminalExitResponse{}, unsupported("terminal")
}

// WireLine records one JSON-RPC line as observed on the wire, in arrival
// order. order increments strictly with arrival; data is the raw line.
type WireLine struct {
	Order int
	Data  string
}

// wireOrderReader wraps the agent's stdout, recording every JSON-RPC line
// with its global arrival order. The receive loop reads sequentially, so the
// order numbers reflect true wire order — the property Zed's session route
// table depends on (response before session-scoped notifications).
type wireOrderReader struct {
	r     io.Reader
	mu    sync.Mutex
	buf   []byte
	lines []WireLine
	seq   int
}

func (w *wireOrderReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.mu.Lock()
		w.buf = append(w.buf, p[:n]...)
		for {
			idx := bytes.IndexByte(w.buf, '\n')
			if idx < 0 {
				break
			}
			w.seq++
			w.lines = append(w.lines, WireLine{Order: w.seq, Data: string(w.buf[:idx])})
			w.buf = w.buf[idx+1:]
		}
		w.mu.Unlock()
	}
	return n, err
}

// WireLines returns all recorded lines, oldest first.
func (w *wireOrderReader) WireLines() []WireLine {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]WireLine, len(w.lines))
	copy(out, w.lines)
	return out
}

// Client wraps the tachi acp subprocess and its client-side connection.
type Client struct {
	cmd  *exec.Cmd
	conn *acpapi.ClientSideConnection
	impl acpapi.Client

	Rec    *EventRecorder
	Stderr bytes.Buffer

	stdin io.WriteCloser
	wire  *wireOrderReader

	closeOnce sync.Once
	closeErr  error
}

// Option configures the ACP client.
type Option func(*options)

type options struct {
	perm PermissionPolicy
}

// WithPermission sets the RequestPermission policy (default AllowAll).
func WithPermission(p PermissionPolicy) Option {
	return func(o *options) { o.perm = p }
}

// Start launches `tachi acp --home home` and completes the Initialize
// handshake. The process is left running; call Close to shut it down.
func Start(bin, home string, opts ...Option) (*Client, error) {
	o := &options{perm: PermissionAllowAll}
	for _, opt := range opts {
		opt(o)
	}
	impl := &recordingClient{rec: &EventRecorder{}, perm: o.perm}
	return startWithImpl(bin, home, impl, impl.rec)
}

// StartWithImpl launches the binary like Start, but with a caller-provided
// client implementation and event recorder (e.g. a client that mimics an
// editor's notification handling, such as dropping updates for sessions
// whose session/new response has not arrived yet).
func StartWithImpl(bin, home string, impl acpapi.Client, rec *EventRecorder) (*Client, error) {
	return startWithImpl(bin, home, impl, rec)
}

func startWithImpl(bin, home string, impl acpapi.Client, rec *EventRecorder) (*Client, error) {
	cmd := exec.Command(bin, "acp", "--home", home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	c := &Client{cmd: cmd, stdin: stdin}
	cmd.Stderr = &c.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start: %w", err)
	}

	c.impl = impl
	c.Rec = rec
	c.wire = &wireOrderReader{r: stdout}
	c.conn = acpapi.NewClientSideConnection(impl, stdin, c.wire)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.conn.Initialize(ctx, acpapi.InitializeRequest{
		ProtocolVersion:    acpapi.ProtocolVersionNumber,
		ClientCapabilities: acpapi.ClientCapabilities{},
	}); err != nil {
		c.Kill()
		_ = c.cmd.Wait() // no Wait goroutine exists yet — reap to avoid a zombie
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	return c, nil
}

// WireLines returns every JSON-RPC line received from the agent, in wire
// arrival order. Used by tests that assert response-before-notification
// ordering (the property editors depend on to route session updates).
func (c *Client) WireLines() []WireLine {
	return c.wire.WireLines()
}

// Conn exposes the underlying client-side connection for protocol calls.
func (c *Client) Conn() *acpapi.ClientSideConnection { return c.conn }

// PermissionRequests returns the permission requests the agent has made.
func (c *Client) PermissionRequests() []acpapi.RequestPermissionRequest {
	if rc, ok := c.impl.(*recordingClient); ok {
		return rc.PermissionRequests()
	}
	return nil
}

// Kill terminates the subprocess WITHOUT reaping it — the single Wait call
// lives in Close's goroutine (concurrent Wait on *exec.Cmd is undefined
// behavior, see Close). Callers that never go through Close (Kill-only
// cleanup) should call cmd.Wait once themselves.
func (c *Client) Kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// Close shuts the connection down cleanly: closing stdin makes the agent's
// connection loop hit EOF and exit (runACPAgent blocks on conn.Done()).
// Idempotent — safe to call from both a test body and its DeferCleanup.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case err := <-done:
			c.closeErr = err
		case <-time.After(5 * time.Second):
			c.Kill()
			c.closeErr = errors.New("acp: agent did not exit after stdin EOF")
		}
	})
	return c.closeErr
}
