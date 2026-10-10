package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// The runner: what a remote task actually executes.
//
// It is the receiving side's half of the design (§7.1): resolve the context to a session,
// build the agent that owns that session, run ONE turn, and report what happened in
// Tachi's own terms. Turning that into protocol events is the executor's job
// (node/executor.go) — this file knows nothing about A2A beyond task ids.
//
// Three decisions are encoded here and each is load-bearing:
//
//   - The workspace comes from the NODE's config, never from the hosting process's cwd
//     (§10.3). A node started from a shell must not do remote work in that shell's
//     directory: the caller cannot see it, and nobody asked for it.
//   - A turn runs with the node's tool ALLOWLIST as a per-run tool view (§10.2), enforced
//     both by hiding the tools from the model and by refusing them at execution. An empty
//     list means no tools at all. Asking the CALLER a question is not on that list's
//     subject: it touches nothing on this machine, so AskUserQuestion is always available
//     (see toolViewOption).
//   - Permission asks are DENIED, not forwarded (§10.1): nobody is attached to a remote
//     task, so an `ask` rule has no one to ask. A deny stays a deny either way — and the
//     caller's answer to a question never becomes an approval (see denyRemoteAsk).
//
// The fourth is the one that shapes this file: a turn can STOP and wait for an answer
// (§7.4), and it then outlives the request that started it. A liveTurn is that object.
type runner struct {
	cfg       *config.Config
	workspace string
	logger    *logger.Logger

	contexts *contextStore
	parking  *parking

	// tasks is the node's task store, handed over by the host once it exists (see
	// consumeTaskStore). It is needed outside any execution: a question whose deadline
	// passes has to end its task, and the store is the only thing that writes a task's
	// state.
	tasks *taskStore

	mu     sync.Mutex
	runs   map[string]*contextRun
	turns  map[a2a.TaskID]*liveTurn
	closed bool
}

// contextRun is the live half of a context binding: the session manager positioned on the
// context's current session, and the agent that owns it. The agent may be evicted and
// rebuilt from the session history (design §7.1.1); nothing in a request depends on it
// having stayed in memory.
type contextRun struct {
	binding *contextBinding
	sm      *session.Manager
	agent   *agent.AIAgent
}

// turnResult is one remote turn, as Tachi measured it.
type turnResult struct {
	// Text is the turn's final answer. It becomes the task's artifact — not the streaming
	// deltas, which are progress, and not "we saw some text", which is not an outcome.
	Text       string
	ExitReason string
	Iterations int
	// Error is set when the turn failed. A caller must report it as a failure: a turn that
	// produced partial text and then died is not a successful task (design §7.3).
	Error error
}

// turnOutcome is what reading a turn's stream produced. Exactly one of the three is set,
// and they are three different things: the turn finished, it is WAITING for an answer, or
// somebody stopped it — and a stopped turn publishes nothing, because the state that says
// so belongs to whoever stopped it.
type turnOutcome struct {
	Result  *turnResult
	Ask     *parkedAsk
	Stopped bool
}

// liveTurn is one turn in flight.
//
// A turn that stops to ask outlives the protocol call that started it: the call returns
// INPUT_REQUIRED (that is where a blocking message/send stops — §6.2), the SDK's execution
// for the task is over, and the agent loop inside this struct stays blocked on the answer
// channel. The next message for the task resumes reading the SAME stream, which is what
// keeps the answer inside the turn that asked the question instead of starting a second one
// (§7.1.2).
type liveTurn struct {
	taskID    a2a.TaskID
	contextID string

	agent   *agent.AIAgent
	logger  *logger.Logger
	parking *parking
	// ctx belongs to the TURN, not to a request: it is what keeps a turn alive across the
	// request that started it, and what a cancel ends (§7.2).
	ctx    context.Context
	cancel context.CancelFunc
	events <-chan agent.AgentEvent

	mu       sync.Mutex
	stopped  bool
	reading  bool
	draining bool
}

func newRunner(cfg *config.Config, workspace string, dataDir string, log *logger.Logger) (*runner, error) {
	contexts, err := openContextStore(filepath.Join(dataDir, contextsDirName))
	if err != nil {
		return nil, err
	}
	// The node reads and writes the SAME session store the local frontends use (design
	// §7.1.1): a remote conversation is a session on this machine, not a parallel universe.
	sm, err := session.NewManager(log)
	if err != nil {
		return nil, fmt.Errorf("node: session store: %w", err)
	}
	// One probe is enough to fail early and clearly if the session store cannot be
	// opened; the per-context managers below are separate instances (a manager's "current"
	// session is per conversation).
	if _, err := sm.List(); err != nil {
		return nil, fmt.Errorf("node: session store unreadable: %w", err)
	}
	r := &runner{
		cfg:       cfg,
		workspace: workspace,
		logger:    log,
		contexts:  contexts,
		runs:      make(map[string]*contextRun),
		turns:     make(map[a2a.TaskID]*liveTurn),
	}
	r.parking = newParking(cfg.Node.QuestionTimeout, cfg.Node.MaxParked, r.questionExpired)
	return r, nil
}

// consumeTaskStore is the host handing over the node's task store (see nodeState). It is
// called once, before the surface accepts work, so the read below needs no lock.
func (r *runner) consumeTaskStore(store *taskStore) { r.tasks = store }

// shutdown ends every turn in flight, parked ones included.
//
// It does NOT rewrite the tasks those turns were working on: the next start resolves
// whatever was still open as interrupted, and it keeps the question a parked task was
// waiting on (see openTaskStore). Writing here would be a second implementation of the same
// rule, in the one place that cannot be tested against a crash.
func (r *runner) shutdown() {
	r.mu.Lock()
	r.closed = true
	ids := make([]a2a.TaskID, 0, len(r.turns))
	for id := range r.turns {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	for _, id := range ids {
		r.stopTurn(id)
	}
}

// questionExpired is the parking lot's deadline firing: nobody answered, so the turn ends
// and the task says so.
//
// The record must not be left saying INPUT_REQUIRED. That state means "a turn in this
// process is waiting for the answer", and after this nobody is — so a task that was never
// answered is FAILED, with the question kept in its status. The alternative (leave it
// parked) would leave a caller polling a question that can never be answered, and a
// mismatch between what the record claims and what the process holds.
func (r *runner) questionExpired(ask *parkedAsk) {
	if ask.stop != nil {
		ask.stop()
	}
	r.forgetTurn(ask.taskID)

	if r.tasks == nil {
		// Nothing else can resolve the task, and stopping the turn has already broken the
		// promise INPUT_REQUIRED makes. Say so instead of hiding it: this is a wiring bug,
		// and the alternative (not expiring) would keep a caller waiting forever.
		r.logger.Error(context.Background(),
			"node: a question expired but this node has no task store; the task record still says it is waiting",
			nil, "task", string(ask.taskID))
		return
	}
	reason := fmt.Sprintf("no answer arrived within %s, so this task was ended rather than left waiting",
		r.parking.timeout)
	if err := r.tasks.failQuestion(ask.taskID, reason); err != nil {
		r.logger.Error(context.Background(), "node: could not resolve an expired question",
			err, "task", string(ask.taskID))
		return
	}
	r.logger.Info(context.Background(), "node: a question expired",
		"task", string(ask.taskID), "question", ask.questionID, "timeout", r.parking.timeout.String())
}

// beginTurn starts a turn for a task that has none, and returns it.
func (r *runner) beginTurn(
	parent context.Context, taskID a2a.TaskID, callerID, contextID, prompt string,
) (*liveTurn, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("node: this node is shutting down")
	}
	if _, ok := r.turns[taskID]; ok {
		// Two turns on one task would write into one session from two places, and the
		// protocol layer is supposed to make it impossible (one execution per task at a
		// time). Refusing beats silently joining a turn that was started by someone else.
		r.mu.Unlock()
		return nil, fmt.Errorf("node: task %s already has a turn in flight", taskID)
	}
	r.mu.Unlock()

	cr, err := r.contextRun(parent, callerID, contextID)
	if err != nil {
		// Session preparation failed: the task fails explicitly. Continuing without a
		// session would be working in a conversation nobody can find again (§7.1).
		return nil, err
	}

	// Tools resolve RELATIVE paths against the node's workspace, not against wherever the
	// hosting process happens to stand: same mechanism the channel uses per thread.
	//
	// The turn's context is DETACHED from the request that started it (design §7.2), and this
	// is not caution: the SDK runs the executor inside an errgroup whose context is cancelled
	// the moment the execution ends — which for a turn that stops to ask a question is exactly
	// when it must keep running. WithoutCancel keeps the request's VALUES (the trace id, the
	// caller) and drops the lifetime; the lifetime is this turn's own, ended by advance
	// reaching an outcome, by a cancel, or by the node shutting down.
	ctx := context.WithoutCancel(parent)
	ctx = wdctx.WithDir(ctx, cr.binding.Workspace)
	// The grant that lets THIS run drive the managed session it owns (agent/managed.go).
	ctx = agent.WithTrustedExecution(ctx)
	ctx, cancel := context.WithCancel(ctx)

	history, err := cr.agent.LoadSessionHistory()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("node: load conversation history: %w", err)
	}

	systemPrompt := agent.BuildSystemPrompt(r.cfg.Language, cr.binding.Workspace, cr.binding.SessionID, r.cfg.ExtraSystemPrompt)
	events := cr.agent.RunConversationStream(ctx, history, prompt, systemPrompt,
		llm.ChatOptions{}, r.toolViewOption()...)

	lt := &liveTurn{
		taskID:    taskID,
		contextID: contextID,
		agent:     cr.agent,
		logger:    r.logger,
		parking:   r.parking,
		ctx:       ctx,
		cancel:    cancel,
		events:    events,
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return nil, errors.New("node: this node is shutting down")
	}
	r.turns[taskID] = lt
	r.mu.Unlock()
	return lt, nil
}

// resumeTurn delivers the caller's answer to the turn waiting for it and reads on.
//
// Every check here repeats one the admission guard already made, and that is the point: the
// guard runs before the SDK starts an execution (so its refusals are protocol errors), while
// this runs inside one (where a refusal can only be a failed task). The alternative — trust
// the guard — would be a wrong answer delivered whenever the two disagree.
func (r *runner) resumeTurn(taskID a2a.TaskID, msg *a2a.Message) (turnOutcome, error) {
	lt, ok := r.turn(taskID)
	if !ok {
		return turnOutcome{}, fmt.Errorf("%w: task %s is waiting for an answer, but no turn in this process is asking",
			errNoWaitingQuestion, taskID)
	}
	ask, ok := r.parking.slot(taskID)
	if !ok {
		return turnOutcome{}, fmt.Errorf("%w: task %s asked %q, and the answer for it already arrived",
			errNoWaitingQuestion, taskID, QuestionID(msg))
	}
	if got := QuestionID(msg); got != ask.questionID {
		return turnOutcome{}, fmt.Errorf("%w: task %s is waiting for %q, not %q",
			errQuestionMismatch, taskID, ask.questionID, got)
	}
	answers, err := answersFor(ask.questions, msg)
	if err != nil {
		return turnOutcome{}, err
	}

	// Consuming the slot BEFORE waking the loop: the answer is single-use, and the loop is
	// about to be handed the only thing that can move it.
	r.parking.take(taskID)
	if !lt.answer(answers) {
		return turnOutcome{}, fmt.Errorf("%w: the turn for task %s is no longer waiting",
			errNoWaitingQuestion, taskID)
	}
	return lt.advance(), nil
}

// turn returns the live turn for a task.
func (r *runner) turn(taskID a2a.TaskID) (*liveTurn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lt, ok := r.turns[taskID]
	return lt, ok
}

// waitingQuestion is the admission guard's view of the parking lot (see questionSource):
// the question a parked task is waiting on, and what was asked.
func (r *runner) waitingQuestion(taskID a2a.TaskID) (*parkedAsk, bool) {
	return r.parking.slot(taskID)
}

// stopTurn ends a running or parked turn. It reports whether there was one: a cancel for a
// task that is not running here is answered honestly rather than silently.
func (r *runner) stopTurn(taskID a2a.TaskID) bool {
	lt, ok := r.turn(taskID)
	if !ok {
		return false
	}
	// The parked question goes with it (it is not going to be answered, and a slot left
	// behind would accept an answer for a loop that is already gone), and so does the turn
	// itself: nothing reads its stream after this except the drain that lets the loop finish.
	lt.stop()
	r.forgetTurn(taskID)
	return true
}

// forgetTurn drops a turn that has ended and any question it was waiting on.
func (r *runner) forgetTurn(taskID a2a.TaskID) {
	r.parking.forget(taskID)
	r.mu.Lock()
	delete(r.turns, taskID)
	r.mu.Unlock()
}

// advance reads the turn's stream until it finishes or stops to ask.
//
// It may be called more than once: a question interrupts the reading, not the turn, and the
// message that answers it resumes reading the same stream.
func (lt *liveTurn) advance() turnOutcome {
	lt.beginRead()
	out := lt.read()
	lt.endRead()
	if out.Stopped {
		// Nobody is reading the rest of the stream, and the loop producing it would block on
		// a send nobody takes — a goroutine that never ends, holding a session and its
		// conversation. The drain is what lets the stopped turn actually finish.
		lt.drainIfIdle()
	}
	return out
}

func (lt *liveTurn) read() turnOutcome {
	for ev := range lt.events {
		switch ev.Type {
		case agent.AgentEventTurnComplete:
			if ev.Result == nil {
				return turnOutcome{Result: &turnResult{Error: errors.New("node: the turn ended without a result")}}
			}
			return turnOutcome{Result: &turnResult{
				Text:       ev.Result.Response,
				ExitReason: ev.Result.ExitReason,
				Iterations: ev.Result.IterationsUsed,
				Error:      ev.Result.Error,
			}}
		case agent.AgentEventError:
			// The loop's own failure exit. The message is what the caller needs to read.
			reason := ev.TextDelta
			if ev.Result != nil && ev.Result.Error != nil {
				return turnOutcome{Result: &turnResult{ExitReason: ev.Result.ExitReason, Error: ev.Result.Error}}
			}
			return turnOutcome{Result: &turnResult{Error: errors.New("node: " + reason)}}
		case agent.AgentEventAskUser:
			if out, published := lt.asking(ev); published {
				return out
			}
			// The question could not be published. The model was told, and the turn goes on
			// rather than returning: there is nothing to wait for.
		}
	}
	if lt.isStopped() {
		return turnOutcome{Stopped: true}
	}
	// The channel closed without a terminal event: the run goroutine died (or the process
	// is going away). Unknown, and reported as such rather than as success.
	return turnOutcome{Result: &turnResult{Error: errors.New("node: the turn ended without reporting an outcome")}}
}

// asking registers a question the loop stopped on and returns it for publication.
//
// The second return says whether the question is now WAITING for an answer. It is false when
// the node will not hold another question, and then the turn keeps going with the model told
// that the question went unanswered (the same shape a non-interactive frontend gives it) —
// a caller that is never asked must not be left waiting on.
func (lt *liveTurn) asking(ev agent.AgentEvent) (turnOutcome, bool) {
	// A question is about to become visible to the caller, and the answer channel must be
	// empty when it does: the channel holds one value with no request id, so a value left
	// over from a question nobody answered would be handed to THIS one as its answer.
	lt.agent.DiscardPendingAskUserAnswer()

	ask := &parkedAsk{
		taskID:     lt.taskID,
		contextID:  lt.contextID,
		questionID: ev.ToolID,
		questions:  ev.Questions,
		wake:       func(answers map[string]string) bool { return lt.agent.TryRespondToAskUser(answers, nil) },
		stop:       lt.stop,
	}
	if err := lt.parking.park(ask); err != nil {
		lt.logger.Warn(lt.ctx, "node: refusing to ask the caller another question",
			"task", string(lt.taskID), "cause", err)
		lt.agent.RespondToAskUser(nil, nil)
		return turnOutcome{}, false
	}
	return turnOutcome{Ask: ask}, true
}

// answer hands the answers to the loop that asked, and reports whether it took them. False
// means the turn is no longer waiting (it was cancelled, expired or ended) — the caller must
// not treat the question as answered.
func (lt *liveTurn) answer(answers map[string]string) bool {
	if lt.isStopped() {
		return false
	}
	return lt.agent.TryRespondToAskUser(answers, nil)
}

// stop ends the turn: the agent loop is cancelled, and whatever it was waiting for is not
// coming.
func (lt *liveTurn) stop() {
	lt.mu.Lock()
	if lt.stopped {
		lt.mu.Unlock()
		return
	}
	lt.stopped = true
	lt.mu.Unlock()

	lt.cancel()
	lt.drainIfIdle()
}

func (lt *liveTurn) isStopped() bool {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.stopped
}

func (lt *liveTurn) beginRead() {
	lt.mu.Lock()
	lt.reading = true
	lt.mu.Unlock()
}

func (lt *liveTurn) endRead() {
	lt.mu.Lock()
	lt.reading = false
	lt.mu.Unlock()
}

// drainIfIdle starts a background reader when nobody else is reading the stream.
func (lt *liveTurn) drainIfIdle() {
	lt.mu.Lock()
	if lt.draining || lt.reading {
		lt.mu.Unlock()
		return
	}
	lt.draining = true
	lt.mu.Unlock()

	go func() {
		for range lt.events {
		}
	}()
}

// contextRun returns the live run for a context, creating the session it is held in on
// first use.
func (r *runner) contextRun(ctx context.Context, callerID, contextID string) (*contextRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cr, ok := r.runs[contextID]; ok {
		return cr, nil
	}

	binding, known := r.contexts.get(contextID)
	if known && binding.CallerID != "" && callerID != "" && binding.CallerID != callerID {
		// A known context belongs to the caller that opened it. Checked only when both
		// sides have an identity: until callers can be authenticated there is nothing to
		// compare, and pretending otherwise would refuse work for no reason.
		return nil, fmt.Errorf("%w: context %s belongs to another caller", ErrContextNotOwned, contextID)
	}

	sm, err := session.NewManager(r.logger)
	if err != nil {
		return nil, fmt.Errorf("node: session manager: %w", err)
	}

	if !known {
		// A new context: open the session it will live in. This is the one place a remote
		// conversation is born, and its working directory is the node's, not the hosting
		// process's.
		//
		// The workspace is created HERE, on first use, rather than at startup: a node that
		// never receives a task should not touch the filesystem, and a workspace that cannot
		// be created is a fact about the TASK that needs it.
		if err := os.MkdirAll(r.workspace, dataDirPerm); err != nil {
			return nil, fmt.Errorf("node: create workspace %s: %w", r.workspace, err)
		}
		sess, err := sm.New(r.cfg.DefaultProviderName(), r.workspace)
		if err != nil {
			return nil, fmt.Errorf("node: create session: %w", err)
		}
		// A remote conversation is not the user's own: the background memory consolidation
		// must leave it alone (design §2.3), and the ordinary frontends must not open it and
		// continue it as a local chat (design §2.1). Both are properties of the session, so
		// both are written on the record.
		sess.SkipDream = true
		sess.Kind = session.KindA2AExecution
		if err := sm.UpdateMeta(sess); err != nil {
			return nil, fmt.Errorf("node: mark remote session: %w", err)
		}
		binding = &contextBinding{
			ContextID: contextID,
			CallerID:  callerID,
			SessionID: sess.ID,
			Workspace: r.workspace,
			CreatedAt: sess.CreatedAt,
		}
		if err := r.contexts.put(binding); err != nil {
			return nil, err
		}
	} else if _, err := sm.Load(binding.SessionID); err != nil {
		// The binding is the promise that this conversation can be continued. A session
		// that is gone must fail the task, never be replaced by a silent new one (§7.1.2).
		return nil, fmt.Errorf("node: load session %s for context %s: %w", binding.SessionID, contextID, err)
	}

	a, _, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Logger: r.logger,
		// External + a handler that refuses: a remote turn has no human attached, and an
		// ask with nobody to answer it must be a refusal rather than a silent approval.
		PermissionMode: agent.PermissionModeExternal,
		// No title generation: a remote task has no window to title, and the extra model
		// call would be charged to this node for nothing.
		TitleGenEnabled: ptr(false),
		FullConfig:      r.cfg,
		SystemConfig:    agent.SystemConfigFromConfig(r.cfg),
		SessionManager:  sm,
	})
	if err != nil {
		return nil, fmt.Errorf("node: build agent: %w", err)
	}
	a.SetPermissionHandler(denyRemoteAsk)

	cr := &contextRun{binding: binding, sm: sm, agent: a}
	r.runs[contextID] = cr
	return cr, nil
}

// toolViewOption turns the node's allowlist into the run's tool view: the operator's tools,
// plus AskUserQuestion.
//
// AskUserQuestion is not an exception to the allowlist: the list says what this node may DO
// to the machine it runs on, and asking the caller a question does nothing to it. Leaving
// asking to the list would tie the one form of interaction a remote task has to a switch
// about capability (§10.2) — and the operator would then have to open a tool to be asked a
// question.
//
// So the empty list is still a policy and still means "nothing that touches this machine",
// and the model is offered exactly one tool: the one that can only talk to the caller.
func (r *runner) toolViewOption() []agent.RunOption {
	names := make([]string, 0, len(r.cfg.Node.Tools)+1)
	names = append(names, r.cfg.Node.Tools...)
	names = append(names, tools.ToolNameAskUser)
	return []agent.RunOption{agent.WithToolSet(names...)}
}

// denyRemoteAsk refuses every permission ask on a remote turn. The refusal is RETURNED as
// a failed tool call (the model sees why), which is what "no approver attached" means
// (§10.1) — not an approval nobody gave, and not a question sent to the caller.
//
// This is also the line between the two kinds of interruption (§7.4): a business question
// goes to the caller as INPUT_REQUIRED, while a TOOL AUTHORIZATION never does. An answer
// from the far side may tell the model what to do with the workspace; it must never be what
// lets a command run, so the approval path stays closed no matter who is asking.
func denyRemoteAsk(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

// ErrContextNotOwned is returned when a caller reaches a context that belongs to another.
var ErrContextNotOwned = errors.New("node: context belongs to another caller")

func ptr[T any](v T) *T { return &v }
