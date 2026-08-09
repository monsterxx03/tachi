package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/agent/acpctx"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/stretchr/testify/require"
)

// fakeTerminalConn records every terminal-API call and lets tests control the
// wait/output behavior, standing in for *acp.AgentSideConnection.
type fakeTerminalConn struct {
	mu       sync.Mutex
	created  []acp.CreateTerminalRequest
	killed   []acp.KillTerminalRequest
	released []acp.ReleaseTerminalRequest
	outputs  []acp.TerminalOutputRequest
	waits    []acp.WaitForTerminalExitRequest
	updates  []acp.SessionNotification

	createErr error
	waitErr   error
	output    string
	truncated bool
	exitCode  *int
	signal    *string
	// blockWait, when non-nil, makes WaitForTerminalExit block until the
	// channel is closed, the terminal is killed, or ctx is done — simulating
	// a still-running command that the real protocol would unblock on kill.
	blockWait chan struct{}

	unblockCh   chan struct{}
	unblockOnce sync.Once
}

// unblockWait releases any blocked WaitForTerminalExit. Called by
// KillTerminal to mirror the real protocol, where killing the command makes
// wait_for_exit return.
func (f *fakeTerminalConn) unblockWait() {
	f.mu.Lock()
	if f.unblockCh == nil {
		f.unblockCh = make(chan struct{})
	}
	ch := f.unblockCh
	f.mu.Unlock()
	f.unblockOnce.Do(func() { close(ch) })
}

func (f *fakeTerminalConn) CreateTerminal(_ context.Context, req acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if f.createErr != nil {
		return acp.CreateTerminalResponse{}, f.createErr
	}
	return acp.CreateTerminalResponse{TerminalId: "term_test"}, nil
}

func (f *fakeTerminalConn) KillTerminal(_ context.Context, req acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	f.mu.Lock()
	f.killed = append(f.killed, req)
	f.mu.Unlock()
	// Real clients respond to wait_for_exit once the command is killed.
	f.unblockWait()
	return acp.KillTerminalResponse{}, nil
}

func (f *fakeTerminalConn) TerminalOutput(_ context.Context, req acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs = append(f.outputs, req)
	return acp.TerminalOutputResponse{Output: f.output, Truncated: f.truncated}, nil
}

func (f *fakeTerminalConn) ReleaseTerminal(_ context.Context, req acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, req)
	return acp.ReleaseTerminalResponse{}, nil
}

func (f *fakeTerminalConn) WaitForTerminalExit(ctx context.Context, req acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	f.mu.Lock()
	f.waits = append(f.waits, req)
	block := f.blockWait
	unblock := f.unblockCh
	waitErr := f.waitErr
	exitCode := f.exitCode
	signal := f.signal
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-unblock:
		case <-ctx.Done():
			return acp.WaitForTerminalExitResponse{}, ctx.Err()
		}
	}
	if waitErr != nil {
		return acp.WaitForTerminalExitResponse{}, waitErr
	}
	return acp.WaitForTerminalExitResponse{ExitCode: exitCode, Signal: signal}, nil
}

func (f *fakeTerminalConn) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, n)
	return nil
}

func newTerminalTestCtx() context.Context {
	ctx := context.Background()
	ctx = acpctx.WithSessionID(ctx, acp.SessionId("sess_test"))
	ctx = wdctx.WithDir(ctx, "/abs/work")
	return ctx
}

func parseBashResult(t *testing.T, raw string) BashResult {
	t.Helper()
	var r BashResult
	require.NoError(t, json.Unmarshal([]byte(raw), &r))
	return r
}

// terminalTool returns a BashTool with ACP mode enabled and a short foreground
// window (so timeout tests run fast).
func terminalTool() *BashTool {
	tool := NewBashTool(BashToolConfig{})
	tool.SetACPMode(true)
	tool.foregroundWindow = 200 * time.Millisecond
	return tool
}

func TestBashTool_ACPTerminal_Success(t *testing.T) {
	ctx := WithToolID(newTerminalTestCtx(), "call_1")
	fake := &fakeTerminalConn{exitCode: acp.Ptr(0), output: "hello from client\n"}
	tool := terminalTool()

	out, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "echo hello"})
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.Equal(t, "hello from client\n", result.Stdout)
	require.Equal(t, 0, result.ExitCode)
	require.False(t, result.Interrupted)

	// terminal/create carries bash -c <command>, the absolute session cwd and
	// the same 1MB output cap as local execution.
	require.Len(t, fake.created, 1)
	create := fake.created[0]
	require.Equal(t, "bash", create.Command)
	require.Equal(t, []string{"-c", "echo hello"}, create.Args)
	require.NotNil(t, create.Cwd)
	require.Equal(t, "/abs/work", *create.Cwd)
	require.NotNil(t, create.OutputByteLimit)
	require.Equal(t, maxOutputSize, *create.OutputByteLimit)
	require.Equal(t, acp.SessionId("sess_test"), create.SessionId)

	// The terminal is embedded in the tool call via a tool_call_update.
	require.Len(t, fake.updates, 1)
	upd := fake.updates[0]
	require.Equal(t, acp.SessionId("sess_test"), upd.SessionId)
	require.NotNil(t, upd.Update.ToolCallUpdate)
	require.Equal(t, acp.ToolCallId("call_1"), upd.Update.ToolCallUpdate.ToolCallId)
	require.Len(t, upd.Update.ToolCallUpdate.Content, 1)
	require.NotNil(t, upd.Update.ToolCallUpdate.Content[0].Terminal)
	require.Equal(t, "term_test", upd.Update.ToolCallUpdate.Content[0].Terminal.TerminalId)

	// final output fetch + exactly one release
	require.Len(t, fake.outputs, 1)
	require.Len(t, fake.released, 1)
	require.Equal(t, "term_test", fake.released[0].TerminalId)

	// wait_for_exit targets the right terminal in the right session
	require.Len(t, fake.waits, 1)
	require.Equal(t, acp.SessionId("sess_test"), fake.waits[0].SessionId)
	require.Equal(t, "term_test", fake.waits[0].TerminalId)
}

func TestBashTool_ACPTerminal_TimeoutKills(t *testing.T) {
	ctx := newTerminalTestCtx()
	fake := &fakeTerminalConn{blockWait: make(chan struct{}), output: "partial\n"}
	tool := NewBashTool(BashToolConfig{})
	tool.SetACPMode(true)
	tool.foregroundWindow = 50 * time.Millisecond

	out, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "sleep 100"})
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.True(t, result.Interrupted)
	require.Equal(t, -1, result.ExitCode)
	require.Contains(t, result.Stderr, "killed after")
	require.Equal(t, "partial\n", result.Stdout)

	require.Len(t, fake.killed, 1)
	require.Equal(t, acp.SessionId("sess_test"), fake.killed[0].SessionId)
	require.Equal(t, "term_test", fake.killed[0].TerminalId)
	require.Len(t, fake.released, 1)
}

func TestBashTool_ACPTerminal_CancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(newTerminalTestCtx())
	fake := &fakeTerminalConn{blockWait: make(chan struct{}), output: "partial\n"}
	tool := terminalTool()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	out, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "sleep 100"})
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.True(t, result.Interrupted)
	require.Contains(t, result.Stderr, "interrupted")

	require.Len(t, fake.killed, 1)
	require.Equal(t, acp.SessionId("sess_test"), fake.killed[0].SessionId)
	require.Equal(t, "term_test", fake.killed[0].TerminalId)
	require.Len(t, fake.released, 1)
}

// TestBashTool_ACPTerminal_TimeoutWidensWindow verifies B2: in ACP mode the
// timeout is a hard kill deadline that may WIDEN the default window, so an
// explicit timeout lets long-running commands survive past the 15s (here:
// 50ms) default.
func TestBashTool_ACPTerminal_TimeoutWidensWindow(t *testing.T) {
	ctx := newTerminalTestCtx()
	fake := &fakeTerminalConn{blockWait: make(chan struct{})}
	fake.exitCode = acp.Ptr(0)
	fake.output = "build done\n"
	tool := NewBashTool(BashToolConfig{})
	tool.SetACPMode(true)
	tool.foregroundWindow = 50 * time.Millisecond // would kill without the explicit timeout

	// Unblock the wait after the default window would have fired — the
	// command must NOT have been killed.
	go func() {
		time.Sleep(150 * time.Millisecond)
		fake.mu.Lock()
		close(fake.blockWait)
		fake.mu.Unlock()
	}()

	out, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "sleep 1", Timeout: acp.Ptr(300000)})
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.False(t, result.Interrupted)
	require.Equal(t, 0, result.ExitCode)
	require.Len(t, fake.killed, 0) // never killed
}

// TestBashTool_ACPTerminal_RejectsBackgroundParams verifies W3: background
// management params are rejected explicitly in ACP mode instead of being
// silently ignored.
func TestBashTool_ACPTerminal_RejectsBackgroundParams(t *testing.T) {
	tool := terminalTool()

	for _, args := range []string{
		`{"command": "echo hi", "background": true, "bg_name": "x"}`,
		`{"command": "echo hi", "list_bg": true}`,
		`{"command": "echo hi", "stop_name": "x"}`,
	} {
		_, err := tool.ExecuteContext(context.Background(), args)
		require.ErrorContains(t, err, "background process management is not supported in ACP terminal mode")
	}
}

func TestBashTool_ACPTerminal_WaitError(t *testing.T) {
	ctx := newTerminalTestCtx()
	fake := &fakeTerminalConn{waitErr: errors.New("method not found")}
	tool := terminalTool()

	_, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "ls"})
	require.ErrorContains(t, err, "wait_for_exit")

	// the client-side command must not be left running
	require.Len(t, fake.killed, 1)
	require.Len(t, fake.released, 1)
}

func TestBashTool_ACPTerminal_SignalTerminated(t *testing.T) {
	ctx := newTerminalTestCtx()
	sig := "SIGKILL"
	fake := &fakeTerminalConn{signal: &sig, output: "out\n"}
	tool := terminalTool()

	out, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "kill -9 $$"})
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.Equal(t, -1, result.ExitCode)
	require.Contains(t, result.Stderr, "SIGKILL")
	require.Len(t, fake.released, 1)
}

func TestBashTool_ACPTerminal_RelativeCwdResolved(t *testing.T) {
	ctx := wdctx.WithDir(newTerminalTestCtx(), "rel")
	fake := &fakeTerminalConn{exitCode: acp.Ptr(0)}
	tool := terminalTool()

	_, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "pwd"})
	require.NoError(t, err)
	require.Len(t, fake.created, 1)
	require.NotNil(t, fake.created[0].Cwd)
	require.True(t, filepath.IsAbs(*fake.created[0].Cwd))
}

func TestBashTool_ACPTerminal_NoToolIDSkipsEmbed(t *testing.T) {
	ctx := newTerminalTestCtx() // no WithToolID
	fake := &fakeTerminalConn{exitCode: acp.Ptr(0)}
	tool := terminalTool()

	_, err := tool.executeTerminal(ctx, fake, &bashArgs{Command: "echo hi"})
	require.NoError(t, err)
	require.Len(t, fake.updates, 0)
	require.Len(t, fake.released, 1)
}

func TestBashTool_ACPTerminal_NoCommandRejected(t *testing.T) {
	ctx := newTerminalTestCtx()
	fake := &fakeTerminalConn{}
	tool := terminalTool()

	_, err := tool.executeTerminal(ctx, fake, &bashArgs{})
	require.ErrorContains(t, err, "command is required")
	require.Len(t, fake.created, 0) // nothing reached the client
}

// TestBashTool_ACPModeWithoutConnFallsBackLocal verifies the defensive path:
// acpMode set but no connection in ctx → local execution (never a nil-pointer
// or a call into a missing client).
func TestBashTool_ACPModeWithoutConnFallsBackLocal(t *testing.T) {
	tool := terminalTool()
	out, err := tool.ExecuteContext(context.Background(), `{"command": "echo local"}`)
	require.NoError(t, err)
	result := parseBashResult(t, out)
	require.Contains(t, result.Stdout, "local")
}

func TestBashTool_ACPModeProperties(t *testing.T) {
	tool := NewBashTool(BashToolConfig{})
	// local mode keeps the background management params
	require.Contains(t, tool.Properties(), "background")
	require.Contains(t, tool.Properties(), "bg_name")
	require.Contains(t, tool.Properties(), "stop_name")
	require.Contains(t, tool.Properties(), "list_bg")

	tool.SetACPMode(true)
	props := tool.Properties()
	require.NotContains(t, props, "background")
	require.NotContains(t, props, "bg_name")
	require.NotContains(t, props, "stop_name")
	require.NotContains(t, props, "list_bg")
	require.Contains(t, props, "command")
	require.Contains(t, props, "timeout")
	// ACP mode timeout is a hard kill — no background mention.
	require.Contains(t, props["timeout"].Description, "killed")
}
