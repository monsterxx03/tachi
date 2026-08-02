package manager

import (
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// TestSpliceArtifactIntoCache guards the J1 channel cache path: after a
// research/review registers an artifact, the reminder must land in the
// thread's in-memory ca.history so the next turn (which reads ca.history,
// not disk) sees it.
func TestSpliceArtifactIntoCache(t *testing.T) {
	mgr := newTestManagerWithProvider(t)

	ca := &cachedAgent{
		history: []llm.Message{
			{Role: "user", Content: "帮我 review"},
			{Role: "assistant", Content: "好"},
		},
	}
	ref := session.ArtifactRef{
		Kind:  session.ArtifactKindReview,
		Title: "代码审查（3 轮）",
		Path:  "/work/.tachi/reviews/20260802-210636/round-3-judge-x.md",
	}

	mgr.spliceArtifactIntoCache(ca, ref)

	n := len(ca.history)
	if n != 3 {
		t.Fatalf("expected 3 messages after splice, got %d", n)
	}
	last := ca.history[n-1]
	if last.Role != "user" {
		t.Errorf("spliced message should be a user message, got role %q", last.Role)
	}
	if last.Content != session.FormatArtifactReminder([]session.ArtifactRef{ref}) {
		t.Errorf("spliced content mismatch: %q", last.Content)
	}
}

// TestSpliceArtifactIntoCache_NilHistory ensures a nil cache (first turn /
// after eviction) is a no-op — the disk reload path covers that case.
func TestSpliceArtifactIntoCache_NilHistory(t *testing.T) {
	mgr := newTestManagerWithProvider(t)

	ca := &cachedAgent{} // history == nil
	ref := session.ArtifactRef{Kind: session.ArtifactKindResearch, Title: "t", Path: "/tmp/r.html"}
	mgr.spliceArtifactIntoCache(ca, ref) // must not panic
	if ca.history != nil {
		t.Fatalf("expected nil history to stay nil, got %d messages", len(ca.history))
	}
}
