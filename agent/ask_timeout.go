package agent

import (
	"context"
	"time"
)

// askTimeoutKey carries the bound on ONE ask — a permission card or an
// AskUserQuestion — for a run. Zero (or absent) means "wait for the user", which
// is what an interactive turn wants.
//
// It is a context value rather than a field because the bound belongs to the RUN,
// not to the agent: the same conversation answers a card normally in a turn the
// user started and must not be parked forever in a turn a finished task woke.
type askTimeoutKey struct{}

// WithAskTimeout bounds each ask this run makes. On expiry the ask is answered
// as "the user was not available": a permission is refused and a question
// returns an error telling the model to put it in its reply, so the run can
// finish instead of waiting for someone who is not there.
func WithAskTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, askTimeoutKey{}, d)
}

// AskTimeoutFromCtx reports the run's ask bound; 0 means none.
func AskTimeoutFromCtx(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	d, _ := ctx.Value(askTimeoutKey{}).(time.Duration)
	return d
}
