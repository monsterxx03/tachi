package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildSystemPrompt_Extra(t *testing.T) {
	t.Run("empty extra appends nothing", func(t *testing.T) {
		prompt := BuildSystemPrompt("en", "/project", "sess-1", "")
		assert.NotContains(t, prompt, "## Extra Instructions")
		// Ends with the session line (no extra block).
		assert.True(t, strings.HasSuffix(prompt, "- Session ID: sess-1\n"))
	})

	t.Run("extra is appended at the end", func(t *testing.T) {
		extra := "## Extra Instructions\nAlways run tests before finishing."
		prompt := BuildSystemPrompt("en", "/project", "sess-1", extra)
		assert.Contains(t, prompt, "- Session ID: sess-1")
		assert.Contains(t, prompt, "## Extra Instructions\nAlways run tests before finishing.")
		// The extra block must come after the session line.
		assert.True(t, strings.Index(prompt, "## Extra Instructions") > strings.Index(prompt, "- Session ID: sess-1"))
		// And be the final content.
		assert.True(t, strings.HasSuffix(prompt, "Always run tests before finishing.\n"))
	})

	t.Run("extra whitespace is trimmed", func(t *testing.T) {
		prompt := BuildSystemPrompt("en", "/project", "", "\n\n   Custom content.  \n\n")
		assert.Contains(t, prompt, "Custom content.")
		assert.True(t, strings.HasSuffix(prompt, "Custom content.\n"))
	})

	t.Run("with roots also appends extra", func(t *testing.T) {
		prompt := BuildSystemPromptWithRoots("en", "/project", []string{"/shared-lib"}, "sess-1", "Custom suffix.")
		assert.Contains(t, prompt, "- Additional workspace roots: /shared-lib")
		assert.True(t, strings.HasSuffix(prompt, "Custom suffix.\n"))
	})
}
