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

// TestBuildSystemPromptFrontendCapabilities pins where a frontend capability
// section lands: after the environment block but BEFORE the user's
// extra_system_prompt, so a user-configured prompt stays last.
func TestBuildSystemPromptFrontendCapabilities(t *testing.T) {
	base := BuildSystemPrompt("en", "", "", "")
	withHint := BuildSystemPrompt("en", "", "", "", WithFrontendCapabilities(MermaidCapabilityPrompt))

	if strings.Contains(base, "## Diagramming") {
		t.Error("the Mermaid hint must only appear when a frontend asks for it")
	}
	if !strings.Contains(withHint, "## Diagramming") {
		t.Error("expected the Mermaid hint in the prompt")
	}
	if !strings.Contains(withHint, "sequenceDiagram") || !strings.Contains(withHint, "flowchart") {
		t.Error("the hint should name the diagram kinds it wants")
	}

	withBoth := BuildSystemPrompt("en", "", "", "USER EXTRA PROMPT", WithFrontendCapabilities(MermaidCapabilityPrompt))
	if strings.Index(withBoth, "## Diagramming") > strings.Index(withBoth, "USER EXTRA PROMPT") {
		t.Error("frontend capabilities must come before the user's extra prompt")
	}

	// Empty sections are ignored rather than leaving blank blocks.
	if got := BuildSystemPrompt("en", "", "", "", WithFrontendCapabilities("", "   ")); got != base {
		t.Error("blank sections must not change the prompt")
	}
}

// TestBuildSystemPromptWithoutWorkingDir pins the two meanings of an empty cwd:
// by default it means "use this process's project root" (what tui / channel / -p
// rely on), and with WithoutWorkingDir it means "no workspace chosen yet" — which
// the desktop needs, because substituting its process cwd would advertise "/" for
// a Finder-launched GUI app.
func TestBuildSystemPromptWithoutWorkingDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Default: the process's project root is filled in.
	if got := BuildSystemPrompt("en", "", "sid", ""); !strings.Contains(got, "- Working directory: "+dir) {
		t.Errorf("expected the process project root %q in the prompt", dir)
	}

	// Explicit "not set yet": nothing is substituted, and the model is told to ask.
	got := BuildSystemPrompt("en", "", "sid", "", WithoutWorkingDir())
	if strings.Contains(got, "- Working directory: "+dir) {
		t.Error("WithoutWorkingDir must not substitute the process directory")
	}
	if !strings.Contains(got, "- Working directory: (not set yet") {
		t.Errorf("expected an explicit unset line, got %q", promptLine(got, "- Working directory:"))
	}

	// A real directory is unaffected by the option.
	if got := BuildSystemPrompt("en", dir, "sid", "", WithoutWorkingDir()); !strings.Contains(got, "- Working directory: "+dir) {
		t.Errorf("a set working directory must still be advertised, got %q", promptLine(got, "- Working directory:"))
	}
}

// promptLine returns the first line with the given prefix (test failure messages).
func promptLine(prompt, prefix string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
