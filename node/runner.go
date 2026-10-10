package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/monsterxx03/tachi/agent"
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
// (node/executor.go) — this file knows nothing about A2A.
//
// Three decisions are encoded here and each is load-bearing:
//
//   - The workspace comes from the NODE's config, never from the hosting process's cwd
//     (§10.3). A node started from a shell must not do remote work in that shell's
//     directory: the caller cannot see it, and nobody asked for it.
//   - A turn runs with the node's tool ALLOWLIST as a per-run tool view (§10.2), enforced
//     both by hiding the tools from the model and by refusing them at execution. An empty
//     list means no tools at all.
//   - Permission asks are DENIED, not forwarded (§10.1): nobody is attached to a remote
//     task, so an `ask` rule has no one to ask. A deny stays a deny either way.
type runner struct {
	cfg       *config.Config
	workspace string
	logger    *logger.Logger

	contexts *contextStore

	mu       sync.Mutex
	runs     map[string]*contextRun
	turnStop map[string]context.CancelFunc
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
	return &runner{
		cfg:       cfg,
		workspace: workspace,
		logger:    log,
		contexts:  contexts,
		runs:      make(map[string]*contextRun),
		turnStop:  make(map[string]context.CancelFunc),
	}, nil
}

// runTurn runs one turn of the conversation identified by contextID, with prompt as the
// caller's message, and returns its outcome. taskID is used only to register a stop
// function, so a cancel request can end this turn.
func (r *runner) runTurn(ctx context.Context, taskID, callerID, contextID, prompt string) (turnResult, error) {
	cr, err := r.contextRun(ctx, callerID, contextID)
	if err != nil {
		// Session preparation failed: the task fails explicitly. Continuing without a
		// session would be working in a conversation nobody can find again (§7.1).
		return turnResult{}, err
	}

	// Tools resolve RELATIVE paths against the node's workspace, not against wherever the
	// hosting process happens to stand: same mechanism the channel uses per thread.
	ctx = wdctx.WithDir(ctx, cr.binding.Workspace)
	// The grant that lets THIS run drive the managed session it owns (agent/managed.go).
	ctx = agent.WithTrustedExecution(ctx)
	ctx, stop := context.WithCancel(ctx)
	r.registerStop(taskID, stop)
	defer r.forgetStop(taskID)
	defer stop()

	history, err := cr.agent.LoadSessionHistory()
	if err != nil {
		return turnResult{}, fmt.Errorf("node: load conversation history: %w", err)
	}

	systemPrompt := agent.BuildSystemPrompt(r.cfg.Language, cr.binding.Workspace, cr.binding.SessionID, r.cfg.ExtraSystemPrompt)
	events := cr.agent.RunConversationStream(ctx, history, prompt, systemPrompt,
		llm.ChatOptions{}, r.toolViewOption()...)

	for ev := range events {
		switch ev.Type {
		case agent.AgentEventTurnComplete:
			if ev.Result == nil {
				return turnResult{Error: errors.New("node: the turn ended without a result")}, nil
			}
			return turnResult{
				Text:       ev.Result.Response,
				ExitReason: ev.Result.ExitReason,
				Iterations: ev.Result.IterationsUsed,
				Error:      ev.Result.Error,
			}, nil
		case agent.AgentEventError:
			// The loop's own failure exit. The message is what the caller needs to read.
			reason := ev.TextDelta
			if ev.Result != nil && ev.Result.Error != nil {
				return turnResult{ExitReason: ev.Result.ExitReason, Error: ev.Result.Error}, nil
			}
			return turnResult{Error: errors.New("node: " + reason)}, nil
		}
	}
	// The channel closed without a terminal event: the run goroutine died (or the process
	// is going away). Unknown, and reported as such rather than as success.
	return turnResult{}, errors.New("node: the turn ended without reporting an outcome")
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

// toolViewOption turns the node's allowlist into the run's tool view.
//
// Empty is spelled out rather than left to WithToolSet(): "no tools" and "a list that
// happens to be empty" must not be the same code path by accident — one of them is a
// policy and the other is a typo.
func (r *runner) toolViewOption() []agent.RunOption {
	if len(r.cfg.Node.Tools) == 0 {
		return []agent.RunOption{agent.WithNoTools()}
	}
	return []agent.RunOption{agent.WithToolSet(r.cfg.Node.Tools...)}
}

// denyRemoteAsk refuses every permission ask on a remote turn. The refusal is RETURNED as
// a failed tool call (the model sees why), which is what "no approver attached" means
// (§10.1) — not an approval nobody gave, and not a question sent to the caller.
func denyRemoteAsk(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func (r *runner) registerStop(taskID string, stop context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnStop[taskID] = stop
}

func (r *runner) forgetStop(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.turnStop, taskID)
}

// stopTurn ends a running turn. It reports whether there was one: a cancel for a task
// that is not running here is answered honestly rather than silently.
func (r *runner) stopTurn(taskID string) bool {
	r.mu.Lock()
	stop, ok := r.turnStop[taskID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	stop()
	return true
}

// ErrContextNotOwned is returned when a caller reaches a context that belongs to another.
var ErrContextNotOwned = errors.New("node: context belongs to another caller")

func ptr[T any](v T) *T { return &v }
