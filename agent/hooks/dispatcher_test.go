package hooks

// Why this file exists: dispatcher.go is where a user's config.yaml hook actually runs — which
// registrations are kept, callbacks before commands, the JSON on a command's stdin, a panicking
// callback, and the asynchronous case that has to outlive the context it was dispatched from.
// None of it had a test, so none of those was a behaviour anybody could break on purpose and then
// find out about: a hook that silently stops firing looks exactly like a hook that never matched.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// hookScript writes a script that can write its result to $OUT, and returns the command that runs
// it plus that path.
//
// A script rather than `sh -c '...'`: execCommand splits the command with strings.Fields, so a
// quoted argument containing a space is cut in half. Hook commands are "program plus arguments",
// and this helper keeps the tests on that side of the line.
func hookScript(t *testing.T, body string) (cmd, out string) {
	t.Helper()
	out = filepath.Join(t.TempDir(), "out")
	return hookScriptTo(t, out, body), out
}

// hookScriptTo is hookScript with the output path named by the caller, so that two handlers can
// write to one log file and their order can be read off it.
func hookScriptTo(t *testing.T, out, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hook.sh")
	script := "#!/bin/sh\nOUT=" + out + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "sh " + path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// waitForFile polls for a file an async handler is expected to write. Polling rather than sleeping:
// a fixed sleep is either slower than it needs to be or flaky on a loaded machine.
func waitForFile(t *testing.T, path string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %v", path, within)
	return ""
}

// The payload reaches every callback as the same JSON bytes, and callbacks run in the order they
// were registered — a handler that depends on another one having run first relies on exactly this.
func TestDispatchRunsCallbacksInOrderWithOnePayload(t *testing.T) {
	var order []string
	var payloads [][]byte
	d := NewDispatcher(nil)
	for _, name := range []string{"first", "second"} {
		name := name
		d.RegisterCallback(EventToolCall, name, func(_ context.Context, event string, payload []byte) {
			order = append(order, name+"/"+event)
			payloads = append(payloads, payload)
		})
	}

	d.Dispatch(context.Background(), EventToolCall, Payload{
		Event: EventToolCall, SessionID: "s1", ToolName: "bash",
	})

	if want := []string{"first/tool_call", "second/tool_call"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("callbacks ran as %v, want %v", order, want)
	}
	var got Payload
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("callback payload is not JSON: %v (%q)", err, payloads[0])
	}
	if got.SessionID != "s1" || got.ToolName != "bash" {
		t.Errorf("payload = %+v, want the fields Dispatch was given", got)
	}
	if string(payloads[0]) != string(payloads[1]) {
		t.Error("the two callbacks got different payload bytes")
	}
}

// Both handler types fire for the same event: a user's command hook and a Go callback registered
// on one event each run once.
//
// The order BETWEEN the two types is deliberately not asserted here. The package comment and
// Dispatch's doc say "callbacks run first, then commands", but Dispatch walks one list in
// registration order, and production registers the config commands before the built-in callback
// (agent/agent_configure.go:204, then :220) — so what actually happens is the reverse. Which side
// is right is a behaviour decision, not something this test gets to settle.
func TestBothHandlerTypesFire(t *testing.T) {
	cmd, out := hookScript(t, `printf 'command\n' >> "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventTurnStart, Handler{Command: cmd})
	called := false
	d.RegisterCallback(EventTurnStart, "cb", func(context.Context, string, []byte) { called = true })

	d.Dispatch(context.Background(), EventTurnStart, Payload{Event: EventTurnStart})

	if !called {
		t.Fatal("the callback did not run for an event it was registered on")
	}
	if got, want := readFile(t, out), "command\n"; got != want {
		t.Fatalf("the command handler wrote %q, want %q", got, want)
	}
}

// Two synchronous commands for one event run in registration order — a config.yaml list is ordered,
// and the second script may depend on what the first one did.
func TestSyncCommandsKeepRegistrationOrder(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	first := hookScriptTo(t, log, `printf 'first\n' >> "$OUT"`)
	second := hookScriptTo(t, log, `printf 'second\n' >> "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventTurnComplete, Handler{Command: first})
	d.RegisterCommand(EventTurnComplete, Handler{Command: second})

	d.Dispatch(context.Background(), EventTurnComplete, Payload{Event: EventTurnComplete})

	if got, want := readFile(t, log), "first\nsecond\n"; got != want {
		t.Fatalf("commands ran as %q, want %q", got, want)
	}
}

// A callback is the agent's own Go code, and a hook must never be able to take the agent loop down
// with it — nor to stop the handlers behind it.
func TestCallbackPanicIsContained(t *testing.T) {
	d := NewDispatcher(nil)
	d.RegisterCallback(EventError, "boom", func(context.Context, string, []byte) {
		panic("boom")
	})
	ran := false
	d.RegisterCallback(EventError, "after", func(context.Context, string, []byte) { ran = true })

	d.Dispatch(context.Background(), EventError, Payload{Event: EventError}) // must not panic

	if !ran {
		t.Fatal("a panicking callback stopped the callbacks registered after it")
	}
}

// An empty registration is not a handler. It must not appear in Events (which is what decides
// whether an event is subscribed at all) and must not be dispatched.
func TestEmptyRegistrationsAreIgnored(t *testing.T) {
	d := NewDispatcher(nil)
	d.RegisterCallback(EventTurnStart, "nil-fn", nil)
	d.RegisterCommand(EventTurnStart, Handler{Name: "no-command"})

	if got := d.Events(); len(got) != 0 {
		t.Fatalf("Events() = %v, want none", got)
	}
	d.Dispatch(context.Background(), EventTurnStart, Payload{Event: EventTurnStart}) // must not panic
}

// A command hook with no timeout in config.yaml still gets one: an external script that hangs must
// not hang the dispatch, and a timeout that WAS configured must survive registration.
func TestCommandTimeoutDefaults(t *testing.T) {
	d := NewDispatcher(nil)
	d.RegisterCommand(EventTurnStart, Handler{Command: "true"})
	d.RegisterCommand(EventTurnStart, Handler{Command: "true", Timeout: time.Minute})

	d.mu.RLock()
	handlers := d.handlers[EventTurnStart]
	d.mu.RUnlock()

	if len(handlers) != 2 {
		t.Fatalf("registered %d handlers, want 2", len(handlers))
	}
	if handlers[0].Timeout != 5*time.Second {
		t.Errorf("default timeout = %v, want 5s", handlers[0].Timeout)
	}
	if handlers[1].Timeout != time.Minute {
		t.Errorf("configured timeout = %v, want it left at 1m", handlers[1].Timeout)
	}
}

// The payload arrives on stdin as JSON: that is the only interface a user's script has for
// "which tool", "did it fail", "which session".
func TestCommandReceivesPayloadOnStdin(t *testing.T) {
	cmd, out := hookScript(t, `cat > "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventToolResult, Handler{Command: cmd})

	d.Dispatch(context.Background(), EventToolResult, Payload{
		Event: EventToolResult, SessionID: "s2", ToolName: "read_file", IsError: true,
	})

	var got Payload
	if err := json.Unmarshal([]byte(readFile(t, out)), &got); err != nil {
		t.Fatalf("stdin was not the payload JSON: %v", err)
	}
	if got.SessionID != "s2" || got.ToolName != "read_file" || !got.IsError {
		t.Errorf("payload on stdin = %+v, want the dispatched fields", got)
	}
}

// Two things a command hook is configured with, both expanded before exec: the template variables
// in the command string, and the extra environment.
func TestCommandTemplateVarsAndEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$MARK\" > \"$OUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(nil)
	d.RegisterCommand(EventSessionStart, Handler{
		// {{WORKSPACE_DIR}} is expanded in the COMMAND, so the expanded path has to be runnable.
		Command: "{{WORKSPACE_DIR}}/hook.sh",
		Env:     map[string]string{"MARK": "hello", "OUT": out},
	})

	d.Dispatch(context.Background(), EventSessionStart, Payload{Event: EventSessionStart, WorkspaceDir: dir})

	if got, want := readFile(t, out), "hello"; got != want {
		t.Fatalf("script output = %q, want %q", got, want)
	}
}

// A command that fails is logged, not propagated: the handlers after it still run, and Dispatch
// still returns normally.
func TestFailingCommandDoesNotStopTheDispatch(t *testing.T) {
	failing, _ := hookScript(t, `exit 3`)
	good, out := hookScript(t, `printf ok > "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventTurnComplete, Handler{Command: failing})
	d.RegisterCommand(EventTurnComplete, Handler{Command: good})

	d.Dispatch(context.Background(), EventTurnComplete, Payload{Event: EventTurnComplete})

	if got, want := readFile(t, out), "ok"; got != want {
		t.Fatalf("the handler after a failing one wrote %q, want %q", got, want)
	}
}

// An async command is detached from the dispatch context on purpose (the docs say Dispatch returns
// immediately and does not wait). Tying it to that context would kill a notify script the moment
// the turn it reported on was cancelled — so the test cancels the context right after Dispatch and
// expects the script to finish anyway.
func TestAsyncCommandOutlivesTheDispatchContext(t *testing.T) {
	cmd, out := hookScript(t, `sleep 0.5; printf done > "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventSessionEnd, Handler{Command: cmd, Async: true})

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	d.Dispatch(ctx, EventSessionEnd, Payload{Event: EventSessionEnd})
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Errorf("Dispatch took %v with an async handler, want it to return without waiting", took)
	}
	cancel()

	if got, want := waitForFile(t, out, 5*time.Second), "done"; got != want {
		t.Fatalf("async command wrote %q, want %q — it was tied to the dispatch context", got, want)
	}
}

// Events is the subscription list the agent consults to decide whether an event is worth building
// a payload for, so it has to name exactly the events that have a handler — and list each once,
// however many handlers they have between them.
func TestEventsListsExactlyWhatWasRegistered(t *testing.T) {
	d := NewDispatcher(nil)
	d.RegisterCallback(EventTurnStart, "cb", func(context.Context, string, []byte) {})
	d.RegisterCommand(EventTurnStart, Handler{Command: "true"})
	d.RegisterCommand(EventToolCall, Handler{Command: "true"})

	got := d.Events()
	sort.Strings(got)
	if want := []string{EventToolCall, EventTurnStart}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Events() = %v, want %v", got, want)
	}
}

// Event names are exact keys: a config.yaml typo ("Tool_Call") can only be silent if this holds.
func TestDispatchMatchesTheEventNameExactly(t *testing.T) {
	called := false
	d := NewDispatcher(nil)
	d.RegisterCallback(EventToolCall, "cb", func(context.Context, string, []byte) { called = true })

	for _, name := range []string{"Tool_Call", "tool", "tool_call ", EventToolResult} {
		d.Dispatch(context.Background(), name, Payload{Event: name})
	}

	if called {
		t.Error("a near-miss event name reached the handler for " + EventToolCall)
	}
}

// Close drops every handler and leaves a dispatcher that can still be dispatched to (a late event
// during shutdown must not panic; the map is nil afterwards).
func TestDispatchAfterCloseDoesNothing(t *testing.T) {
	cmd, out := hookScript(t, `printf ran > "$OUT"`)
	d := NewDispatcher(nil)
	d.RegisterCommand(EventTurnStart, Handler{Command: cmd})

	d.Close()
	d.Dispatch(context.Background(), EventTurnStart, Payload{Event: EventTurnStart})

	if _, err := os.Stat(out); err == nil {
		t.Fatal("a handler ran after Close()")
	}
}
