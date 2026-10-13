package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestForkTitleMarksTheRowButNeverInventsOne pins the two halves of the rule the
// sidebar sees: a fork of a named conversation is marked (two identical rows in one
// group are otherwise indistinguishable), and a fork of a conversation that has no
// title yet stays nameless so its own first message names it — the same way any new
// session gets its title.
func TestForkTitleMarksTheRowButNeverInventsOne(t *testing.T) {
	if got := forkTitle("改导出"); got != "改导出"+forkTitleSuffix {
		t.Errorf("forkTitle(改导出) = %q, want the source's title plus %q", got, forkTitleSuffix)
	}
	if got := forkTitle(""); got != "" {
		t.Errorf("forkTitle(\"\") = %q, want \"\" (the fork's first message names it)", got)
	}
	if !strings.Contains(forkTitle("改导出"), "改导出") {
		t.Errorf("the fork's row must still say which conversation it came from")
	}
}

// TestForkResultVOCarriesWhatTheFrontendReads pins the binding's shape: the
// frontend switches to `session.id` by name and renders `error` verbatim, and this
// is the only Go test that sees those two JSON keys at all. A renamed field is a
// silent no-op in the window (the menu item would do nothing visible), which is
// exactly the failure a VO test exists to catch.
func TestForkResultVOCarriesWhatTheFrontendReads(t *testing.T) {
	info := SessionInfo{ID: "2026-10-13-213116-abcd1234", Title: "改导出 · 分支", ProjectID: "p1", CreatedAt: time.Now()}
	ok := ForkResultVO{Session: &info}
	data, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sess, hasSession := wire["session"].(map[string]any)
	if !hasSession {
		t.Fatalf("a successful fork must carry `session`: %s", data)
	}
	if sess["id"] != "2026-10-13-213116-abcd1234" {
		t.Errorf("session.id = %v — the frontend switches on this key", sess["id"])
	}
	if sess["title"] != "改导出 · 分支" {
		t.Errorf("session.title = %v", sess["title"])
	}
	if _, hasErr := wire["error"]; hasErr {
		t.Errorf("a successful fork must not carry `error`: %s", data)
	}

	refused := ForkResultVO{Error: "对话正在运行，请先停止这一轮再分支"}
	data, err = json.Marshal(refused)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire = nil
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["error"] != "对话正在运行，请先停止这一轮再分支" {
		t.Errorf("a refusal must reach the reader verbatim: %s", data)
	}
	if _, hasSession := wire["session"]; hasSession {
		t.Errorf("a refused fork must not carry a session: %s", data)
	}
}
