package tools

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPathPolicy_CheckPath(t *testing.T) {
	policy := &PathPolicy{
		AllowedWriteDirs: []string{"/home/user/.tachi/memory", "/tmp/project/.tachi/memory"},
	}

	tests := []struct {
		path    string
		allowed bool
	}{
		{"/home/user/.tachi/memory/topics/design.md", true},
		{"/home/user/.tachi/memory/inbox.md", true},
		{"/home/user/.tachi/memory", true},
		{"/tmp/project/.tachi/memory/topics/arch.md", true},
		{"/home/user/.tachi/config.yaml", false},
		{"/etc/passwd", false},
		{"/home/user/.tachi/memoryfake/evil.md", false},
		{"/tmp/project/src/main.go", false},
	}

	for _, tt := range tests {
		err := policy.CheckPath(tt.path)
		if tt.allowed && err != nil {
			t.Errorf("CheckPath(%q): expected allowed, got error: %v", tt.path, err)
		}
		if !tt.allowed && err == nil {
			t.Errorf("CheckPath(%q): expected denied, got nil", tt.path)
		}
	}
}

func TestPathPolicy_Nil(t *testing.T) {
	var policy *PathPolicy
	if err := policy.CheckPath("/anything"); err != nil {
		t.Errorf("nil policy should allow everything, got: %v", err)
	}
}

func TestPathPolicy_Context(t *testing.T) {
	ctx := context.Background()

	// No policy in context.
	if p := GetPathPolicy(ctx); p != nil {
		t.Error("expected nil policy from bare context")
	}

	// Set policy.
	policy := &PathPolicy{AllowedWriteDirs: []string{"/tmp/mem"}}
	ctx = WithPathPolicy(ctx, policy)

	got := GetPathPolicy(ctx)
	if got == nil {
		t.Fatal("expected non-nil policy")
	}
	if len(got.AllowedWriteDirs) != 1 || got.AllowedWriteDirs[0] != "/tmp/mem" {
		t.Errorf("unexpected policy: %+v", got)
	}
}

func TestPathPolicy_TrailingSlash(t *testing.T) {
	// Ensure paths work with/without trailing slash.
	policy := &PathPolicy{
		AllowedWriteDirs: []string{"/tmp/memory/"},
	}

	absPath := filepath.Clean("/tmp/memory/topics/test.md")
	if err := policy.CheckPath(absPath); err != nil {
		t.Errorf("should allow path within dir (trailing slash): %v", err)
	}
}
