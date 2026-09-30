package agent

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// imageStub is a tool that answers with an image, the way ReadFile does for a screenshot: the
// text names the file, and the bytes travel out of band (tools.AddImageParts).
func imageStub(raw []byte, mediaType string) *stubTool {
	return &stubTool{
		name:     "ReadFile",
		desc:     "Read a file",
		parallel: true,
		executeFn: func(ctx context.Context, _ string) (string, error) {
			tools.AddImageParts(ctx, []llm.ContentPart{{
				Type:      llm.ContentPartImage,
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(raw),
			}})
			return "[Image: shot.png, " + mediaType + "]", nil
		},
	}
}

// newImageRecordAgent wires an agent whose session records go to a real temp-dir store — the
// point being that the blob write and the record write are one path, so a test that reads the
// blob back afterwards is testing what a reload will find.
func newImageRecordAgent(t *testing.T, mp *mockStreamProvider) (*AIAgent, *session.Manager) {
	t.Helper()
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	a := newTestAgent(t, mp, func(ag *AIAgent) { ag.SetSessionManager(sm) })
	return a, sm
}

// TestAgentLoop_RecordsToolImageParts pins the record site that covers every image a TOOL
// returns (ReadFile on a screenshot, an MCP screenshot tool): the tool_result record must keep
// the parts the request carried, with the bytes in the session's blob directory — the record's
// own text still says an image was there, but the model never sees it again from a rebuild.
func TestAgentLoop_RecordsToolImageParts(t *testing.T) {
	raw := []byte("fake png bytes returned by a tool")
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{
		toolCallSeq("ReadFile", "call-1", `{"path":"shot.png"}`),
		textSeq("done"),
	}}
	a, sm := newImageRecordAgent(t, mp)
	a.RegisterTool(imageStub(raw, "image/png"))

	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, "看下图", "", llm.ChatOptions{MaxTokens: 4096}))
	require.NotNil(t, result)

	msgs, err := sm.LoadMessages()
	require.NoError(t, err)

	var toolRec *session.Message
	for i := range msgs {
		if msgs[i].Type == session.MessageTypeToolResult {
			toolRec = &msgs[i]
		}
	}
	require.NotNil(t, toolRec, "the tool result must be recorded")
	require.Len(t, toolRec.Parts, 1, "the tool's image must be recorded with it")
	assert.Equal(t, session.PartTypeImage, toolRec.Parts[0].Type)
	assert.Equal(t, "image/png", toolRec.Parts[0].MediaType)
	require.NotEmpty(t, toolRec.Parts[0].Ref, "the record must reference a stored blob")
	assert.Empty(t, toolRec.Parts[0].Data, "the bytes must not be inline in the record")

	stored, err := sm.LoadImage(toolRec.Parts[0].Ref)
	require.NoError(t, err)
	assert.Equal(t, raw, stored, "the blob must be exactly the bytes the model was given")

	// And the reload hands the same parts back — this is what a restart, a --resume or a
	// session switch does, and the provider's prefix cache depends on it being identical.
	history, err := a.LoadSessionHistory()
	require.NoError(t, err)
	require.Len(t, history, 4, "user + assistant(tool_call) + tool result + assistant")
	require.Len(t, history[2].ContentParts, 1)
	assert.Equal(t, base64.StdEncoding.EncodeToString(raw), history[2].ContentParts[0].Data)
	assert.Equal(t, "image/png", history[2].ContentParts[0].MediaType)
}

// TestAgentLoop_RecordsUserImageParts covers the other record site: a prompt that arrived with
// an image (@-reference expansion, a channel attachment, a desktop paste). The text part travels
// with it, because that is what the provider was sent — a rebuild with the image but without its
// text block would be a different request, not a restored one.
func TestAgentLoop_RecordsUserImageParts(t *testing.T) {
	raw := []byte("fake png bytes pasted by the user")
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("看到了")}}
	a, sm := newImageRecordAgent(t, mp)

	pending := []llm.ContentPart{{
		Type:      llm.ContentPartImage,
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(raw),
	}}
	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, "看下图", "",
		llm.ChatOptions{MaxTokens: 4096}, WithPendingImages(pending)))
	require.NotNil(t, result)

	msgs, err := sm.LoadMessages()
	require.NoError(t, err)

	var userRec *session.Message
	for i := range msgs {
		if msgs[i].Type == session.MessageTypeUser {
			userRec = &msgs[i]
		}
	}
	require.NotNil(t, userRec)
	require.Len(t, userRec.Parts, 2, "the text part goes with the image")
	assert.Equal(t, session.PartTypeText, userRec.Parts[0].Type)
	assert.Equal(t, "看下图", userRec.Parts[0].Text)
	assert.Equal(t, session.PartTypeImage, userRec.Parts[1].Type)
	require.NotEmpty(t, userRec.Parts[1].Ref)

	history, err := a.LoadSessionHistory()
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Len(t, history[0].ContentParts, 2, "text part first, image second — as sent")
	assert.Equal(t, "看下图", history[0].ContentParts[0].Text)
	assert.Equal(t, base64.StdEncoding.EncodeToString(raw), history[0].ContentParts[1].Data)
}

// TestRecordParts_TextOnlyIsNil keeps the record honest: parts exist so an image survives a
// reload, and a text-only parts array would rebuild the request in a different SHAPE than it
// was sent in (a string becomes an array), changing the prefix of every turn that follows it.
func TestRecordParts_TextOnlyIsNil(t *testing.T) {
	assert.Nil(t, RecordParts(nil))
	assert.Nil(t, RecordParts([]llm.ContentPart{{Type: llm.ContentPartText, Text: "hi"}}))
	assert.Nil(t, RecordParts([]llm.ContentPart{{Type: llm.ContentPartImage, MediaType: "image/png"}}),
		"an image part with no bytes is not worth recording")
}
