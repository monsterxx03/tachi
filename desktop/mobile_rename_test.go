package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMobileRename: a phone's 重命名 goes through the window's own writer, so the two screens cannot
// end up naming one conversation two ways — and the broadcast is how the OTHER screen finds out.
func TestMobileRename(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	m, ts := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	code, out := mobileCall(t, ts, "POST", "/v1/sessions/"+s.ID+"/title", "secret", map[string]any{"title": "  新的名字  "})
	if code != http.StatusOK {
		t.Fatalf("rename = %d %v, want 200", code, out)
	}
	if got := d.sessionRecord(s.ID).Title; got != "新的名字" {
		t.Errorf("title = %q, want 新的名字 (trimmed)", got)
	}
	// A row's title is its identity in the list, so this is the list's event.
	select {
	case b := <-c.out:
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		if ev["type"] != mobileEvSessionsChanged {
			t.Fatalf("event = %v, want sessions_changed", ev)
		}
	default:
		t.Fatal("a rename did not reach the other screens")
	}
}

// TestMobileRenameRefusals: an empty title and an unknown conversation each keep their own answer.
func TestMobileRenameRefusals(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	if code, out := mobileCall(t, ts, "POST", "/v1/sessions/"+s.ID+"/title", "secret", map[string]any{"title": "   "}); code != http.StatusBadRequest || out["error"] != "bad_request" {
		t.Errorf("empty title = %d %v, want 400 bad_request", code, out)
	}
	if code, out := mobileCall(t, ts, "POST", "/v1/sessions/nope/title", "secret", map[string]any{"title": "x"}); code != http.StatusNotFound || out["error"] != "no_session" {
		t.Errorf("unknown session = %d %v, want 404 no_session", code, out)
	}
}
