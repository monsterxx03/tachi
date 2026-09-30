package agent

import (
	"encoding/base64"
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
	direct, err := ConvertSessionToLLMMessages(mustLoad(t, sm), "openai", sm)
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

// TestLoadSessionHistory_RestoresImageParts is the reason image parts are persisted at all: a
// history rebuilt from the store — which is what a restart, a `--resume`, or a session switch
// does — must carry the images the live turn sent, byte for byte. Anything less and the
// provider's prefix cache misses from the first image to the end of the conversation, and the
// model loses sight of an image it was really given.
//
// The records go through the real store (a temp-dir FileStore), so this covers the whole path:
// RecordParts on the way in, the blob in images/, the reference on the record, and the rebuild
// through the session manager on the way out.
func TestLoadSessionHistory_RestoresImageParts(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 10)
	sm := newTestSessionManager(t, "anthropic")
	a.SetSessionManager(sm)

	userImage := base64.StdEncoding.EncodeToString([]byte("fake png bytes for the user turn"))
	toolImage := base64.StdEncoding.EncodeToString([]byte("fake jpeg bytes from a tool result"))

	// The live parts, exactly as the loop builds them: a user message whose text part comes
	// first, a tool message that is images only (its text lives in Content).
	liveUserParts := []llm.ContentPart{
		{Type: llm.ContentPartText, Text: "看下这张图"},
		{Type: llm.ContentPartImage, MediaType: "image/png", Data: userImage},
	}
	liveToolParts := []llm.ContentPart{
		{Type: llm.ContentPartImage, MediaType: "image/jpeg", Data: toolImage},
	}

	userMsg := &session.Message{Type: session.MessageTypeUser, Content: "看下这张图", Parts: RecordParts(liveUserParts)}
	toolMsg := &session.Message{
		Type: session.MessageTypeToolResult, Name: "ReadFile", Result: "[Image: a.jpg, image/jpeg]",
		ToolCallID: "call-1", Parts: RecordParts(liveToolParts),
	}
	for _, m := range []*session.Message{
		userMsg,
		{Type: session.MessageTypeToolCall, Name: "ReadFile", Args: `{"path":"a.jpg"}`, ToolCallID: "call-1"},
		toolMsg,
	} {
		if err := sm.AppendMessage(m); err != nil {
			t.Fatalf("append message: %v", err)
		}
	}

	// The bytes belong in the store, not in the record: that is what keeps a conversation with
	// screenshots in it a readable JSONL file. The reference on the record and the blob behind
	// it have to agree, or a reload would be the first thing to notice.
	onDisk := mustLoad(t, sm)
	ref := onDisk[0].Parts[1].Ref
	if ref == "" || onDisk[0].Parts[1].Data != "" {
		t.Fatalf("image part on disk should be a blob reference with no inline bytes, got %+v", onDisk[0].Parts[1])
	}
	stored, err := sm.LoadImage(ref)
	if err != nil {
		t.Fatalf("load image blob: %v", err)
	}
	if string(stored) != "fake png bytes for the user turn" {
		t.Errorf("stored blob is not the image that was sent: %q", stored)
	}

	history, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(history) != 3 {
		t.Fatalf("expected 3 messages (user, assistant+tool_call, tool), got %d: %+v", len(history), history)
	}
	if !reflect.DeepEqual(history[0].ContentParts, liveUserParts) {
		t.Errorf("user image parts not restored:\n  got:  %+v\n  want: %+v", history[0].ContentParts, liveUserParts)
	}
	if !reflect.DeepEqual(history[2].ContentParts, liveToolParts) {
		t.Errorf("tool image parts not restored:\n  got:  %+v\n  want: %+v", history[2].ContentParts, liveToolParts)
	}

	// And the same history converted a second time must be identical — a rebuild that drifts
	// turn after turn is the failure this guards.
	again, err := a.LoadSessionHistory()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !reflect.DeepEqual(history, again) {
		t.Errorf("reloaded history is not stable:\n  first:  %+v\n  second: %+v", history, again)
	}
}
