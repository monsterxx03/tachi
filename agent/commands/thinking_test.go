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
	status := FormatThinkingStatus("high")
	assert.Contains(t, status, "**high**")
	for _, lvl := range ThinkingLevels {
		assert.Contains(t, status, lvl)
	}
	// The option list should not be empty.
	assert.Contains(t, status, FormatThinkingOptions())
	assert.True(t, strings.HasPrefix(status, "🧠"))
}

func TestFormatThinkingOptions(t *testing.T) {
	opts := FormatThinkingOptions()
	for _, lvl := range ThinkingLevels {
		assert.Contains(t, opts, lvl)
		assert.Contains(t, opts, ThinkingLevelDescriptions[lvl])
	}
}
