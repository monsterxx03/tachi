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

func TestThinkingEffortLevels(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		{"deepseek-v4-flash", []string{"low", "high"}},
		{"deepseek-v4-flash-20260801", []string{"low", "high"}},
		{"deepseek-v4-pro", []string{"low", "high", "max"}},
		{"deepseek-chat", []string{"low", "high", "max"}},
		{"deepseek-reasoner", []string{"low", "high", "max"}},
		{"DEEPSEEK-V4-PRO", []string{"low", "high", "max"}}, // case insensitive
		{"claude-sonnet-4-6", nil},                           // non-DeepSeek: unrestricted
		{"gpt-5.4", nil},
		{"", nil},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ThinkingEffortLevels(tt.model)
			if tt.want == nil {
				if got != nil {
					t.Errorf("ThinkingEffortLevels(%q) = %v, want nil", tt.model, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("ThinkingEffortLevels(%q) = %v, want %v", tt.model, got, tt.want)
				return
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("ThinkingEffortLevels(%q) = %v, want %v", tt.model, got, tt.want)
					return
				}
			}
		})
	}
}

func TestNormalizeThinkingEffort(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		// Empty → passthrough (model default)
		{"empty effort", "deepseek-v4-flash", "", ""},
		// Supported levels pass through unchanged
		{"flash low", "deepseek-v4-flash", "low", "low"},
		{"flash high", "deepseek-v4-flash", "high", "high"},
		{"pro low", "deepseek-v4-pro", "low", "low"},
		{"pro high", "deepseek-v4-pro", "high", "high"},
		{"pro max", "deepseek-v4-pro", "max", "max"},
		// Degradation: unsupported level → highest supported at-or-below
		{"flash max degrades to high", "deepseek-v4-flash", "max", "high"},
		{"flash medium degrades to low", "deepseek-v4-flash", "medium", "low"},
		{"pro medium degrades to low", "deepseek-v4-pro", "medium", "low"},
		{"flash xhigh degrades to high", "deepseek-v4-flash", "xhigh", "high"},
		{"pro xhigh degrades to high", "deepseek-v4-pro", "xhigh", "high"},
		// Unknown family / unknown effort: passthrough
		{"claude passthrough", "claude-sonnet-4-6", "max", "max"},
		{"unknown effort passthrough", "deepseek-v4-pro", "turbo", "turbo"},
		{"unknown model passthrough", "some-model", "max", "max"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeThinkingEffort(tt.model, tt.effort)
			if got != tt.want {
				t.Errorf("NormalizeThinkingEffort(%q, %q) = %q, want %q", tt.model, tt.effort, got, tt.want)
			}
		})
	}
}