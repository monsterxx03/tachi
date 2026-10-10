package agent

import (
	"context"
	"errors"
	"fmt"
)

// Managed sessions: a conversation a SUBSYSTEM drives, not the person at the keyboard.
//
// Today there is one kind — a receiving node's A2A execution (session.KindA2AExecution) —
// and the rule is written for any future kind: an ordinary frontend must not open such a
// session and continue it as its own chat. The reasons are the node's, not the UI's
// (design §2.1): a local turn would go around the context's serial execution, the node's
// permissions and its task records, and it could race the remote agent over one history.
//
// WHERE the check lives is the load-bearing part. Every frontend calls RunConversationStream
// directly — tui, desktop, channel, acp, the one-off commands — so a wrapper each of them has
// to remember to call is a check the next caller simply will not have. It sits inside the
// public entry, before any side effect: before the reminder block is built, before the user
// turn is written, before a title is generated and before a checkpoint is taken.
//
// The grant is carried on the context and set by the subsystem that owns the session
// (node/runner.go). It is not a boundary against other code in this process — Go code can
// call anything — but it IS one against everything that reaches a turn from outside: a model
// cannot set it (it is not an argument), and neither can session metadata, a tool result or a
// remote request.

// ErrManagedSession is returned (as an error event) when a turn was refused because its
// session belongs to a subsystem.
var ErrManagedSession = errors.New("this session is driven by a subsystem, not by a local frontend")

type trustedExecutionKey struct{}

// WithTrustedExecution marks a run as the trusted execution of the managed session it
// drives: the subsystem owning that session calls this, and nothing else does.
func WithTrustedExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedExecutionKey{}, true)
}

func trustedExecutionFrom(ctx context.Context) bool {
	trusted, _ := ctx.Value(trustedExecutionKey{}).(bool)
	return trusted
}

// managedSessionRefusal reports why this run may not touch its session, or nil when it may.
func (a *AIAgent) managedSessionRefusal(ctx context.Context) error {
	if a == nil || a.Config.SessionManager == nil || trustedExecutionFrom(ctx) {
		return nil
	}
	cur := a.Config.SessionManager.Current()
	if !cur.Managed() {
		return nil
	}
	return fmt.Errorf("%w (kind=%s, session=%s)", ErrManagedSession, cur.Kind, cur.ID)
}

// managedSessionWriteRefusal is the same rule for a caller that is NOT a run: it reports why
// this agent's session may not be MODIFIED, or nil when it may.
//
// It exists because not every write goes through a turn. A rewind rewrites the conversation and
// restores workspace files with no run involved, and a receiving node's execution is exactly the
// session a local frontend must not rewrite from behind (design §2.1) — the node would keep
// appending to a history that was cut under it, and its context binding and retention holds
// would still describe a conversation that no longer exists.
//
// Two functions rather than one because the shapes differ (a run reports through an error event
// on its channel; a command reports to the caller), and they read the SAME fact —
// session.Session.Managed() — so they cannot disagree about what "managed" means.
//
// No context grant here, deliberately: no subsystem rewinds today, and a trusted writer that
// ever needs one must say so explicitly rather than inherit an exemption from the turn path.
func (a *AIAgent) managedSessionWriteRefusal() error {
	if a == nil || a.Config.SessionManager == nil {
		return nil
	}
	cur := a.Config.SessionManager.Current()
	if !cur.Managed() {
		return nil
	}
	return fmt.Errorf("%w (kind=%s, session=%s)", ErrManagedSession, cur.Kind, cur.ID)
}

// refuseManagedSession reports a refused turn the way every other early exit does: an error
// event the caller renders, with the reason in it. Nothing has been written at this point —
// that is what makes the refusal safe rather than merely visible.
func (a *AIAgent) refuseManagedSession(ctx context.Context, ch chan<- AgentEvent, err error) {
	a.Config.Logger.Error(ctx, "Agent: refused a turn on a managed session", err)
	ch <- AgentEvent{
		Type:      AgentEventError,
		TextDelta: err.Error(),
		Result:    &RunResult{ExitReason: ExitReasonError, Error: err},
	}
	close(ch)
}
