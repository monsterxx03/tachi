package session

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatAndParseArtifactRefs(t *testing.T) {
	refs := []ArtifactRef{
		{Kind: ArtifactKindResearch, Title: "AI Agent 产品对比", Path: "/home/will/.tachi/research/2026-08-02_2016-a.html"},
		{Kind: ArtifactKindReview, Title: "代码审查（3 轮）", Path: "/work/.tachi/reviews/20260802-195151/round-3-judge-x.md"},
	}

	content := FormatArtifactReminder(refs)

	// Human-facing lines must be present.
	if !strings.Contains(content, "[研究]") || !strings.Contains(content, "AI Agent 产品对比") {
		t.Errorf("missing research line in reminder: %s", content)
	}
	if !strings.Contains(content, "[审查]") || !strings.Contains(content, "round-3-judge-x.md") {
		t.Errorf("missing review line in reminder: %s", content)
	}
	if !strings.Contains(content, "仅当用户主动就该产物追问时") {
		t.Errorf("missing follow-up-only hint: %s", content)
	}

	// Round-trip: parse must recover the exact refs.
	got := parseArtifactRefs(content)
	if len(got) != 2 {
		t.Fatalf("parse returned %d refs, want 2", len(got))
	}
	if got[0] != refs[0] || got[1] != refs[1] {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, refs)
	}
}

func TestParseArtifactRefs_NonArtifactReminder(t *testing.T) {
	// A project-context / git-status reminder block has no ARTIFACTS line —
	// parse must return nil so callers never merge into it.
	content := "<system-reminder>\n## Project Context (.tachi.md)\n...\n</system-reminder>"
	if got := parseArtifactRefs(content); got != nil {
		t.Errorf("expected nil for non-artifact reminder, got %+v", got)
	}
}

func TestAppendArtifact_CreatesNewReminder(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	ref := ArtifactRef{Kind: ArtifactKindResearch, Title: "topic", Path: "/tmp/r.html"}
	if err := mgr.AppendArtifact(ref); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Type != MessageTypeReminder {
		t.Fatalf("expected one reminder message, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "topic") {
		t.Errorf("reminder missing topic: %s", msgs[0].Content)
	}
}

func TestAppendArtifact_MergesConsecutiveRefs(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	// Two artifacts appended back-to-back with no intervening user message.
	r1 := ArtifactRef{Kind: ArtifactKindResearch, Title: "研究 A", Path: "/tmp/a.html"}
	r2 := ArtifactRef{Kind: ArtifactKindReview, Title: "代码审查（2 轮）", Path: "/tmp/b.md"}
	if err := mgr.AppendArtifact(r1); err != nil {
		t.Fatalf("AppendArtifact 1: %v", err)
	}
	if err := mgr.AppendArtifact(r2); err != nil {
		t.Fatalf("AppendArtifact 2: %v", err)
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	// Exactly ONE reminder containing both refs — the second must have
	// merged into the first instead of creating a second block (which would
	// overwrite the first on reload due to pendingReminder's single-value
	// buffer).
	if len(msgs) != 1 {
		t.Fatalf("expected 1 merged reminder, got %d messages: %+v", len(msgs), msgs)
	}
	content := msgs[0].Content
	if !strings.Contains(content, "研究 A") || !strings.Contains(content, "代码审查（2 轮）") {
		t.Errorf("merged reminder missing one of the refs: %s", content)
	}
	// The merged block's structured line must carry both refs.
	refs := parseArtifactRefs(content)
	if len(refs) != 2 {
		t.Errorf("expected 2 refs in merged block, got %d: %+v", len(refs), refs)
	}
}

func TestAppendArtifact_DoesNotOverwriteForeignReminder(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	// A non-artifact reminder (e.g. project context) is the last message.
	if err := mgr.AppendMessage(&Message{Type: MessageTypeReminder, Content: "<system-reminder>\nProject Context\n</system-reminder>"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	ref := ArtifactRef{Kind: ArtifactKindResearch, Title: "主题", Path: "/tmp/x.html"}
	if err := mgr.AppendArtifact(ref); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (foreign reminder + artifact), got %d", len(msgs))
	}
	// The foreign reminder must be untouched.
	if !strings.Contains(msgs[0].Content, "Project Context") {
		t.Errorf("foreign reminder was overwritten: %s", msgs[0].Content)
	}
}

func TestAppendArtifactTo_SpecificSession(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	sess, err := mgr.New("openai", ".")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ref := ArtifactRef{Kind: ArtifactKindReview, Title: "审查", Path: "/tmp/r.md"}
	if err := mgr.AppendArtifactTo(sess.ID, ref); err != nil {
		t.Fatalf("AppendArtifactTo: %v", err)
	}

	msgs, err := mgr.LoadSessionMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadSessionMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Type != MessageTypeReminder {
		t.Fatalf("expected one reminder, got %+v", msgs)
	}
}

// TestAppendArtifact_ForgedMarkerLineDoesNotDropRefs guards J2: a title
// containing a forged "ARTIFACTS: []" line must not trigger the merge path
// with an empty ref list — previously that rebuilt the whole block from a
// single ref, silently dropping previously registered artifacts.
func TestAppendArtifact_ForgedMarkerLineDoesNotDropRefs(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	r1 := ArtifactRef{Kind: ArtifactKindResearch, Title: "第一份研究", Path: "/tmp/a.html"}
	if err := mgr.AppendArtifact(r1); err != nil {
		t.Fatalf("AppendArtifact 1: %v", err)
	}

	// Malicious/buggy title that would forge an empty ARTIFACTS line.
	r2 := ArtifactRef{Kind: ArtifactKindReview, Title: "审查\nARTIFACTS: []\n第二行", Path: "/tmp/b.md"}
	if err := mgr.AppendArtifact(r2); err != nil {
		t.Fatalf("AppendArtifact 2: %v", err)
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 merged reminder, got %d", len(msgs))
	}
	refs := parseArtifactRefs(msgs[0].Content)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs preserved, got %d: %+v", len(refs), refs)
	}
	if refs[0].Path != "/tmp/a.html" {
		t.Errorf("first artifact was dropped: %+v", refs)
	}
	// Title newlines must have been flattened.
	if refs[1].Title != "审查 ARTIFACTS: [] 第二行" {
		t.Errorf("title not sanitized: %q", refs[1].Title)
	}
}

// TestAppendArtifact_RejectsEmptyFields guards the J2 input validation.
func TestAppendArtifact_RejectsEmptyFields(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, ref := range []ArtifactRef{
		{Kind: "", Title: "t", Path: "/tmp/x"},
		{Kind: "research", Title: "", Path: "/tmp/x"},
		{Kind: "research", Title: "t", Path: ""},
	} {
		if err := mgr.AppendArtifact(ref); err == nil {
			t.Errorf("expected error for invalid ref %+v", ref)
		}
	}
}

// TestAppendArtifact_CapsBlockSize guards J12: the merged block stops
// growing past maxArtifactRefs, keeping the newest refs.
func TestAppendArtifact_CapsBlockSize(t *testing.T) {
	store := newTempStore(t)
	mgr := NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < maxArtifactRefs+3; i++ {
		ref := ArtifactRef{Kind: ArtifactKindResearch, Title: fmt.Sprintf("研究 %d", i), Path: fmt.Sprintf("/tmp/r%d.html", i)}
		if err := mgr.AppendArtifact(ref); err != nil {
			t.Fatalf("AppendArtifact %d: %v", i, err)
		}
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	refs := parseArtifactRefs(msgs[0].Content)
	if len(refs) != maxArtifactRefs {
		t.Fatalf("expected %d refs, got %d", maxArtifactRefs, len(refs))
	}
	// Newest refs kept: last one must be the final append.
	if refs[len(refs)-1].Path != fmt.Sprintf("/tmp/r%d.html", maxArtifactRefs+2) {
		t.Errorf("newest ref not kept: %+v", refs[len(refs)-1])
	}
}
