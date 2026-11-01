package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// String args for tool calls as stored in session messages.
const bashArgs = `{"command": "ls"}`

func TestConvertSessionToLLMMessages_Anthropic_SimpleExchange(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeThinking, Content: "User says hi", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "Hi there!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "hello"},
		{
			Role:    "assistant",
			Content: "Hi there!",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "User says hi", Signature: "sig-1"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Anthropic simple exchange mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_Anthropic_ToolCall(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeThinking, Content: "Need to list files", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call_1"},
		{Type: session.MessageTypeThinking, Content: "Got file list", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Here are the files: output"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "list files"},
		{
			Role:    "assistant",
			Content: "", // no text — pure tool_use
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Need to list files", Signature: "sig-1"},
			},
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "Bash",
						Arguments: bashArgs,
					},
				},
			},
		},
		{Role: "tool", Content: "output", ToolCallID: "call_1", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Here are the files: output",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Got file list", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Anthropic tool call mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_OpenAI_PrependsThinking(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeThinking, Content: "User says hi", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "Hi there!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "openai", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "hello"},
		{
			Role:           "assistant",
			Content:        "User says hi\n\nHi there!",
			ThinkingBlocks: nil, // thinking embedded in Content
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("OpenAI prepend mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_OpenAI_ToolCall(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeThinking, Content: "Need ls", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call_1"},
		{Type: session.MessageTypeThinking, Content: "Summarize result", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Files: output"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "openai", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "list files"},
		{
			Role:    "assistant",
			Content: "Need ls", // thinking only, no assistant text
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "Bash",
						Arguments: bashArgs,
					},
				},
			},
		},
		{Role: "tool", Content: "output", ToolCallID: "call_1", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Summarize result\n\nFiles: output",
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("OpenAI tool call mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_MultipleThinkingBlocks(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "think carefully"},
		{Type: session.MessageTypeThinking, Content: "First thought", Signature: "sig-1"},
		{Type: session.MessageTypeThinking, Content: "Second thought", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Answer"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	assistant := result[1]
	if len(assistant.ThinkingBlocks) != 2 {
		t.Errorf("expected 2 thinking blocks, got %d", len(assistant.ThinkingBlocks))
	}
}

func TestConvertSessionToLLMMessages_ContinuationAfterMaxTokens(t *testing.T) {
	// When the LLM hits max_tokens, the session records:
	//   thinking → assistant (partial) → user ("Please continue...")
	// This should reconstruct as two separate messages.
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeThinking, Content: "This is a long", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "partial response"},
		{Type: session.MessageTypeUser, Content: "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."},
		{Type: session.MessageTypeThinking, Content: "Continuing", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "more text"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{
			Role:    "assistant",
			Content: "partial response",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "This is a long", Signature: "sig-1"},
			},
		},
		{Role: "user", Content: "Please continue where you left off. Break your output into smaller chunks to avoid hitting the output token limit."},
		{
			Role:    "assistant",
			Content: "more text",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Continuing", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("continuation mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_ParallelToolCalls(t *testing.T) {
	// When the LLM calls multiple tools in a single response, the session records:
	//   thinking → tool_call_A → tool_result_A → tool_call_B → tool_result_B → thinking → assistant
	// Both tool calls should be grouped under ONE assistant message.
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "run two commands"},
		{Type: session.MessageTypeThinking, Content: "Need both status and diff", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git status"}`, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "status output", ToolCallID: "call_1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git diff"}`, ToolCallID: "call_2"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "diff output", ToolCallID: "call_2"},
		{Type: session.MessageTypeThinking, Content: "Summarize results", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "Here's the summary"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "run two commands"},
		{
			Role:    "assistant",
			Content: "",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Need both status and diff", Signature: "sig-1"},
			},
			ToolCalls: []llm.ToolCall{
				{
					ID:       "call_1",
					Type:     "function",
					Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command": "git status"}`},
				},
				{
					ID:       "call_2",
					Type:     "function",
					Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command": "git diff"}`},
				},
			},
		},
		{Role: "tool", Content: "status output", ToolCallID: "call_1", Name: "Bash"},
		{Role: "tool", Content: "diff output", ToolCallID: "call_2", Name: "Bash"},
		{
			Role:    "assistant",
			Content: "Here's the summary",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "Summarize results", Signature: "sig-2"},
			},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("parallel tool calls mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

func TestConvertSessionToLLMMessages_SkipsConfirm(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeConfirm},
		{Type: session.MessageTypeAssistant, Content: "Hi!"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 messages (confirm skipped), got %d: %+v", len(result), result)
	}
}

// reminderBlock renders a realistic reminder block: every producer renders through
// systemreminder.RenderPieces, which is why a block ends with a newline.
func reminderBlock(line string) string {
	return systemreminder.RenderPieces([]systemreminder.Piece{{Name: "test", Lines: []string{line}}})
}

// TestConvertSessionToLLMMessages_ReminderSeamMatchesTheLiveWrap pins the byte-exact
// seam between a turn's reminder block and the user's text.
//
// The live turn sends the two as one string, block + text (systemreminder.WrapUserMessage
// prepends a block that already ends with a newline). A reload has to rebuild the same
// bytes: the conversion used to insert one more "\n" between them, which is invisible in
// the UI but changes the REQUEST, so the provider's prompt cache missed on every turn
// appended since the last reload — measured on a real session as a 47k-token re-read.
func TestConvertSessionToLLMMessages_ReminderSeamMatchesTheLiveWrap(t *testing.T) {
	block := reminderBlock("Current date: Sunday, September 13, 2026 12:51:24 CST")
	const text = "看看竞品呢，还有什么值得做的功能"

	result, err := ConvertSessionToLLMMessages([]session.Message{
		{Type: session.MessageTypeReminder, Content: block},
		{Type: session.MessageTypeUser, Content: text},
	}, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected one user message, got %d: %+v", len(result), result)
	}
	if want := block + text; result[0].Content != want {
		t.Errorf("the seam must match the live wrap byte for byte\n  got:  %q\n  want: %q", result[0].Content, want)
	}
	if strings.Contains(result[0].Content, systemreminder.ReminderBlockClose+"\n\n") {
		t.Error("a blank line was inserted after the reminder block: the reloaded history no longer matches what was sent")
	}
}

// TestConvertSessionToLLMMessages_MidTurnReminderStaysWhereItWasSent covers the other
// half: the loop injects a reminder mid-turn as a user-role message of its own, after
// that iteration's tool results (injectLoopReminders), and a reload has to put it back
// there. Buffering it onto the NEXT user message instead moved stale content — a finished
// background task, an old plan id — onto a later turn.
func TestConvertSessionToLLMMessages_MidTurnReminderStaysWhereItWasSent(t *testing.T) {
	prefix, midTurn := reminderBlock("Current date: ... 10:00:00"), reminderBlock(`Background task "x" finished successfully`)
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeReminder, Content: prefix},
		{Type: session.MessageTypeUser, Content: "do the thing"},
		{Type: session.MessageTypeThinking, Content: "first", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "working"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call-1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call-1"},
		{Type: session.MessageTypeReminder, Content: midTurn, Iteration: 2, Seq: 3},
		{Type: session.MessageTypeThinking, Content: "second", Signature: "sig-2"},
		{Type: session.MessageTypeAssistant, Content: "done"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: prefix + "do the thing"},
		{
			Role:           "assistant",
			Content:        "working",
			ThinkingBlocks: []llm.ThinkingBlock{{Type: "thinking", Thinking: "first", Signature: "sig-1"}},
			ToolCalls: []llm.ToolCall{{
				ID: "call-1", Type: "function",
				Function: llm.ToolCallFunction{Name: "Bash", Arguments: bashArgs},
			}},
		},
		{Role: "tool", Content: "output", ToolCallID: "call-1", Name: "Bash"},
		// The injected block keeps its own message, exactly where it was sent.
		{Role: "user", Content: midTurn},
		{
			Role:           "assistant",
			Content:        "done",
			ThinkingBlocks: []llm.ThinkingBlock{{Type: "thinking", Thinking: "second", Signature: "sig-2"}},
		},
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("mid-turn reminder mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

// TestConvertSessionToLLMMessages_MidTurnReminderAtTheEnd covers the shape with nothing
// after the block: it is still its own message, not a wrapper for a user message that
// does not exist.
func TestConvertSessionToLLMMessages_MidTurnReminderAtTheEnd(t *testing.T) {
	tail := reminderBlock("Background task finished, no turn followed")
	result, err := ConvertSessionToLLMMessages([]session.Message{
		{Type: session.MessageTypeUser, Content: "hello"},
		{Type: session.MessageTypeAssistant, Content: "hi"},
		{Type: session.MessageTypeReminder, Content: tail, Iteration: 3, Seq: 4},
	}, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	last := result[len(result)-1]
	if last.Role != "user" || last.Content != tail {
		t.Errorf("a trailing mid-turn block must survive as its own message, got %+v", last)
	}
}

// TestConvertSessionToLLMMessages_ReminderBeforeAUserMessageStaysAWrap pins the shape the
// buffer exists for, including the artifact case: consecutive reminder records all belong
// to the user message that follows them, so they stay ONE prefix rather than splitting
// into several messages.
func TestConvertSessionToLLMMessages_ReminderBeforeAUserMessageStaysAWrap(t *testing.T) {
	artifact, date := reminderBlock("近期产物：/tmp/report.html"), reminderBlock("Current date: ... 11:00:00")

	result, err := ConvertSessionToLLMMessages([]session.Message{
		{Type: session.MessageTypeReminder, Content: artifact},
		{Type: session.MessageTypeReminder, Content: date},
		{Type: session.MessageTypeUser, Content: "接着看"},
	}, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected the two blocks to ride the same user message, got %d: %+v", len(result), result)
	}
	if want := activityWrap(artifact, date) + "接着看"; result[0].Content != want {
		t.Errorf("got %q, want %q", result[0].Content, want)
	}
}

// activityWrap is reminderPrefix for the test's two blocks.
func activityWrap(blocks ...string) string {
	prefix := strings.Join(blocks, "\n")
	if !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix
}

// TestConvertSessionToLLMMessages_StrandedReminderBeforeALaterTurn covers the one shape
// position alone gets wrong: a block the loop injected mid-turn that turned out to be its
// turn's LAST record (a stop or a hard failure right after the injection). The next user
// message belongs to another turn — its iteration is 0, the block's is not — so the block
// must stay where it was sent rather than ride that message.
func TestConvertSessionToLLMMessages_StrandedReminderBeforeALaterTurn(t *testing.T) {
	stranded, next := reminderBlock("Background task finished, then the turn ended"),
		reminderBlock("Current date: ... 12:10:00")

	result, err := ConvertSessionToLLMMessages([]session.Message{
		{Type: session.MessageTypeUser, Content: "u1"},
		{Type: session.MessageTypeThinking, Content: "t", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call-1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "out", ToolCallID: "call-1"},
		{Type: session.MessageTypeReminder, Content: stranded, Iteration: 2, Seq: 3},
		{Type: session.MessageTypeReminder, Content: next},
		{Type: session.MessageTypeUser, Content: "u2"},
	}, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []llm.Message{
		{Role: "user", Content: "u1"},
		{
			Role:           "assistant",
			ThinkingBlocks: []llm.ThinkingBlock{{Type: "thinking", Thinking: "t", Signature: "sig-1"}},
			ToolCalls: []llm.ToolCall{{
				ID: "call-1", Type: "function",
				Function: llm.ToolCallFunction{Name: "Bash", Arguments: bashArgs},
			}},
		},
		{Role: "tool", Content: "out", ToolCallID: "call-1", Name: "Bash"},
		{Role: "user", Content: stranded},
		{Role: "user", Content: next + "u2"},
	}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("stranded-before-later-turn mismatch:\n  got:  %+v\n  want: %+v", result, expected)
	}
}

// TestConvertSessionToLLMMessages_ContinuationReminderKeepsItsIteration is the positive
// side of the same rule: the length-continuation prompt's reminder and the prompt itself
// are recorded with the SAME iteration, so they stay one wrapped message.
func TestConvertSessionToLLMMessages_ContinuationReminderKeepsItsIteration(t *testing.T) {
	block := reminderBlock("Output truncated; continue where you left off")
	result, err := ConvertSessionToLLMMessages([]session.Message{
		{Type: session.MessageTypeThinking, Content: "long", Signature: "sig-1"},
		{Type: session.MessageTypeAssistant, Content: "partial"},
		{Type: session.MessageTypeReminder, Content: block, Iteration: 2, Seq: 5},
		{Type: session.MessageTypeUser, Content: "Please continue.", Iteration: 2, Seq: 5},
	}, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected the reminder to ride the continuation prompt, got %d messages: %+v", len(result), result)
	}
	if want := block + "Please continue."; result[1].Content != want {
		t.Errorf("got %q, want %q", result[1].Content, want)
	}
}

// DisplayContent is DISPLAY ONLY and must never reach the provider: Content is what the model
// was actually sent, and the rebuilt history is replayed to it on every later turn. Sending the
// display text instead would hand the model a different prefix than it saw (invalidating the
// provider's prompt cache from that point) and rewrite what the history claims it was told.
//
// The two are deliberately DISJOINT here. In production the display IS a prefix of the content
// (atfile keeps the user's text and appends the file to it), so "did the display leak" would be
// unanswerable from the content alone.
func TestConvertSessionToLLMMessages_IgnoresDisplayContent(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "what-the-model-was-sent", DisplayContent: "what-the-user-typed"},
		{Type: session.MessageTypeAssistant, Content: "看过了"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(result), result)
	}
	if result[0].Content != "what-the-model-was-sent" {
		t.Errorf("user message sent to the provider = %q, want the recorded content", result[0].Content)
	}
	for _, m := range result {
		if strings.Contains(m.Content, "what-the-user-typed") {
			t.Errorf("the display text leaked into the provider's history: %q", m.Content)
		}
	}
}

// TestConvertSessionToLLMMessages_StripsOnlyTheUnansweredToolCalls pins the case a killed process
// leaves behind, which is the reason the strip exists at all.
//
// The loop records each tool CALL before it executes, and each RESULT as it finishes
// (tool_executor.go), so a process that dies in the middle of a batch leaves an assistant message
// whose calls are only PARTLY answered. The provider rejects that outright — every tool_use block
// needs its tool_result — and the old strip never noticed, because it asked "does any tool result
// follow this message?" and kept the whole batch when even one did. So the orphan rode along on
// every later request, and the session stayed broken until the record was edited by hand.
//
// The last request's leading history is the shape that matters: the user message that started the
// interrupted turn is still there, and the next turn's text is appended after this conversion.
func TestConvertSessionToLLMMessages_StripsOnlyTheUnansweredToolCalls(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "两条都跑一下"},
		{Type: session.MessageTypeThinking, Content: "两条命令一起发", Signature: "sig-1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git status"}`, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git diff"}`, ToolCallID: "call_2"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "status output", ToolCallID: "call_1"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, m := range result {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_2" {
				t.Errorf("the unanswered tool call %q is still in the history: %+v", tc.ID, result)
			}
		}
	}
	var calls []llm.ToolCall
	for _, m := range result {
		calls = append(calls, m.ToolCalls...)
	}
	if len(calls) != 1 || calls[0].ID != "call_1" {
		t.Errorf("the answered tool call must survive: got %+v", calls)
	}
	// The thinking block belongs to the same assistant message and stays: the call it was made for
	// survived, so the block is still the reasoning behind it.
	if len(result) < 2 || len(result[1].ThinkingBlocks) != 1 {
		t.Errorf("the assistant message lost its thinking block: %+v", result)
	}
}

// TestConvertSessionToLLMMessages_DropsAnAssistantMessageThatLosesEveryCall is the other end:
// when NO call of a tool-call-only assistant message can be answered, that message has nothing
// left to say. An emptied assistant message (no text, no thinking, no calls) is not a valid
// message for the provider either — and it would sit between the user's message and the next
// turn's, so it goes entirely.
func TestConvertSessionToLLMMessages_DropsAnAssistantMessageThatLosesEveryCall(t *testing.T) {
	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "看看目录"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_2"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].Role != "user" {
		t.Fatalf("expected the orphaned assistant message to be dropped, got %+v", result)
	}
}

// A call the user typed (/sh) is recorded for the transcript but is NOT history: the model never
// made it. Both halves must vanish, and the surrounding turns must come out exactly as they would
// without them — a stray tool_result would be rejected by every provider, and a synthetic tool_use
// would tell the model it ran something it never did (and shift the cached prefix).
func TestConvertSessionToLLMMessages_SkipsUserTypedToolCall(t *testing.T) {
	withEcho := []session.Message{
		{Type: session.MessageTypeUser, Content: "my name is xiao"},
		{Type: session.MessageTypeAssistant, Content: "noted"},
		// /sh echo hi — display only
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "sh_1", AutoExpand: true},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "hi", ToolCallID: "sh_1", AutoExpand: true},
		{Type: session.MessageTypeUser, Content: "what did I just say?"},
		{Type: session.MessageTypeAssistant, Content: "your name is xiao"},
	}
	without := []session.Message{
		{Type: session.MessageTypeUser, Content: "my name is xiao"},
		{Type: session.MessageTypeAssistant, Content: "noted"},
		{Type: session.MessageTypeUser, Content: "what did I just say?"},
		{Type: session.MessageTypeAssistant, Content: "your name is xiao"},
	}

	got, err := ConvertSessionToLLMMessages(withEcho, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, err := ConvertSessionToLLMMessages(without, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("/sh pair leaked into the model history:\n  got:  %+v\n  want: %+v", got, want)
	}
	for _, m := range got {
		if m.Role == "tool" {
			t.Fatalf("a tool result reached the history: %+v", m)
		}
	}

	// Only the marker decides it: the same pair WITHOUT AutoExpand is history, and must survive as a
	// well-formed assistant(tool_use) + tool(result).
	asHistory := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeToolCall, Name: "Bash", Args: bashArgs, ToolCallID: "call_1"},
		{Type: session.MessageTypeToolResult, Name: "Bash", Result: "output", ToolCallID: "call_1"},
		{Type: session.MessageTypeAssistant, Content: "done"},
	}
	kept, err := ConvertSessionToLLMMessages(asHistory, "anthropic", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept) != 4 || kept[1].Role != "assistant" || len(kept[1].ToolCalls) != 1 ||
		kept[2].Role != "tool" || kept[3].Role != "assistant" {
		t.Fatalf("an ordinary tool call stopped being history: %+v", kept)
	}
}
