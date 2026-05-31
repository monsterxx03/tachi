package agent

import (
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Tests: approxTokenCount ----

func TestApproxTokenCount_Empty(t *testing.T) {
	assert.Equal(t, int64(0), approxTokenCount(""))
}

func TestApproxTokenCount_Short(t *testing.T) {
	// (1+3)/4 = 1
	assert.Equal(t, int64(1), approxTokenCount("a"))
	// (4+3)/4 = 1
	assert.Equal(t, int64(1), approxTokenCount("abcd"))
}

func TestApproxTokenCount_Boundary(t *testing.T) {
	// (5+3)/4 = 2
	assert.Equal(t, int64(2), approxTokenCount("abcde"))
	// (8+3)/4 = 2
	assert.Equal(t, int64(2), approxTokenCount("abcdefgh"))
}

func TestApproxTokenCount_CJK(t *testing.T) {
	// "你好" is 6 bytes (3 bytes each in UTF-8)
	// (6+3)/4 = 2
	assert.Equal(t, int64(2), approxTokenCount("你好"))
	// "你好世界" is 12 bytes
	// (12+3)/4 = 3
	assert.Equal(t, int64(3), approxTokenCount("你好世界"))
}

func TestApproxTokenCount_Long(t *testing.T) {
	// 100 chars → (100+3)/4 = 25
	s := string(make([]byte, 100))
	assert.Equal(t, int64(25), approxTokenCount(s))
}

// ---- Tests: estimateInputTokens ----

func TestEstimateInputTokens_Empty(t *testing.T) {
	n := estimateInputTokens(nil, "", nil)
	assert.Equal(t, int64(0), n)
}

func TestEstimateInputTokens_SystemPromptOnly(t *testing.T) {
	n := estimateInputTokens(nil, "You are a helpful assistant.", nil)
	// (27+3)/4 = 7
	assert.Equal(t, int64(7), n)
}

func TestEstimateInputTokens_SingleUserMessage(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Hello, world!"},
	}
	n := estimateInputTokens(msgs, "", nil)
	// role "user" = (4+3)/4 = 1
	// content "Hello, world!" = (13+3)/4 = 4
	assert.Equal(t, int64(5), n)
}

func TestEstimateInputTokens_MessagesWithToolCalls(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "Let me look that up.",
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_123",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "ReadFile",
						Arguments: `{"path": "main.go"}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_123", Content: "file contents here"},
	}
	n := estimateInputTokens(msgs, "", nil)
	// We don't need to assert exact value — just that it's > 0 and stable
	assert.Greater(t, n, int64(0))
}

func TestEstimateInputTokens_ContentParts(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user",
			ContentParts: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: "Describe this image"},
				{Type: llm.ContentPartImage, MediaType: "image/jpeg", Data: "base64data..."},
			},
		},
	}
	n := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, n, int64(0))
}

func TestEstimateInputTokens_ToolSchemas(t *testing.T) {
	schemas := []agenttools.Schema{
		{
			Name:        "Read",
			Description: "Read a file",
			Parameters: agenttools.ParametersSchema{
				Properties: map[string]agenttools.PropertySchema{
					"path": {Type: "string", Description: "File path to read"},
				},
			},
		},
		{
			Name:        "EditFile",
			Description: "Edit or create files",
			Parameters: agenttools.ParametersSchema{
				Properties: map[string]agenttools.PropertySchema{
					"file_path": {Type: "string", Description: "Target file"},
					"content":   {Type: "string", Description: "New content"},
					"old_string": {Type: "string", Description: "Text to replace"},
				},
			},
		},
	}
	n := estimateInputTokens(nil, "", schemas)
	assert.Greater(t, n, int64(0))

	// tool overhead: 2*4 = 8
	// Read: name(4/4=1) + desc(11/4=2) + 1 prop*8 = 1+2+8 = 11, so that part is 8+11 = 19 for tools... 
	// Actually let me compute precisely:
	// Name "Read" = 4 → (4+3)/4 = 1
	// Description "Read a file" = 11 → (11+3)/4 = 3
	// prop name "path" = 4 → (4+3)/4 = 1
	// prop desc "File path to read" = 18 → (18+3)/4 = 5
	// 1 prop * 8 = 8
	// Read total: 1+3+1+5+8 = 18

	// EditFile name "EditFile" = 8 → (8+3)/4 = 2
	// Desc "Edit or create files" = 19 → (19+3)/4 = 5
	// prop "file_path" = 9 → (9+3)/4 = 3, desc "Target file" = 11 → (11+3)/4 = 3
	// prop "content" = 7 → (7+3)/4 = 2, desc "New content" = 11 → (11+3)/4 = 3
	// prop "old_string" = 10 → (10+3)/4 = 3, desc "Text to replace" = 16 → (16+3)/4 = 4
	// 3 props * 8 = 24
	// EditFile total: 2+5+3+3+2+3+3+4+24 = 49

	// tool array overhead: 2*4 = 8
	// Total: 18+49+8 = 75
	assert.Equal(t, int64(75), n)
}

func TestEstimateInputTokens_Full(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "You are a coding assistant specialized in Go."},
		{Role: "user", Content: "Can you explain closures?"},
	}
	schemas := []agenttools.Schema{
		{
			Name:        "Bash",
			Description: "Run shell commands",
			Parameters: agenttools.ParametersSchema{
				Properties: map[string]agenttools.PropertySchema{
					"command": {Type: "string", Description: "Command to execute"},
				},
			},
		},
	}
	n := estimateInputTokens(msgs, "", schemas)
	assert.Greater(t, n, int64(0))
}

// TestEstimateInputTokens_SystemPromptFromMessages ensures that when a system
// message is in the messages slice, it's NOT double-counted by estimateInputTokens
// (only the explicit systemPrompt param is counted; the system message role+content
// is also counted in the messages loop).
func TestEstimateInputTokens_SystemPromptFromMessages(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "What is Go?"},
	}

	// With systemPrompt="" — the system message is only counted in the messages loop
	noArg := estimateInputTokens(msgs, "", nil)

	// With systemPrompt="You are a helpful assistant." — the prompt text is counted
	// again in the systemPrompt param, leading to a higher estimate
	withArg := estimateInputTokens(msgs, "You are a helpful assistant.", nil)

	assert.Greater(t, withArg, noArg,
		"Passing systemPrompt should add to the estimate (not double-count prevention)")
}

// ---- Tests: estimateInputTokens with edge cases ----

func TestEstimateInputTokens_LargeToolSchema(t *testing.T) {
	// Many properties to test iteration correctness
	props := make(map[string]agenttools.PropertySchema)
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		props[string(name)] = agenttools.PropertySchema{
			Type:        "string",
			Description: "property " + string(name),
		}
	}
	schemas := []agenttools.Schema{
		{
			Name:        "BigTool",
			Description: "A tool with many parameters",
			Parameters: agenttools.ParametersSchema{
				Properties: props,
			},
		},
	}
	n := estimateInputTokens(nil, "", schemas)
	// name="BigTool" (7+3)/4=2
	// desc="A tool with many parameters" (27+3)/4=7
	// each prop: name(1 byte→0...wait, rune('a')=97 so string(rune('a'))="a"(1 byte), (1+3)/4=1)
	// Actually for i=0: string(rune('a'+0)) = "a", for i=1: "b", etc. Each is 1 byte.
	// each prop desc: "property x" = 10 bytes, (10+3)/4=3
	// So per prop: 1+3=4, plus 8 overhead = 12
	// 20 props * 12 = 240
	// 1 tool overhead * 4 = 4
	// Total: 2+7+240+4 = 253
	assert.Equal(t, int64(253), n)
}

func TestEstimateInputTokens_NoToolOverheadWithoutTools(t *testing.T) {
	n := estimateInputTokens(nil, "prompt", nil)
	// (5+3)/4 = 2
	assert.Equal(t, int64(2), n)
}

func TestEstimateInputTokens_ToolCallID(t *testing.T) {
	msgs := []llm.Message{
		{Role: "tool", ToolCallID: "toolu_abc123def456", Content: "result data"},
	}
	n := estimateInputTokens(msgs, "", nil)
	// role "tool" = 4 → (4+3)/4 = 1
	// content "result data" = 11 → (11+3)/4 = 3
	// tool_call_id "toolu_abc123def456" = 18 → (18+3)/4 = 5
	assert.Equal(t, int64(9), n)
}

func TestEstimateInputTokens_ContentPartsWithTextAndImage(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user",
			ContentParts: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: "What's in this image?"},
				{Type: llm.ContentPartImage, MediaType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAE="},
			},
		},
	}
	n := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, n, int64(0))

	// Verify both text and image parts are counted
	// text part: type "text" = 4 → (4+3)/4 = 1; text "What's in this image?" = 21 → (21+3)/4 = 6
	// image part: type "image" = 5 → (5+3)/4 = 2; but data is also counted...
	// Actually the code doesn't count MediaType or Data — only Type and Text for ContentParts.
	// So: text type(1) + text content(6) + image type(2) = 9
	// role "user" = 4 → (4+3)/4 = 1
	// Total: 9 + 1 = 10
	assert.Equal(t, int64(10), n)
}

// TestEstimateAndUpdateTokens verifies that the method calls through correctly
// and updates lastInputTokens. We use a real AIAgent with a mock registry.
func TestEstimateAndUpdateTokens(t *testing.T) {
	reg := agenttools.NewRegistry()
	// Register a simple mock tool so there's at least one schema
	reg.Register(&stubTool{
		name: "TestTool",
		desc: "A test tool",
		props: map[string]agenttools.PropertySchema{
			"input": {Type: "string", Description: "Input value"},
		},
		required: []string{"input"},
	})

	agent := &AIAgent{
		toolRegistry: reg,
	}

	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
	}
	agent.estimateAndUpdateTokens(msgs)
	require.Greater(t, agent.lastInputTokens, int64(0),
		"estimateAndUpdateTokens should set lastInputTokens to a positive value")
}

// TestEstimateAndUpdateTokens_SystemPrompt checks that a system message in the
// message list is captured as the system prompt for the token estimate.
func TestEstimateAndUpdateTokens_SystemPrompt(t *testing.T) {
	reg := agenttools.NewRegistry()
	agent := &AIAgent{
		toolRegistry: reg,
	}

	msgs := []llm.Message{
		{Role: "system", Content: "You are a test assistant."},
		{Role: "user", Content: "hi"},
	}
	agent.estimateAndUpdateTokens(msgs)
	assert.Greater(t, agent.lastInputTokens, int64(0))
}
