package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/session"
)

// TestTurnChangesFallBackToTheToolPaths pins the fallback the user asked for by name: with no
// checkpoints to read, the panel must still answer — from the working tree against HEAD, the
// way it always did — and it must SAY which source it used. A number that looks the same
// while meaning something narrower is worse than a smaller number that declares itself.
func TestTurnChangesFallBackToTheToolPaths(t *testing.T) {
	repo := gitRepo(t)
	_, svc, sid := newRootsApp(t, repo)
	tracked := filepath.Join(repo, "src/main.go")
	writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tnewOne()\n}\n")

	vo := svc.GetTurnChanges(sid, 0, []string{tracked})
	if vo.Source != changeSourceTools {
		t.Errorf("Source = %q, want %q (no checkpoint pair exists in this fixture)", vo.Source, changeSourceTools)
	}
	if len(vo.Files) != 1 || vo.Files[0].Path != "src/main.go" {
		t.Errorf("files = %+v, want the working-tree diff of the path it was given", vo.Files)
	}
	if !strings.Contains(vo.Note, "检查点") {
		t.Errorf("Note = %q, must say the numbers came from the tool calls and why", vo.Note)
	}

	// A turn number with no pair behind it is the same story: the fallback, labelled.
	vo = svc.GetTurnChanges(sid, 7, nil)
	if vo.Source != changeSourceTools {
		t.Errorf("Source = %q for an unknown turn, want %q", vo.Source, changeSourceTools)
	}
}

// TestPathsFromUnifiedListsEveryTouchedFile is the review's file list: it is taken from the SAME
// diff the reviewer will read, so nothing may be dropped on the way (a file git calls new or
// deleted is as much part of "this turn's changes" as a modified one).
func TestPathsFromUnifiedListsEveryTouchedFile(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/added.txt b/added.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/added.txt",
		"@@ -0,0 +1,2 @@",
		"+one",
		"+two",
		"diff --git a/gone.txt b/gone.txt",
		"deleted file mode 100644",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-was here",
		"",
	}, "\n")

	got := pathsFromUnified(diff)
	want := []string{"added.txt", "gone.txt"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTurnStampsCoverAPageThatOpensMidTurn: a page is the newest N records, so the opening
// record of a long turn sits on an earlier page. The stamp has to be resolved as "the turn in
// force at this index" rather than "the record that IS the boundary": otherwise exactly the
// longest turns — the shell-heavy ones whose numbers no tool call declares — lose their turn on
// reload, and with it the diff panel's scope and the review's.
func TestTurnStampsCoverAPageThatOpensMidTurn(t *testing.T) {
	cp := &TurnChangesVO{Files: 3, Added: 7, Source: changeSourceCheckpoint}
	stamps := map[int]turnStamp{
		0: {Turn: 1, Changes: cp},
		6: {Turn: 2}, // a turn that wrote nothing: the turn without its numbers
	}

	// Records 4 and 5 belong to turn 1, whose boundary (0) is not part of this page.
	page := []session.Message{
		{Type: session.MessageTypeAssistant, Content: "接着写", Timestamp: time.Now()},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "ok", Timestamp: time.Now()},
	}
	for i, m := range buildSessionMessages(page, 4, stamps) {
		if m.Turn != 1 {
			t.Errorf("record %d: Turn = %d, want 1 (its boundary is on an earlier page)", 4+i, m.Turn)
		}
		if m.Changes == nil || m.Changes.Files != 3 {
			t.Errorf("record %d: Changes = %+v, want the turn's own numbers", 4+i, m.Changes)
		}
	}

	// The next boundary takes over at its own record, and a turn with no numbers must not
	// inherit the previous turn's.
	next := []session.Message{{Type: session.MessageTypeUser, Content: "第二问", Timestamp: time.Now()}}
	msgs := buildSessionMessages(next, 6, stamps)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Turn != 2 {
		t.Errorf("Turn = %d, want 2", msgs[0].Turn)
	}
	if msgs[0].Changes != nil {
		t.Errorf("Changes = %+v, want nil: the previous turn's numbers are not this turn's", msgs[0].Changes)
	}
}
