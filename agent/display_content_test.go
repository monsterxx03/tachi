package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// recordedUserMessages returns the user records a turn wrote to the session.
func recordedUserMessages(t *testing.T, sm *fakeSessionManager) []session.Message {
	t.Helper()
	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	var out []session.Message
	for _, m := range msgs {
		if m.Type == session.MessageTypeUser {
			out = append(out, m)
		}
	}
	return out
}

// TestRun_DisplayUserMessageRecorded pins the two-sided record for a turn whose text the
// FRONTEND transformed before calling the agent (@-file expansion): Content is what the model
// received — the file inlined between UNTRUSTED FILE CONTENT markers — and DisplayContent is
// what the user typed. A transcript rebuilt from the session reads the latter, so without it
// a reloaded conversation showed the whole file body in the user's bubble.
func TestRun_DisplayUserMessageRecorded(t *testing.T) {
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("看过了")}}
	sm := &fakeSessionManager{}
	a := newTestAgent(t, mp, func(a *AIAgent) { a.SetSessionManager(sm) })

	sent := "看看这个 @README.md\n\n--- BEGIN UNTRUSTED FILE CONTENT: README.md ---\n# smoke\n---\n"
	typed := "看看这个 @README.md"
	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, sent, "", llm.ChatOptions{MaxTokens: 4096},
		WithDisplayUserMessage(typed)))
	require.NotNil(t, result)
	require.Equal(t, ExitReasonStop, result.ExitReason)

	users := recordedUserMessages(t, sm)
	require.Len(t, users, 1)
	assert.Equal(t, sent, users[0].Content, "Content must stay exactly what the model was sent")
	assert.Equal(t, typed, users[0].DisplayContent)

	// The session title comes from the user's own words too: titling from the expanded text
	// would name the conversation after the inlined file (and pay a whole file's tokens for
	// the title request).
	require.NotNil(t, sm.Current())
	assert.NotContains(t, sm.Current().Title, "UNTRUSTED")
}

// The other half: nothing extra is written when there was no transformation. An empty
// DisplayContent is also what every session written before the field existed looks like, so
// "same text" and "old record" take one code path in the readers.
func TestRun_DisplayUserMessageOmittedWhenUnchanged(t *testing.T) {
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("ok")}}
	sm := &fakeSessionManager{}
	a := newTestAgent(t, mp, func(a *AIAgent) { a.SetSessionManager(sm) })

	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, "go", "", llm.ChatOptions{MaxTokens: 4096},
		WithDisplayUserMessage("go")))
	require.NotNil(t, result)

	users := recordedUserMessages(t, sm)
	require.Len(t, users, 1)
	assert.Equal(t, "go", users[0].Content)
	assert.Empty(t, users[0].DisplayContent)
}

// The steer path takes the same rule: a steered message whose @-references the frontend expanded
// keeps the user's own text on the session record, while Content stays what was injected. A steer
// is a user record too — it renders as a bubble after a reload, so it needs the same two-sided
// record as the turn's own prompt.
func TestRun_SteerDisplayRecorded(t *testing.T) {
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{
		toolCallSeq("Bash", "call_1", `{"command":"ls"}`),
		textSeq("done"),
	}}
	sm := &fakeSessionManager{}
	a := newTestAgent(t, mp, func(a *AIAgent) { a.SetSessionManager(sm) })
	a.RegisterTool(echoStub())

	steerCh := make(chan SteerInput, 1)
	ch := a.RunConversationStream(t.Context(), nil, "hi", "sys",
		llm.ChatOptions{MaxTokens: 4096}, WithSteerChannel(steerCh))
	sent := "看看 @README.md\n\n--- BEGIN UNTRUSTED FILE CONTENT: README.md ---\n# smoke\n---\n"
	steerCh <- SteerInput{Text: sent, Display: "看看 @README.md"}
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	require.Equal(t, ExitReasonStop, result.ExitReason)

	users := recordedUserMessages(t, sm)
	require.Len(t, users, 2, "the turn's prompt and the steer")
	assert.Equal(t, "hi", users[0].Content)
	assert.Equal(t, sent, users[1].Content)
	assert.Equal(t, "看看 @README.md", users[1].DisplayContent)
	assert.Greater(t, users[1].Iteration, 0, "a steer record names the call it was injected before")
}
