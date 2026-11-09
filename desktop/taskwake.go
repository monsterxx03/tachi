package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

const (
	// wakeSettle is how long a queue is allowed to collect before its wake turn
	// starts. A batch of tasks that finish together should cost ONE turn, not one
	// per task; a task that ends a moment later simply wakes the next one.
	wakeSettle = 800 * time.Millisecond

	// wakeIdlePoll is the fallback when the run is reported busy but has no done
	// channel to wait on (a simulated turn). It is a poll, not a sleep on the
	// critical path: the normal case waits on the channel itself.
	wakeIdlePoll = 200 * time.Millisecond

	// wakeAskTimeout bounds one permission card or question in a wake turn. The
	// user may be asleep; a wake turn must not park the conversation forever on an
	// ask nobody will answer. Two minutes is long enough for someone who is around.
	wakeAskTimeout = 2 * time.Minute

	// wakeIdleSettle gives the frontend's own "the turn is over" flush a moment to
	// take the conversation first. A message the user typed WHILE the previous turn
	// ran is queued in the UI and flushed on agent:idle; starting the wake
	// immediately would make that send be refused as "turn running", and the user's
	// message would wait behind a wake they did not ask for.
	wakeIdleSettle = 250 * time.Millisecond
)

// errWakeBusy says the conversation was not free after all (a user message
// arrived between the idle check and beginTurn). The caller puts the events back
// and waits for the next quiet moment; nothing is lost.
var errWakeBusy = errors.New("wake: the conversation is busy")

// taskQueueFor returns the task queue of one logical conversation, creating it
// (and its watcher) on first use.
//
// The queue is keyed by the LOGICAL conversation, not by session id: a
// compaction moves the conversation into a child session, and an event that
// arrived just before it must still reach whichever session is the leaf now.
func (d *desktopApp) taskQueueFor(logicalID string) *tasks.Queue {
	d.taskMu.Lock()
	defer d.taskMu.Unlock()
	if d.taskQueues == nil {
		d.taskQueues = make(map[string]*tasks.Queue)
	}
	if q := d.taskQueues[logicalID]; q != nil {
		return q
	}
	q := tasks.New()
	d.taskQueues[logicalID] = q

	// A queue with no conversation (an agent built with no session loaded) still
	// needs to exist so the reminder has something to read; it just has nobody to
	// wake. The same is true after shutdown, where a late build must not start a
	// watcher on a cancelled context.
	if logicalID == "" || d.taskClosed {
		return q
	}
	if d.taskCtx == nil {
		// The watchers live as long as the app; stopTaskQueues cancels them at
		// shutdown. One goroutine per conversation ever opened is the price of not
		// having a central dispatcher to route events through.
		d.taskCtx, d.taskCancel = context.WithCancel(context.Background())
	}
	go d.watchTaskQueue(d.taskCtx, logicalID, q)
	return q
}

// stopTaskQueues stops every watcher. Called at shutdown; a wake turn in flight
// is cancelled through its own context.
func (d *desktopApp) stopTaskQueues() {
	d.taskMu.Lock()
	cancel := d.taskCancel
	d.taskCancel = nil
	d.taskCtx = nil
	d.taskClosed = true
	d.taskQueues = nil
	d.taskMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// watchTaskQueue turns "something finished" into "the conversation continues".
//
// It is the idle half of the queue: the other half is the reminder collector,
// which takes the same events before a request when a turn is already running.
// Taking is destructive, so the two never report the same task twice.
func (d *desktopApp) watchTaskQueue(ctx context.Context, logicalID string, q *tasks.Queue) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.Signals():
		}
		d.drainWake(ctx, logicalID, q)
	}
}

// drainWake settles a batch, then runs one turn carrying it. It returns only
// when the batch has been delivered or the app is shutting down, so the watcher
// handles one wake at a time per conversation.
func (d *desktopApp) drainWake(ctx context.Context, logicalID string, q *tasks.Queue) {
	for {
		// The user's Stop holds the wake until they speak again: otherwise a chain
		// of tasks (test → fix → test) would keep running after "stop", which is the
		// one brake there is now that wakes are unlimited.
		if q.Paused() {
			return
		}
		events := d.settleAndTake(ctx, q, wakeSettle)
		if len(events) == 0 {
			return
		}
		// Paused while the batch was settling: put it back, the next user message
		// resumes and pokes the queue.
		if q.Paused() {
			q.Restore(events)
			return
		}

		sessionID, ok := d.waitForIdle(ctx, logicalID)
		if !ok {
			q.Restore(events)
			return
		}
		// The user's queued message (if any) goes first; see wakeIdleSettle.
		select {
		case <-time.After(wakeIdleSettle):
		case <-ctx.Done():
			q.Restore(events)
			return
		}

		started, err := d.runWakeTurn(ctx, logicalID, sessionID, events)
		switch {
		case started && err == nil:
			return
		case !started && errors.Is(err, errWakeBusy):
			// A user message (or a cron run) took the conversation between the idle
			// check and beginTurn. Put the events back and wait for the next quiet
			// moment; the loop's next idle wait does exactly that.
			q.Restore(events)
			continue
		case !started:
			// Nothing started (no agent, no config, a deleted session). The events
			// stay pending, so the next user message carries them in through the
			// reminder; retrying here would spin on a permanent failure.
			logger.New("desktop").Error(ctx, "task wake: could not start a turn", err, "conversation", logicalID)
			q.Restore(events)
			return
		default:
			// The turn started and failed: the events were recorded as its user
			// message before the request, so they are in the session and must not be
			// reported a second time by the next wake.
			logger.New("desktop").Error(ctx, "task wake turn failed", err, "conversation", logicalID)
			return
		}
	}
}

// settleAndTake waits out the settle window and returns everything pending. A
// signal arriving inside the window resets it, so a burst becomes one batch.
func (d *desktopApp) settleAndTake(ctx context.Context, q *tasks.Queue, window time.Duration) []tasks.Event {
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return q.Pending()
		case <-q.Signals():
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(window)
		case <-timer.C:
			return q.Pending()
		}
	}
}

// waitForIdle resolves the conversation's current leaf session and waits until
// no turn of it is running. It re-resolves after every wait: a turn may have
// compacted the conversation into a child session while it ran.
//
// The second return is false only when the app is shutting down (or the leaf
// vanished) — a busy conversation is waited out, never refused, because the
// event will keep until the user's turn is over.
func (d *desktopApp) waitForIdle(ctx context.Context, logicalID string) (string, bool) {
	for {
		if ctx.Err() != nil {
			return "", false
		}
		leaf, err := d.leafSession(logicalID)
		if err != nil {
			logger.New("desktop").Warn(ctx, "task wake: no leaf session", "conversation", logicalID, "err", err)
			return "", false
		}
		id := leaf.ID

		d.mu.Lock()
		r := d.ownerOfLocked(id)
		busy := r != nil && r.running
		var done chan struct{}
		if r != nil {
			done = r.turnDone
		}
		d.mu.Unlock()

		if !busy {
			return id, true
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return "", false
			}
		} else {
			// A simulated turn keeps no done channel; poll it rather than spin.
			select {
			case <-time.After(wakeIdlePoll):
			case <-ctx.Done():
				return "", false
			}
		}
	}
}

// runWakeTurn runs one turn whose user message is the finished tasks. Everything
// else is an ordinary turn: the same history, the same tool set, the same
// transcript lane — so the user sees the work continue instead of a private
// side channel.
//
// The one difference is notification: a successful wake must not push (the user
// may be asleep; the result is waiting in the conversation). A failed one still
// does, because that is news worth interrupting for. See the wakeTurn gate in
// handleEventIn.
//
// started reports whether a turn actually began. Everything before beginTurn is
// a refusal the caller can retry or hand to the next user message; once the turn
// began, its user message is on disk and must not be reported again.
func (d *desktopApp) runWakeTurn(ctx context.Context, logicalID, sessionID string, events []tasks.Event) (started bool, err error) {
	cfg := d.config()
	if cfg == nil {
		return false, fmt.Errorf("wake: no configuration")
	}
	r, err := d.prepareSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("prepare session: %w", err)
	}
	if r.agent == nil {
		return false, fmt.Errorf("wake: agent unavailable")
	}

	turnCtx, _, steerCh, turnDone, ok := d.beginTurnWith(ctx, sessionID, r)
	if !ok {
		return false, errWakeBusy
	}
	// Nobody may be at the keyboard: an ask has a bound (see wakeAskTimeout), and
	// the agent turns it into "refused / put it in your reply".
	turnCtx = agent.WithAskTimeout(turnCtx, wakeAskTimeout)
	d.mu.Lock()
	if run := d.runs[sessionID]; run != nil {
		run.wakeTurn = true
	}
	d.mu.Unlock()

	endReason := "error"
	defer func() {
		d.endTurn(sessionID, endReason)
		close(turnDone)
	}()

	modelText := systemreminder.RenderPieces([]systemreminder.Piece{{
		Name:  "completed-tasks",
		Lines: systemreminder.TaskLines(events),
	}})
	display := wakeDisplayText(events)

	// A conversation whose agent was built lazily (the app restarted and nobody
	// opened this session yet) has no in-memory history. A wake turn must still see
	// the conversation it continues — otherwise the model is handed "a task
	// finished" with no idea what the task was. Same restore LoadSession does.
	history := d.runHistory(sessionID)
	if len(history) == 0 {
		if h, herr := r.agent.LoadSessionHistory(); herr == nil && len(h) > 0 {
			history = h
			d.setRunHistory(sessionID, h)
		}
	}
	history, modelText = mergeTrailingUserMessage(history, modelText)

	ropts := []agent.RunOption{
		agent.WithSteerChannel(steerCh),
		agent.WithDisplayUserMessage(display),
		agent.WithExtraTools(tools.NewSendFileTool()),
	}
	// The same run-scoped extras a normal turn carries: a wake is a continuation of
	// the conversation, not a lesser kind of turn.
	if d.cronScheduler() != nil {
		ropts = append(ropts, agent.WithExtraTools(d.newDesktopCronTool(sessionID)))
	}
	if delegator := d.delegatorFor(d.config()); delegator != nil {
		ropts = append(ropts, agent.WithDelegation(delegator))
	}

	// One line per wake, so a self-sustaining chain is visible after the fact even
	// though nothing limits it.
	logger.New("desktop").Info(ctx, "task wake: continuing the conversation",
		"conversation", logicalID, "session", sessionID, "tasks", len(events))
	d.setSessionState(sessionID, AgentState{Status: StatusThinking, Label: "继续", Detail: "任务完成"})

	stream := r.agent.RunConversationStream(
		turnCtx, history, modelText, d.systemPromptFor(sessionID),
		llm.ChatOptions{MaxTokens: cfg.MaxTokens},
		ropts...,
	)
	for ev := range stream {
		switch ev.Type {
		case agent.AgentEventTurnComplete:
			endReason = "complete"
		case agent.AgentEventError:
			if ev.Result != nil &&
				(ev.Result.ExitReason == agent.ExitReasonInterrupted ||
					ev.Result.ExitReason == agent.ExitReasonCancelled) {
				endReason = "interrupted"
			}
		}
		d.handleEventIn(sessionID, ev, laneTranscript)
	}
	return true, nil
}

// existingTaskQueue returns a conversation's queue without creating one (and
// without starting a watcher). Used by the pause/resume paths, which only ever
// act on a conversation that already has an agent.
func (d *desktopApp) existingTaskQueue(logicalID string) *tasks.Queue {
	d.taskMu.Lock()
	defer d.taskMu.Unlock()
	return d.taskQueues[logicalID]
}

// runLogicalID is the conversation a run belongs to, read off its session
// manager. Empty when nothing is loaded (a simulated turn, an agent-less run).
func runLogicalID(r *sessionRun) string {
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.LogicalConversationID()
	}
	return ""
}

// pauseWake holds a conversation against automatic wakes. Called when a turn
// the user stopped ends.
func (d *desktopApp) pauseWake(logicalID string) {
	if logicalID != "" {
		if q := d.existingTaskQueue(logicalID); q != nil {
			q.Pause()
		}
	}
}

// resumeWake lets it wake again. Called when the user speaks — a new message or
// a steer — because being present is the condition the pause waits for.
func (d *desktopApp) resumeWake(logicalID string) {
	if logicalID != "" {
		if q := d.existingTaskQueue(logicalID); q != nil {
			q.Resume()
		}
	}
}

// wakeForSession is the logical conversation of a session's run, read under the
// app lock (prepareSession writes r.sm there) and without creating a run.
func (d *desktopApp) wakeForSession(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return runLogicalID(d.ownerOfLocked(id))
}

// postTaskEvent is where a remote delegation joins the same queue local tasks
// use.
//
// It only posts to a conversation this process has actually worked in (a queue
// exists only for those). The delegation records are shared by every entry point
// through ~/.tachi, and a record can belong to a conversation another process is
// driving right now — waking it from here would start a second turn on a session
// that is already running elsewhere. A conversation not opened in this process
// still learns about its task from RemoteTasksReminder on its next turn.
func (d *desktopApp) postTaskEvent(logicalID string, e tasks.Event) {
	if logicalID == "" {
		return
	}
	// A conversation a channel owns is continued by the CHANNEL: it holds that
	// thread's agent, history and lock, and a second runtime on the same session
	// would write two narratives into one file.
	if turns := d.channelRuntime(); turns != nil && turns.PostTask(logicalID, e) {
		return
	}
	q := d.existingTaskQueue(logicalID)
	if q == nil {
		return
	}
	q.Post(e)
}

// stopAllTasks terminates this session's background processes and holds the
// automatic wake — the desktop's 「全停」, the counterpart of the TUI's Ctrl+X.
//
// It is executed DIRECTLY by the composer (not as a command turn) so it works
// while a turn is running, which is exactly when "stop everything" is wanted.
// A running sub-agent is not tracked here: it has no cancel handle, and its
// completion is reported like any other task.
func (d *desktopApp) stopAllTasks(id string) string {
	if id == "" {
		return "没有活跃会话"
	}
	d.mu.Lock()
	r := d.ownerOfLocked(id)
	var a *agent.AIAgent
	logical := ""
	if r != nil {
		a = r.agent
		logical = runLogicalID(r)
	}
	d.mu.Unlock()
	if a == nil {
		return "没有在跑的会话"
	}

	running := a.Config.ProcessManager.List()
	a.KillBackgroundProcesses()
	// A deliberate stop should not be followed by the agent picking the work up
	// on its own a second later; the next message releases it (see drainWake).
	if logical != "" {
		d.pauseWake(logical)
	}
	if len(running) == 0 {
		return "没有在跑的后台任务"
	}
	return fmt.Sprintf("已停止 %d 个后台任务", len(running))
}

// wakeDisplayText is the shared summary renderer (systemreminder), under the name a
// wake caller reads.
func wakeDisplayText(events []tasks.Event) string { return systemreminder.TaskDisplayText(events) }
