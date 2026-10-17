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

// Multicast discovery has ONE switch: `enabled` covers both looking and being looked for, because a
// machine that may speak to its neighbours on a network may be spoken to about on it. That is a
// decision a second key would quietly undo — and a second key is exactly what a reader would add
// back after reading the old comments — so the shape is asserted here rather than left to review.
func TestMulticastDiscoveryHasOneSwitch(t *testing.T) {
	leaves := make(map[string]bool)
	for _, path := range YAMLPaths() {
		leaves[path] = true
	}
	if !leaves["node.discovery.mdns.enabled"] {
		t.Error("node.discovery.mdns.enabled is not a leaf of the config")
	}
	if leaves["node.discovery.mdns.advertise"] {
		t.Error("node.discovery.mdns.advertise is back: announcing is the same switch as browsing, " +
			"and a machine that can find others without being findable is not a thing this config says")
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
