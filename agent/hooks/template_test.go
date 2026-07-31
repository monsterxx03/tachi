package hooks

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestExpandVars_HooksDirRespectsBaseDir verifies {{HOOKS_DIR}} resolves under
// config.BaseDir() (honoring --home) instead of a hardcoded ~/.tachi/hooks.
func TestExpandVars_HooksDirRespectsBaseDir(t *testing.T) {
	tmpBase := t.TempDir()
	config.SetBaseDir(tmpBase)
	t.Cleanup(func() { config.SetBaseDir("") })

	out := expandVars("bash {{HOOKS_DIR}}/notify.sh", "sess1", "/ws")
	want := "bash " + filepath.Join(tmpBase, "hooks", "notify.sh")
	if out != want {
		t.Errorf("expandVars = %q, want %q", out, want)
	}
}

// TestExpandVars_SessionAndWorkspace verifies the other template variables
// still expand correctly.
func TestExpandVars_SessionAndWorkspace(t *testing.T) {
	tmpBase := t.TempDir()
	config.SetBaseDir(tmpBase)
	t.Cleanup(func() { config.SetBaseDir("") })

	out := expandVars("echo {{SESSION_ID}} {{WORKSPACE_DIR}} {{TIMESTAMP}}", "abc123", "/proj")
	for _, want := range []string{"abc123", "/proj"} {
		if !strings.Contains(out, want) {
			t.Errorf("expandVars missing %q: %q", want, out)
		}
	}
}
