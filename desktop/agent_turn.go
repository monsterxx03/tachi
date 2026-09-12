package main

import (
	"context"
	"errors"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// SendMessage starts a turn. It returns immediately; state changes are
// streamed to the frontend via the "agent:state" event and to the menu bar.
func (s *AgentService) SendMessage(text string) string {
	if len(text) == 0 {
		return "empty"
	}
	s.desk.startTurn(text)
	return "ok"
}

// Stop aborts the current turn (real agent via cancel, or simulated) and
// returns to idle.
func (s *AgentService) Stop() string {
	s.desk.stopTurn()
	return "ok"
}

// StopAndSend stops the current turn (if any) and immediately starts a new one
// with text — the "send now" action for the pending queue. Unlike Stop
// followed by SendMessage, it waits for the previous turn's goroutine to fully
// exit first, so the new turn can never race the old one for run state. In the
// simulated fallback it stops the sim and waits for it to wind down.
func (s *AgentService) StopAndSend(text string) string {
	if text == "" {
		return "empty"
	}
	d := s.desk
	d.mu.Lock()
	id := d.activeID
	if id == "" {
		d.mu.Unlock()
		return refuseNoSession
	}
	r := d.getRun(id)
	cancel := r.turnCancel
	done := r.turnDone
	noAgent := r.agent == nil
	wasRunning := r.running
	d.mu.Unlock()

	if noAgent {
		d.stopSimulatedTurn()
		// The sim goroutine clears simCh when it finishes; poll briefly so the
		// new turn doesn't bounce off the still-active sim lock.
		for i := 0; i < 40; i++ {
			d.mu.Lock()
			ch := d.simCh
			d.mu.Unlock()
			if ch == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	} else if wasRunning {
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				return "stop timeout"
			}
		}
	}
	d.startTurn(text)
	return "ok"
}

// Steer answers the agent's steer_check for a session: it delivers the text
// the frontend queued while the turn was running so the agent loop can inject
// it at the current steer point (after the just-finished tool calls, before
// the next LLM call). Empty text is the "nothing queued" signal that simply
// unblocks the loop.
//
// The channel is drained before writing so a reply that races a timed-out or
// already-answered steer point can neither wedge the loop nor leave a stale
// value behind for the next steer point to consume.
func (s *AgentService) Steer(sessionID, text string) string {
	d := s.desk
	d.mu.Lock()
	r := d.getRun(sessionID)
	ch := r.steerCh
	running := r.running
	// Steered text carries @-file references too, so expand it against the
	// same root the turn's tools use (the caller already holds the lock).
	root := d.expansionRoot(r)
	d.mu.Unlock()
	if ch == nil || !running {
		return "not running"
	}
	// An empty steer stays empty: it is the "nothing queued" signal that
	// unblocks the agent loop.
	expanded := atfile.Expand(root, text)
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- agent.SteerInput{Text: expanded.Text, Images: expanded.Images}:
		return "ok"
	default:
		return "dropped"
	}
}

// startTurn dispatches to the active session's real agent when configured,
// otherwise to the simulated fallback so the UI always responds.
func (d *desktopApp) startTurn(text string) {
	d.mu.Lock()
	id := d.activeID
	if id == "" {
		d.mu.Unlock()
		return
	}
	r := d.getRun(id)
	d.mu.Unlock()

	// Ensure this session has its own agent (lazy per-session build). No config
	// (bootstrap failed) → fall back to the simulated turn.
	if r.agent == nil {
		if d.cfg == nil {
			d.startSimulatedTurn(context.Background(), id, text)
			return
		}
		pr, err := d.prepareSession(context.Background(), id)
		if err != nil || pr.agent == nil {
			d.startSimulatedTurn(context.Background(), id, text)
			return
		}
		r = pr
	}

	// Mark the run busy and take the turn scaffolding (cancellable context,
	// steer channel, done channel) before touching the history: a busy session
	// must not be prepared for a second turn at all.
	ctx, cancel, steerCh, turnDone, ok := d.beginTurn(id, r)
	if !ok {
		return
	}
	_ = cancel // held by the run (r.turnCancel); Stop is the user-facing path

	history := d.runHistory(id)
	// Expand @-file references before anything else: text files are inlined,
	// images become multi-modal content parts. This must happen BEFORE the
	// trailing-user merge below — history stores already-expanded user
	// messages, so expanding the merged text would inline the same files twice.
	// References resolve against the session's working directory (the root the
	// tools run in), never the app bundle's cwd.
	expanded := atfile.Expand(d.expansionRoot(r), text)
	text = expanded.Text
	// An interrupted session may end with a user message and no matching
	// assistant reply. Merge that trailing user message into the new user
	// message (instead of appending an artificial assistant reply) so the
	// provider never sees consecutive user messages.
	if n := len(history); n > 0 && history[n-1].Role == "user" {
		text = history[n-1].Content + "\n" + text
		history = history[:n-1]
	}

	go func() {
		// endReason describes why the stream finished; surfaced to the frontend
		// in the agent:idle event so it can decide whether to auto-flush the
		// pending queue (only a natural completion auto-flushes — after a stop
		// or an error the queue stays for the user to review/clear).
		endReason := "error"
		exitReason, iters := "", 0
		defer func() {
			// One line per turn: how it ended. Without it a turn that produced nothing (or
			// ended as interrupted for no visible reason) leaves no trace at all — the
			// frontend only learns the exit reason through the payload it renders, and a
			// dropped reply then looks like a silent backend.
			logger.New("desktop").Info(context.Background(), "turn finished",
				"session", id, "reason", endReason, "exit", exitReason, "iterations", iters)
			d.endTurn(id, endReason)
			close(turnDone)
		}()
		d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "理解中…"})

		// Images extracted from @-image references ride along as multi-modal
		// content parts on the trailing user message; SendFile lets the agent
		// hand the user a file it produced. No callback is needed: the
		// transcript renders the attachment card from the tool call itself, so
		// the live turn and reloaded history take one rendering path.
		ropts := []agent.RunOption{agent.WithSteerChannel(steerCh), agent.WithExtraTools(tools.NewSendFileTool())}
		if len(expanded.Images) > 0 {
			ropts = append(ropts, agent.WithPendingImages(expanded.Images))
		}
		ch := r.agent.RunConversationStream(ctx, history, text, d.systemPromptFor(id), llm.ChatOptions{
			MaxTokens: d.cfg.MaxTokens,
		}, ropts...)
		for ev := range ch {
			switch ev.Type {
			case agent.AgentEventTurnComplete:
				endReason = "complete"
			case agent.AgentEventError:
				if ev.Result != nil && (ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled) {
					endReason = "interrupted"
				}
			}
			if ev.Result != nil {
				exitReason = ev.Result.ExitReason
				iters = ev.Result.IterationsUsed
			}
			d.handleEvent(id, ev)
		}
	}()
}

func (d *desktopApp) stopTurn() {
	d.mu.Lock()
	var c context.CancelFunc
	if id := d.activeID; id != "" {
		if r := d.runs[id]; r != nil {
			c = r.turnCancel
		}
	}
	d.mu.Unlock()
	if c != nil {
		c()
	}
	d.stopSimulatedTurn()
}

// beginTurn marks a session's run busy and returns the scaffolding every turn
// shares, whatever it is about to run (a conversation reply or a slash command):
// a cancellable context that carries the session's working directory (so tools
// run where the session does), the steer channel the frontend answers, and the
// done channel StopAndSend waits on. ok is false when there is nothing to run —
// no run, or one already busy, since a session runs one turn at a time.
//
// Callers must NOT hold d.mu.
func (d *desktopApp) beginTurn(id string, r *sessionRun) (ctx context.Context, cancel context.CancelFunc, steerCh chan agent.SteerInput, turnDone chan struct{}, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r == nil || r.running {
		return nil, nil, nil, nil, false
	}
	r.running = true
	// Snapshot cost/credit at turn start so the footer can show this turn's
	// incremental cost/credit when it completes.
	r.turnStartCost = r.cost
	r.turnStartCredit = r.credit

	ctx, cancel = context.WithCancel(context.Background())
	if r.sm != nil {
		if cur := r.sm.Current(); cur != nil && cur.WorkingDir != "" {
			ctx = wdctx.WithDir(ctx, cur.WorkingDir)
		}
	}
	r.turnCtx, r.turnCancel = ctx, cancel
	// Steer wiring: the agent emits steer_check and parks on steerCh between
	// tool calls; the frontend answers via AgentService.Steer. Buffered (cap 1)
	// so the reply can never be lost to a select+default send racing ahead of
	// the agent's receive. Per turn, never closed — Steer only writes while the
	// turn is running (r.steerCh is cleared to nil on exit).
	steerCh = make(chan agent.SteerInput, 1)
	turnDone = make(chan struct{})
	r.steerCh = steerCh
	r.turnDone = turnDone
	return ctx, cancel, steerCh, turnDone, true
}

// endTurn finishes a turn: clears the run's busy state, re-homes it if
// auto-compaction moved the conversation, and tells the frontend the turn is
// over. The session hand-off has to happen BEFORE agent:idle, because idle is
// what drains the queue of messages the user typed while the turn ran — under
// the old id those would be sent to the session the conversation has left.
func (d *desktopApp) endTurn(id, endReason string) {
	d.mu.Lock()
	if r := d.runs[id]; r != nil {
		r.running = false
		r.steerCh = nil
		r.turnDone = nil
	}
	d.mu.Unlock()
	d.emitIdle(d.followCompaction(id), endReason)
}

// handleEvent maps AgentEvent types to the running state, and forwards the raw
// event to the frontend so it can do streaming rendering.
func (d *desktopApp) handleEvent(id string, ev agent.AgentEvent) {
	isCurrent := d.currentID() == id
	switch ev.Type {
	case agent.AgentEventThinkingDelta:
		d.tpsAdd(id, ev.ThinkingDelta)
		d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "推理中…"})
	case agent.AgentEventTextDelta:
		d.tpsAdd(id, ev.TextDelta)
		if d.stateFor(id).Status != StatusThinking {
			d.setSessionState(id, AgentState{Status: StatusThinking, Label: "思考", Detail: "生成中…"})
		}
	case agent.AgentEventToolCallStart, agent.AgentEventToolCallArgs:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行", Detail: "调用 " + ev.ToolName})
		// Push a lightweight tool event carrying the human-readable args summary
		// (reuses tools.ToolArgsSummary) so the frontend can render the title
		// and full args.
		if d.app != nil && isCurrent {
			d.app.Event.Emit("agent:tool", map[string]any{
				"name":   ev.ToolName,
				"title":  tools.ToolArgsSummary(ev.ToolName, ev.ToolArgs),
				"args":   ev.ToolArgs,
				"change": changeVO(ev.ToolName, ev.ToolArgs),
			})
		}
	case agent.AgentEventToolResult:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行", Detail: "工具完成"})
		// A saved plan changes the plan panel. The panel's payload is the FILE
		// (GetPlan), so the event only says "re-read it": that keeps the panel and the
		// record from drifting, and it is why the tool args are not carried here.
		// Success only — a SavePlan that failed to write changed nothing.
		if isCurrent && d.app != nil && ev.ToolName == tools.ToolNameSavePlan && !ev.ToolIsError {
			d.app.Event.Emit("agent:plan", map[string]any{"sessionId": id})
		}
	case agent.AgentEventAskUser:
		// The agent loop is parked, waiting for the user's answers. Push the
		// questions to the frontend, which renders the form and answers via
		// AgentService.AnswerQuestion (the TUI does the same through
		// RespondToAskUser).
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "提问", Detail: "等待回答"})
		if d.app != nil {
			d.app.Event.Emit("agent:ask", AskEvent{
				SessionID: id,
				ToolID:    ev.ToolID,
				Questions: ev.Questions,
			})
		}
		// The turn cannot proceed without the user, so tell them — unless they
		// are already looking at the window (see notifier).
		questions := make([]string, 0, len(ev.Questions))
		for _, q := range ev.Questions {
			questions = append(questions, q.Question)
		}
		d.notify.notifyAsk(d.sessionTitle(id), questions)
	case agent.AgentEventAutoCompactStart:
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "处理", Detail: "压缩上下文…"})
		// Compaction is an LLM call of its own and takes a while, so the
		// transcript says what the pause is. (The raw event is forwarded too,
		// but the notice is Go's, like the TUI's and ACP's wording.)
		if d.app != nil {
			d.app.Event.Emit("agent:compact_start", map[string]any{"sessionId": id})
		}
	case agent.AgentEventAutoCompactDone:
		// Failure keeps the original history — the loop retries on its next
		// iteration (which is also where a cancelled turn ends: the loop top
		// notices ctx.Done and terminates as interrupted).
		if ev.Result != nil && ev.Result.Error != nil {
			// A user stop and the compact timeout are different stories: one is
			// "you cancelled it", the other is the loop about to retry. Only the
			// real failure touches the status label — a cancelled turn reports
			// itself a moment later through the interrupted path.
			reason := "failed"
			switch {
			case errors.Is(ev.Result.Error, context.Canceled):
				reason = "cancelled"
			case errors.Is(ev.Result.Error, context.DeadlineExceeded):
				reason = "timeout"
			default:
				d.setSessionState(id, AgentState{Status: StatusBusy, Label: "处理", Detail: "压缩失败，继续"})
			}
			if d.app != nil {
				d.app.Event.Emit("agent:compact_done", map[string]any{
					"sessionId": id,
					"reason":    reason,
					"error":     ev.Result.Error.Error(),
				})
			}
			break
		}
		// Success: the agent swapped the conversation into a child session. The
		// run keeps its current key until the turn ends (see followCompaction).
		d.mu.Lock()
		moved := ""
		if r := d.getRun(id); r != nil && r.sm != nil {
			if cur := r.sm.Current(); cur != nil {
				moved = cur.ID
				if moved != id {
					r.compaction = &compactionRecord{SessionID: moved, OldMsgCount: ev.OldMsgCount, Summary: ev.CompactSummary}
				}
			}
		}
		d.mu.Unlock()
		if d.app != nil {
			d.app.Event.Emit("agent:compact_done", map[string]any{
				"sessionId": id,
				"movedTo":   moved,
				"oldCount":  ev.OldMsgCount,
				"summary":   ev.CompactSummary,
			})
		}
	case agent.AgentEventUsage:
		// Recompute the session's cumulative cost/credit from the usage ledger
		// and push it to the status bar (frontend listens for "agent:cost").
		r := d.getRun(id)
		d.rebuildCostCredit(r)
		if d.app != nil {
			d.mu.Lock()
			cost, credit := r.cost, r.credit
			rate := r.cacheHitRate
			hasCacheHit := r.hasCacheHit
			d.mu.Unlock()
			if isCurrent {
				d.app.Event.Emit("agent:usage", ev.Usage)
			}
			d.app.Event.Emit("agent:cost", map[string]any{"sessionId": id, "cost": cost, "credit": credit, "cacheHitRate": rate, "hasCacheHit": hasCacheHit})
		}
	case agent.AgentEventTurnComplete:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已回复"})
		d.mu.Lock()
		r := d.getRun(id)
		if ev.Messages != nil {
			r.history = ev.Messages
		}
		r.turnCost = r.cost - r.turnStartCost
		r.turnCredit = r.credit - r.turnStartCredit
		turnCost, turnCredit := r.turnCost, r.turnCredit
		d.mu.Unlock()
		if d.app != nil && ev.Result != nil && isCurrent {
			d.app.Event.Emit("agent:result", ev.Result)
			d.app.Event.Emit("agent:turn", map[string]any{
				"sessionId":  id,
				"durationMs": ev.Result.Duration.Milliseconds(),
				"iterations": ev.Result.IterationsUsed,
				"cost":       turnCost,
				"credit":     turnCredit,
			})
		}
		// The other moment worth interrupting for (see notifier): the turn is
		// done and nobody is looking at the window. A user-initiated stop is
		// excluded — announcing "回合完成" right after the user hit stop is noise.
		if ev.Result != nil && ev.Result.ExitReason != agent.ExitReasonInterrupted && ev.Result.ExitReason != agent.ExitReasonCancelled {
			d.notify.notifyTurnDone(d.sessionTitle(id), ev.Result.IterationsUsed, ev.Result.Duration)
		}
	case agent.AgentEventError:
		d.tpsReset(id)
		detail := "见日志"
		if ev.Result != nil && ev.Result.Error != nil {
			detail = ev.Result.Error.Error()
		}
		// User-initiated stop is a normal conclusion, NOT an error — same
		// semantics as tui (Ctrl+C), acp (prompt cancel) and channel (/stop).
		// Keep the partial history so the next turn can continue from it.
		interrupted := ev.Result != nil &&
			(ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled)
		if interrupted {
			d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
			d.mu.Lock()
			if r := d.getRun(id); ev.Messages != nil {
				r.history = ev.Messages
			}
			d.mu.Unlock()
		} else {
			d.setSessionState(id, AgentState{Status: StatusError, Label: "出错", Detail: detail})
		}
		if d.app != nil {
			d.app.Event.Emit("agent:error", map[string]any{"sessionId": id, "error": detail, "interrupted": interrupted})
		}
	}
	// Forward the raw event for every session so the frontend can keep a
	// per-session message cache (background sessions keep streaming too).
	if d.app != nil {
		d.app.Event.Emit("agent:event", map[string]any{"sessionId": id, "event": ev})
	}
}

// ── Simulated fallback (used only when the real agent failed to bootstrap) ──

// emitIdle notifies the frontend that a turn goroutine has fully finished.
// For real agents it is emitted from the turn goroutine's defer (after running
// is reset); the simulated fallback calls it explicitly at both exit paths.
// reason ∈ {complete, interrupted, error} — the frontend only auto-flushes its
// pending queue on "complete".
// emitIdle tells the frontend a turn's goroutine has fully exited. It carries
// whether that turn was the DISPLAYED session, because that — not the
// frontend's own currentId — is what decides if a queued message may be
// auto-sent: a compaction switch can move the session between the event being
// emitted and the callback running, and the comparison has to survive that.
func (d *desktopApp) emitIdle(id, reason string) {
	if d.app != nil {
		d.app.Event.Emit("agent:idle", map[string]any{
			"sessionId": id,
			"reason":    reason,
			"current":   d.currentID() == id,
		})
	}
}

func (d *desktopApp) setSessionState(id string, st AgentState) {
	d.mu.Lock()
	r := d.getRun(id)
	r.state = st
	isCurrent := d.activeID == id
	d.mu.Unlock()
	if isCurrent {
		d.reflectState(st)
	}
}

func (d *desktopApp) stateFor(id string) AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[id]; r != nil {
		return r.state
	}
	return AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
}

func (d *desktopApp) currentState() AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[d.activeID]; r != nil {
		return r.state
	}
	return AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
}

func (d *desktopApp) reflectState(st AgentState) {
	if d.tray != nil {
		d.tray.SetLabel(st.Label)
		if icon := trayIcon(st.Status); icon != nil {
			d.tray.SetTemplateIcon(icon)
		}
	}
	if d.app != nil {
		d.app.Event.Emit("agent:state", st)
	}
}

func (d *desktopApp) startSimulatedTurn(ctx context.Context, id, _ string) {
	d.mu.Lock()
	if d.simCh != nil {
		select {
		case <-d.simCh:
		default:
			d.mu.Unlock()
			return
		}
	}
	stop := make(chan struct{})
	d.simCh = stop
	d.mu.Unlock()

	sequence := []AgentState{
		{Status: StatusThinking, Label: "思考", Detail: "理解用户意图…"},
		{Status: StatusToolRunning, Label: "执行", Detail: "调用工具 grep"},
		{Status: StatusBusy, Label: "处理", Detail: "组织回答…"},
	}

	go func() {
		for _, st := range sequence {
			select {
			case <-stop:
				d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
				d.emitIdle(id, "interrupted")
				d.endSimulatedTurn(stop)
				return
			case <-time.After(1400 * time.Millisecond):
			}
			d.setSessionState(id, st)
		}
		select {
		case <-stop:
			d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
			d.emitIdle(id, "interrupted")
			d.endSimulatedTurn(stop)
			return
		case <-time.After(1200 * time.Millisecond):
		}
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已完成回答"})
		d.emitIdle(id, "complete")
		d.endSimulatedTurn(stop)
	}()
}

func (d *desktopApp) stopSimulatedTurn() {
	d.mu.Lock()
	stop := d.simCh
	d.mu.Unlock()
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

func (d *desktopApp) endSimulatedTurn(stop chan struct{}) {
	d.mu.Lock()
	if d.simCh == stop {
		d.simCh = nil
	}
	d.mu.Unlock()
}

// runHistory returns the session's in-memory conversation history (nil when the
// session is unknown).
func (d *desktopApp) runHistory(id string) []llm.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[id]; r != nil {
		return r.history
	}
	return nil
}

// setRunHistory replaces the session's in-memory conversation history — used
// after a compaction, which hands the run a shorter one.
func (d *desktopApp) setRunHistory(id string, history []llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[id]; r != nil {
		r.history = history
	}
}

// desktopApp holds everything that outlives a single request: the Wails app,
// the main window, the menu-bar tray, and (in S2) the real tachi agent with its
// session manager and usage ledger.
// sessionRun holds per-session runtime state so multiple conversations can run
// independently (each keeps its own history / in-flight turn / status).
type sessionRun struct {
	// agent is this session's OWN AIAgent (per-session isolation, like channel's
	// per-thread cachedAgent). Lazily created; provider/thinking switches are
	// applied in place via SetResolvedProvider/SetThinking.
	agent *agent.AIAgent
	// sm is this session's OWN session manager (current bound to this session),
	// so concurrent turns never steal another session's "current".
	sm *session.Manager
	// agentProvider is the config provider name the agent was last configured
	// for. Maintained for introspection/debugging; switches are applied in place.
	agentProvider string

	running    bool
	history    []llm.Message
	turnCtx    context.Context
	turnCancel context.CancelFunc
	state      AgentState

	// steerCh is this session's current-turn steer channel (buffered, cap 1):
	// the frontend writes agent.SteerInput into it at steer points so the
	// agent loop can inject pending user input between tool calls. Recreated
	// per turn; never closed (a send on a closed channel panics) — cleared to
	// nil when the turn goroutine exits.
	steerCh chan agent.SteerInput
	// turnDone is closed once the current turn's goroutine has fully exited
	// (running already reset, agent:idle already emitted). StopAndSend waits on
	// it so "stop, then immediately reply" never races a still-running turn.
	turnDone chan struct{}

	// cost/credit are the session's cumulative CNY cost and ledger credit
	// ("积分"), rebuilt from the usage ledger (see rebuildCostCredit) — the
	// same ledger aggregation the TUI /usage report uses.
	cost   float64
	credit float64

	// cacheHitRate is the session's cumulative prompt-cache hit rate (0..1),
	// rebuilt from the usage ledger alongside cost/credit.
	cacheHitRate float64

	// Live output rate (tokens/sec) for the status bar, following the TUI
	// semantics: tpsTokens accumulates agent.EstimateTokenCount of text +
	// thinking deltas; the rate is frozen into tpsLast at segment boundaries.
	tpsTokens int64
	tpsStart  time.Time
	tpsLast   int64

	// hasCacheHit indicates a recent call had usage data, so the cache-hit ring
	// should display even when the rate is 0% (a brand-new prompt has no cache
	// to hit, but we still want to show it rather than hide it).
	hasCacheHit bool

	// Per-turn footer bookkeeping: cost/credit snapshot at turn start so the
	// turn's incremental cost/credit (run.cost - turnStartCost) can be shown.
	turnStartCost   float64
	turnStartCredit float64
	turnCost        float64
	turnCredit      float64

	// compaction records what auto-compaction did to this conversation during
	// the current turn (nil = it did not compact). The agent loop compacts
	// in-flight and moves the conversation into a CHILD session (see
	// agent.FinalizeCompact), so the result is held here and the run is re-homed
	// once the turn settles — the stream must keep its original key until then,
	// or the running message would be re-keyed under the frontend's feet.
	compaction *compactionRecord
}
