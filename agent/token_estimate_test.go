package agent

import (
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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
		Config: AgentConfig{ToolRegistry: reg},
		conv:   newConvState(),
	}

	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
	}
	agent.EstimateAndUpdateTokens(nil, msgs)
	require.Greater(t, agent.LastInputEstimate(), int64(0),
		"EstimateAndUpdateTokens should set a positive token estimate")
	tb := agent.LastTokenBreakdown()
	assert.Equal(t, agent.LastInputEstimate(), tb.Total,
		"token estimate should match breakdown Total")
	assert.Greater(t, tb.UserMessages, int64(0), "user message should be broken down")
	assert.Greater(t, tb.InternalTools, int64(0), "internal tools should be broken down")
}

// TestEstimateAndUpdateTokens_SystemPrompt checks that a system message in the
// message list is captured as the system prompt for the token estimate.
func TestEstimateAndUpdateTokens_SystemPrompt(t *testing.T) {
	reg := agenttools.NewRegistry()
	agent := &AIAgent{
		Config: AgentConfig{ToolRegistry: reg},
		conv:   newConvState(),
	}

	msgs := []llm.Message{
		{Role: "system", Content: "You are a test assistant."},
		{Role: "user", Content: "hi"},
	}
	agent.EstimateAndUpdateTokens(nil, msgs)
	assert.Greater(t, agent.LastInputEstimate(), int64(0))
	tb := agent.LastTokenBreakdown()
	assert.Greater(t, tb.SystemPrompt, int64(0), "system prompt should be broken down")
	assert.Equal(t, agent.LastInputEstimate(), tb.Total)
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

// TestTokenBreakdown_ToolResultCategory verifies that "tool" role messages
// are captured in the ToolResults category.
func TestTokenBreakdown_ToolResultCategory(t *testing.T) {
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
	assert.Greater(t, tb.ToolResults, int64(0), "tool result msg should be in ToolResults category")
	assert.Equal(t, tb.Total, tb.UserMessages+tb.AssistantMessages+tb.ToolResults,
		"Total should equal user + assistant + tool results (no system prompt, no tools)")
}

// TestTokenBreakdown_OtherCategory verifies that "system" role messages
// are NOT counted in Other (they're tracked via SystemPrompt), and that
// Other only captures genuinely unrecognized roles.
func TestTokenBreakdown_OtherCategory(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "system", Content: "additional context"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.UserMessages, int64(0))
	assert.Equal(t, int64(0), tb.Other, "system msgs should NOT be in Other (counted in SystemPrompt)")
	assert.Greater(t, tb.Total, tb.UserMessages,
		"Total should include system msg tokens even though they're not in Other")
	assert.Equal(t, int64(0), tb.SystemPrompt,
		"SystemPrompt should be 0 since systemPrompt param is empty")
}

// TestTokenBreakdown_SteerIsUser verifies that "steer" messages are
// counted as user input alongside regular "user" messages.
func TestTokenBreakdown_SteerIsUser(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: llm.RoleSteer, Content: "please use Go"},
	}
	tb := estimateInputTokens(msgs, "", nil)
	assert.Greater(t, tb.UserMessages, int64(0))
	assert.Equal(t, int64(0), tb.Other, "steer msgs should not be in Other")
	assert.Equal(t, tb.Total, tb.UserMessages,
		"Total should equal user (steer merged into UserMessages)")
}

// TestContextEstimateAnchorsOnTheRealPromptSize pins the calibration that keeps the context ring
// honest. The character estimate alone is biased on mixed content — measured 0.815x (≈18% low)
// against the provider's own accounting on a long Chinese conversation full of JSON tool results —
// so the reported size is anchored on the last call's REAL prompt size and scaled by the estimate's
// movement since: the bias then applies to one turn's additions instead of the whole prompt.
func TestContextEstimateAnchorsOnTheRealPromptSize(t *testing.T) {
	c := newConvState()

	// Before any call in this process there is nothing to anchor on: the estimate is all there is.
	c.setEstimate(1000, tokenbreakdown.Breakdown{Total: 1000})
	if got := c.contextEstimate(); got != 1000 {
		t.Errorf("with no anchor the estimate itself must be reported, got %d", got)
	}

	// A call whose real prompt (2000) was bigger than the estimate made for it (1500): the real
	// number wins, because that is what the provider billed.
	c.setEstimate(1500, tokenbreakdown.Breakdown{Total: 1500})
	c.setPromptAnchor(2000, 1500)
	if got := c.contextEstimate(); got != 2000 {
		t.Errorf("the anchor must replace the estimate, got %d want 2000", got)
	}

	// The next turn adds messages: the estimate's movement is applied to the ANCHOR (×1.13 here),
	// so the reported size keeps the anchor's absolute scale instead of the estimate's bias.
	c.setEstimate(1700, tokenbreakdown.Breakdown{Total: 1700})
	if got := c.contextEstimate(); got != 2266 { // 2000 × 1700/1500
		t.Errorf("growth must scale the anchor, got %d want 2266", got)
	}

	// Compaction REPLACES the history with a summary: the estimate drops, and the reported size
	// must drop with it — an additive "+delta with a floor at the anchor" would sit on the
	// pre-compaction number, which is the one number compaction exists to bring down.
	c.setEstimate(200, tokenbreakdown.Breakdown{Total: 200})
	if got := c.contextEstimate(); got != 266 { // 2000 × 200/1500
		t.Errorf("a shrink must scale the anchor down too, got %d want 266", got)
	}
}

// TestRecordAssistantTurnAnchorsTheContextEstimate pins the WIRING, not the arithmetic: a completed
// API call is the moment the real prompt size becomes known, so that is where the anchor has to be
// taken (a test of contextEstimate alone would pass even if nothing ever called setPromptAnchor).
func TestRecordAssistantTurnAnchorsTheContextEstimate(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 5)
	defer a.Close()

	a.conv.setEstimate(1500, tokenbreakdown.Breakdown{Total: 1500})
	a.recordAssistantTurn(&RunState{}, "hi", &llm.Usage{
		InputTokens:          500,  // Anthropic: the cache-miss part alone…
		CacheReadInputTokens: 1500, // …plus what the cache served = 2000 for the prompt
	}, nil)

	if got := a.conv.contextEstimate(); got != 2000 {
		t.Errorf("a completed call must anchor the reported context size, got %d want 2000", got)
	}
}

// TestOneOffRunsDoNotMoveTheContextAnchor pins that a side-channel run leaves the main
// conversation's anchor alone. One-off runs (SkipSessionWrites: /review, /commit, dream, a channel
// ambient turn) share this agent's convState but never touch its estimate, so anchoring on their
// own prompt size would pair a side-channel prompt with the MAIN conversation's estimate and
// collapse the reported context size to the side-channel's number until the next main-turn call.
func TestOneOffRunsDoNotMoveTheContextAnchor(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 5)
	defer a.Close()

	// The main conversation's call: 2000 billed against a 1500 estimate, so the anchor is 2000.
	a.conv.setEstimate(1500, tokenbreakdown.Breakdown{Total: 1500})
	a.recordAssistantTurn(&RunState{}, "hi", &llm.Usage{InputTokens: 2000}, nil)
	if got := a.conv.contextEstimate(); got != 2000 {
		t.Fatalf("setup: the main call must anchor the report, got %d want 2000", got)
	}

	// A side-channel run reports a MUCH smaller prompt — it does not carry the conversation. It
	// must not become the number the main conversation reports.
	a.recordAssistantTurn(&RunState{SkipSessionWrites: true}, "review", &llm.Usage{InputTokens: 300}, nil)

	if got := a.conv.contextEstimate(); got != 2000 {
		t.Errorf("a one-off run must not move the anchor: got %d, want 2000 (the main conversation)", got)
	}
}

// TestAutoCompactFiresOnTheReportedSize pins that the auto-compact TRIGGER follows the number the
// meter shows, not the raw character estimate. On mixed content the two differ by ~18%, and a
// trigger that disagreed with the ring would compact while the ring still looked roomy, or hold off
// while it warned — the reader has to be able to predict it. Both directions are pinned here,
// because "fire on the anchored value" is only half of it: an OVER-counting estimate must equally
// not fire a compaction the real prompt does not justify.
func TestAutoCompactFiresOnTheReportedSize(t *testing.T) {
	autoCompactAgent := func(window, threshold float64) *AIAgent {
		enabled := true
		a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 5)
		t.Cleanup(a.Close)
		a.Config.FullConfig = &config.Config{
			Compact: config.CompactConfig{Auto: &enabled, Threshold: threshold},
		}
		a.Config.Resolved.ContextWindow = int64(window)
		return a
	}
	// The anchor: a call billed `real` against an estimate of `atAnchor`; then the estimate moves to
	// `now` (what the current prompt is estimated at). contextEstimate() = real × now/atAnchor.
	anchorThen := func(a *AIAgent, real, atAnchor, now int64) {
		a.conv.setEstimate(atAnchor, tokenbreakdown.Breakdown{})
		a.conv.setPromptAnchor(real, atAnchor)
		a.conv.setEstimate(now, tokenbreakdown.Breakdown{})
	}

	// UNDER-counting (mixed CJK + JSON): 760 raw is 76% of the 1000-token window — under the 80%
	// threshold — but the last call billed 900 and the estimate has moved 700→760 since, so the
	// prompt is really at ~977 (97%). The compaction the ring is about to warn about must fire.
	under := autoCompactAgent(1000, 0.8)
	anchorThen(under, 900, 700, 760)
	if !under.shouldAutoCompact() {
		t.Error("the trigger must fire on the reported size (977), not the raw estimate (760)")
	}

	// OVER-counting (plain English): 900 raw reads 90%, but the same prompt was billed at 500 — the
	// conversation is really at 64%, and compacting it would cut a history the ring says is fine.
	over := autoCompactAgent(1000, 0.8)
	anchorThen(over, 500, 700, 900)
	if over.shouldAutoCompact() {
		t.Error("an over-counting estimate must not fire a compaction the real prompt does not justify")
	}
}
