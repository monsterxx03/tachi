package agent

import (
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// TestKeywordExtractionBudget pins how the two memory deadlines combine. The
// extractor call happens inside Recall's context, so a budget above
// memory.timeout is not a bigger budget — it is a call the parent kills early,
// and the log then reports the keyword call as the thing that timed out.
func TestKeywordExtractionBudget(t *testing.T) {
	cases := []struct {
		name   string
		mem    config.MemoryConfig
		expect time.Duration
	}{
		{"unset keyword budget uses the default", config.MemoryConfig{Timeout: 10 * time.Second}, defaultKeywordExtractionTimeout},
		{"configured budget below the recall deadline is kept", config.MemoryConfig{Timeout: 10 * time.Second, KeywordTimeout: 4 * time.Second}, 4 * time.Second},
		{"budget above the recall deadline is cut to it", config.MemoryConfig{Timeout: 5 * time.Second, KeywordTimeout: 8 * time.Second}, 5 * time.Second},
		{"no recall deadline leaves the budget alone", config.MemoryConfig{KeywordTimeout: 8 * time.Second}, 8 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keywordExtractionBudget(c.mem); got != c.expect {
				t.Errorf("keywordExtractionBudget() = %v, want %v", got, c.expect)
			}
		})
	}
}

// TestKeywordExtractorClampsTimeout pins the two ends of the constructor: zero
// falls back to the default, and an absurd value is clamped rather than turned
// into a call that can never be waited out.
func TestKeywordExtractorClampsTimeout(t *testing.T) {
	cases := []struct {
		given  time.Duration
		expect time.Duration
	}{
		{0, defaultKeywordExtractionTimeout},
		{3 * time.Second, 3 * time.Second},
		{60 * time.Second, maxKeywordExtractionTimeout},
	}
	for _, c := range cases {
		ext := NewLLMKeywordExtractor(nil, "", c.given, logger.Default())
		if ext.timeout != c.expect {
			t.Errorf("NewLLMKeywordExtractor(timeout=%v).timeout = %v, want %v", c.given, ext.timeout, c.expect)
		}
	}
}
