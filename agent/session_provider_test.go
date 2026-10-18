package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/session"
)

// The session fallback must not offer the conversation being served: doing so
// put the user's own last messages into the "memories from past sessions" block
// on every turn with no topic match.
func TestTopicSessionProviderExcludesCurrentConversation(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)

	cur, err := sm.New("alpha", t.TempDir())
	if err != nil {
		t.Fatalf("new current session: %v", err)
	}
	other, err := sm.New("alpha", t.TempDir())
	if err != nil {
		t.Fatalf("new other session: %v", err)
	}
	// New() repoints current at each new session; put it back on `cur`.
	sm.SetCurrent(cur)

	p := &topicSessionProvider{manager: sm}
	got, err := p.RecentSessions(t.Context(), 3)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected only the non-current session, got %d", len(got))
	}
	if got[0].ID != other.ID {
		t.Fatalf("got session %q, want %q", got[0].ID, other.ID)
	}
}
