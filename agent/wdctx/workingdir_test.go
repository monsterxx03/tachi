package wdctx

import (
	"context"
	"sync"
	"testing"
)

func TestDir_NoFallbackNoContextDir(t *testing.T) {
	resetGlobals()
	// No fallback set, no context dir — should return "."
	got := Dir(t.Context())
	if got != "." {
		t.Errorf("Dir() = %q, want %q", got, ".")
	}
}

func TestDir_WithContextDir(t *testing.T) {
	resetGlobals()
	ctx := WithDir(t.Context(), "/tmp/worktree")
	got := Dir(ctx)
	if got != "/tmp/worktree" {
		t.Errorf("Dir() = %q, want %q", got, "/tmp/worktree")
	}
}

func TestDir_FallbackDir(t *testing.T) {
	resetGlobals()
	SetFallbackDir(func() string { return "/fallback/path" })
	// No context dir, should use fallback
	got := Dir(t.Context())
	if got != "/fallback/path" {
		t.Errorf("Dir() = %q, want %q", got, "/fallback/path")
	}
}

func TestDir_ContextDirTakesPriorityOverFallback(t *testing.T) {
	resetGlobals()
	SetFallbackDir(func() string { return "/fallback/path" })
	ctx := WithDir(t.Context(), "/ctx/path")
	got := Dir(ctx)
	if got != "/ctx/path" {
		t.Errorf("Dir() = %q, want %q", got, "/ctx/path")
	}
}

func TestSetFallbackDir_OnceBehavior(t *testing.T) {
	resetGlobals()

	var callCount int
	SetFallbackDir(func() string { callCount++; return "/first" })
	SetFallbackDir(func() string { callCount++; return "/second" })
	SetFallbackDir(func() string { callCount++; return "/third" })

	// Should use the first registered function
	got := Dir(t.Context())
	if got != "/first" {
		t.Errorf("Dir() = %q, want %q", got, "/first")
	}
	// The first function should be called exactly once
	if callCount != 1 {
		t.Errorf("fallback called %d times, want 1", callCount)
	}
}

func TestDir_NilContext(t *testing.T) {
	resetGlobals()
	// Nil context without fallback should return "."
	got := Dir(context.TODO())
	if got != "." {
		t.Errorf("Dir(nil) = %q, want %q", got, ".")
	}
}

func TestDir_NilContextWithFallback(t *testing.T) {
	resetGlobals()
	SetFallbackDir(func() string { return "/fallback" })
	// Nil context with fallback should use fallback
	got := Dir(context.TODO())
	if got != "/fallback" {
		t.Errorf("Dir(nil) = %q, want %q", got, "/fallback")
	}
}

func TestWithDir_DoesNotModifyParent(t *testing.T) {
	resetGlobals()
	parent := t.Context()
	child := WithDir(parent, "/child/path")

	// Parent should not have the dir
	parentDir := Dir(parent)
	if parentDir != "." {
		t.Errorf("Dir(parent) = %q, want %q", parentDir, ".")
	}

	// Child should have the dir
	childDir := Dir(child)
	if childDir != "/child/path" {
		t.Errorf("Dir(child) = %q, want %q", childDir, "/child/path")
	}
}

func TestDir_EmptyStringInContextFallsBack(t *testing.T) {
	resetGlobals()
	SetFallbackDir(func() string { return "/fallback" })
	// Setting empty string should be treated as "not set" and fall through
	ctx := WithDir(t.Context(), "")
	got := Dir(ctx)
	if got != "/fallback" {
		t.Errorf("Dir() = %q, want %q", got, "/fallback")
	}
}

// resetGlobals restores package-level state for isolated test runs.
func resetGlobals() {
	fallbackDirMu = sync.Mutex{}
	fallbackOnce = sync.Once{}
	fallbackDir = nil
}
