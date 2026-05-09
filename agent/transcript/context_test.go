package transcript

import (
	"context"
	"testing"
)

func TestContext_WithBuilder(t *testing.T) {
	b := NewBuilder()
	ctx := WithBuilder(context.Background(), b)
	got := BuilderFromContext(ctx)
	if got != b {
		t.Error("BuilderFromContext did not return the stored builder")
	}
}

func TestContext_BuilderFromContext_Nil(t *testing.T) {
	b := BuilderFromContext(context.Background())
	if b != nil {
		t.Error("BuilderFromContext should return nil for empty context")
	}
}