package node

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// nodeExecutor is the receiving side as the protocol sees it: one accepted task = one
// Tachi turn, and the turn's outcome is what the task's state says.
//
// It is deliberately thin. Everything about *how* a turn runs lives in the runner; what
// lives here is the mapping, and one rule dominates it: **publish what was confirmed**.
// A turn that produced partial text and then died is FAILED, an answer is an artifact, and
// a turn that was stopped by a cancel publishes nothing at all — the canceller's CANCELED
// is the state, and writing an outcome here would overwrite the truth with a guess.
type nodeExecutor struct {
	runner *runner
}

var _ a2asrv.AgentExecutor = (*nodeExecutor)(nil)

func (e *nodeExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// A NEW task has to be announced by the executor (the server records what the
		// executor yields); a continuation already has one.
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		res, err := e.runner.runTurn(ctx, string(execCtx.TaskID),
			callerFrom(ctx), execCtx.ContextID, messageText(execCtx.Message))

		if ctx.Err() != nil {
			// This turn was stopped — a cancel arrived, or the caller went away. The state
			// belongs to whoever stopped it.
			return
		}
		switch {
		case err != nil:
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failureMessage(err)), nil)
			return
		case res.Error != nil:
			// The turn ran and failed. Its partial answer is NOT a result (design §7.3):
			// what failed is the work, and reporting it as completed would be the record
			// claiming something that did not happen.
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failureMessage(res.Error)), nil)
			return
		}

		if res.Text != "" {
			if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(res.Text)), nil) {
				return
			}
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *nodeExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// Stop the turn FIRST. The protocol's concurrent-cancel path writes the
		// cancellation into the running execution and then waits for that execution to
		// end before answering the caller — a cancel that stopped nothing would hang the
		// caller instead of stopping the work.
		e.runner.stopTurn(string(execCtx.TaskID))

		if execCtx.StoredTask == nil {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// failureMessage is the status message a failed task carries.
func failureMessage(err error) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))
}
