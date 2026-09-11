package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// TestResolvePath pins the resolution rule every path-taking tool now goes
// through. It is a refactor pin, not a spec: each row records what the tools did
// before the join was hoisted into one function, including the surprises.
func TestResolvePath(t *testing.T) {
	ctx := wdctx.WithDir(context.Background(), "/work/tree")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"absolute passes through untouched", "/etc/hosts", "/etc/hosts"},
		{"relative joins the working directory", "src/main.go", "/work/tree/src/main.go"},
		{"dot is the working directory itself", ".", "/work/tree"},
		{"empty is the working directory itself", "", "/work/tree"},
		{"leading traversal is cleaned", "../shared/lib.go", "/work/shared/lib.go"},
		{"interior traversal is cleaned", "a/../b.go", "/work/tree/b.go"},
		// Pinned, not endorsed: tools do NOT expand "~" (the @-file layer does), so
		// a tilde is a literal directory name here. Changing this is a separate
		// decision — a refactor must not change it by accident.
		{"tilde is a literal segment", "~/notes.md", "/work/tree/~/notes.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolvePath(ctx, tt.in); got != tt.want {
				t.Errorf("ResolvePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestResolvePathWithoutDir covers the fallback path: with nothing bound to the
// context the rule still applies against whatever wdctx.Dir falls back to, which
// is what the main agent (cd-tracked directory) relies on.
func TestResolvePathWithoutDir(t *testing.T) {
	want := filepath.Join(wdctx.Dir(context.Background()), "a.go")
	if got := ResolvePath(context.Background(), "a.go"); got != want {
		t.Errorf("ResolvePath(a.go) = %q, want %q", got, want)
	}
}

// TestResolveSearchPath covers the search variant: an empty path means the working
// directory, and the result is always absolute (ripgrep needs it).
func TestResolveSearchPath(t *testing.T) {
	ctx := wdctx.WithDir(context.Background(), "/work/tree")

	for _, in := range []string{"", ".", "sub/dir"} {
		got, err := resolveSearchPath(ctx, in)
		if err != nil {
			t.Fatalf("resolveSearchPath(%q): %v", in, err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("resolveSearchPath(%q) = %q, want an absolute path", in, got)
		}
	}

	// An empty or "." path searches the working directory itself.
	for _, in := range []string{"", "."} {
		if got, _ := resolveSearchPath(ctx, in); got != "/work/tree" {
			t.Errorf("resolveSearchPath(%q) = %q, want /work/tree", in, got)
		}
	}
}
