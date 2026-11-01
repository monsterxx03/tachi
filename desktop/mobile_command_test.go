package main

import (
	"net/http"
	"strings"
	"testing"
)

// A message that starts with "/" is a COMMAND, not something to say to the model: these are the
// decisions that make /sh reachable from the phone at all (issue #87). Before this the text went to
// the model as prose — the reader typed "运行一下 ls" and got a paragraph about it.
//
// The run itself is covered end to end by the mobile-api smoke scenario (it drives the real HTTP
// surface and the real socket, and asserts the card carries autoExpand). What lives here is the
// routing table, which is cheap to pin and easy to get wrong in the other direction (a plain message
// accidentally treated as a command).
func TestMobileSlashCommandRouting(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Unknown name: the phone shows 「没有这个命令」 rather than quietly sending it to the model.
	if code, got := d.mobileSend(s.ID, "/nope", "send", nil); code != http.StatusBadRequest || got != "unknown_command" {
		t.Fatalf("unknown command = (%d,%s), want (400,unknown_command)", code, got)
	}
	// A one-off command's output goes to the desktop's side panel, which the phone does not have.
	// Refusing by name beats running half of it and leaving an anchor line with nothing behind.
	for _, text := range []string{"/review", "/commit"} {
		if code, got := d.mobileSend(s.ID, text, "send", nil); code != http.StatusConflict || got != "command_desktop_only" {
			t.Fatalf("%s = (%d,%s), want (409,command_desktop_only)", text, code, got)
		}
	}
	// A command takes no @-references: dropping the attachment silently would lose the file.
	if code, got := d.mobileSend(s.ID, "/sh ls", "send", []string{"a.png"}); code != http.StatusBadRequest || got != "bad_request" {
		t.Fatalf("command + attachments = (%d,%s), want (400,bad_request)", code, got)
	}
	// steer addresses a turn in flight; a command starts one, so it is never steer text.
	if code, got := d.mobileSend(s.ID, "/sh ls", "steer", nil); code != http.StatusConflict || got != "not_running" {
		t.Fatalf("command as steer = (%d,%s), want (409,not_running)", code, got)
	}

	// A known command reaches the dispatcher. This sandbox has no model provider, so the run cannot
	// start — and THAT is the assertion: the answer is the command path's refusal, not the message
	// path's. If the "/" routing ever regressed, this would answer like a plain send.
	if code, got := d.mobileSend(s.ID, "/sh echo hi", "send", nil); code != http.StatusConflict || got != "command_refused" {
		t.Fatalf("known command = (%d,%s), want (409,command_refused)", code, got)
	}

	// …and a plain message still goes down the message path (here: no provider either, but the code
	// must not be one of the command refusals).
	if _, got := d.mobileSend(s.ID, "hello", "send", nil); strings.Contains(got, "command") || got == "unknown_command" {
		t.Fatalf(`plain message = (%q), want the message path, not a command refusal`, got)
	}
}
