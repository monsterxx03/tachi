package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var fallbackCounter atomic.Int64

// NewTraceID generates a new trace ID in the format "turn_<random8hex>".
// Example: "turn_a1b2c3d4".
// Falls back to a time+counter based ID if crypto/rand is unavailable.
func NewTraceID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp + atomic counter to maintain uniqueness.
		return fmt.Sprintf("turn_%x_%x", time.Now().UnixNano(), fallbackCounter.Add(1))
	}
	return "turn_" + hex.EncodeToString(b)
}

type traceIDKey struct{}

// WithTraceID attaches a trace ID to the context.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFromContext retrieves the trace ID from the context.
// Returns empty string if not set.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}
	return ""
}
