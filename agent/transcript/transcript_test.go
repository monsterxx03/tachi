package transcript

import (
	"testing"
)

func TestTranscript_New(t *testing.T) {
	tr := New()
	if tr == nil {
		t.Fatal("New() returned nil")
	}
	if tr.SessionID != "" {
		t.Error("SessionID should be empty")
	}
	if len(tr.Turns) != 0 {
		t.Error("Turns should be empty")
	}
}

func TestTranscript_Events_Empty(t *testing.T) {
	tr := New()
	if tr.Events() != 0 {
		t.Errorf("Events() = %d, want 0", tr.Events())
	}
}

func TestTranscript_Events(t *testing.T) {
	tr := &Transcript{
		Turns: []Turn{
			{ID: 1, Events: []Event{{}, {}, {}}},
			{ID: 2, Events: []Event{{}, {}}},
		},
	}
	if tr.Events() != 5 {
		t.Errorf("Events() = %d, want 5", tr.Events())
	}
}

func TestTranscript_SubagentCount_Zero(t *testing.T) {
	tr := New()
	if tr.SubagentCount() != 0 {
		t.Error("SubagentCount() should be 0 for empty transcript")
	}
}

func TestTranscript_SubagentCount(t *testing.T) {
	tr := &Transcript{
		Turns: []Turn{
			{ID: 1, Events: []Event{
				{Name: "SubAgent", Type: EventToolCall},
				{Name: "ReadFile", Type: EventToolCall},
			}},
			{ID: 2, Events: []Event{
				{Name: "SubAgent", Type: EventToolCall, Children: []Event{
					{Name: "SubAgent", Type: EventToolCall}, // nested sub-agent
				}},
			}},
		},
	}
	if tr.SubagentCount() != 3 {
		t.Errorf("SubagentCount() = %d, want 3", tr.SubagentCount())
	}
}

func TestTranscript_MarshalJSON(t *testing.T) {
	tr := &Transcript{
		SessionID: "test-123",
		Turns: []Turn{
			{ID: 1, Events: []Event{
				{Type: EventThinking, Content: "hello"},
			}},
		},
	}

	data, err := tr.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	if len(data) == 0 {
		t.Error("MarshalJSON returned empty data")
	}

	// Round-trip
	var tr2 Transcript
	if err := tr2.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if tr2.SessionID != tr.SessionID {
		t.Errorf("SessionID mismatch: %q vs %q", tr2.SessionID, tr.SessionID)
	}
	if len(tr2.Turns) != 1 {
		t.Errorf("Turn count: %d, want 1", len(tr2.Turns))
	}
	if tr2.Turns[0].Events[0].Content != "hello" {
		t.Errorf("Content mismatch: %q", tr2.Turns[0].Events[0].Content)
	}
}

func TestSubevents(t *testing.T) {
	ev := &Event{
		Type: EventToolCall,
		Name: "SubAgent",
		Children: []Event{
			{Type: EventThinking, Content: "child thinking"},
			{Type: EventToolCall, Name: "ReadFile"},
			{Type: EventToolResult, Name: "ReadFile", Content: "result"},
		},
	}

	sub := Subevents(ev)
	if len(sub) != 4 {
		t.Errorf("Subevents() = %d, want 4", len(sub))
	}
}

func TestSubevents_NoChildren(t *testing.T) {
	ev := &Event{Type: EventThinking, Content: "alone"}
	sub := Subevents(ev)
	if len(sub) != 1 {
		t.Errorf("Subevents() = %d, want 1", len(sub))
	}
}

func TestSubevents_Nested(t *testing.T) {
	ev := &Event{
		Type: EventToolCall,
		Name: "SubAgent",
		Children: []Event{
			{
				Type: EventToolCall,
				Name: "SubAgent",
				Children: []Event{
					{Type: EventThinking, Content: "deep"},
				},
			},
		},
	}

	sub := Subevents(ev)
	// ev + child SubAgent + deep thinking = 3
	if len(sub) != 3 {
		t.Errorf("Subevents() = %d, want 3", len(sub))
	}
}