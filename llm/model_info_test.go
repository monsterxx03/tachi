package llm

import (
	"testing"
)

func TestModelContextWindow_Claude(t *testing.T) {
	tests := []struct {
		model string
		want  int64
	}{
		{"claude-sonnet-4-6", 1_000_000},
		{"claude-sonnet-4-6-20250514", 1_000_000},
		{"claude-opus-4", 1_000_000},
		{"claude-opus-4-20250514", 1_000_000},
		{"claude-haiku", 200_000},
		{"claude-haiku-3-5", 200_000},
		{"claude-sonnet-4", 200_000},       // without -6 suffix → safe default 200K
		{"claude-3-opus", 200_000},          // unknown Claude variant → safe default 200K
		{"claude-2", 200_000},               // unknown Claude variant → safe default 200K
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ModelContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ModelContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestModelContextWindow_GPT(t *testing.T) {
	tests := []struct {
		model string
		want  int64
	}{
		{"gpt-5.4", 1_050_000},
		{"gpt-5.4-preview", 1_050_000},
		{"gpt-5.5", 1_050_000},
		{"gpt-5.5-turbo", 1_050_000},
		{"gpt-5.3-codex", 400_000},
		{"gpt-5.3-codex-something", 400_000},
		{"gpt-4o", 400_000},
		{"gpt-4", 400_000},
		{"GPT-4O", 400_000}, // case insensitivity
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ModelContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ModelContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestModelContextWindow_Others(t *testing.T) {
	tests := []struct {
		model string
		want  int64
	}{
		{"qwen-max", 1_000_000},
		{"qwen-plus", 1_000_000},
		{"glm-4", 200_000},
		{"minimax-m2", 204_800},
		{"kimi-v2", 256_000},
		{"deepseek-v4-flash", 1_000_000},
		{"deepseek-chat", 1_000_000},
		{"deepseek-v4-pro", 1_000_000},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ModelContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ModelContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestModelContextWindow_Unknown(t *testing.T) {
	got := ModelContextWindow("unknown-model")
	if got != 0 {
		t.Errorf("ModelContextWindow(unknown) = %d, want 0", got)
	}
}

func TestModelContextWindow_EmptyString(t *testing.T) {
	got := ModelContextWindow("")
	if got != 0 {
		t.Errorf("ModelContextWindow('') = %d, want 0", got)
	}
}