package node

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
	// sessionsDir is the session store this node works in, resolved ONCE when the node is
	// built. Every manager the runner creates is built on it too, rather than on whatever the
	// process-global base dir says at that moment: the two must agree, and the retention
	// handle below is built from this one — a mismatch shows up as "the session I just created
	// cannot be protected", which is exactly how it was found (two homes in one process, the
	// shape an integration test needs).
	sessionsDir string

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
// turnOutcome is how one turn ended, for the executor to publish.
type turnOutcome struct {
	Result  *turnResult
	Ask     *parkedAsk
	Stopped bool

	// Files are what the turn handed over with SendFile, for the executor to publish as an artifact
	// of the task. They travel WITH the outcome rather than being read from the turn, because the
	// outcome is the one thing both executor paths already carry (a new turn and a resumed one).
	Files []filePart
}

// turnFiles is the basket a turn's SendFile calls drop into.
//
// It lives on the TURN, not on the record or the request: a turn that stops to ask a question and
// is later resumed is still ONE turn (§7.1.2), so a file handed over before the question is handed
// over when the work ends.
type turnFiles struct {
	mu    sync.Mutex
	files []filePart
}

// add takes one file, or says why this turn will not carry it. The bound is the same one every
// direction uses (maxFilePartsPerMessage): without it a remote agent decides how many attachments
// one task's artifact grows to.
func (t *turnFiles) add(f filePart) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.files) >= maxFilePartsPerMessage {
		return fmt.Errorf("this task already carries %d files; %q was not taken",
			maxFilePartsPerMessage, f.Name)
	}
	t.files = append(t.files, f)
	return nil
}

// take returns what the turn collected and empties the basket, so one turn's files cannot be
// published twice however many times an outcome is read.
func (t *turnFiles) take() []filePart {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.files
	t.files = nil
	return out
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

	// handed is what this turn's SendFile calls collected (see turnFiles).
	handed *turnFiles
}

func newRunner(cfg *config.Config, workspace string, dataDir string, sessionsDir string, log *logger.Logger) (*runner, error) {
	contexts, err := openContextStore(filepath.Join(dataDir, contextsDirName))
	if err != nil {
		return nil, err
	}
	// The node reads and writes the SAME session store the local frontends use (design
	// §7.1.1): a remote conversation is a session on this machine, not a parallel universe.
	sm, err := newSessionManagerFor(sessionsDir, log)
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
		cfg:         cfg,
		workspace:   workspace,
		logger:      log,
		sessionsDir: sessionsDir,
		contexts:    contexts,
		retention:   retention,
		runs:        make(map[string]*contextRun),
		turns:       make(map[a2a.TaskID]*liveTurn),
	}
	r.parking = newParking(cfg.Node.QuestionTimeout, cfg.Node.MaxParked, r.questionExpired)
	return r, nil
}

// newSessionManagerFor builds a manager on an EXPLICIT session directory.
//
// Not session.NewManager, which resolves the process-global base dir when it is CALLED: a
// per-context manager is created mid-turn, so a process that has moved its base dir since the
// node started would create sessions somewhere the node's retention record is not looking —
// and the context it was creating would fail with "this session is gone" on its very first
// turn.
//
// The cap matches what NewManager would have set: a node's own sessions are one per
// conversation, and the count-based sweep is the local frontends' job (retention keeps it off
// anything still in use).
func newSessionManagerFor(dir string, log *logger.Logger) (*session.Manager, error) {
	store, err := session.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	sm := session.NewManagerWithStore(store, log)
	sm.SetMaxKeep(session.DefaultMaxKeep)
	return sm, nil
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
// state says nothing about another execution still running in the same session, nor about any
// other owner's hold (design §9.4).
//
// It is also called when a turn is stopped rather than finished — a cancel, a deadline, or
// this node shutting down. In every one of those the EXECUTION is over, which is what the hold
// was protecting, and a task the process leaves behind is resolved as interrupted (with the
// session it no longer needs) at the next start.
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

// holdForTurn writes the ONE reason a session must survive a turn: an execution is running in it.
//
// It is taken per turn rather than once per conversation, because that is exactly the lifetime it
// describes — the session is protected while a turn is writing into it, and released when the turn
// ends (node/retention.go's ReasonTask comment says why there is no second reason).
func (r *runner) holdForTurn(cr *contextRun, taskID a2a.TaskID) error {
	binding := *cr.binding
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

	// The running task's hold moves to the session the conversation just moved into. The turn is
	// already writing there, so a failure is reported and the record below still follows it —
	// otherwise the next task would open the session this turn has left.
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

	// The old session is released LAST, and only for the ref that just moved: if it still carries
	// another execution's hold, that one keeps it exactly where it is.
	if old != nil && old.ID != next.ID {
		for _, ref := range []string{string(held.taskID)} {
			if ref == "" {
				continue
			}
			if err := r.retention.Release(old.ID, nodeHoldOwner, ref); err != nil {
				r.logger.Error(context.Background(), "node: could not release the pre-compaction session",
					err, "session", old.ID, "ref", ref)
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
// session" and "the task ended" leaves a reason nobody will ever withdraw — and with no deadline
// on a hold (there is none: a running execution has no predictable end), this owner is the ONLY
// thing that can withdraw it (design §9.4: 恢复后对账). The record converges on what is real: a
// hold whose execution this process does not have is dropped.
func (r *runner) reconcile() {
	holds, err := r.retention.HoldsByOwner(nodeHoldOwner)
	if err != nil {
		// Unreadable protection is not "no protection": the next start tries again.
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
			case session.ReasonContextRetired:
				// Written by a version that kept a conversation's session for a retention window.
				// This node keeps nothing for a conversation, so the record is brought forward
				// rather than reported as an unknown reason: nothing is running, and the session
				// is the session store's again. (The retention layer already ignores it when it
				// answers "may this be deleted", so a reader is not blocked while this waits for
				// the next start.)
				r.releaseReconciled(sessionID, h, "this version keeps no session for a conversation")
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
	parent context.Context, taskID a2a.TaskID, callerID, contextID, prompt string, received []filePart,
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

	// The session this turn runs in is now protected, before a single word is written into it
	// (design §9.4: context/session 绑定成功且保护持久化后才能执行). A sweep in another process
	// knows nothing about this turn, and the hold is the only thing that tells it.
	if err := r.holdForTurn(cr, taskID); err != nil {
		return nil, err
	}

	// What came WITH the message: bytes become a local path this turn can read (or, for an image, a
	// content part the model is handed directly), and the prompt says what arrived and where it is.
	// The session is known by now, which is why this happens here and not in the executor.
	prompt, images := r.receiveFiles(cr, prompt, received)

	// Everything that rides on a remote turn's context — its detached lifetime, the node's
	// workspace, the trusted-execution grant and the usage source — is assembled in one place.
	ctx, cancel := r.turnContext(parent, cr, taskID)

	history, err := cr.agent.LoadSessionHistory()
	if err != nil {
		cancel()
		// The turn never started, so the task's hold goes with it: what is left is the
		// context's own reason to keep the session.
		_ = r.releaseTaskHold(cr, taskID)
		return nil, fmt.Errorf("node: load conversation history: %w", err)
	}

	systemPrompt := agent.BuildSystemPrompt(r.cfg.Language, cr.binding.Workspace, cr.binding.SessionID, r.cfg.ExtraSystemPrompt)
	handed := &turnFiles{}
	ropts := r.toolViewOption()
	if r.offersSendFile() {
		// The receiving side's SendFile: same tool, different meaning — the callback puts the file
		// into this task's artifact instead of into a chat window (see sendFileToCaller).
		tool := tools.NewSendFileTool()
		tool.SetCallback(r.sendFileToCaller(handed))
		ropts = append(ropts, agent.WithExtraTools(tool))
	}
	if len(images) > 0 {
		// The images ride on the turn's own user message and are recorded with it, so a later turn
		// sees them again out of the session's blobs rather than out of a file that might be gone.
		ropts = append(ropts, agent.WithPendingImages(images))
	}
	events := cr.agent.RunConversationStream(ctx, history, prompt, systemPrompt,
		llm.ChatOptions{}, ropts...)

	lt := &liveTurn{
		taskID:    taskID,
		contextID: contextID,
		agent:     cr.agent,
		logger:    r.logger,
		parking:   r.parking,
		ctx:       ctx,
		cancel:    cancel,
		events:    events,
		handed:    handed,
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

// receiveFiles writes the files a message carried into the conversation's own directory and turns
// them into what this turn runs with: the caller's text plus one line per file, and the images as
// content parts the model is handed directly.
//
// An IMAGE does not become a file here. The model is given its bytes as an image, and the session
// record keeps them (agent.RecordParts → the store's blobs), so a later turn still sees the image
// by re-reading the record — a path would be a second, weaker copy of something already durable
// (this is the same reason a pasted screenshot is attached as a part, not read back from disk).
//
// A file that cannot be stored does NOT fail the turn: the caller's text is still work this node can
// do, and the line says what went wrong, which is a fact the model can act on (ask for it again) and
// a reader can see. Refusing the whole request instead would trade a disk problem for the work that
// had nothing to do with it.
func (r *runner) receiveFiles(cr *contextRun, prompt string, files []filePart) (string, []llm.ContentPart) {
	if len(files) == 0 {
		return prompt, nil
	}
	sessionID := cr.binding.SessionID
	var lines []string
	var images []llm.ContentPart
	for _, f := range files {
		size := strutil.HumanBytes(f.Size())
		// An image part is for an image a PROVIDER takes — llm's table, the same one the Read tool
		// checks — while the label only says the sender meant it as one. A .bmp satisfies the second
		// and not the first, so it takes the file path below instead of failing the whole request as
		// a part no provider accepts.
		if strings.HasPrefix(f.MediaType, "image/") && llm.ImageMediaTypeForBytes(f.Bytes) != "" {
			images = append(images, llm.ContentPart{
				Type:      llm.ContentPartImage,
				MediaType: f.MediaType,
				Data:      base64.StdEncoding.EncodeToString(f.Bytes),
			})
			lines = append(lines, fmt.Sprintf("- %s (%s, %s) — attached to this message as an image", f.Name, f.MediaType, size))
			continue
		}
		path, err := session.SaveReceivedFile(sessionID, f.Name, f.Bytes)
		if err != nil {
			r.logger.Error(context.Background(), "node: could not store a file the caller sent",
				err, "session", sessionID, "file", f.Name)
			lines = append(lines, fmt.Sprintf("- %s (%s, %s) — NOT saved: %v", f.Name, f.MediaType, size, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%s, %s) — saved on this machine at: %s", f.Name, f.MediaType, size, path))
	}
	if len(lines) == 0 {
		return prompt, images
	}
	block := "\n\nFiles the caller sent with this message:\n" + strings.Join(lines, "\n")
	return prompt + block, images
}

// offersSendFile reports whether SendFile is available to a remote turn.
//
// It is one lookup in the default set rather than a hardcoded true so the set stays the single
// answer to "what may a remote turn do": this call site exists because sending a file READS it from
// this machine — any path the model can name, not only the workspace — and puts its bytes on the
// wire, which is exactly the kind of capability the list is for.
func (r *runner) offersSendFile() bool {
	return slices.Contains(remoteTaskTools, tools.ToolNameSendFile)
}

// sendFileToCaller is the receiving side's SendFile: the remote agent hands a file back to whoever
// asked for the work.
//
// Two things make it different from the channel's callback, and both are about who owns the file:
//
//   - It reads the bytes HERE and now. The channel queues a path for a send that happens later, and
//     that path is a file this machine keeps; this one is a file in a workspace somebody else can
//     change, and the artifact is written when the turn ends. A path kept until then would be a
//     promise about a file that may no longer say what it said.
//   - It REFUSES rather than warns: the size, the media type and the count are the same policy the
//     other two directions use (asFilePart), and the tool reports the failure to the model. "Handed
//     over" must never be a sentence about a file that did not leave.
func (r *runner) sendFileToCaller(files *turnFiles) tools.SendFileCallback {
	return func(name, mediaType, localPath string) error {
		info, err := os.Stat(localPath)
		if err != nil {
			return err
		}
		if info.Size() > maxFilePartBytes {
			return fmt.Errorf("a file of %s cannot be sent: the limit is %s",
				strutil.HumanBytes(info.Size()), strutil.HumanBytes(maxFilePartBytes))
		}
		bytes, err := os.ReadFile(localPath)
		if err != nil {
			return err
		}
		part := a2a.NewRawPart(bytes)
		part.Filename, part.MediaType = name, mediaType
		file, refusal := asFilePart(part)
		if refusal != nil {
			return refusal
		}
		r.logger.Info(context.Background(), "node: the remote agent handed a file back",
			"name", file.Name, "media_type", file.MediaType, "bytes", file.Size())
		return files.add(file)
	}
}

// turnContext returns the context ONE remote turn runs in, and the cancel that ends it.
//
// It is a function rather than four lines inside beginTurn because four separate things ride on
// this context and each one was a decision:
//
//   - WithoutCancel detaches the turn from the request that started it (design §7.2), and that
//     is not caution: the SDK runs the executor inside an errgroup whose context is cancelled the
//     moment the execution ends — which, for a turn that stopped to ask a question, is exactly
//     when it must keep running. The request's VALUES (trace id, caller) survive; its lifetime
//     does not. The lifetime is this turn's own, ended by advance reaching an outcome, by a
//     cancel, or by the node shutting down.
//   - the node's workspace, so tools resolve relative paths against it and never against wherever
//     the hosting process happens to stand (§10.3).
//   - the trusted-execution grant that lets THIS run drive the managed session it owns
//     (agent/managed.go). Nothing else in this process sets it.
//   - the usage source, so every model call the turn makes is attributable to the remote task
//     that caused it (§9.5).
//
// Its own lifetime (the returned cancel) is deliberately not derived from the SDK's: that is the
// whole point of the first item.
func (r *runner) turnContext(parent context.Context, cr *contextRun, taskID a2a.TaskID) (context.Context, context.CancelFunc) {
	ctx := context.WithoutCancel(parent)
	ctx = wdctx.WithDir(ctx, cr.binding.Workspace)
	ctx = agent.WithTrustedExecution(ctx)
	// The KIND stays whatever the call was for — a compaction or a title call inside this turn is
	// still a compact or a title call — because the source is a second dimension. Folding them
	// into one "a2a" kind would lose what the work actually cost on this machine (§9.5).
	ctx = llm.WithUsageSource(ctx, llm.UsageSource{
		Origin:    llm.UsageSourceA2A,
		TaskID:    string(taskID),
		ContextID: cr.binding.ContextID,
	})
	return context.WithCancel(ctx)
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

// ownerEndedReason is what a task the machine's owner stopped says about itself. It is the text
// the CALLER reads (taskText hands a terminal task's status message over), so it says who ended
// it and what that leaves unknown — the same shape the startup recovery uses for a death.
const ownerEndedReason = "the machine's owner stopped this execution; side effects before that point are unknown"

// EndSessionWork stops whatever this session is executing, on the MACHINE OWNER's orders, and
// reports how many executions it ended.
//
// It is deliberately NOT the protocol's cancel. That one arrives from the CALLER, belongs to it,
// and means "I no longer want this"; this one is the owner of the machine saying "this has gone
// on long enough" — the escape hatch for an execution that will never end by itself (a peer that
// died, a state a bug left running), and the reason a session it holds can be deleted afterwards.
// Nothing about it is available to a caller: it is reached through the loopback control interface.
//
// The state is written HERE because the stopper owns it (nodeExecutor.publish: a stopped turn
// keeps nothing to say about itself), and stopTurn has already withdrawn the task's hold — the
// turn's ending is what releases it, exactly as it does for a caller's cancel.
func (r *runner) EndSessionWork(ctx context.Context, sessionID string) (int, error) {
	binding, ok := r.contexts.bySession(sessionID)
	if !ok {
		// Nothing is running here, and saying so is the answer: an entry point asks this about a
		// session it is about to delete, and "no" is what lets the delete go ahead.
		return 0, nil
	}

	r.mu.Lock()
	taskIDs := make([]a2a.TaskID, 0, len(r.turns))
	for id, lt := range r.turns {
		if lt.contextID == binding.ContextID {
			taskIDs = append(taskIDs, id)
		}
	}
	r.mu.Unlock()

	ended := 0
	for _, id := range taskIDs {
		if !r.stopTurn(id) {
			continue // already gone between the snapshot and here
		}
		if err := r.tasks.endByOwner(id, ownerEndedReason); err != nil {
			r.logger.Error(ctx, "node: could not record the end of a task the owner stopped",
				err, "task", string(id))
			continue
		}
		ended++
	}
	return ended, nil
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
				return turnOutcome{
					Result: &turnResult{Error: errors.New("node: the turn ended without a result")},
					Files:  lt.handed.take(),
				}
			}
			return turnOutcome{
				Result: &turnResult{
					Text:       ev.Result.Response,
					ExitReason: ev.Result.ExitReason,
					Iterations: ev.Result.IterationsUsed,
					Error:      ev.Result.Error,
				},
				Files: lt.handed.take(),
			}
		case agent.AgentEventError:
			// The loop's own failure exit. The message is what the caller needs to read. Files
			// already handed over go with it: what failed is the work, and an attachment it did
			// produce is part of the result a failure is supposed to leave behind (§7.3).
			reason := ev.TextDelta
			if ev.Result != nil && ev.Result.Error != nil {
				return turnOutcome{
					Result: &turnResult{ExitReason: ev.Result.ExitReason, Error: ev.Result.Error},
					Files:  lt.handed.take(),
				}
			}
			return turnOutcome{
				Result: &turnResult{Error: errors.New("node: " + reason)},
				Files:  lt.handed.take(),
			}
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
		// sides have an identity — on loopback nobody does, so there is nothing to compare,
		// and pretending otherwise would refuse work for no reason.
		return nil, fmt.Errorf("%w: context %s belongs to another caller", ErrContextNotOwned, contextID)
	}

	sm, err := newSessionManagerFor(r.sessionsDir, r.logger)
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
		// WHO asked, in the identity the handshake established (empty on a surface that
		// identifies nobody — loopback). Written with the rest of the session's nature and
		// before the turn runs: a record in a list that says somebody's work is in here has
		// to be able to say whose (design §2.2).
		sess.SourceNodeID = callerID
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
		// Memory is NOT suppressed: the recall reminder fires and both memory tools are in the
		// run's tool view. That is a deliberate reversal of the earlier isolation rule — the
		// operator's machines are meant to work as one agent over one long-term store, and a
		// caller who can already read the workspace through Bash gains nothing from being kept
		// away from the notes beside it. What it does mean is that a remote conversation can
		// read AND write facts the operator's own conversations later recall, so the boundary
		// for memory is the same one as for everything else: who can reach this node (§10.2).
		FullConfig:     r.cfg,
		SystemConfig:   agent.SystemConfigFromConfig(r.cfg),
		SessionManager: sm,
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

// remoteTaskTools is what a remote turn is offered, by default.
//
// The list is the operator's machine being handed to a caller, so it is deliberately short: the
// tools that read and write FILES, run a command, look something up, use a skill, hand a file back,
// reach the operator, and ask the caller a question. What is NOT here is a tool about THIS
// PROCESS's own state — scheduler jobs, plans, findings, sub-agents, delegation to further nodes:
// none of those are about the work the caller asked for, and a remote turn that starts scheduling
// or filing work is doing something nobody asked the machine to do.
//
// NotifyUser is the one entry that reaches someone OTHER than the caller, and it belongs here for
// the same reason SendFile does: a task that ran on the operator's machine may have something to
// say to the operator (finished, needs a decision), and the caller is not a substitute for them.
// It is the HOST's config that decides whether the tool exists at all (an enabled channel with
// `notify: true`; see pkg/channel), so naming it here only stops the policy from being what keeps
// a host that IS set up for it from saying anything.
//
// It replaced a configured allowlist (config `node.tools`). The allowlist was the wrong shape for
// the same reason a security switch with a surprising empty value is: what a node can do is a
// property of WHAT THIS SOFTWARE IS, and every deployment that wanted the obvious set had to write
// it down and keep it in step with the tools as they were added. Memory is included on purpose
// (MemoryRecall/RecordMemory, and the recall reminder): a node's memory is the operator's own
// long-term store, shared with their local conversations, and they asked for their machines to work
// as one agent rather than as a stranger with a file system.
var remoteTaskTools = []string{
	tools.ToolNameRead,
	tools.ToolNameEdit,
	tools.ToolNameWrite,
	tools.ToolNameGrep,
	tools.ToolNameGlob,
	tools.ToolNameBash,
	tools.ToolNameWebFetch,
	tools.ToolNameWebSearch,
	tools.ToolNameSkill,
	tools.ToolNameAskUser,
	tools.ToolNameSendFile,
	tools.ToolNameNotifyUser,
	tools.ToolNameMemoryRecall,
	tools.ToolNameRecordMemory,
}

// toolViewOption turns the node's tool policy into the run's tool view.
//
// AskUserQuestion is in that list and is not an exception to anything: the list says what this node
// may DO to the machine it runs on, and asking the caller a question does nothing to it. Leaving
// asking out would tie the one form of interaction a remote task has to a capability decision
// (§10.2) — and the operator would then have to open a tool to be asked a question.
func (r *runner) toolViewOption() []agent.RunOption {
	return []agent.RunOption{agent.WithToolSet(remoteTaskTools...)}
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
