// Package transcript provides a tree-structured execution record for AI agents.
// Unlike session messages (flat JSONL designed for LLM resume), transcripts
// capture the full nested execution flow — including sub-agent thinking, tool
// calls, and results. They are primarily for debugging, analysis, and TUI
// visualization of sub-agent progress.
package transcript

import (
	"encoding/json"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// EventType enumerates the kinds of atomic events in an agent execution trace.
type EventType string

const (
	EventThinking   EventType = "thinking"
	EventText       EventType = "text"
	EventUser       EventType = "user"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
)

// Event is a single node in the transcript tree. For SubAgent tool calls,
// Children contains the child agent's complete sub-transcript.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"ts"`
	Name      string    `json:"name,omitempty"`     // tool name (for tool_call / tool_result)
	Content   string    `json:"content,omitempty"`  // thinking text / text delta / tool result
	Args      string    `json:"args,omitempty"`     // tool_call JSON arguments
	IsError   bool      `json:"is_error,omitempty"` // tool_result error flag
	Children  []Event   `json:"children,omitempty"` // SubAgent sub-transcript (recursive)
}

// Turn groups events from a single LLM API stream call (one iteration of the
// agent loop). A turn may contain multiple tool calls — including those that
// spawn sub-agents.
type Turn struct {
	ID     int     `json:"id"`
	Events []Event `json:"events"`
}

// Transcript is a complete agent execution record. Each Turn corresponds to a
// single LLM API call (including auto-continuations from length exhaustion).
type Transcript struct {
	SessionID string `json:"session_id,omitempty"`
	Turns     []Turn `json:"turns"`
}

// New creates an empty Transcript.
func New() *Transcript {
	return &Transcript{}
}

// Events returns the total number of events across all turns (non-recursive).
func (t *Transcript) Events() int {
	n := 0
	for _, turn := range t.Turns {
		n += len(turn.Events)
	}
	return n
}

// SubagentCount walks the entire tree and returns the total number of
// SubAgent tool call events.
func (t *Transcript) SubagentCount() int {
	return countSubagentCalls(t.Turns)
}

func countSubagentCalls(turns []Turn) int {
	n := 0
	for _, turn := range turns {
		for _, ev := range turn.Events {
			if ev.Name == tools.ToolNameSubAgent {
				n++
			}
			// Recurse into children (recursive sub-agents)
			childTurns := []Turn{{Events: ev.Children, ID: -1}}
			n += countSubagentCalls(childTurns)
		}
	}
	return n
}

// Subevents returns a flat list of all events reachable from the given event
// (including itself and all nested children). Useful for testing and analysis.
func Subevents(ev *Event) []Event {
	var result []Event
	result = append(result, *ev)
	for i := range ev.Children {
		result = append(result, Subevents(&ev.Children[i])...)
	}
	return result
}

// MarshalJSON serializes a Transcript to JSON with indentation.
func (t *Transcript) MarshalJSON() ([]byte, error) {
	type alias Transcript
	return fileutil.MarshalJSON((*alias)(t))
}

// UnmarshalJSON deserializes JSON into a Transcript.
func (t *Transcript) UnmarshalJSON(data []byte) error {
	type alias Transcript
	return json.Unmarshal(data, (*alias)(t))
}
