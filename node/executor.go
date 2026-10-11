package node

import (
	"context"
	"errors"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/monsterxx03/tachi/agent/tools"
)

// nodeExecutor is the receiving side as the protocol sees it: one accepted task = one
// Tachi turn, and the turn's outcome is what the task's state says.
//
// It is deliberately thin. Everything about *how* a turn runs lives in the runner; what
// lives here is the mapping, and one rule dominates it: **publish what was confirmed**.
// A turn that produced partial text and then died is FAILED, an answer is an artifact, and
// a turn that was stopped by a cancel publishes nothing at all — the canceller's CANCELED
// is the state, and writing an outcome here would overwrite the truth with a guess.
//
// A turn can also STOP to ask a question, and that is the part that does not fit in one call
// (§7.4): the executor publishes INPUT_REQUIRED and RETURNS, because that is where a blocking
// message/send stops — while the turn itself stays alive in the runner, and the message that
// answers the question runs this mapping again as a continuation of the same turn.
type nodeExecutor struct {
	runner *runner
}

var _ a2asrv.AgentExecutor = (*nodeExecutor)(nil)

// nodeState is the optional half of an AgentExecutor: what an executor that keeps state
// OUTSIDE a request needs from the rest of the node.
//
//   - consumeTaskStore receives the node's task store. It is opened by the host after the
//     executor is built (the store owns the startup recovery, so there may be exactly one),
//     and it is what lets a question whose deadline passes end its task.
//   - reconcile repairs what the last process left behind, once both halves of its state exist
//     (the task store and the retention record). It runs before the surface accepts work.
//   - shutdown ends the work that is waiting for something that is no longer coming.
type nodeState interface {
	consumeTaskStore(*taskStore)
	reconcile()
	shutdown()
}

var _ nodeState = (*nodeExecutor)(nil)

func (e *nodeExecutor) consumeTaskStore(store *taskStore) { e.runner.consumeTaskStore(store) }

func (e *nodeExecutor) reconcile() { e.runner.reconcile() }

func (e *nodeExecutor) shutdown() { e.runner.shutdown() }

// EndSessionWork forwards the machine owner's stop to the runner.
//
// The forwarding exists because the SURFACE hands out the executor (that is what a node builds
// its handler around), while the operation is about one of this machine's sessions rather than
// about a caller's task — so it is not part of a2asrv.AgentExecutor, and it is reached through
// the workEnder assertion the local control interface makes.
func (e *nodeExecutor) EndSessionWork(ctx context.Context, sessionID string) (int, error) {
	return e.runner.EndSessionWork(ctx, sessionID)
}

// waitingQuestion is the admission guard's view of what this node is waiting on (see
// questionSource): the guard decides whether a message is an answer BEFORE the SDK starts an
// execution for it, which is the only place a refusal can still be a protocol error.
func (e *nodeExecutor) waitingQuestion(taskID a2a.TaskID) (*parkedAsk, bool) {
	return e.runner.waitingQuestion(taskID)
}

func (e *nodeExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask != nil {
			e.continueTask(ctx, execCtx, yield)
			return
		}

		// A NEW task has to be announced by the executor (the server records what the
		// executor yields); a continuation already has one.
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		// What the message carried besides text: the admission guard already refused anything this
		// node will not take, so this is the same reading of the same Parts (node/partfile.go).
		files, _ := filePartsOf(execCtx.Message.Parts)
		lt, err := e.runner.beginTurn(ctx, execCtx.TaskID, callerFrom(ctx),
			execCtx.ContextID, messageText(execCtx.Message), files)
		if err != nil {
			// The turn never started: session preparation failed, or the node is shutting
			// down. A task that could not start is failed, with the reason as its status.
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failureMessage(err)), nil)
			return
		}
		e.publish(execCtx, yield, lt.advance())
	}
}

// continueTask resumes the turn that asked the question this message answers.
func (e *nodeExecutor) continueTask(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	stored := execCtx.StoredTask
	if stored.Status.State != a2a.TaskStateInputRequired {
		// Only a task waiting for an answer can be continued: the protocol layer refuses the
		// states around this one (a running task is busy, a finished one is finished), so
		// anything else is a path that got past it and must not be guessed at.
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			failureMessage(&notWaitingError{taskID: string(execCtx.TaskID), state: stored.Status.State})), nil)
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
		return
	}

	out, err := e.runner.resumeTurn(execCtx.TaskID, execCtx.Message)
	if err != nil {
		// The answer could not be delivered. Every one of these was refused by the guard
		// before the SDK started this execution, so reaching it here means something the
		// guard could not see — and a question that can no longer be answered leaves a task
		// that cannot continue.
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failureMessage(err)), nil)
		return
	}
	e.publish(execCtx, yield, out)
}

// publish turns one turn's outcome into the events the protocol sees.
func (e *nodeExecutor) publish(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool, out turnOutcome) {
	switch {
	case out.Stopped:
		// This turn was stopped — a cancel, a shutdown, an expired question. The state
		// belongs to whoever stopped it, and the turn keeps nothing to say: a cancel writes
		// CANCELED, an expiry wrote FAILED when it fired.
		e.runner.forgetTurn(execCtx.TaskID)
		return

	case out.Ask != nil:
		// The turn is parked, and the question is the answer the caller has to give. The
		// turn stays in the runner; this call ends here, which is exactly where a blocking
		// message/send belongs (a task in INPUT_REQUIRED is an interrupted state, §6.2).
		//
		// The task record is written before this reaches the caller (the SDK applies the
		// event to the store on its way through), so the question is on disk before anyone
		// can see it — and before anyone can answer it.
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, questionMessage(out.Ask)), nil)
		return
	}

	e.runner.forgetTurn(execCtx.TaskID)

	if out.Result == nil {
		publishFiles(execCtx, yield, out.Files)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			failureMessage(errors.New("node: the turn ended without reporting an outcome"))), nil)
		return
	}
	if out.Result.Error != nil {
		// The turn ran and failed. Its partial answer is NOT a result (design §7.3):
		// what failed is the work, and reporting it as completed would be the record
		// claiming something that did not happen.
		publishFiles(execCtx, yield, out.Files)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failureMessage(out.Result.Error)), nil)
		return
	}

	if out.Result.Text != "" {
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(out.Result.Text)), nil) {
			return
		}
	}
	publishFiles(execCtx, yield, out.Files)
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

// publishFiles puts the files a turn handed over into the task's artifact.
//
// Files ride as their own artifact with `raw` Parts: the protocol's carrier for a file is an
// artifact (§6.4), and the BYTES are what leaves — never a path on this machine, which is the thing
// the design forbids handing a caller. Emitted on a failed task too: what the work produced before
// it failed is part of the result a failure keeps (§7.3).
func publishFiles(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool, files []filePart) {
	if len(files) == 0 {
		return
	}
	parts := make([]*a2a.Part, 0, len(files))
	for _, f := range files {
		part := a2a.NewRawPart(f.Bytes)
		part.Filename, part.MediaType = f.Name, f.MediaType
		parts = append(parts, part)
	}
	yield(a2a.NewArtifactEvent(execCtx, parts...), nil)
}

func (e *nodeExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// Stop the turn FIRST. The protocol's concurrent-cancel path writes the
		// cancellation into the running execution and then waits for that execution to
		// end before answering the caller — a cancel that stopped nothing would hang the
		// caller instead of stopping the work. For a PARKED turn this is the only thing
		// that ends it: the SDK's execution for that task is already over, so nothing else
		// will ever wake the loop.
		e.runner.stopTurn(execCtx.TaskID)

		if execCtx.StoredTask == nil {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// notWaitingError is why a continuation was refused: the task's state says it is not waiting
// for an answer.
type notWaitingError struct {
	taskID string
	state  a2a.TaskState
}

func (e *notWaitingError) Error() string {
	return "node: task " + e.taskID + " is " + string(e.state) + "; it is not waiting for an answer"
}

// failureMessage is the status message a failed task carries.
func failureMessage(err error) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))
}

// questionMessage is how a question reaches the caller: what is being asked, in a text part
// per question, plus the same questions as structured data, and the question's ID in the
// metadata so the answer can name the question it answers.
//
// The ID is what makes an answer unambiguous. Without it, "which question is this an answer
// to?" has no answer beyond "whatever is parked", and a late answer to a question that is no
// longer open would silently become the answer to the current one.
func questionMessage(ask *parkedAsk) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, questionParts(ask.questions)...)
	msg.Metadata = map[string]any{QuestionIDMetadataKey: ask.questionID}
	return msg
}

// questionParts renders a question set for a caller.
//
// Both shapes, because they answer different readers: the text is what a person reads, and
// the structured part is what a client builds a form from — §6.4's structured part is the
// only way the options stay options instead of becoming prose the caller has to parse.
//
// The structured half is built out of maps and slices on purpose. The SDK clones a task
// through gob on its way into the store, and gob can carry only builtin or registered types
// inside an `any` slot: handing it `[]tools.Question` fails the whole event with
// "gob: type not registered for interface" and the task ends up FAILED (the SDK registers
// map[string]any and []any, which is why the JSON-shaped spelling is the one that works).
func questionParts(questions []tools.Question) []*a2a.Part {
	parts := make([]*a2a.Part, 0, len(questions)+1)
	for _, q := range questions {
		var b strings.Builder
		b.WriteString(q.Question)
		for _, opt := range q.Options {
			b.WriteString("\n- ")
			b.WriteString(opt.Label)
			if opt.Description != "" {
				b.WriteString(": ")
				b.WriteString(opt.Description)
			}
		}
		if q.MultiSelect {
			b.WriteString("\n(multiple answers allowed)")
		}
		parts = append(parts, a2a.NewTextPart(b.String()))
	}
	// The same set, for a client that renders the options rather than reading them.
	parts = append(parts, a2a.NewDataPart(map[string]any{"questions": questionsData(questions)}))
	return parts
}

// questionsData is the question set in the shape a data part can carry: maps, slices and
// strings only (see questionParts).
func questionsData(questions []tools.Question) []any {
	out := make([]any, 0, len(questions))
	for _, q := range questions {
		options := make([]any, 0, len(q.Options))
		for _, opt := range q.Options {
			option := map[string]any{"label": opt.Label, "description": opt.Description}
			if opt.Preview != "" {
				option["preview"] = opt.Preview
			}
			options = append(options, option)
		}
		out = append(out, map[string]any{
			"question":    q.Question,
			"header":      q.Header,
			"options":     options,
			"multiSelect": q.MultiSelect,
		})
	}
	return out
}
