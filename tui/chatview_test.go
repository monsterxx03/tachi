package tui

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/session"
)

func TestFormatToolCallSummary(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]int
		want  string
	}{
		{
			name:  "empty",
			input: nil,
			want:  "",
		},
		{
			name:  "single tool",
			input: map[string]int{"ReadFile": 1},
			want:  "ReadFile",
		},
		{
			name:  "single tool with multiple calls",
			input: map[string]int{"ReadFile": 3},
			want:  "ReadFile(3)",
		},
		{
			name:  "multiple tools sorted",
			input: map[string]int{"Bash": 1, "Grep": 2, "ReadFile": 5},
			want:  "Bash, Grep(2), ReadFile(5)",
		},
		{
			name:  "mixed single and multiple",
			input: map[string]int{"Glob": 1, "Grep": 1, "ReadFile": 2},
			want:  "Glob, Grep, ReadFile(2)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatToolCallSummary(tt.input)
			if got != tt.want {
				t.Errorf("formatToolCallSummary(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// A user record whose text the frontend transformed before sending (@-file expansion) keeps
// the user's own words in DisplayContent; a rebuilt transcript shows those, not the file the
// model was given. Both shapes go through one path: a record without it (every session written
// before the field, and every ordinary message) falls back to Content.
func TestChatView_LoadHistory_UserShowsDisplayContent(t *testing.T) {
	c := NewChatView()
	c.LoadHistory([]session.Message{
		{Type: session.MessageTypeUser, Content: "看看 @README.md\n\n--- BEGIN UNTRUSTED FILE CONTENT ---\n# smoke\n---\n", DisplayContent: "看看 @README.md"},
		{Type: session.MessageTypeUser, Content: "普通的一句"},
	})

	if len(c.items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(c.items))
	}
	if got := c.items[0].msg.Content; got != "看看 @README.md" {
		t.Errorf("bubble text = %q, want the user's own words", got)
	}
	if strings.Contains(c.items[0].msg.Content, "UNTRUSTED") {
		t.Error("the inlined file leaked into the rebuilt bubble")
	}
	if got := c.items[1].msg.Content; got != "普通的一句" {
		t.Errorf("a record without a display text must render its content, got %q", got)
	}
}
