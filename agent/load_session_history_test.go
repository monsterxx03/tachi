package agent

import (
	"reflect"
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// newTestSessionManager returns a session.Manager backed by a temp-dir file
// store, plus the created session, ready for message appends.
func newTestSessionManager(t *testing.T, providerName string) *session.Manager {
	t.Helper()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)
	if _, err := sm.New(providerName, t.TempDir()); err != nil {
		t.Fatalf("new session: %v", err)
	}
	return sm
}

func TestLoadSessionHistory_NoSessionManager(t *testing.T) {
	a := newBareTestAgent(t, nil, 10)

	history, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if history != nil {
		t.Errorf("expected nil history with no session manager, got %+v", history)
	}
}

func TestLoadSessionHistory_EmptySession(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 10)
	a.SetSessionManager(newTestSessionManager(t, "anthropic"))

	history, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if history != nil {
		t.Errorf("expected nil history for empty session, got %+v", history)
	}
}

func TestLoadSessionHistory_ConvertsMessages(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 10)
	sm := newTestSessionManager(t, "anthropic")
	a.SetSessionManager(sm)

	for _, m := range []*session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeAssistant, Content: "Hi there!"},
	} {
		if err := sm.AppendMessage(m); err != nil {
			t.Fatalf("append message: %v", err)
		}
	}

	history, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "Hi there!"},
	}
	if !reflect.DeepEqual(history, expected) {
		t.Errorf("history mismatch:\n  got:  %+v\n  want: %+v", history, expected)
	}
}

// TestLoadSessionHistory_UsesProviderName guards the invariant that callers
// migrating off hand-rolled LoadMessages + ConvertSessionToLLMMessages rely on:
// the conversion uses the agent's own provider name.
func TestLoadSessionHistory_UsesProviderName(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "openai"}, 10)
	sm := newTestSessionManager(t, "openai")
	a.SetSessionManager(sm)

	for _, m := range []*session.Message{
		{Type: session.MessageTypeUser, Content: "hi"},
		{Type: session.MessageTypeThinking, Content: "pondering", Signature: "sig"},
		{Type: session.MessageTypeAssistant, Content: "answer"},
	} {
		if err := sm.AppendMessage(m); err != nil {
			t.Fatalf("append message: %v", err)
		}
	}

	history, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// OpenAI conversion prepends thinking into the assistant content rather
	// than emitting ThinkingBlocks (see TestConvertSessionToLLMMessages_OpenAI_PrependsThinking).
	direct, err := ConvertSessionToLLMMessages(mustLoad(t, sm), "openai")
	if err != nil {
		t.Fatalf("direct convert: %v", err)
	}
	if !reflect.DeepEqual(history, direct) {
		t.Errorf("LoadSessionHistory diverged from direct conversion:\n  got:  %+v\n  want: %+v", history, direct)
	}
}

func mustLoad(t *testing.T, sm *session.Manager) []session.Message {
	t.Helper()
	msgs, err := sm.LoadMessages()
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	return msgs
}
