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

// TestRenameLandsOnTheCopyABoundSessionReads: a session bound to a run has its OWN in-memory copy
// of the record (the one a running turn mutates), and THAT copy is what the phone's snapshot reads
// back (sessionRecord -> r.sm.Current()). Writing only the stable manager left the message page
// showing the old title while the list — which reads the store — showed the new one (issue #153).
func TestRenameLandsOnTheCopyABoundSessionReads(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// A copy the store does not share, bound as this session's run — exactly the shape of a live
	// turn's view: a copy captured before the rename.
	bound := d.newSessionManager()
	if bound == nil {
		t.Fatal("no per-session manager")
	}
	captured := *s
	captured.Title = "旧名字"
	bound.SetCurrent(&captured)
	d.mu.Lock()
	d.runs[s.ID] = &sessionRun{sm: bound}
	d.mu.Unlock()

	if res := (&AgentService{desk: d}).RenameSession(s.ID, "新的名字"); res != "ok" {
		t.Fatalf("RenameSession = %q, want ok", res)
	}
	if got := d.sessionRecord(s.ID).Title; got != "新的名字" {
		t.Errorf("the copy a bound session reads still says %q — that is what the phone's message page shows", got)
	}
	// And the store itself, which the LIST reads.
	stored, err := d.sm.Load(s.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.Title != "新的名字" {
		t.Errorf("store title = %q, want 新的名字", stored.Title)
	}
}
