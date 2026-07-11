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
	// "你好" is 2 CJK chars → 2 tokens (1:1 tokenization)
	assert.Equal(t, int64(2), approxTokenCount("你好"))
	// "你好世界" is 4 CJK chars → 4 tokens (old chars/4 gave 3, was an underestimate)
	assert.Equal(t, int64(4), approxTokenCount("你好世界"))
}

func TestApproxTokenCount_Long(t *testing.T) {
	// 100 ASCII alphanumeric chars in one word → (100+3)/4 = 25
	var buf [100]byte
	for i := range buf {
		buf[i] = 'x'
	}
	s := string(buf[:])
	assert.Equal(t, int64(25), approxTokenCount(s))
}

// ---- Tests: estimateInputTokens ----

func TestEstimateInputTokens_Empty(t *testing.T) {
	tb := estimateInputTokens(nil, "", nil)
	assert.Equal(t, int64(0), tb.Total)
	assert.Equal(t, int64(0), tb.SystemPrompt)
	assert.Equal(t, int64(0), tb.InternalTools)
	assert.Equal(t, int64(0), tb.MCPTools)
	assert.Equal(t, int64(0), tb.UserMessages)
	assert.Equal(t, int64(0), tb.AssistantMessages)
}

func TestEstimateInputTokens_SystemPromptOnly(t *testing.T) {
	tb := estimateInputTokens(nil, "You are a helpful assistant.", nil)
	// "You"=1 + "are"=1 + "a"=1 + "helpful"=2 + "assistant"=3 + "."=1 = 9
	// (old chars/4 gave 7, underestimated punctuation)
	assert.Equal(t, int64(9), tb.Total)
	assert.Equal(t, int64(9), tb.SystemPrompt)
	assert.Equal(t, int64(0), tb.InternalTools)
	assert.Equal(t, int64(0), tb.MCPTools)
}

func TestEstimateInputTokens_SingleUserMessage(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Hello, world!"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	// role "user" = 1 (4 alphanumeric chars)
	// content: "Hello"=2 + ","=1 + "world"=2 + "!"=1 = 6
	// Total = 1 + 6 = 7 (old chars/4 gave 5, underestimated punctuation)
	assert.Equal(t, int64(7), tb.Total)
	assert.Equal(t, int64(7), tb.UserMessages)
	assert.Equal(t, int64(0), tb.AssistantMessages)
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
	tb := estimateInputTokens(msgs, "", nil)
	// We don't need to assert exact value — just that it's > 0 and stable
	assert.Greater(t, tb.Total, int64(0))
	assert.Greater(t, tb.AssistantMessages, int64(0), "assistant message should be counted")
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
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.Total, int64(0))
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
					"path":       {Type: "string", Description: "Target file"},
					"content":    {Type: "string", Description: "New content"},
					"old_string": {Type: "string", Description: "Text to replace"},
				},
			},
		},
	}
	tb := estimateInputTokens(nil, "", schemas)
	assert.Greater(t, tb.Total, int64(0))

	// Read: name=1 + desc("Read"=1+"a"=1+"file"=1)=3 + prop("path"=1 + desc("File"=1+"path"=1+"to"=1+"read"=1)=4) + overhead 8 = 17
	// EditFile: name=2 + desc("Edit"=1+"or"=1+"create"=2+"files"=2)=6
	//   + "path"(1+desc("Target"=2+"file"=1)=3) + "content"(2+desc("New"=1+"content"=2)=3)
	//   + "old_string"("old"=1+"_"=1+"string"=2=4 + desc("Text"=1+"to"=1+"replace"=2)=4) + 3*8 overhead = 49
	// tool array overhead: 2*4 = 8
	// Total: 17+49+8 = 74
	assert.Equal(t, int64(74), tb.Total)
	assert.Equal(t, int64(74), tb.InternalTools, "both tools are built-in (no mcp__ prefix)")
	assert.Equal(t, int64(0), tb.MCPTools)
	assert.Equal(t, int64(0), tb.SystemPrompt)
	assert.Equal(t, int64(0), tb.UserMessages)
	assert.Equal(t, int64(0), tb.AssistantMessages)
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
	tb := estimateInputTokens(msgs, "", schemas)
	assert.Greater(t, tb.Total, int64(0))
	assert.Greater(t, tb.UserMessages, int64(0), "user message should be counted")
	assert.Greater(t, tb.InternalTools, int64(0), "Bash is a built-in tool")
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

	assert.Greater(t, withArg.Total, noArg.Total,
		"Passing systemPrompt should add to the estimate (not double-count prevention)")
}

// ---- Tests: estimateInputTokens with edge cases ----

func TestEstimateInputTokens_LargeToolSchema(t *testing.T) {
	// Many properties to test iteration correctness
	props := make(map[string]agenttools.PropertySchema)
	for i := range 20 {
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
	tb := estimateInputTokens(nil, "", schemas)
	// name="BigTool" (7+3)/4=2
	// desc="A tool with many parameters" (27+3)/4=7
	// each prop: name(1 byte), (1+3)/4=1; desc "property x" (10), (10+3)/4=3
	// So per prop: 1+3=4, plus 8 overhead = 12
	// 20 props * 12 = 240
	// 1 tool overhead * 4 = 4
	// Total: 2+7+240+4 = 253
	assert.Equal(t, int64(253), tb.Total)
	assert.Equal(t, int64(253), tb.InternalTools, "BigTool is not an MCP tool")
	assert.Equal(t, int64(0), tb.MCPTools)
}

func TestEstimateInputTokens_NoToolOverheadWithoutTools(t *testing.T) {
	tb := estimateInputTokens(nil, "prompt", nil)
	// (6+3)/4 = 2
	assert.Equal(t, int64(2), tb.Total)
	assert.Equal(t, int64(2), tb.SystemPrompt)
}

func TestEstimateInputTokens_ToolCallID(t *testing.T) {
	msgs := []llm.Message{
		{Role: "tool", ToolCallID: "toolu_abc123def456", Content: "result data"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	// role "tool" = 1 (4 alphanumeric chars)
	// content: "result"=2 + "data"=1 = 3
	// tool_call_id: "toolu"=2 + "_"=1 + "abc123def456"=3 = 6
	// Total = 1 + 3 + 6 = 10 (old chars/4 gave 9)
	assert.Equal(t, int64(10), tb.Total)
	// Role "tool" is not "user" or "assistant", so it appears only in Total
	assert.Equal(t, int64(0), tb.UserMessages)
	assert.Equal(t, int64(0), tb.AssistantMessages)
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
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.Total, int64(0))

	// text part: type "text"=1 + text "What's in this image?"
	//   "What"=1 + "'"=1 + "s"=1 + "in"=1 + "this"=1 + "image"=2 + "?"=1 = 8
	// image part: type "image"=2 + text ""=0 = 2
	// role "user" = 1
	// Total: 1 + 8 + 2 + 1 = 12 (old chars/4 gave 10)
	assert.Equal(t, int64(12), tb.Total)
	assert.Equal(t, int64(12), tb.UserMessages)
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
	agent.EstimateAndUpdateTokens(msgs)
	require.Greater(t, agent.lastInputTokens, int64(0),
		"estimateAndUpdateTokens should set lastInputTokens to a positive value")
	tb := agent.LastTokenBreakdown()
	assert.Equal(t, agent.lastInputTokens, tb.Total,
		"lastInputTokens should match breakdown Total")
	assert.Greater(t, tb.UserMessages, int64(0), "user message should be broken down")
	assert.Greater(t, tb.InternalTools, int64(0), "internal tools should be broken down")
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
	agent.EstimateAndUpdateTokens(msgs)
	assert.Greater(t, agent.lastInputTokens, int64(0))
	tb := agent.LastTokenBreakdown()
	assert.Greater(t, tb.SystemPrompt, int64(0), "system prompt should be broken down")
	assert.Equal(t, agent.lastInputTokens, tb.Total)
}

// TestTokenBreakdown_MCPTools verifies that MCP-prefixed tool schemas are
// categorized under MCPTools instead of InternalTools.
func TestTokenBreakdown_MCPTools(t *testing.T) {
	schemas := []agenttools.Schema{
		{
			Name:        "Bash",
			Description: "Run bash commands",
			Parameters: agenttools.ParametersSchema{
				Properties: map[string]agenttools.PropertySchema{
					"command": {Type: "string", Description: "Command to execute"},
				},
			},
		},
		{
			Name:        "mcp__postgres__query",
			Description: "Query a PostgreSQL database",
			Parameters: agenttools.ParametersSchema{
				Properties: map[string]agenttools.PropertySchema{
					"sql": {Type: "string", Description: "SQL query"},
				},
			},
		},
	}
	tb := estimateInputTokens(nil, "", schemas)
	assert.Greater(t, tb.InternalTools, int64(0), "Bash should be in InternalTools")
	assert.Greater(t, tb.MCPTools, int64(0), "mcp__postgres__query should be in MCPTools")
	assert.GreaterOrEqual(t, tb.Total, tb.SystemPrompt+tb.InternalTools+tb.MCPTools+tb.UserMessages+tb.AssistantMessages,
		"Total should be >= sum of named categories (may include uncategorized messages)")
}

// TestTokenBreakdown_MixedRoles verifies that user and assistant messages
// are correctly attributed to their respective categories.
func TestTokenBreakdown_MixedRoles(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
		{Role: "user", Content: "How are you?"},
		{Role: "assistant", Content: "I'm doing well, thanks!"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.UserMessages, int64(0), "user messages should be counted")
	assert.Greater(t, tb.AssistantMessages, int64(0), "assistant messages should be counted")
	assert.Equal(t, tb.Total, tb.UserMessages+tb.AssistantMessages,
		"Total should equal sum of user + assistant (no system prompt, no tools)")
}

// TestTokenBreakdown_ToolResultNotInUserMessages verifies that "tool" role
// messages don't leak into UserMessages — they contribute only to Total.
func TestTokenBreakdown_ToolResultNotInUserMessages(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "run a command"},
		{Role: "assistant", Content: "Running...", ToolCalls: []llm.ToolCall{
			{ID: "call_1", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "file1.txt\nfile2.txt"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.UserMessages, int64(0))
	assert.Greater(t, tb.AssistantMessages, int64(0))
	// Total should be > user + assistant because "tool" role msg is counted in Total only
	assert.Greater(t, tb.Total, tb.UserMessages+tb.AssistantMessages,
		"tool result msg should be in Total but not in UserMessages/AssistantMessages")
}
