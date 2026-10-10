package commands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
)

func TestIsValidThinkingLevel(t *testing.T) {
	for _, lvl := range ThinkingLevels {
		assert.True(t, IsValidThinkingLevel(lvl), "level %q should be valid", lvl)
	}
	for _, lvl := range []string{"", "turbo", "off", "High", "MAX"} {
		assert.False(t, IsValidThinkingLevel(lvl), "level %q should be invalid", lvl)
	}
}

func TestThinkingLevelsDerivedFromEffortLevels(t *testing.T) {
	// ThinkingLevels = ThinkingEffortLevels + "default", in order.
	want := append(append([]string{}, ThinkingEffortLevels...), "default")
	assert.Equal(t, want, ThinkingLevels)
	assert.NotContains(t, ThinkingEffortLevels, "default")
}

func TestThinkingLevelOf(t *testing.T) {
	f := false
	cases := []struct {
		name     string
		thinking *bool
		effort   string
		want     string
	}{
		{"unset defaults to empty", nil, "", ""},
		{"disabled", &f, "", "none"},
		{"effort set", nil, "high", "high"},
		// Round-trip: ThinkingOverrideFromLevel then ThinkingLevelOf.
		{"round-trip none", func() *bool { v := false; return &v }(), "", "none"},
		{"round-trip effort", nil, "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ThinkingLevelOf(tc.thinking, tc.effort))
		})
	}
}

func TestThinkingOverrideFromLevel(t *testing.T) {
	t.Run("none disables thinking", func(t *testing.T) {
		thinking, effort := ThinkingOverrideFromLevel("none")
		require.NotNil(t, thinking)
		assert.False(t, *thinking)
		assert.Equal(t, "", effort)
	})

	t.Run("effort level leaves switch nil", func(t *testing.T) {
		thinking, effort := ThinkingOverrideFromLevel("high")
		assert.Nil(t, thinking)
		assert.Equal(t, "high", effort)
	})

	t.Run("max passes through unchanged", func(t *testing.T) {
		_, effort := ThinkingOverrideFromLevel("max")
		assert.Equal(t, "max", effort)
	})

	t.Run("any effort level passes through unchanged", func(t *testing.T) {
		_, effort := ThinkingOverrideFromLevel("xhigh")
		assert.Equal(t, "xhigh", effort)
	})
}

func TestEffectiveThinking(t *testing.T) {
	// Provider config default: thinking high.
	resolved := llm.ResolvedProvider{
		Model:          "deepseek-v4-flash",
		ThinkingEffort: "high",
	}

	t.Run("empty level falls back to provider defaults", func(t *testing.T) {
		thinking, effort := EffectiveThinking("", resolved)
		assert.Nil(t, thinking)
		assert.Equal(t, "high", effort)
	})

	t.Run("default level falls back to provider defaults", func(t *testing.T) {
		thinking, effort := EffectiveThinking("default", resolved)
		assert.Nil(t, thinking)
		assert.Equal(t, "high", effort)
	})

	t.Run("session override wins", func(t *testing.T) {
		thinking, effort := EffectiveThinking("none", resolved)
		require.NotNil(t, thinking)
		assert.False(t, *thinking)
		assert.Equal(t, "", effort)
	})

	t.Run("session override passes through", func(t *testing.T) {
		thinking, effort := EffectiveThinking("max", resolved)
		assert.Nil(t, thinking)
		assert.Equal(t, "max", effort) // passed to the API unchanged
	})
}

func TestFormatThinkingStatus(t *testing.T) {
	status := FormatThinkingStatus("high", nil)
	assert.Contains(t, status, "**high**")
	for _, lvl := range ThinkingLevels {
		assert.Contains(t, status, lvl)
	}
	// The option list should not be empty.
	assert.Contains(t, status, FormatThinkingOptions(nil))
	assert.True(t, strings.HasPrefix(status, "🧠"))
}

func TestFormatThinkingOptions(t *testing.T) {
	opts := FormatThinkingOptions(nil)
	for _, lvl := range ThinkingLevels {
		assert.Contains(t, opts, lvl)
		assert.Contains(t, opts, ThinkingLevelDescriptions[lvl])
	}
}

func TestIsThinkingLevelSupported(t *testing.T) {
	declared := []string{"low", "medium", "high", "xhigh"}

	t.Run("undeclared provider accepts every level", func(t *testing.T) {
		for _, lvl := range ThinkingLevels {
			assert.True(t, IsThinkingLevelSupported(lvl, nil), "level %q", lvl)
		}
	})

	t.Run("declared provider accepts only its own", func(t *testing.T) {
		for _, lvl := range declared {
			assert.True(t, IsThinkingLevelSupported(lvl, declared), "level %q", lvl)
		}
		assert.False(t, IsThinkingLevelSupported("max", declared))
		assert.False(t, IsThinkingLevelSupported("none", declared))
	})

	t.Run("default and empty are not efforts", func(t *testing.T) {
		// "default" means "follow the provider's thinking_level" and "" means
		// "no override" — neither is sent as an effort, so a declared set
		// must not turn them into a refusal.
		assert.True(t, IsThinkingLevelSupported("default", declared))
		assert.True(t, IsThinkingLevelSupported("", declared))
	})
}

func TestSupportedThinkingLevels(t *testing.T) {
	t.Run("undeclared provider offers every level", func(t *testing.T) {
		assert.Equal(t, ThinkingLevels, SupportedThinkingLevels(nil))
	})

	t.Run("declared provider offers its levels plus default, in order", func(t *testing.T) {
		assert.Equal(t, []string{"low", "high", "default"},
			SupportedThinkingLevels([]string{"high", "low"}))
	})
}

func TestFormatThinkingRejected(t *testing.T) {
	msg := FormatThinkingRejected("max", "gpt-6-astra", []string{"low", "medium", "high", "xhigh"})
	assert.Contains(t, msg, "**max**")
	assert.Contains(t, msg, "gpt-6-astra")
	assert.Contains(t, msg, "**xhigh**")
	assert.NotContains(t, msg, "**max** — ") // the rejected level is never offered as an option

	// No provider name → no empty parenthetical.
	assert.NotContains(t, FormatThinkingRejected("max", "", []string{"high"}), "（）")
}
