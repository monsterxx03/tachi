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
		{"claude-sonnet-4", 200_000}, // without -6 suffix → safe default 200K
		{"claude-3-opus", 200_000},   // unknown Claude variant → safe default 200K
		{"claude-2", 200_000},        // unknown Claude variant → safe default 200K
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

func TestModelSupportsVision(t *testing.T) {
	visionModels := []string{
		"claude-sonnet-4-6", "claude-opus-4", "claude-haiku-3-5", "claude-3-5-sonnet",
		"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "gpt-5.2", "gpt-5.1-codex",
		"o4-mini", "o4", "gpt-4-turbo", "gpt-4-vision-preview",
		"qwen-vl-max", "qwen2.5-vl-72b", "qwen3-vl-plus",
		"glm-4v", "glm-4.1v", "glm-4.5v",
		"gemini-2.5-pro", "gemini-1.5-flash",
		"mimo-2.5", "mimo-vl-7b", "mimo-7b",
		"kimi-k3", "kimi-k2", "kimi-latest",
		"minimax-m2", "minimax-abab6.5s",
	}
	for _, m := range visionModels {
		t.Run("vision_"+m, func(t *testing.T) {
			if !ModelSupportsVision(m) {
				t.Errorf("ModelSupportsVision(%q) = false, want true", m)
			}
		})
	}

	textOnlyModels := []string{
		"deepseek-v4-flash", "deepseek-reasoner", "deepseek-chat",
		"gpt-4", "gpt-3.5-turbo", "o1-mini", "o1", "o3", "gpt-4.5",
		"qwen-turbo", "qwen2.5-72b", "qwen-max",
		"glm-4", "glm-4.5", "glm-4.6",
		"", "unknown-model",
	}
	for _, m := range textOnlyModels {
		t.Run("text_"+m, func(t *testing.T) {
			if ModelSupportsVision(m) {
				t.Errorf("ModelSupportsVision(%q) = true, want false", m)
			}
		})
	}
}
