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
		// gpt-6 家族：四个已登记的模型同上下文，未登记的变体由家族记录兜底。
		{"gpt-6-luna", 1_050_000},
		{"gpt-6-sol", 1_050_000},
		{"gpt-6-astra", 1_050_000},
		{"gpt-6.1-sol", 1_050_000},
		{"gpt-6-sol-2026-09-22", 1_050_000},
		{"gpt-6-unknown-variant", 1_050_000},
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
		{"glm-5.2", 1_000_000},
		{"glm-5.3", 1_000_000},
		{"glm-5.3-flash", 1_000_000},
		{"minimax-m2", 204_800},
		{"minimax-m2.7", 204_800},
		{"minimax-m3", 1_000_000},
		{"kimi-v2", 256_000},
		{"kimi-k3", 1_000_000},
		{"deepseek-v4-flash", 1_000_000},
		{"deepseek-chat", 1_000_000},
		{"deepseek-v4-pro", 1_000_000},
		// flash 简写别名（连字符 / 下划线）与 v4-flash 同上下文。
		{"deepseek-flash", 1_000_000},
		{"deepseek_flash", 1_000_000},
		// MiMo-V2.5 系列：官方名为 mimo-v2.5（旧代码 "mimo-2.5" 漏 v 导致
		// 上下文解析为 0，重构后统一并兼容历史别名）。
		{"mimo-v2.5", 1_000_000},
		{"mimo-v2.5-pro", 1_000_000},
		{"mimo-2.5", 1_000_000}, // 历史别名兼容
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
		"gpt-6-luna", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-sol-preview",
		"o4-mini", "o4", "gpt-4-turbo", "gpt-4-vision-preview",
		"qwen-vl-max", "qwen2.5-vl-72b", "qwen3-vl-plus",
		"glm-4v", "glm-4.1v", "glm-4.5v", "glm-5.3-flash",
		"gemini-2.5-pro", "gemini-1.5-flash",
		"mimo-2.5", "mimo-vl-7b", "mimo-7b",
		"kimi-k3", "kimi-k2", "kimi-latest",
		"minimax-m2", "minimax-m2.7", "minimax-m3", "minimax-abab6.5s",
		// DeepSeek 只有 vision 变体支持图像输入；flash 简写别名与
		// deepseek-v4-flash-vision-exp 同能力。
		"deepseek-v4-flash-vision-exp", "deepseek-flash", "deepseek_flash", "DeepSeek-Flash",
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

// The reasoning families select max_completion_tokens over max_tokens (see
// isReasoningModelPrefix), so a new family missing from it fails at request
// build time. gpt-6 joined with the four models added alongside it.
func TestIsReasoningModelPrefix(t *testing.T) {
	reasoning := []string{
		"o1", "o1-mini", "o3", "o4", "o4-mini",
		"gpt-5.4", "gpt-5.5-turbo",
		"gpt-6", "gpt-6-luna", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-sol",
		"gpt-6-sol-2026-09-22", "GPT-6-ASTRA",
	}
	for _, m := range reasoning {
		t.Run("reasoning_"+m, func(t *testing.T) {
			if !IsReasoningModelPrefix(m) {
				t.Errorf("IsReasoningModelPrefix(%q) = false, want true", m)
			}
		})
	}

	notReasoning := []string{"gpt-4o", "gpt-4.1", "gpt-4", "gpt-3.5-turbo", "claude-opus", "deepseek-chat", ""}
	for _, m := range notReasoning {
		t.Run("plain_"+m, func(t *testing.T) {
			if IsReasoningModelPrefix(m) {
				t.Errorf("IsReasoningModelPrefix(%q) = true, want false", m)
			}
		})
	}
}

// TestModelSupportsReasoningEffortNone: reasoning models whose API rejects
// reasoning.effort = "none" (gpt-6 Astra / 6.1 Sol) must not be sent it — the
// rejected parameter fails the whole request. Everything else keeps the
// behavior that existed before the capability was recorded.
// Source: https://developers.openai.com/api/docs/models/gpt-6-astra
//
//	https://developers.openai.com/api/docs/models/gpt-6.1-sol
func TestModelSupportsReasoningEffortNone(t *testing.T) {
	allowed := []string{
		"gpt-5.4", "gpt-5.5-turbo",
		"gpt-6-luna", "gpt-6-sol", "gpt-6-sol-2026-09-22",
		// o-series: no record says otherwise, so the pre-existing answer stands.
		"o3", "o4-mini",
	}
	for _, m := range allowed {
		t.Run("none_ok_"+m, func(t *testing.T) {
			if !ModelSupportsReasoningEffortNone(m) {
				t.Errorf("ModelSupportsReasoningEffortNone(%q) = false, want true (sending none is how thinking_level: none works)", m)
			}
		})
	}

	rejected := []string{"gpt-6-astra", "gpt-6-astra-2026-09-30", "GPT-6.1-SOL", "gpt-6.1-sol"}
	for _, m := range rejected {
		t.Run("none_rejected_"+m, func(t *testing.T) {
			if ModelSupportsReasoningEffortNone(m) {
				t.Errorf("ModelSupportsReasoningEffortNone(%q) = true, want false", m)
			}
		})
	}

	// Not a reasoning model at all → no reasoning field either way. A reasoning
	// family with no registry row (o1/o3) keeps the pre-existing answer: this
	// query blocks a value known to be rejected, it does not withhold one for
	// lack of a row.
	for _, m := range []string{"gpt-4o", "claude-opus", "unknown-model", ""} {
		t.Run("no_reasoning_"+m, func(t *testing.T) {
			if ModelSupportsReasoningEffortNone(m) {
				t.Errorf("ModelSupportsReasoningEffortNone(%q) = true, want false", m)
			}
		})
	}
}

// TestModelThinkingEfforts pins the effort sets the registry records: the two
// gpt-6 variants whose API rejects "none" (Astra / 6.1 Sol) and nothing else.
// nil = no claim = every level, which is what an undeclared model keeps — the
// answer every model had before this capability existed.
// Source: https://developers.openai.com/api/docs/models/gpt-6-astra
//
//	https://developers.openai.com/api/docs/models/gpt-6.1-sol
func TestModelThinkingEfforts(t *testing.T) {
	want := []string{"low", "medium", "high", "xhigh", "max"}
	for _, m := range []string{"gpt-6-astra", "gpt-6-astra-2026-09-30", "GPT-6.1-SOL"} {
		t.Run("declared_"+m, func(t *testing.T) {
			got := ModelThinkingEfforts(m)
			if len(got) != len(want) {
				t.Fatalf("ModelThinkingEfforts(%q) = %v, want %v", m, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("ModelThinkingEfforts(%q) = %v, want %v", m, got, want)
				}
			}
		})
	}

	// No claim: the family's other members, models outside the family, and
	// names the registry has never seen.
	for _, m := range []string{"gpt-6-luna", "gpt-6-sol", "gpt-5.5", "claude-opus", "unknown-model", ""} {
		t.Run("no_claim_"+m, func(t *testing.T) {
			if got := ModelThinkingEfforts(m); got != nil {
				t.Errorf("ModelThinkingEfforts(%q) = %v, want nil (no claim = every level)", m, got)
			}
		})
	}
}

// TestModelThinkingEffortsIsACopy pins that a caller cannot edit the registry:
// the set is put on a resolved provider that is then handed around (and the
// same slice backs two records).
func TestModelThinkingEffortsIsACopy(t *testing.T) {
	got := ModelThinkingEfforts("gpt-6-astra")
	if len(got) == 0 {
		t.Fatal("no effort set for gpt-6-astra")
	}
	got[0] = "tampered"
	if again := ModelThinkingEfforts("gpt-6-astra"); again[0] == "tampered" {
		t.Error("the registry was mutated through the returned slice")
	}
}
