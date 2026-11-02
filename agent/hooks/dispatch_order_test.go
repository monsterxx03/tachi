package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Callbacks run BEFORE commands, whatever order they were registered in.
//
// That is the ordering the package comment and Dispatch's godoc have always promised, and it is the
// one production needs: agent_configure.go registers the user's config commands first and the
// built-in Herdr callback second, so with a single registration-order pass a slow synchronous hook
// script (async: false, 5s default budget) delayed the built-in bookkeeping by that much. The
// issue this pins was exactly that gap between the promise and the code.
//
// Registration order still decides WITHIN a kind — the two callbacks below fire in the order they
// were added, and so would two commands.
func TestCallbacksRunBeforeCommandsWhateverTheRegistrationOrder(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "order.log")
	script := filepath.Join(dir, "hook.sh")
	// The command appends its own name, the callback appends its own: one file, one order.
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\necho command >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	appendLine := func(line string) func(context.Context, string, []byte) {
		return func(context.Context, string, []byte) {
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				t.Errorf("open log: %v", err)
				return
			}
			defer f.Close()
			_, _ = f.WriteString(line + "\n")
		}
	}

	d := NewDispatcher(nil)
	// Registered in the order production uses: the command first, the callback second.
	d.RegisterCommand("tool_call", Handler{Name: "user-script", Command: script, Async: false})
	d.RegisterCallback("tool_call", "builtin", appendLine("callback"))

	d.Dispatch(context.Background(), "tool_call", Payload{Event: "tool_call", ToolName: "bash"})

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read order log: %v", err)
	}
	if want := "callback\ncommand\n"; string(got) != want {
		t.Fatalf("order = %q, want %q (callbacks first, then commands)", string(got), want)
	}
}
