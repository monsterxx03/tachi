package transcript

import "context"

// contextKey is an unexported type for context keys to avoid collisions.
type contextKey string

const builderKey contextKey = "transcript-builder"

// WithBuilder stores a Builder in the context. Sub-agent executors retrieve
// it via BuilderFromContext so child agents can write to the correct
// sub-transcript.
func WithBuilder(ctx context.Context, b *Builder) context.Context {
	return context.WithValue(ctx, builderKey, b)
}

// BuilderFromContext retrieves a Builder from the context. Returns nil if
// no builder was stored — callers should handle nil gracefully.
func BuilderFromContext(ctx context.Context) *Builder {
	b, _ := ctx.Value(builderKey).(*Builder)
	return b
}