package transcript

import (
	"testing"
)

func TestBuilder_Empty(t *testing.T) {
	b := NewBuilder()
	tr := b.Build()
	if len(tr.Turns) != 0 {
		t.Errorf("Expected 0 turns, got %d", len(tr.Turns))
	}
}

func TestBuilder_RecordUserMessage_EmptyContent(t *testing.T) {
	b := NewBuilder()
	b.RecordUserMessage("")
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 0 {
		t.Errorf("Expected 0 turns for empty user message, got %d", len(tr.Turns))
	}
}

func TestBuilder_DrainCompletedTurns(t *testing.T) {
	b := NewBuilder()

	// Turn 1: finalized by StartTurn
	b.StartTurn()
	b.RecordThinking("first thinking")
	b.FinalizeTurn()

	// Turn 2: also finalized
	b.StartTurn()
	b.RecordText("second text")
	b.FinalizeTurn()

	// Turn 3: in progress (not finalized, not drained)
	b.StartTurn()
	b.RecordThinking("in progress...")

	// Drain should return turns 1 and 2, but not 3
	drained := b.DrainCompletedTurns()
	if len(drained) != 2 {
		t.Fatalf("Expected 2 drained turns, got %d", len(drained))
	}
	if drained[0].ID != 1 {
		t.Errorf("First drained turn ID = %d, want 1", drained[0].ID)
	}
	if drained[1].ID != 2 {
		t.Errorf("Second drained turn ID = %d, want 2", drained[1].ID)
	}

	// After drain, Build() should show 0 finalized turns (turn 3 is still in progress)
	tr := b.Build()
	if len(tr.Turns) != 0 {
		t.Errorf("Expected 0 turns after drain, got %d", len(tr.Turns))
	}

	// Second drain should be empty
	drained2 := b.DrainCompletedTurns()
	if len(drained2) != 0 {
		t.Errorf("Second drain should be empty, got %d", len(drained2))
	}

	// After finalizing turn 3, drain should capture it
	b.FinalizeTurn()
	drained3 := b.DrainCompletedTurns()
	if len(drained3) != 1 {
		t.Fatalf("Third drain should have 1 turn, got %d", len(drained3))
	}
	if drained3[0].ID != 3 {
		t.Errorf("Third drained turn ID = %d, want 3", drained3[0].ID)
	}
}

func TestBuilder_RecordUserMessage(t *testing.T) {
	b := NewBuilder()
	b.RecordUserMessage("Hello, what files are in this project?")

	// User turn is finalized immediately, so it's in the snapshot.
	tr := b.Build()
	if len(tr.Turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(tr.Turns))
	}
	events := tr.Turns[0].Events
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != EventUser {
		t.Errorf("Expected EventUser, got %s", ev.Type)
	}
	if ev.Content != "Hello, what files are in this project?" {
		t.Errorf("Unexpected content: %q", ev.Content)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestBuilder_RecordUserMessage_ThenAgentTurn(t *testing.T) {
	b := NewBuilder()
	b.RecordUserMessage("read foo.go")

	// Agent turn follows
	b.StartTurn()
	b.RecordThinking("Let me read the file...")
	b.RecordText("Here is the content.")
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 2 {
		t.Fatalf("Expected 2 turns (user + agent), got %d", len(tr.Turns))
	}
	if tr.Turns[0].Events[0].Type != EventUser {
		t.Errorf("Turn 0 should be user, got %s", tr.Turns[0].Events[0].Type)
	}
	if tr.Turns[1].Events[0].Type != EventThinking {
		t.Errorf("Turn 1 should be agent, got %s", tr.Turns[1].Events[0].Type)
	}
	// Turn IDs should be sequential
	if tr.Turns[0].ID != 1 {
		t.Errorf("User turn ID = %d, want 1", tr.Turns[0].ID)
	}
	if tr.Turns[1].ID != 2 {
		t.Errorf("Agent turn ID = %d, want 2", tr.Turns[1].ID)
	}
}

func TestBuilder_StartTurn(t *testing.T) {
	b := NewBuilder()

	// Turn 1: record event, then StartTurn auto-finalizes
	b.StartTurn()
	b.RecordThinking("x")

	// StartTurn for turn 2 should auto-finalize turn 1
	b.StartTurn()

	tr := b.Build()
	// Turn 1 finalized, turn 2 is current (not finalized), so 1 turn
	if len(tr.Turns) != 1 {
		t.Errorf("Expected 1 finalized turn, got %d", len(tr.Turns))
	}
	if tr.Turns[0].ID != 1 {
		t.Errorf("Expected turn ID 1, got %d", tr.Turns[0].ID)
	}

	// After finalizing turn 2, we should have 2 turns
	b.RecordText("turn 2")
	b.FinalizeTurn()
	tr2 := b.Build()
	if len(tr2.Turns) != 2 {
		t.Errorf("Expected 2 turns after finalize, got %d", len(tr2.Turns))
	}
}

func TestBuilder_RecordThinking(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordThinking("Need to search files...")
	b.RecordThinking("Looking at lib/...")
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(tr.Turns))
	}
	events := tr.Turns[0].Events
	if len(events) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(events))
	}
	if events[0].Type != EventThinking {
		t.Errorf("Expected thinking, got %s", events[0].Type)
	}
	if events[0].Content != "Need to search files..." {
		t.Errorf("Unexpected thinking content: %q", events[0].Content)
	}
}

func TestBuilder_RecordText(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordText("Here is the answer.")
	b.FinalizeTurn()

	tr := b.Build()
	events := tr.Turns[0].Events
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventText {
		t.Errorf("Expected text, got %s", events[0].Type)
	}
	if events[0].Content != "Here is the answer." {
		t.Errorf("Unexpected content: %q", events[0].Content)
	}
}

func TestBuilder_RecordText_EmptyContent(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordText("")
	b.RecordThinking("")
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 0 {
		t.Errorf("Empty turn should not be added: got %d turns", len(tr.Turns))
	}
}

func TestBuilder_RecordToolCall_Simple(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()

	rec := b.RecordToolCall("ReadFile", `{"path":"/foo/bar.go"}`)
	rec.RecordToolResult("package main\n\nfunc main() {}", false)
	b.FinalizeTurn()

	tr := b.Build()
	events := tr.Turns[0].Events
	if len(events) != 2 {
		t.Fatalf("Expected 2 events (call + result), got %d", len(events))
	}
	if events[0].Type != EventToolCall {
		t.Errorf("Expected tool_call, got %s", events[0].Type)
	}
	if events[0].Name != "ReadFile" {
		t.Errorf("Expected ReadFile, got %s", events[0].Name)
	}
	if events[0].Args != `{"path":"/foo/bar.go"}` {
		t.Errorf("Unexpected args: %q", events[0].Args)
	}
	if events[0].Children != nil {
		t.Error("Non-SubAgent should have nil Children")
	}
	if events[1].Type != EventToolResult {
		t.Errorf("Expected tool_result, got %s", events[1].Type)
	}
	if events[1].Name != "ReadFile" {
		t.Errorf("Expected ReadFile in result, got %s", events[1].Name)
	}
	if events[1].Content != "package main\n\nfunc main() {}" {
		t.Errorf("Unexpected result: %q", events[1].Content)
	}
	if events[1].IsError {
		t.Error("Expected IsError=false")
	}
}

func TestBuilder_RecordToolCall_Error(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()

	rec := b.RecordToolCall("ReadFile", `{"path":"/missing"}`)
	rec.RecordToolResult("Error: file not found", true)
	b.FinalizeTurn()

	tr := b.Build()
	events := tr.Turns[0].Events
	if len(events) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(events))
	}
	if !events[1].IsError {
		t.Error("Expected IsError=true")
	}
}

func TestBuilder_SubAgent(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()

	// Parent records a SubAgent tool call
	rec := b.RecordToolCall("SubAgent", `{"prompt":"search for session files"}`)
	sub := rec.SubBuilder()

	// Sub-agent records its own events
	sub.StartTurn()
	sub.RecordThinking("I need to search the session package...")
	subRec := sub.RecordToolCall("Glob", `{"pattern":"session/**/*.go"}`)
	subRec.RecordToolResult("session/store.go\nsession/manager.go", false)
	sub.FinalizeTurn()

	sub.StartTurn()
	sub.RecordText("Found 2 session files.")
	sub.FinalizeTurn()

	// Complete the parent tool call
	rec.RecordToolResult("Found session/store.go and session/manager.go", false)

	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 1 {
		t.Fatalf("Expected 1 parent turn, got %d", len(tr.Turns))
	}

	parentEvents := tr.Turns[0].Events
	// Parent events: tool_call(SubAgent) + tool_result(SubAgent)
	if len(parentEvents) != 2 {
		t.Fatalf("Expected 2 parent events, got %d", len(parentEvents))
	}

	// Check the tool_call event has children
	tc := parentEvents[0]
	if tc.Type != EventToolCall {
		t.Errorf("Expected tool_call, got %s", tc.Type)
	}
	if tc.Name != "SubAgent" {
		t.Errorf("Expected SubAgent, got %s", tc.Name)
	}
	if len(tc.Children) == 0 {
		t.Fatal("Expected children for SubAgent call")
	}

	// Children should contain sub-agent's events from both turns, flattened
	children := tc.Children
	// Turn 1: thinking + tool_call + tool_result = 3
	// Turn 2: text = 1
	// Total: 4
	if len(children) != 4 {
		t.Fatalf("Expected 4 children events, got %d: %+v", len(children), children)
	}
	if children[0].Type != EventThinking {
		t.Errorf("Expected thinking as first child, got %s", children[0].Type)
	}
	if children[1].Type != EventToolCall {
		t.Errorf("Expected tool_call as second child, got %s", children[1].Type)
	}
}

func TestBuilder_SubAgent_SubBuilderReturnsSameInstance(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	rec := b.RecordToolCall("SubAgent", `{}`)
	s1 := rec.SubBuilder()
	s2 := rec.SubBuilder()
	if s1 != s2 {
		t.Error("SubBuilder() should return the same instance on multiple calls")
	}
}

func TestBuilder_SubBuilder_NilForRegularTool(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	rec := b.RecordToolCall("ReadFile", `{"path":"foo"}`)
	sub := rec.SubBuilder()
	// Non-SubAgent tool calls should not get a sub-builder.
	if sub != nil {
		t.Error("SubBuilder should return nil for non-SubAgent tool calls")
	}
}

func TestBuilder_MultipleTurns(t *testing.T) {
	b := NewBuilder()

	// Turn 1: thinking + text
	b.StartTurn()
	b.RecordThinking("Let me check...")
	b.RecordText("The answer is 42.")
	b.FinalizeTurn()

	// Turn 2: tool call
	b.StartTurn()
	rec := b.RecordToolCall("ReadFile", `{"path":"ans.txt"}`)
	rec.RecordToolResult("42", false)
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 2 {
		t.Fatalf("Expected 2 turns, got %d", len(tr.Turns))
	}
	if tr.Events() != 4 {
		t.Errorf("Expected 4 total events, got %d", tr.Events())
	}
}

func TestBuilder_BuildReturnsSnapshot(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordThinking("first")
	b.FinalizeTurn()

	snap1 := b.Build()
	if len(snap1.Turns) != 1 {
		t.Fatalf("Snapshot should have 1 turn")
	}

	// Add another turn
	b.StartTurn()
	b.RecordText("second")
	b.FinalizeTurn()

	snap2 := b.Build()
	if len(snap2.Turns) != 2 {
		t.Fatalf("Second snapshot should have 2 turns")
	}
	// snap1 should be unchanged (it's a copy)
	if len(snap1.Turns) != 1 {
		t.Errorf("First snapshot was mutated: got %d turns", len(snap1.Turns))
	}
}

func TestBuilder_RecordToolCall_AutoCreateTurn(t *testing.T) {
	// Calling RecordToolCall without StartTurn should auto-create a turn
	b := NewBuilder()
	rec := b.RecordToolCall("ReadFile", `{"path":"x"}`)
	rec.RecordToolResult("ok", false)
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(tr.Turns))
	}
	if len(tr.Turns[0].Events) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(tr.Turns[0].Events))
	}
}

func TestBuilder_DoubleRecordToolResult(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	rec := b.RecordToolCall("ReadFile", `{}`)
	rec.RecordToolResult("first", false)
	// Second call should be a no-op
	rec.RecordToolResult("second", false)
	b.FinalizeTurn()

	tr := b.Build()
	events := tr.Turns[0].Events
	if len(events) != 2 {
		t.Fatalf("Expected 2 events (one result), got %d", len(events))
	}
	if events[1].Content != "first" {
		t.Errorf("Expected 'first', got %q", events[1].Content)
	}
}

func TestBuilder_FinalizeTurn_Idempotent(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordThinking("x")
	b.FinalizeTurn()
	b.FinalizeTurn() // second call should be safe

	tr := b.Build()
	if len(tr.Turns) != 1 {
		t.Errorf("Expected 1 turn, got %d", len(tr.Turns))
	}
}

func TestBuilder_FinalizeTurn_EmptyTurn(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	// No events recorded
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 0 {
		t.Errorf("Empty turn should not be added: got %d turns", len(tr.Turns))
	}
}

func TestBuilder_Timestamp(t *testing.T) {
	b := NewBuilder()
	b.StartTurn()
	b.RecordThinking("test")
	b.FinalizeTurn()

	tr := b.Build()
	events := tr.Turns[0].Events
	if events[0].Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestBuilder_TurnIDsAreSequential(t *testing.T) {
	b := NewBuilder()
	for i := range 5 {
		b.StartTurn()
		b.RecordText("turn")
		b.FinalizeTurn()
		_ = i
	}

	tr := b.Build()
	if len(tr.Turns) != 5 {
		t.Fatalf("Expected 5 turns, got %d", len(tr.Turns))
	}
	for i, turn := range tr.Turns {
		if turn.ID != i+1 {
			t.Errorf("Turn %d: expected ID %d, got %d", i, i+1, turn.ID)
		}
	}
}

func TestBuilder_Reset(t *testing.T) {
	b := NewBuilder()
	b.RecordUserMessage("session 1")
	b.StartTurn()
	b.RecordThinking("thinking in s1")
	b.FinalizeTurn()

	tr := b.Build()
	if len(tr.Turns) != 2 {
		t.Fatalf("Expected 2 turns before reset, got %d", len(tr.Turns))
	}

	b.Reset()
	tr2 := b.Build()
	if len(tr2.Turns) != 0 {
		t.Errorf("Expected 0 turns after reset, got %d", len(tr2.Turns))
	}

	// After reset, turn IDs should restart from 1
	b.RecordUserMessage("session 2")
	b.StartTurn()
	b.RecordText("reply")
	b.FinalizeTurn()

	tr3 := b.Build()
	if len(tr3.Turns) != 2 {
		t.Fatalf("Expected 2 turns after reset, got %d", len(tr3.Turns))
	}
	if tr3.Turns[0].ID != 1 {
		t.Errorf("Turn ID should restart, got %d", tr3.Turns[0].ID)
	}
	if tr3.Turns[1].ID != 2 {
		t.Errorf("Second turn ID should be 2, got %d", tr3.Turns[1].ID)
	}
}

func TestBuilder_ThreadSafety_Basic(t *testing.T) {
	// Basic test: concurrent reads via Build while writing.
	b := NewBuilder()
	b.StartTurn()

	// Write and build concurrently
	done := make(chan bool, 2)
	go func() {
		for range 100 {
			b.RecordThinking("concurrent thinking")
		}
		done <- true
	}()
	go func() {
		for range 100 {
			_ = b.Build()
		}
		done <- true
	}()

	<-done
	<-done

	// Should not panic
	b.FinalizeTurn()
	_ = b.Build()
}