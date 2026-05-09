// Package wdctx provides a context-based working directory mechanism that
// allows sub-agents running in isolated git worktrees to resolve file paths
// relative to their worktree directory instead of the main process CWD.
//
// The working directory flows through context.Context, allowing multiple
// concurrent sub-agents to each have their own isolated filesystem view
// without sharing mutable global state.
//
// SetFallbackDir must be called once (typically from tools package init) to
// provide the fallback function used when no working directory is set in the
// context. Without it, Dir() returns ".".
package wdctx

import (
	"context"
	"sync"
)

type contextKey struct{}

var (
	fallbackDir   func() string
	fallbackDirMu sync.Mutex
	fallbackOnce  sync.Once
)

// SetFallbackDir registers the fallback working directory function.
// Must be called exactly once; subsequent calls are no-ops.
// This is typically called from the tools package init() to wire up
// getWorkingDir() without creating a circular import.
func SetFallbackDir(fn func() string) {
	fallbackOnce.Do(func() {
		fallbackDir = fn
	})
}

// WithDir returns a new context with the given working directory.
// Use this to bind a sub-agent to its isolated worktree path.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, contextKey{}, dir)
}

// Dir retrieves the working directory from ctx. When no directory is set in
// the context, it falls back to the registered fallback function. This ensures
// backward compatibility for the main agent, which tracks the working directory
// via cd commands.
//
// Handles nil context gracefully by falling through to the fallback.
func Dir(ctx context.Context) string {
	if ctx != nil {
		if dir, ok := ctx.Value(contextKey{}).(string); ok && dir != "" {
			return dir
		}
	}
	fallbackDirMu.Lock()
	fn := fallbackDir
	fallbackDirMu.Unlock()
	if fn != nil {
		return fn()
	}
	return "."
}