package mcp

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b64Image builds base64 image data of exactly n decoded bytes.
func b64Image(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", n)))
}

func imageParts(parts []llm.ContentPart) []llm.ContentPart {
	var out []llm.ContentPart
	for _, p := range parts {
		if p.Type == llm.ContentPartImage {
			out = append(out, p)
		}
	}
	return out
}

// A server that answers with text AND an image — the shape a screenshot tool
// uses — must yield both: the text as the tool output and the image as a
// content part. Dropping the image is the bug this pins.
func TestExtractMCPImages_AttachesImageBesideText(t *testing.T) {
	data := b64Image(64)
	content := []mcp.Content{
		mcp.TextContent{Type: "text", Text: "Took a screenshot."},
		mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: data},
	}

	parts, note := extractMCPImages(content)

	assert.Empty(t, note)
	imgs := imageParts(parts)
	require.Len(t, imgs, 1)
	assert.Equal(t, "image/png", imgs[0].MediaType)
	assert.Equal(t, data, imgs[0].Data)
}

// Text-only results must behave exactly as before: no parts, no note.
func TestExtractMCPImages_TextOnlyIsUnchanged(t *testing.T) {
	content := []mcp.Content{mcp.TextContent{Type: "text", Text: "Hello"}}

	parts, note := extractMCPImages(content)

	assert.Empty(t, parts)
	assert.Empty(t, note)
}

// An image at the budget is attached; one byte over is refused with a note.
// The comparison is against the DECODED size: base64 inflates by 4/3, so
// measuring the encoded string would refuse an image the Read tool accepts.
// The boundary is the point — a screenshot just under the cap is the case
// this exists to serve.
func TestExtractMCPImages_SizeBudgetIsDecoded(t *testing.T) {
	atLimit := []mcp.Content{
		mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: b64Image(llm.MaxImageSize)},
	}
	parts, note := extractMCPImages(atLimit)
	require.Len(t, imageParts(parts), 1)
	assert.Empty(t, note)

	overLimit := []mcp.Content{
		mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: b64Image(llm.MaxImageSize + 1)},
	}
	parts, note = extractMCPImages(overLimit)
	assert.Empty(t, imageParts(parts))
	assert.Contains(t, note, "image/png")
	assert.Contains(t, note, "not attached")
}

// Past the count cap, extra images are dropped WITH a note. Silence here is
// what lets a model claim it saw a screenshot it never received.
func TestExtractMCPImages_CountCapReportsTheDrop(t *testing.T) {
	var content []mcp.Content
	for i := 0; i < mcpImageMaxCount+2; i++ {
		content = append(content, mcp.ImageContent{
			Type: "image", MIMEType: "image/png", Data: b64Image(8),
		})
	}

	parts, note := extractMCPImages(content)

	assert.Len(t, imageParts(parts), mcpImageMaxCount)
	assert.Contains(t, note, strconv.Itoa(mcpImageMaxCount))
}

// Empty data is not an image: a server sending a placeholder must not produce
// a content part with no bytes, which every provider would reject.
func TestExtractMCPImages_EmptyDataSkipped(t *testing.T) {
	content := []mcp.Content{
		mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: ""},
	}

	parts, note := extractMCPImages(content)

	assert.Empty(t, parts)
	assert.Empty(t, note)
}

// decodedImageSize must equal the true decoded length at every padding case,
// because the size limit is compared against it. base64.StdEncoding.DecodedLen
// is an upper bound and over-counts a partial final group.
func TestDecodedImageSize_MatchesActual(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 1024, 1025, 1026, 1027} {
		data := b64Image(n)
		raw, err := base64.StdEncoding.DecodeString(data)
		require.NoError(t, err)
		assert.Equal(t, int64(len(raw)), decodedImageSize(data),
			"decoded size mismatch for %d bytes (encoded %q)", n, data)
	}
}

// End to end through the tool: ExecuteContext must land the image on the
// registry's side channel. This is the contract the agent loop consumes —
// extractMCPImages alone would pass while the wiring still dropped it.
func TestMCPTool_ExecuteContext_AttachesImageToCtx(t *testing.T) {
	data := b64Image(32)
	mgr := NewManager(t.Context(), nil, nil)
	addTestClient(mgr, "test", &stubMCPClient{
		callTool: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{Type: "text", Text: "Took a screenshot."},
					mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: data},
				},
			}, nil
		},
	})
	tool := MCPTool{serverName: "test", serverTool: &mcp.Tool{Name: "screenshot"}, manager: mgr}

	imageCtx := tools.WithImagePartsCarrier(context.Background())
	output, err := tool.ExecuteContext(imageCtx, "{}")
	require.NoError(t, err)
	assert.Equal(t, "Took a screenshot.", output)

	imgs := imageParts(tools.ImagePartsFromCtx(imageCtx))
	require.Len(t, imgs, 1)
	assert.Equal(t, "image/png", imgs[0].MediaType)
	assert.Equal(t, data, imgs[0].Data)
}

// The drop note rides the text output, which is what the model reads.
func TestMCPTool_ExecuteContext_DropNoteReachesOutput(t *testing.T) {
	mgr := NewManager(t.Context(), nil, nil)
	addTestClient(mgr, "test", &stubMCPClient{
		callTool: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{Type: "text", Text: "shot"},
					mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: b64Image(llm.MaxImageSize + 1)},
				},
			}, nil
		},
	})
	tool := MCPTool{serverName: "test", serverTool: &mcp.Tool{Name: "screenshot"}, manager: mgr}

	output, err := tool.ExecuteContext(tools.WithImagePartsCarrier(context.Background()), "{}")

	require.NoError(t, err)
	assert.Contains(t, output, "shot")
	assert.Contains(t, output, "not attached")
}

// An error result's images are described by MIME and size rather than a bare
// "[non-text content]" — a server answering with an error screenshot is the
// case where the old placeholder left the user with nothing to read.
func TestContentToString_NamesImages(t *testing.T) {
	content := []mcp.Content{
		mcp.TextContent{Type: "text", Text: "failed: "},
		mcp.ImageContent{Type: "image", MIMEType: "image/png", Data: b64Image(1024)},
	}

	result := contentToString(content)

	assert.Contains(t, result, "failed: ")
	assert.Contains(t, result, "image/png")
	assert.NotContains(t, result, "[non-text content]")
}
