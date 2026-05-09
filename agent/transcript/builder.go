package transcript

import (
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
)

// Builder incrementally constructs a Transcript during agent execution.
// It is safe for concurrent use by the main agent goroutine and sub-agent
// goroutines — each goroutine writes to its own turn within its own builder
// instance, while the parent builder's RecordToolResult only runs after
// the sub-agent completes.
type Builder struct {
	mu     sync.Mutex
	turns  []Turn
	curTurn *Turn
	nextID int
}

// NewBuilder creates a new transcript builder.
func NewBuilder() *Builder {
	return &Builder{nextID: 1}
}

// Reset clears all recorded turns and resets the turn counter.
// Use when starting a new session to avoid mixing transcripts across sessions.
func (b *Builder) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.turns = nil
	b.curTurn = nil
	b.nextID = 1
}

// StartTurn begins a new LLM API call turn. Call this at the start of every
// iteration of the agent loop. It also finalizes any previously pending turn.
func (b *Builder) StartTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Finalize any pending turn (idempotent; only adds if has events).
	if b.curTurn != nil && len(b.curTurn.Events) > 0 {
		b.turns = append(b.turns, *b.curTurn)
	}

	b.curTurn = &Turn{ID: b.nextID}
	b.nextID++
}

// DrainCompletedTurns returns and removes all completed turns (those finalized
// by StartTurn or RecordUserMessage). The in-progress curTurn is not included.
// Call after each turn to persist incrementally — this avoids keeping the full
// transcript in memory and enables crash resilience.
func (b *Builder) DrainCompletedTurns() []Turn {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.turns) == 0 {
		return nil
	}
	result := make([]Turn, len(b.turns))
	copy(result, b.turns)
	b.turns = b.turns[:0]
	return result
}

// RecordThinking appends a thinking block event to the current turn.
func (b *Builder) RecordThinking(content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.curTurn == nil || content == "" {
		return
	}
	b.curTurn.Events = append(b.curTurn.Events, Event{
		Type:      EventThinking,
		Timestamp: time.Now(),
		Content:   content,
	})
}

// RecordText appends an aggregated text delta to the current turn.
// Call once per turn (not per token) to avoid bloat.
func (b *Builder) RecordText(content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.curTurn == nil || content == "" {
		return
	}
	b.curTurn.Events = append(b.curTurn.Events, Event{
		Type:      EventText,
		Timestamp: time.Now(),
		Content:   content,
	})
}

// RecordUserMessage records the user's input message that triggered this
// conversation turn. It creates a standalone turn for the user event so that
// the transcript naturally reflects: user input → agent response(s).
func (b *Builder) RecordUserMessage(content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if content == "" {
		return
	}

	// Finalize any pending turn before creating the user turn.
	if b.curTurn != nil && len(b.curTurn.Events) > 0 {
		b.turns = append(b.turns, *b.curTurn)
	}
	b.curTurn = nil

	// User message as its own turn (not part of any agent turn).
	b.turns = append(b.turns, Turn{
		ID: b.nextID,
		Events: []Event{{
			Type:      EventUser,
			Timestamp: time.Now(),
			Content:   content,
		}},
	})
	b.nextID++
}

// RecordToolCall begins recording a tool invocation. It appends the
// tool_call event immediately and returns a ToolCallRecorder that the
// caller must use to write the corresponding tool_result.
//
// For SubAgent tool calls, use rec.SubBuilder() to obtain a child Builder
// that the sub-agent can write into. The child's events will appear nested
// under the tool_call event in the final transcript.
func (b *Builder) RecordToolCall(name string, args string) *ToolCallRecorder {
	b.mu.Lock()
	defer b.mu.Unlock()

	ev := Event{
		Type:      EventToolCall,
		Timestamp: time.Now(),
		Name:      name,
		Args:      args,
	}

	if b.curTurn == nil {
		// Auto-create a turn if one hasn't been started (defensive).
		b.curTurn = &Turn{ID: b.nextID}
		b.nextID++
	}
	b.curTurn.Events = append(b.curTurn.Events, ev)

	// Return a recorder that references the event in the slice.
	// We use a pointer to the last element so mutations persist.
	idx := len(b.curTurn.Events) - 1
	return &ToolCallRecorder{
		builder: b,
		evPtr:   &b.curTurn.Events[idx],
	}
}

// Build returns an immutable snapshot of the current transcript.
// Safe to call at any time during or after execution.
func (b *Builder) Build() *Transcript {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := &Transcript{
		Turns: make([]Turn, len(b.turns)),
	}
	copy(t.Turns, b.turns)
	return t
}

// finalizeTurn moves curTurn into turns (called at the end of each turn).
func (b *Builder) finalizeTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.curTurn != nil && len(b.curTurn.Events) > 0 {
		b.turns = append(b.turns, *b.curTurn)
	}
	b.curTurn = nil
}

// FinalizeTurn moves the current turn into the completed turns list.
// Call this after all tool results for a turn have been recorded.
func (b *Builder) FinalizeTurn() {
	b.finalizeTurn()
}

// ToolCallRecorder manages recording a single tool_call → tool_result pair.
// For SubAgent invocations, use SubBuilder() to obtain a child Builder for
// the sub-agent's nested transcript.
type ToolCallRecorder struct {
	builder   *Builder
	evPtr     *Event
	subBuilder *Builder // non-nil only for SubAgent calls
	done      bool
}

// SubBuilder returns a child Builder for the sub-agent to write into.
// Returns nil for non-SubAgent tool calls. The first call creates the
// sub-builder lazily, only when the tool name is "SubAgent".
func (r *ToolCallRecorder) SubBuilder() *Builder {
	if r.subBuilder == nil && r.evPtr.Name == tools.ToolNameSubAgent {
		r.subBuilder = NewBuilder()
	}
	return r.subBuilder
}

// RecordToolResult writes the tool invocation result and closes this recorder.
// For SubAgent calls, any events written to the SubBuilder() are flushed into
// the parent event's Children before the result is recorded.
func (r *ToolCallRecorder) RecordToolResult(content string, isError bool) {
	r.builder.mu.Lock()
	defer r.builder.mu.Unlock()

	if r.done {
		return
	}
	r.done = true

	// Flush sub-agent transcript into the tool_call event's Children.
	if r.subBuilder != nil {
		r.subBuilder.finalizeTurn()
		t := r.subBuilder.Build()
		// Collapse turns into a flat []Event for the parent.
		children := make([]Event, 0)
		for _, turn := range t.Turns {
			children = append(children, turn.Events...)
		}
		r.evPtr.Children = children
	}

	// Append tool_result event to the parent turn.
	if r.builder.curTurn != nil {
		r.builder.curTurn.Events = append(r.builder.curTurn.Events, Event{
			Type:      EventToolResult,
			Timestamp: time.Now(),
			Name:      r.evPtr.Name,
			Content:   content,
			IsError:   isError,
		})
	}
}