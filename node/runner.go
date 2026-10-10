package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	// retention is where this node's reasons to keep a session live (design §9.4). It is the
	// SAME record the sweep reads, in every process that shares this data dir, which is the
	// whole point: a node's session must survive a cleanup run by a desktop that has never
	// heard of this context.
	retention *session.Retention

	// tasks is the node's task store, handed over by the host once it exists (see
	// consumeTaskStore). It is needed outside any execution: a question whose deadline
	// passes has to end its task, and the store is the only thing that writes a task's
	// state.
	tasks *taskStore

	mu     sync.Mutex
	runs   map[string]*contextRun
	turns  map[a2a.TaskID]*liveTurn
	closed bool
	// unwound counts turns whose event stream has been read to its end, which is where the
	// agent loop finishes its own work (session writes, checkpoints). Waiting for it is what
	// makes "this node has stopped" mean the work is over rather than merely told to stop.
	unwound sync.WaitGroup
}

// contextRun is the live half of a context binding: the session manager positioned on the
// context's current session, and the agent that owns it. The agent may be evicted and
// rebuilt from the session history (design §7.1.1); nothing in a request depends on it
// having stayed in memory.
type contextRun struct {
	binding *contextBinding
	sm      *session.Manager
	agent   *agent.AIAgent

	// holds is what this run has written into the retention record, so it can be moved and
	// released without guessing which session and which task they name. Guarded by the
	// runner's lock.
	holds holdState
}

// holdState is the retention record as this process wrote it.
type holdState struct {
	// sessionID is the session the holds are on. It follows the conversation across a
	// compaction, which is why it is a field here rather than "the binding's session" read
	// wherever it is needed: the two agree at rest, and only this one is written under the
	// same lock as the release.
	sessionID string
	// taskID is the task whose execution this run is protecting — empty between tasks.
	taskID a2a.TaskID
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

	// done is called once, when this turn's stream has been read to its end (see
	// runner.unwound). It is the turn's side of "the loop has finished".
	done func()

	mu       sync.Mutex
	stopped  bool
	reading  bool
	draining bool
}

func newRunner(cfg *config.Config, workspace string, dataDir string, sessionsDir string, log *logger.Logger) (*runner, error) {
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
	// Required, not optional: without it the sessions a remote task works in would be ordinary
	// sessions to every sweep in every process, which is the gap this exists to close.
	retention, err := session.NewRetention(sessionsDir, log)
	if err != nil {
		return nil, fmt.Errorf("node: retention: %w", err)
	}
	r := &runner{
		cfg:       cfg,
		workspace: workspace,
		logger:    log,
		contexts:  contexts,
		retention: retention,
		runs:      make(map[string]*contextRun),
		turns:     make(map[a2a.TaskID]*liveTurn),
	}
	r.parking = newParking(cfg.Node.QuestionTimeout, cfg.Node.MaxParked, r.questionExpired)
	return r, nil
}

// consumeTaskStore is the host handing over the node's task store (see nodeState). It is
// called once, before the surface accepts work, so the read below needs no lock.
func (r *runner) consumeTaskStore(store *taskStore) { r.tasks = store }

// shutdownGrace bounds how long shutdown waits for the turns it just stopped to finish their
// own unwinding. It is the same rule the listeners get (see shutdownGraceful): a bounded wait,
// then termination, because a turn that will not stop must not hold the process open.
const shutdownGrace = 3 * time.Second

// nodeHoldOwner is this node's name in the retention record. Ownership is recorded so a
// release can only ever drop this node's holds, whichever other subsystems share the data dir.
const nodeHoldOwner = "node"

// contextHold is the context's reason to keep a session: a remote conversation can be
// continued while it is within its retention window, and that window is measured from the last
// time the context was USED — a conversation in daily use stays, an abandoned one ages out.
func (r *runner) contextHold(binding contextBinding) session.Hold {
	h := session.Hold{
		Owner:     nodeHoldOwner,
		Reason:    session.ReasonContext,
		Ref:       binding.ContextID,
		SessionID: binding.SessionID,
		Since:     binding.CreatedAt,
	}
	if retention := r.cfg.Node.ContextRetention; retention > 0 {
		h.ExpiresAt = binding.lastUsed().Add(retention)
	}
	return h
}

// taskHold is a running task's reason to keep the session it is working in. It has no
// deadline: the task's own lifecycle ends it (a task that dies with the process is resolved
// as interrupted at the next start, and reconciliation drops this hold then).
func (r *runner) taskHold(sessionID string, taskID a2a.TaskID) session.Hold {
	return session.Hold{
		Owner:     nodeHoldOwner,
		Reason:    session.ReasonTask,
		Ref:       string(taskID),
		SessionID: sessionID,
		Since:     time.Now().UTC(),
	}
}

// releaseTaskHold drops the task's own hold, and nothing else: a task reaching a terminal
// state says nothing about whether its conversation can still be continued, so the context's
// hold stays until its own window ends (design §9.4).
//
// It is also called when a turn is stopped rather than finished — a cancel, a deadline, or
// this node shutting down. In every one of those the EXECUTION is over, which is what the hold
// was protecting; the session stays covered by the context's hold, and a task the process
// leaves behind is resolved as interrupted (with the session it no longer needs) at the next
// start.
func (r *runner) releaseTaskHold(cr *contextRun, taskID a2a.TaskID) error {
	if cr == nil || taskID == "" {
		return nil
	}
	r.mu.Lock()
	if cr.holds.taskID != taskID {
		r.mu.Unlock()
		return nil
	}
	sessionID := cr.holds.sessionID
	cr.holds.taskID = ""
	r.mu.Unlock()

	if sessionID == "" {
		return nil
	}
	return r.retention.Release(sessionID, nodeHoldOwner, string(taskID))
}

// holdForTurn writes the two reasons a session must survive a task: the context's (refreshed,
// so its window starts at this use) and the task's.
//
// The context's comes first and the task's second, because the task's hold is the one that
// must not exist without the other: a task hold whose session nobody protects as a
// conversation would be released into a sweep the moment the task ends.
func (r *runner) holdForTurn(cr *contextRun, taskID a2a.TaskID) error {
	binding := *cr.binding
	if err := r.retention.Acquire(r.contextHold(binding)); err != nil {
		return fmt.Errorf("node: protect the conversation's session: %w", err)
	}
	if err := r.retention.Acquire(r.taskHold(binding.SessionID, taskID)); err != nil {
		return fmt.Errorf("node: protect the task's session: %w", err)
	}

	r.mu.Lock()
	cr.holds = holdState{sessionID: binding.SessionID, taskID: taskID}
	r.mu.Unlock()
	return nil
}

// onSessionSwitch moves this context's protection and binding to the session its conversation
// now lives in.
//
// It runs in the MIDDLE of a turn, because that is when a compaction switches sessions
// (agent/compact.go's FinalizeCompact). That timing is the whole reason this is a
// notification rather than something noticed later: the child session is already being
// written to while nothing protects it, and a sweep in another process has no way to know it
// is the middle of somebody's turn (design §9.4).
//
// The order is fixed and each step matters: protect the new session, persist the binding that
// names it, then release what the old session no longer needs.
func (r *runner) onSessionSwitch(cr *contextRun, old, next *session.Session) {
	if cr == nil || next == nil {
		return
	}

	r.mu.Lock()
	binding := *cr.binding
	held := cr.holds
	r.mu.Unlock()

	binding.SessionID = next.ID
	now := time.Now().UTC()
	binding.LastUsedAt = now

	if err := r.retention.Acquire(r.contextHold(binding)); err != nil {
		// The turn is already writing into the new session, so the honest thing is to report
		// and carry on: the record below still has to follow the conversation, or the next
		// task would open the session this turn has left.
		r.logger.Error(context.Background(), "node: could not protect the compacted session",
			err, "context", binding.ContextID, "session", next.ID)
	}
	if held.taskID != "" {
		if err := r.retention.Acquire(r.taskHold(next.ID, held.taskID)); err != nil {
			r.logger.Error(context.Background(), "node: could not protect the running task's new session",
				err, "task", string(held.taskID), "session", next.ID)
		}
	}
	if err := r.contexts.put(&binding); err != nil {
		r.logger.Error(context.Background(), "node: could not persist the compacted binding",
			err, "context", binding.ContextID, "session", next.ID)
	}

	r.mu.Lock()
	*cr.binding = binding
	cr.holds = holdState{sessionID: next.ID, taskID: held.taskID}
	r.mu.Unlock()

	// The old session is released LAST, and only for the refs that just moved: if it still
	// carries another task's or context's hold, that one keeps it exactly where it is.
	if old != nil && old.ID != next.ID {
		for _, ref := range []struct{ reason, ref string }{
			{session.ReasonContext, binding.ContextID},
			{session.ReasonTask, string(held.taskID)},
		} {
			if ref.ref == "" {
				continue
			}
			if err := r.retention.Release(old.ID, nodeHoldOwner, ref.ref); err != nil {
				r.logger.Error(context.Background(), "node: could not release the pre-compaction session",
					err, "session", old.ID, "ref", ref.ref)
			}
		}
	}

	r.logger.Info(context.Background(), "node: the conversation moved to a new session",
		"context", binding.ContextID, "from", sessionIDOf(old), "to", next.ID)
}

func sessionIDOf(s *session.Session) string {
	if s == nil {
		return ""
	}
	return s.ID
}

// reconcile repairs the retention record against what this node actually holds.
//
// It exists because a hold outlives the process that wrote it: a crash between "protect the
// session" and "the task ended" leaves a reason nobody will ever withdraw, and only the owner
// of a hold's lifetime can say which refs are stale (design §9.4: 恢复后对账). Both directions
// of the mistake are covered — a stale hold is dropped, and a hold for a session that is gone
// is dropped too, so the record converges on what is real.
func (r *runner) reconcile() {
	if dropped, err := r.retention.Sweep(); err != nil {
		r.logger.Error(context.Background(), "node: could not expire retention holds", err)
	} else if dropped > 0 {
		r.logger.Info(context.Background(), "node: released expired retention holds", "count", dropped)
	}

	holds, err := r.retention.HoldsByOwner(nodeHoldOwner)
	if err != nil {
		// Unreadable protection is not "no protection": the sweep already skipped anything it
		// could not read, and the next start tries again.
		r.logger.Error(context.Background(), "node: could not reconcile retention holds", err)
		return
	}

	for sessionID, list := range holds {
		for _, h := range list {
			switch h.Reason {
			case session.ReasonTask:
				if !r.taskIsRunning(a2a.TaskID(h.Ref)) {
					// An execution this process does not have: the task is finished, or it was
					// resolved as interrupted before this start.
					r.releaseReconciled(sessionID, h, "the task it named is not running any more")
				}
			case session.ReasonContext:
				binding, known := r.contexts.get(h.Ref)
				if !known || binding.SessionID != sessionID {
					// The conversation moved on or is gone: this hold names a session it does
					// not hold any more.
					r.releaseReconciled(sessionID, h, "the conversation is no longer in this session")
					continue
				}
				if binding.expired(time.Now().UTC(), r.cfg.Node.ContextRetention) {
					r.releaseReconciled(sessionID, h, "the conversation's retention window has passed")
				}
			default:
				r.logger.Warn(context.Background(), "node: keeping a retention hold this version does not know",
					"reason", h.Reason, "session", sessionID)
			}
		}
	}
}

// taskIsRunning reports whether this node still has a live execution for a task. No task
// store at all means no task can be running: reconciliation runs after the store is handed
// over, so this is the "the node has no tasks" case rather than a missing wiring.
func (r *runner) taskIsRunning(taskID a2a.TaskID) bool {
	if r.tasks == nil {
		return false
	}
	rec, known := r.tasks.record(taskID)
	if !known {
		return false
	}
	return !rec.Task.Status.State.Terminal()
}

func (r *runner) releaseReconciled(sessionID string, h session.Hold, why string) {
	if err := r.retention.Release(sessionID, h.Owner, h.Ref); err != nil {
		r.logger.Error(context.Background(), "node: could not release a retention hold", err,
			"session", sessionID, "ref", h.Ref)
		return
	}
	r.logger.Info(context.Background(), "node: released a retention hold", "session", sessionID,
		"reason", h.Reason, "ref", h.Ref, "why", why)
}

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
	if len(ids) == 0 {
		return
	}

	// A bounded wait, then carry on: the design allows terminating what will not stop
	// (§3.4). Without the wait, a process can exit while a turn is mid-write — the file
	// writes are atomic, so nothing is torn, but a caller reading the task record right
	// afterwards sees a state the node was about to update.
	unwound := make(chan struct{})
	go func() {
		r.unwound.Wait()
		close(unwound)
	}()
	select {
	case <-unwound:
	case <-time.After(shutdownGrace):
		r.logger.Warn(context.Background(), "node: turns did not finish before shutdown",
			"turns", len(ids), "waited", shutdownGrace.String())
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

	// Every turn re-checks the conversation's window and refreshes it. It cannot live in
	// contextRun: that function returns the run this process already has cached, so a
	// conversation that aged out while this node was running would keep working — the check
	// would only ever fire after a restart.
	if err := r.beginContextUse(cr); err != nil {
		return nil, err
	}

	// The session this turn runs in is now protected, before a single word is written into it
	// (design §9.4: context/session 绑定成功且保护持久化后才能执行). A sweep in another process
	// knows nothing about this turn, and the hold is the only thing that tells it.
	if err := r.holdForTurn(cr, taskID); err != nil {
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
		// The turn never started, so the task's hold goes with it: what is left is the
		// context's own reason to keep the session.
		_ = r.releaseTaskHold(cr, taskID)
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
		done:      r.unwound.Done,
	}
	r.unwound.Add(1)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		_ = r.releaseTaskHold(cr, taskID)
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
//
// It also withdraws the TASK's reason to keep the session — the execution is over, whatever
// ended it — and leaves the conversation's own hold exactly where it is (design §9.4).
func (r *runner) forgetTurn(taskID a2a.TaskID) {
	r.parking.forget(taskID)

	r.mu.Lock()
	contextID := ""
	if lt, ok := r.turns[taskID]; ok {
		contextID = lt.contextID
	}
	delete(r.turns, taskID)
	cr := r.runs[contextID]
	r.mu.Unlock()

	if cr != nil {
		if err := r.releaseTaskHold(cr, taskID); err != nil {
			r.logger.Error(context.Background(), "node: could not release the task's session hold",
				err, "task", string(taskID))
		}
	}
}

// advance reads the turn's stream until it finishes or stops to ask.
//
// It may be called more than once: a question interrupts the reading, not the turn, and the
// message that answers it resumes reading the same stream.
func (lt *liveTurn) advance() turnOutcome {
	lt.beginRead()
	out := lt.read()
	lt.endRead()
	if out.Ask != nil {
		// The turn is alive and waiting for an answer: nothing is over.
		return out
	}
	// Finished or stopped, the stream still has to be read to its END: that is where the agent
	// loop runs the rest of its own work (the session writes and the checkpoint bookkeeping
	// that follow the last event), and a producer with nobody reading would block on a send
	// forever. The reader that gets there signals that this turn is really over.
	lt.drainIfIdle()
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
		if lt.done != nil {
			lt.done()
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
			ContextID:  contextID,
			CallerID:   callerID,
			SessionID:  sess.ID,
			Workspace:  r.workspace,
			CreatedAt:  sess.CreatedAt,
			LastUsedAt: time.Now().UTC(),
		}
		// The session exists before it is protected — that order cannot be avoided, so the
		// hold verifies the session is still there and the task fails if a sweep won the race
		// (design §9.4: 既有 session 在获保护前须确认仍存在).
		if err := r.retention.Acquire(r.contextHold(*binding)); err != nil {
			return nil, fmt.Errorf("node: protect the new conversation's session: %w", err)
		}
		if err := r.contexts.put(binding); err != nil {
			return nil, err
		}
	} else {
		if _, err := sm.Load(binding.SessionID); err != nil {
			// The binding is the promise that this conversation can be continued. A session
			// that is gone must fail the task, never be replaced by a silent new one (§7.1.2).
			return nil, fmt.Errorf("node: load session %s for context %s: %w", binding.SessionID, contextID, err)
		}
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

	cr := &contextRun{
		binding: binding,
		sm:      sm,
		agent:   a,
		holds:   holdState{sessionID: binding.SessionID},
	}
	// The conversation can move to a successor session while a turn is running (compaction),
	// and the node's two pieces of state keyed by session id — the binding and the retention
	// holds — have to move with it. The notification is installed HERE because this manager
	// serves exactly this conversation (design §9.4).
	sm.SetSessionSwitchHook(func(old, next *session.Session) { r.onSessionSwitch(cr, old, next) })
	r.runs[contextID] = cr
	return cr, nil
}

// beginContextUse is the per-turn half of a conversation's life: the retention window is
// checked (and the turn REFUSED when it has passed), the use refreshes it, and the hold
// follows the refresh.
//
// Refusing rather than starting fresh is the design's rule (§9.4): a context that aged out must
// fail loudly, because a caller that asked to continue a conversation and silently got a new
// one has been told something untrue about its own history.
func (r *runner) beginContextUse(cr *contextRun) error {
	now := time.Now().UTC()

	r.mu.Lock()
	binding := *cr.binding
	r.mu.Unlock()

	retention := r.cfg.Node.ContextRetention
	if binding.expired(now, retention) {
		// The window has passed, and the hold that kept the session is no longer a reason to:
		// releasing it here (rather than waiting for a sweep) is what lets a cleanup reclaim
		// the session as soon as the node notices.
		if err := r.retention.Release(binding.SessionID, nodeHoldOwner, binding.ContextID); err != nil {
			r.logger.Error(context.Background(), "node: could not release an expired conversation's hold",
				err, "context", binding.ContextID, "session", binding.SessionID)
		}
		return fmt.Errorf("%w: context %s", ErrContextExpired, binding.ContextID)
	}
	if retention <= 0 {
		// No window configured: there is nothing to refresh, and the hold has no deadline
		// either (see contextHold).
		return nil
	}

	binding.LastUsedAt = now
	// The hold is re-written rather than extended in place: the same (owner, ref) replaces the
	// record, which is what keeps ONE entry per conversation however often it is used.
	if err := r.retention.Acquire(r.contextHold(binding)); err != nil {
		return fmt.Errorf("node: refresh the conversation's protection: %w", err)
	}
	if err := r.contexts.put(&binding); err != nil {
		return err
	}

	r.mu.Lock()
	*cr.binding = binding
	r.mu.Unlock()
	return nil
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

// ErrContextExpired is returned when a conversation's retention window has passed. It is
// explicit on purpose: a context that aged out must not be answered with a NEW conversation
// that looks like the old one (design §9.4: 不静默降级成新会话).
var ErrContextExpired = errors.New("node: this conversation's retention window has passed")

func ptr[T any](v T) *T { return &v }
