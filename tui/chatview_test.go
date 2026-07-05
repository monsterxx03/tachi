package tui

import (
	"testing"
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
