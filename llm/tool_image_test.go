package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

const testImageData = "aGVsbG8="

func toolMsgWithImage(content string) Message {
	return Message{
		Role:       "tool",
		ToolCallID: "tc1",
		Content:    content,
		ContentParts: []ContentPart{
			{Type: ContentPartImage, MediaType: "image/png", Data: testImageData},
		},
	}
}

// OpenAI's tool role accepts only text, so a tool's image is lifted into a user
// message placed directly behind the results. Previously the image was degraded
// to a one-line "[Image data: ...]" reference, which carried no pixels.
func TestOpenAIConvertMessages_ToolImageBecomesUserMessage(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "shot", Arguments: "{}"}},
		}},
		toolMsgWithImage("Took a screenshot."),
	}

	out := p.convertMessages(msgs)

	if len(out) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant, tool, user-with-image)", len(out))
	}
	if out[1].Role != "tool" || out[1].Content != "Took a screenshot." {
		t.Errorf("msg 1: got role=%q content=%q, want the plain tool result", out[1].Role, out[1].Content)
	}
	if out[2].Role != "user" {
		t.Fatalf("msg 2: role = %q, want user (the image carrier)", out[2].Role)
	}
	if len(out[2].MultiContent) != 2 {
		t.Fatalf("msg 2: parts = %d, want 2 (note + image)", len(out[2].MultiContent))
	}
	img := out[2].MultiContent[1].ImageURL
	if img == nil || img.URL != "data:image/png;base64,"+testImageData {
		t.Errorf("msg 2 image = %+v, want the data URI", img)
	}

	// The tool message itself must stay text-only: sending MultiContent on a
	// tool role is what every non-OpenAI gateway rejects with HTTP 400.
	body, err := json.Marshal(out[1])
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	if strings.Contains(string(body), "image_url") {
		t.Errorf("tool message must not carry image_url: %s", body)
	}
}

// The history usually ENDS with the tool block that just ran, so the tail
// flush is the path that matters in practice — not the mid-loop one.
func TestOpenAIConvertMessages_ToolImageAtHistoryTail(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "shot", Arguments: "{}"}},
		}},
		toolMsgWithImage("Took a screenshot."),
	}

	out := p.convertMessages(msgs)

	if len(out) != 3 {
		t.Fatalf("messages = %d, want 3 — the trailing image must still be emitted", len(out))
	}
	if out[2].Role != "user" || len(out[2].MultiContent) != 2 {
		t.Fatalf("last message = role %q with %d parts, want a user message carrying the image",
			out[2].Role, len(out[2].MultiContent))
	}
}

// A text-only tool result must keep the exact shape it had: no extra user
// message, plain string content. Any change here would alter every request.
func TestOpenAIConvertMessages_TextOnlyToolResultUnchanged(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "tool", ToolCallID: "tc1", Content: "file1.txt", Name: "bash"},
	}

	out := p.convertMessages(msgs)

	if len(out) != 1 {
		t.Fatalf("messages = %d, want 1 (no image, no extra message)", len(out))
	}
	if out[0].Content != "file1.txt" || out[0].MultiContent != nil {
		t.Errorf("got content=%q multi=%v, want plain string content", out[0].Content, out[0].MultiContent)
	}
}

// Consecutive tool results accumulate into ONE carrier message, which keeps the
// image behind the whole block it belongs to instead of splitting the block.
func TestOpenAIConvertMessages_ConsecutiveToolImagesShareOneCarrier(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "a", Arguments: "{}"}},
			{ID: "tc2", Type: "function", Function: ToolCallFunction{Name: "b", Arguments: "{}"}},
		}},
		toolMsgWithImage("first"),
		{Role: "tool", ToolCallID: "tc2", Content: "second",
			ContentParts: []ContentPart{{Type: ContentPartImage, MediaType: "image/png", Data: testImageData}}},
	}

	out := p.convertMessages(msgs)

	if len(out) != 4 {
		t.Fatalf("messages = %d, want 4 (assistant, tool, tool, one carrier)", len(out))
	}
	if out[3].Role != "user" || len(out[3].MultiContent) != 3 {
		t.Fatalf("carrier = role %q with %d parts, want 3 (note + 2 images)",
			out[3].Role, len(out[3].MultiContent))
	}
}

// The Responses API takes an item ARRAY for function_call_output and accepts
// images in it, so the image goes INTO the result rather than a separate
// message. Previously it was dropped outright.
func TestResponsesFunctionCallOutput_CarriesImage(t *testing.T) {
	out := functionCallOutput("Took a screenshot.", []ContentPart{
		{Type: ContentPartImage, MediaType: "image/png", Data: testImageData},
	})

	if out.OfString.Value != "" {
		t.Errorf("string shorthand must not be used when images are present")
	}
	items := out.OfResponseFunctionCallOutputItemArray
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (text + image)", len(items))
	}
	if items[0].OfInputText == nil || items[0].OfInputText.Text != "Took a screenshot." {
		t.Errorf("item 0 = %+v, want the result text", items[0])
	}
	if items[1].OfInputImage == nil ||
		items[1].OfInputImage.ImageURL.Value != "data:image/png;base64,"+testImageData {
		t.Errorf("item 1 = %+v, want the image data URI", items[1])
	}
}

// Without images the output must stay the plain string shorthand.
func TestResponsesFunctionCallOutput_TextOnlyUsesString(t *testing.T) {
	out := functionCallOutput("plain result", nil)

	if out.OfString.Value != "plain result" {
		t.Errorf("OfString = %q, want the result", out.OfString.Value)
	}
	if out.OfResponseFunctionCallOutputItemArray != nil {
		t.Errorf("array form must not be used without images")
	}
}

// An image-only result (no text) must still produce a well-formed item array:
// an empty text item would be rejected as invalid content.
func TestResponsesFunctionCallOutput_ImageWithoutText(t *testing.T) {
	out := functionCallOutput("", []ContentPart{
		{Type: ContentPartImage, MediaType: "image/png", Data: testImageData},
	})

	items := out.OfResponseFunctionCallOutputItemArray
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (image only)", len(items))
	}
	if items[0].OfInputImage == nil {
		t.Errorf("item 0 = %+v, want the image", items[0])
	}
}
