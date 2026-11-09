package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAskTimeoutRoundTrip(t *testing.T) {
	ctx := WithAskTimeout(context.Background(), 2*time.Minute)
	assert.Equal(t, 2*time.Minute, AskTimeoutFromCtx(ctx))

	// Zero or absent means "wait for the user", which is the interactive default.
	assert.Zero(t, AskTimeoutFromCtx(context.Background()))
	assert.Zero(t, AskTimeoutFromCtx(nil)) //nolint:staticcheck // nil is exactly what the guard is for
	assert.Zero(t, AskTimeoutFromCtx(WithAskTimeout(context.Background(), 0)))
}
