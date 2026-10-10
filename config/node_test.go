package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/creasty/defaults"
)

// The node's workspace is where remote tasks run. Two things about it are easy to get
// wrong quietly: a "~" that is never expanded (which creates a literal "~" directory), and
// an empty value silently becoming something (the process's cwd, of all things).

func TestWorkspaceDirExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to expand against")
	}

	for _, tc := range []struct{ in, want string }{
		{"~/workspace/", filepath.Join(home, "workspace")},
		{"~", home},
		{"/tmp/tachi-node-workspace", "/tmp/tachi-node-workspace"},
	} {
		got, err := (NodeConfig{Workspace: tc.in}).WorkspaceDir()
		if err != nil {
			t.Fatalf("WorkspaceDir(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("WorkspaceDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWorkspaceDirRefusesAnEmptyValue(t *testing.T) {
	if _, err := (NodeConfig{}).WorkspaceDir(); err == nil {
		t.Fatal("an empty workspace must be an error: a remote task with no working " +
			"directory would otherwise run somewhere nobody chose")
	}
}

// The default is a path of the node's own, not the process's cwd — the whole point of
// taking the workspace from config.
func TestWorkspaceDefaultIsNotTheProcessCwd(t *testing.T) {
	// The tag mechanism itself, not the user's file: defaults.Set is what fills every
	// `default:` tag in the struct.
	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		t.Fatalf("defaults.Set: %v", err)
	}
	if cfg.Node.Workspace == "" {
		t.Fatal("node.workspace has no default")
	}
	if cfg.Node.Workspace == "." {
		t.Fatalf("node.workspace defaulted to the process cwd (%q)", cfg.Node.Workspace)
	}
}
