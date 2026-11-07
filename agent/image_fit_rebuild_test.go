package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// bulkyPNG is a screenshot-sized image that does not compress away. Records written
// before images were fitted at ingest hold exactly this: megabytes of picture the
// provider will not accept in a request body.
func bulkyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1500, 800))
	seed := uint32(99)
	for y := 0; y < 800; y++ {
		for x := 0; x < 1500; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 0xff})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	require.Greater(t, buf.Len(), llm.InlineImageBytes, "fixture must be over the inline budget")
	return buf.Bytes()
}

// A session recorded before the fit existed still holds full-size screenshots. A
// rebuild has to bring them under the budget, or the conversation can never be sent
// again — and the fit is deterministic, so the rebuilt request keeps matching the one
// that was sent.
func TestLoadSessionHistory_FitsOversizedStoredImages(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 10)
	sm := newTestSessionManager(t, "anthropic")
	a.SetSessionManager(sm)

	raw := bulkyPNG(t)
	record := &session.Message{
		Type: session.MessageTypeToolResult, Name: "ReadFile", Result: "[Image: shot.png]",
		ToolCallID: "call-1",
		Parts:      RecordParts([]llm.ContentPart{{Type: llm.ContentPartImage, MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}}),
	}
	require.NoError(t, sm.AppendMessage(record))

	history, err := a.LoadSessionHistory()
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Len(t, history[0].ContentParts, 1)

	sent, err := base64.StdEncoding.DecodeString(history[0].ContentParts[0].Data)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(sent), llm.InlineImageBytes, "a stored 4MB screenshot must not be sent as is")
	assert.Equal(t, history[0].ContentParts[0].MediaType, mediaTypeOfFitted(t, sent))

	// The record itself is untouched: the trim keeps originals on disk, and so does this.
	onDisk := mustLoad(t, sm)
	assert.Len(t, onDisk[0].Parts, 1)
	stored, err := sm.LoadImage(onDisk[0].Parts[0].Ref)
	require.NoError(t, err)
	assert.Equal(t, raw, stored, "fitting is a send-time decision, not a rewrite of history")
}

// mediaTypeOfFitted reads the format back out of the bytes the way a provider would.
func mediaTypeOfFitted(t *testing.T, data []byte) string {
	t.Helper()
	mime := llm.ImageMediaTypeForBytes(data)
	require.NotEmpty(t, mime, "fitted bytes must still be a supported image")
	return mime
}
