package llm

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestPromptTokens pins the per-provider meaning of a prompt size, because the two families report
// it differently and the difference is one addition away from being a double count:
//
//   - Anthropic's input_tokens EXCLUDES the cache (read and creation are separate counts), so the
//     prompt is the sum of the three.
//   - OpenAI-family's prompt_tokens INCLUDES the cache reads, so the prompt is that number alone —
//     adding cache_read (or a Responses cache-write detail) would count those tokens twice.
func TestPromptTokens(t *testing.T) {
	// Anthropic's own accounting for the same physical prompt: 1k uncached + 40k cache read.
	anthropic := &Usage{InputTokens: 1000, CacheReadInputTokens: 40000, CacheCreationInputTokens: 500}
	if got := PromptTokens(anthropic, config.ProviderTypeAnthropic); got != 41500 {
		t.Errorf("anthropic prompt = %d, want 41500 (input + cache read + cache creation)", got)
	}

	// The OpenAI-family view of that prompt: one total, cache carried inside it.
	openai := &Usage{InputTokens: 41500, CacheReadInputTokens: 40000}
	for _, providerType := range []string{config.ProviderTypeOpenAI, config.ProviderTypeOpenAIResponses, ""} {
		if got := PromptTokens(openai, providerType); got != 41500 {
			t.Errorf("%q prompt = %d, want 41500 (the total, cache not added again)", providerType, got)
		}
	}

	if got := PromptTokens(nil, config.ProviderTypeAnthropic); got != 0 {
		t.Errorf("nil usage = %d, want 0 (no call to measure yet)", got)
	}
}
