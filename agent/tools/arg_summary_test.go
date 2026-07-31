package tools

import "testing"

func TestToolArgsSummaryBash(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "foreground command",
			args:     `{"command":"git status"}`,
			expected: "git status",
		},
		{
			name:     "background without explicit false",
			args:     `{"command":"go test ./...","background":false}`,
			expected: "go test ./...",
		},
		{
			name:     "background with name",
			args:     `{"command":"go test ./...","background":true,"bg_name":"test"}`,
			expected: "[bg:test] go test ./...",
		},
		{
			name:     "background without name",
			args:     `{"command":"sleep 100","background":true}`,
			expected: "[bg] sleep 100",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolArgsSummary(ToolNameBash, tt.args); got != tt.expected {
				t.Errorf("ToolArgsSummary(Bash, %s) = %q, want %q", tt.args, got, tt.expected)
			}
		})
	}
}

func TestToolArgsTitleBash(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "foreground command",
			args:     `{"command":"git status"}`,
			expected: "git status",
		},
		{
			name:     "background with name",
			args:     `{"command":"make build","background":true,"bg_name":"build"}`,
			expected: "[bg:build] make build",
		},
		{
			name:     "background without name",
			args:     `{"command":"sleep 100","background":true}`,
			expected: "[bg] sleep 100",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolArgsTitle(ToolNameBash, tt.args); got != tt.expected {
				t.Errorf("ToolArgsTitle(Bash, %s) = %q, want %q", tt.args, got, tt.expected)
			}
		})
	}
}
