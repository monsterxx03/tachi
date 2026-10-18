package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// SendMessage starts a turn in the session the CALLER names. It returns immediately; state changes
// are streamed to the frontend via the "agent:state" event and to the menu bar.
//
// The session is named by the caller rather than read from d.activeID, because the two can differ
// — a restart leaves the backend with NO active session while the window still has one on screen,
// and a stale window sends after a switch — and this used to answer "ok" for a message that went
// nowhere: the frontend draws its bubble and a running placeholder BEFORE the call (so the reader
// sees their text and 正在执行), and a turn that never started then never sends an event to clear
// either one. Every path that does not start a turn now says why, and composer.tsx renders that
// reason in the placeholder it opened (see startTurn).
func (s *AgentService) SendMessage(sessionID, text string) string {
	if len(text) == 0 {
		return "empty"
	}
	return s.desk.startTurn(sessionID, text)
}

// Stop aborts the current turn (real agent via cancel, or simulated) and
// returns to idle.
func (s *AgentService) Stop() string {
	s.desk.stopTurn()
	return "ok"
}

// StopAndSend stops the named session's turn (if any) and immediately starts a new one with text —
// the "send now" action for the pending queue. Unlike Stop followed by SendMessage, it waits for
// the previous turn's goroutine to fully exit first, so the new turn can never race the old one for
// run state. In the simulated fallback it stops the sim and waits for it to wind down.
//
// Like SendMessage it is told WHICH session (see there); the frontend puts the text back in its
// queue on any refusal, so an empty answer is not possible.
func (s *AgentService) StopAndSend(sessionID, text string) string {
	if text == "" {
		return "empty"
	}
	d := s.desk
	if sessionID == "" {
		return refuseNoSession
	}
	d.mu.Lock()
	id := sessionID
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
	return d.startTurn(id, text)
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
//
// The session id is resolved through getRun, so a steer aimed at the child of a compaction
// that is still in flight reaches the turn that is producing into it. Read off the map by the
// child's id alone it would find no steer channel and answer "not running" — the frontend shows
// the text in the transcript, the agent's steer_check times out, and the message is gone.
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
	expanded := atfile.Expand(root, text, atfile.Options{Sessions: d.sessionSource()})
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- agent.SteerInput{Text: expanded.Text, Display: text, Images: expanded.Images}:
		return "ok"
	default:
		return "dropped"
	}
}

// startTurn dispatches to the named session's real agent when configured, otherwise to the
// simulated fallback so the UI always responds.
//
// It returns "ok" when a turn is running, or the reason none started — never nothing. Both
// refusals used to be bare `return`s, which is what made a lost message invisible: the caller
// answered "ok", the frontend had already drawn the bubble, and no event ever came to clear it.
func (d *desktopApp) startTurn(id, text string) string {
	if id == "" {
		return refuseNoSession
	}
	d.mu.Lock()
	r := d.getRun(id)
	// A session runs one turn at a time, and this is asked BEFORE anything is prepared: the
	// refusal must not depend on how far the run happens to be built (r.agent nil, a simulated
	// turn in flight), and building an agent is exactly what must not happen for a turn that is
	// about to be refused. beginTurn re-checks it under the lock it needs anyway, so this is the
	// early answer, not the authoritative one.
	//
	// getRun resolves the CHILD of a compaction that is still in flight to the run producing
	// into it, so a message sent in that child answers with this refusal instead of starting a
	// second turn on the same conversation (see ownerOfLocked).
	busy := r.running
	d.mu.Unlock()
	if busy {
		return refuseTurnRunning
	}

	// Ensure this session has its own agent (lazy per-session build). No config
	// (bootstrap failed) → fall back to the simulated turn.
	if r.agent == nil {
		if d.config() == nil {
			if !d.startSimulatedTurn(context.Background(), id, text) {
				return refuseTurnRunning
			}
			return "ok"
		}
		pr, err := d.prepareSession(context.Background(), id)
		if err != nil || pr.agent == nil {
			if !d.startSimulatedTurn(context.Background(), id, text) {
				return refuseTurnRunning
			}
			return "ok"
		}
		r = pr
	}

	// Mark the run busy and take the turn scaffolding (cancellable context,
	// steer channel, done channel) before touching the history: a busy session
	// must not be prepared for a second turn at all.
	ctx, cancel, steerCh, turnDone, ok := d.beginTurn(id, r)
	if !ok {
		return refuseTurnRunning
	}
	_ = cancel // held by the run (r.turnCancel); Stop is the user-facing path

	history := d.runHistory(id)
	// The user's OWN words, before any expansion: the transcript must show what was typed
	// (`@path`), while the model gets the expanded text. Session records keep both.
	// (A merged trailing message — an interrupted session — keeps only THIS turn's text as
	// the display while Content holds both; that path is rare and its display side is
	// cosmetic, which is not worth threading a second display string through the merge.)
	rawText := text
	// Expand @-file references before anything else: text files are inlined,
	// images become multi-modal content parts. This must happen BEFORE the
	// trailing-user merge below — history stores already-expanded user
	// messages, so expanding the merged text would inline the same files twice.
	// References resolve against the session's working directory (the root the
	// tools run in), never the app bundle's cwd.
	expanded := atfile.Expand(d.expansionRoot(r), text, atfile.Options{Sessions: d.sessionSource()})
	text = expanded.Text
	// An interrupted session may end with a user message and no matching
	// assistant reply. Merge that trailing user message into the new user
	// message (instead of appending an artificial assistant reply) so the
	// provider never sees consecutive user messages.
	history, text = mergeTrailingUserMessage(history, text)

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
		ropts := []agent.RunOption{
			agent.WithSteerChannel(steerCh),
			agent.WithExtraTools(tools.NewSendFileTool()),
			agent.WithDisplayUserMessage(rawText),
		}
		// The Cron tool is available in a normal turn when this process actually
		// schedules. It is run-scoped (the closure names this conversation), so a
		// cached agent never carries it into another session.
		if sched := d.cronScheduler(); sched != nil {
			ropts = append(ropts, agent.WithExtraTools(d.newDesktopCronTool(id)))
		}
		// Delegation rides as its own option: it brings both the two tools and the reminders that
		// describe nodes and outstanding work, and it is the run-scoped gate (§10.2.1) — a run
		// that does not pass it can neither call the tools nor be told about them.
		if delegator := d.delegatorFor(d.config()); delegator != nil {
			ropts = append(ropts, agent.WithDelegation(delegator))
		}
		if len(expanded.Images) > 0 {
			ropts = append(ropts, agent.WithPendingImages(expanded.Images))
		}
		ch := r.agent.RunConversationStream(ctx, history, text, d.systemPromptFor(id), llm.ChatOptions{
			MaxTokens: d.config().MaxTokens,
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
	return "ok"
}

func (d *desktopApp) stopTurn() {
	d.mu.Lock()
	var c context.CancelFunc
	if id := d.activeID; id != "" {
		// ownerOfLocked, not d.runs[id]: with the child of an in-flight compaction on screen,
		// the turn to stop is the run that is producing into it — reading the map by the child's
		// id would find nothing and Stop would answer "ok" having cancelled nothing.
		if r := d.ownerOfLocked(id); r != nil {
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
	return d.beginTurnWith(context.Background(), id, r)
}

// beginTurnWith is beginTurn with a parent context. A scheduled run passes the
// scheduler's context so that the job's timeout (and app shutdown) cancels the
// turn it started; an interactive turn keeps the background context it always
// had. Callers must NOT hold d.mu.
func (d *desktopApp) beginTurnWith(parent context.Context, id string, r *sessionRun) (ctx context.Context, cancel context.CancelFunc, steerCh chan agent.SteerInput, turnDone chan struct{}, ok bool) {
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
	// The tool-call count is per TURN, so it starts here (see countToolCall).
	r.toolCalls = 0

	ctx, cancel = context.WithCancel(parent)
	// The session's tree-derived state is re-pointed here, BEFORE the turn goroutine
	// exists: a folder change or a project edit only marks the run (treeStale), because
	// doing it from the UI goroutine could rewrite the store under a running turn. This
	// is the one point where no turn of this session can be in flight.
	var sessionTree string
	var sessionAdditional []string
	if r.sm != nil {
		// The turn's working directory comes from the ONE root-set resolver
		// (sessionRootsFrom), not from the record directly: the prompt, the @-file
		// search and the skill store are all fed from there, and reading WorkingDir
		// here is how bash ended up running in the new tree while the prompt still
		// advertised the old one. The ADDITIONAL roots ride along because the
		// permission policy reads every tree's .tachi/permissions.yaml.
		if cur := r.sm.Current(); cur != nil {
			if primary, additional := d.sessionRootsFrom(cur); primary != "" {
				ctx = wdctx.WithDir(ctx, primary)
				sessionTree = primary
				sessionAdditional = additional
			}
		}
	}
	reloadTree := r.treeStale && r.agent != nil && sessionTree != ""
	// Cleared either way: a session with no workspace has no tree to re-point at, and
	// retrying every turn would be busywork.
	r.treeStale = false
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
	if reloadTree {
		// Everything whose inputs are FIXED AT BUILD TIME and derived from the
		// session's tree gets re-pointed here. Two members today, and they are
		// re-pointed together on purpose — a second, parallel "stale" flag is how
		// one of them ends up pointing at the tree the other just left:
		//   - the skill store's scan roots (sessionSkillStore), so the session stops
		//     offering the old tree's project skills and Skill create's "project"
		//     target stops landing there;
		//   - the bash permission policy's project rules (<tree>/.tachi/permissions.yaml),
		//     so a command the new tree forbids is not judged by the old tree's file.
		// Cheap — dir lists plus tool registration, no scan — and done under d.mu on
		// purpose: holding it is what keeps another turn of this session from starting
		// while they are being swapped.
		r.agent.ReloadSkillsIn(sessionTree)
		// ...and every tree's rules, not just the one the session just moved to:
		// the additional roots keep their own .tachi/permissions.yaml.
		r.agent.SetPermissionTrees(ctx, sessionTree, sessionAdditional)
	}
	return ctx, cancel, steerCh, turnDone, true
}

// markTreeStale records that the session's tree-derived state must be re-pointed at the start
// of its next turn (see sessionRun.treeStale). Called from the UI goroutine — the writers are
// a folder change and a project edit — so it takes d.mu and nothing else; an unknown session
// or one with no run at all is a no-op, since an agent built later reads the new tree anyway.
// Resolved through ownerOfLocked, so a folder change made while the child of a live compaction
// is on screen marks the run that will actually reload its tree.
func (d *desktopApp) markTreeStale(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ownerOfLocked(id); r != nil {
		r.treeStale = true
	}
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
		// The scheduled-turn bookkeeping is per turn, like the tool-call count:
		// a name left behind would label the NEXT turn's notification with the
		// previous job's name.
		r.cronJobName = ""
		r.cronSilent = false
		// The turn is over, so its tool-call count is too (see countToolCall): the device's
		// line about it is about a turn IN FLIGHT, and a number left standing after the turn
		// would read as progress on something that already finished.
		r.toolCalls = 0
	}
	d.mu.Unlock()
	// An AskUserQuestion this turn parked on is gone the moment the turn is, however it
	// ended. The permission half needs no such sweep — its waiting goroutine drops its own
	// row through a defer — but an AskUser wait lives on the AGENT's channel, so a turn
	// stopped before it was answered would otherwise leave the device holding a question
	// nobody can answer any more.
	d.dropAsks(id, askKindAskUser)
	// The pushes a settings save deferred for this session run now: the turn is over, and this is
	// the point where no turn of it can be in flight (see deferred_push.go). Before agent:idle, so
	// the change is in place before the messages the user queued during the turn go out.
	now := d.followCompaction(id)
	d.flushDeferredPushes(id, now)
	d.emitIdle(now, endReason)
}

// emitUsage recomputes a session's cost/credit from the usage ledger and pushes them to
// the status bar (the frontend listens for "agent:cost"). It runs at every point where the
// ledger may have grown — after each API call AND at turn end — because the incremental
// usage event only covers tool-call rounds. usage, when non-nil, is forwarded to the
// frontend as the raw per-call numbers ("agent:usage"); the cost payload always comes from
// the ledger so both paths agree by construction.
func (d *desktopApp) emitUsage(id string, isCurrent bool, usage *llm.Usage) {
	if d.app == nil {
		return
	}
	r := d.getRun(id)
	d.rebuildCostCredit(r)
	d.mu.Lock()
	cost, credit := r.cost, r.credit
	rate := r.cacheHitRate
	hasCacheHit := r.hasCacheHit
	d.mu.Unlock()
	if isCurrent && usage != nil {
		d.app.Event.Emit("agent:usage", usage)
	}
	// The context ring rides the same event as the cost row: both are "this session's
	// numbers", and both are only meaningful right after an API call. Without it the ring
	// waited for the END of the turn (the frontend refreshed it from GetProviderInfo on
	// turn_complete), so a long turn — many tool rounds — left the ring at the value it had
	// when the turn started, while the popover, which fetches on open, already knew. See
	// itest/desktop/drivers/ctx-ring.js.
	est, win := d.contextUsageOf(r)
	d.app.Event.Emit("agent:cost", map[string]any{
		"sessionId": id, "cost": cost, "credit": credit,
		"cacheHitRate": rate, "hasCacheHit": hasCacheHit,
		"contextEstimate": est, "contextWindow": win,
	})
}

// handleEvent maps AgentEvent types to the running state, and forwards the raw
// event to the frontend so it can do streaming rendering.
// runLane says where a run's UI events go.
//
// A conversation turn streams into the transcript, where it belongs. A command that runs an
// one-off fork (/review, /commit) reports to the side-channel lane instead: its content is
// written to its own record file, which the panel reads, and the conversation is left with a
// single anchor line. The session state (busy / tps / usage) updates the same way on both
// lanes — only the CONVERSATION payloads move.
//
// Design: docs/2026-09-12-desktop-oneoff-panel-design.md §4.2
type runLane int

const (
	// laneTranscript: text / thinking / tool events become the conversation.
	laneTranscript runLane = iota
	// laneOneOff: they are dropped from the conversation — the record file is the content —
	// and only the one-off lane's lifecycle events (see emitOneOff) are sent.
	laneOneOff
	// laneCron: a scheduled run. Like laneTranscript it IS the conversation (the reader can
	// watch the work), but its completion notification names the job instead of saying
	// 「回合完成」, and when_relevant can suppress that notification.
	laneCron
)

// intoTranscript says whether this lane's content is the conversation.
func (l runLane) intoTranscript() bool {
	return l == laneTranscript || l == laneCron
}

func (d *desktopApp) handleEvent(id string, ev agent.AgentEvent) {
	d.handleEventIn(id, ev, laneTranscript)
}

func (d *desktopApp) handleEventIn(id string, ev agent.AgentEvent, lane runLane) {
	isCurrent := d.currentID() == id
	// toTranscript decides whether THIS event may become part of the conversation. The
	// state updates below are never gated: a review still shows 执行/思考 in the status bar,
	// still moves the token rate, still refreshes the usage numbers.
	toTranscript := d.app != nil && lane.intoTranscript()
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
		if ev.Type == agent.AgentEventToolCallArgs {
			d.countToolCall(id)
		}
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行",
			Detail: toolDetail(ev.ToolName, ev.ToolArgs)})
		// Push a lightweight tool event carrying the human-readable args summary
		// (reuses tools.ToolArgsSummary) so the frontend can render the title
		// and full args.
		if toTranscript && isCurrent {
			d.app.Event.Emit("agent:tool", map[string]any{
				"name":   ev.ToolName,
				"title":  tools.ToolArgsSummary(ev.ToolName, ev.ToolArgs),
				"args":   ev.ToolArgs,
				"change": changeVO(ev.ToolName, ev.ToolArgs),
				// The tool call's own id: a response can call the same tool SEVERAL times, and the
				// starts arrive before the args (the consumer emits a start per block while the
				// response streams, the executor the args when it runs them), so the frontend cannot
				// tell the cards apart by name — every arg landed on the newest one (issue #39).
				"toolCallId": ev.ToolID,
			})
		}
	case agent.AgentEventToolResult:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusToolRunning, Label: "执行", Detail: "工具完成"})
		// A saved plan changes the plan panel. The panel's payload is the FILE
		// (GetPlan), so the event only says "re-read it": that keeps the panel and the
		// record from drifting, and it is why the tool args are not carried here.
		// Success only — a SavePlan that failed to write changed nothing.
		if toTranscript && isCurrent && ev.ToolName == tools.ToolNameSavePlan && !ev.ToolIsError {
			d.app.Event.Emit("agent:plan", map[string]any{"sessionId": id})
		}
	case agent.AgentEventAskUser:
		// The agent loop is parked, waiting for the user's answers. Push the
		// questions to the frontend, which renders the form and answers via
		// AgentService.AnswerQuestion (the TUI does the same through
		// RespondToAskUser).
		d.tpsReset(id)
		// Record it as a pending ask BEFORE the state is written: the marker this session
		// carries (AgentState.Waiting) is read from this table at emission (waitingSentence),
		// so registering after it would leave that one state without a marker.
		d.registerAsk(&pendingAsk{
			kind:      askKindAskUser,
			sessionID: id,
			toolID:    ev.ToolID,
			questions: ev.Questions,
		})
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "提问", Detail: "等待回答"})
		if d.app != nil {
			d.app.Event.Emit("agent:ask", AskEvent{
				SessionID: id,
				ToolID:    ev.ToolID,
				Questions: ev.Questions,
			})
		}
		// The turn cannot proceed without the user, so tell them — unless they
		// are looking at this conversation already (see watching).
		questions := make([]string, 0, len(ev.Questions))
		for _, q := range ev.Questions {
			questions = append(questions, q.Question)
		}
		d.notify.notifyAsk(id, questions)
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
		// run keeps its current key until the turn ends (see followCompaction) —
		// and from this moment the child IS this conversation, which is why the
		// record below is also what the rest of the desktop resolves the child
		// through (ownerOfLocked): a turn cannot start on it, and the sidebar's
		// spinner names it rather than the parent's key.
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
		d.emitUsage(id, isCurrent, ev.Usage)
	case agent.AgentEventTurnComplete:
		d.tpsReset(id)
		d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已回复"})
		// Refresh the status bar here as well, not only on the incremental usage event:
		// that event is emitted for tool-call rounds alone, so a turn ending in a plain
		// reply left the cost and the cache-hit ring at zero until a session switch.
		d.emitUsage(id, isCurrent, nil)
		d.mu.Lock()
		r := d.getRun(id)
		// Only the conversation may hand its history over. A side-channel run's messages are the
		// review's or commit's own prompt and answer; adopting them would replace what the next
		// turn sends — the model would forget the conversation, and the context ring, rebuilt from
		// that history, would collapse to the side-channel's size. (The loop hands a one-off run's
		// partial history over like any other, so the guard has to be here, where the lane is known.)
		if ev.Messages != nil && lane.intoTranscript() {
			r.history = ev.Messages
		}
		r.turnCost = r.cost - r.turnStartCost
		r.turnCredit = r.credit - r.turnStartCredit
		turnCost, turnCredit := r.turnCost, r.turnCredit
		cronName, cronSilent := r.cronJobName, r.cronSilent
		d.mu.Unlock()
		if toTranscript && ev.Result != nil && isCurrent {
			d.app.Event.Emit("agent:result", ev.Result)
			d.app.Event.Emit("agent:turn", map[string]any{
				"sessionId":  id,
				"durationMs": ev.Result.Duration.Milliseconds(),
				"iterations": ev.Result.IterationsUsed,
				"cost":       turnCost,
				"credit":     turnCredit,
				// The checkpoint this turn was recorded as, so the transcript can name the
				// turn its own user bubble started (it assembles that bubble itself and
				// never sees the record index a reload would have given it).
				"checkpointTurn": ev.CheckpointTurn,
				// What the turn changed, from its own two trees — the exact answer, shell
				// commands included. Absent when the checkpoints have no pair for it, which
				// is the frontend's cue to fall back to what the tool calls declared.
				"changes": changeSummaryVO(r.agent, ev.CheckpointTurn),
			})
		}
		// The other moment worth interrupting for (see notifier): the turn is
		// done and the user is not looking at this conversation. A user-initiated stop is
		// excluded — announcing "回合完成" right after the user hit stop is noise.
		// A one-off run ends here too — the command lane forwards its last TurnComplete — but it
		// has its own notification, raised where its outcome is known (see notifyOneOffDone):
		// announcing 「回合完成」 for a run with no turn on screen says nothing useful.
		// A scheduled run also ends here, but names the job and can be silent
		// (when_relevant → [SILENT]): the reply is already in the conversation.
		interrupted := ev.Result != nil &&
			(ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled)
		switch lane {
		case laneTranscript:
			if ev.Result != nil && !interrupted {
				d.notify.notifyTurnDone(id, ev.Result.IterationsUsed, ev.Result.Duration)
				// 设备也响一声（另一种音频，见 link.AlertKindDone）：同一条「你不在看」的判据，
				// 而用户离电脑远时未必听得见系统通知，也未必在看屏幕。
				d.alertIfUnwatched(id, link.AlertKindDone)
			}
		case laneCron:
			if ev.Result != nil && !interrupted && cronName != "" && !cronSilent {
				d.notify.notifyCronDone(id, cronName)
				d.alertIfUnwatched(id, link.AlertKindDone)
			}
		}
	case agent.AgentEventError:
		d.tpsReset(id)
		detail := "见日志"
		if ev.Result != nil && ev.Result.Error != nil {
			detail = ev.Result.Error.Error()
		}
		// User-initiated stop is a normal conclusion, NOT an error — same
		// semantics as tui (Ctrl+C), acp (prompt cancel) and channel (/stop).
		interrupted := ev.Result != nil &&
			(ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled)
		if interrupted {
			d.setSessionState(id, AgentState{Status: StatusIdle, Label: "空闲", Detail: "已停止"})
		} else {
			d.setSessionState(id, AgentState{Status: StatusError, Label: "出错", Detail: detail})
		}
		// The partial history is kept on EVERY exit that carries one — not only the interrupted
		// one. A stream that dies mid-turn (the client's own timeout, a dropped connection) has
		// already EXECUTED tool calls by then: the loop hands rs.Messages over, the session file
		// already holds them, and dropping them here is how the next turn's request comes to be
		// missing work the model actually did — so it repeats it (issue #38; tui and acp have
		// always taken whatever the event carried). What arrives is complete pairs only, since a
		// partially accumulated assistant message is never appended to rs.Messages.
		//
		// The STATE above still says 出错: keeping the history is not the same as calling the turn
		// a success, and the frontend draws the failure from its own event.
		d.mu.Lock()
		if r := d.getRun(id); ev.Messages != nil && lane.intoTranscript() {
			r.history = ev.Messages
		}
		cronName := ""
		if r := d.ownerOfLocked(id); r != nil {
			cronName = r.cronJobName
		}
		d.mu.Unlock()
		// A scheduled run has no failure bubble on screen while the user is
		// elsewhere; the notification is the only signal that the report they
		// were waiting for did not arrive.
		if lane == laneCron && !interrupted && cronName != "" {
			d.notify.notifyCronFailed(id, cronName)
		}
		// A conversation turn concludes its newest assistant bubble; an one-off run has no
		// bubble to conclude (its failure is reported on its own lane, see emitOneOff), and
		// patching "the newest assistant" would hit the turn the review was FOR.
		if toTranscript {
			d.app.Event.Emit("agent:error", map[string]any{"sessionId": id, "error": detail, "interrupted": interrupted})
		}
	}
	// Forward the raw event for every session so the frontend can keep a
	// per-session message cache (background sessions keep streaming too). This is the second
	// of the two paths into the transcript (the typed events above are the first), so a
	// side-channel run must be kept out of it as well.
	if toTranscript {
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

// setSessionState records one session's own state and reports it.
//
// The EVENT is per session and ALWAYS emitted, carrying the session it is about. The
// desktop runs each session's turn in its own goroutine — switching sessions cancels
// nothing — so the frontend keeps one state per conversation, and a background turn's
// progress (and, more to the point, its END) has to reach the session it belongs to. It
// used to be emitted only while that session was the displayed one, which pinned the
// composer's stop control on whatever conversation happened to be on screen when a turn
// started, and left it spinning there for good once the turn was over; clicking it did
// nothing, because Stop acts on the DISPLAYED session's turn.
//
// The menu bar is the one surface that is NOT per session — it describes the displayed
// session — so only that one reflects there.
// toolDetail 是状态屏那一行的内容：哪个工具、拿的什么参数。
//
// 参数摘要复用前端标题用的同一个函数（tools.ToolArgsSummary）：只写工具名说不出「它在
// 干什么」，而这块屏存在的意义正是回答那个问题。
//
// 长度不在这里收：设备那条副标题有自己的字节上限，由 Snapshot 那道边界统一裁
// （见 badge.go 的 deviceDetail）——状态里存原样的说法，别的消费者不必跟着窄。
func toolDetail(name, argsJSON string) string {
	summary := tools.ToolArgsSummary(name, argsJSON)

	if summary == "" {
		return "调用 " + name
	}
	return "调用 " + name + "：" + summary
}

// countToolCall 记下这一轮又多了一个**开始执行**的工具调用——设备那一行「第 N 个工具
// 调用」读的就是它（见 badge.go 的 Snapshot）。
//
// 只有 AgentEventToolCallArgs 走这里，不是 ToolCallStart：start 是模型流式吐出一个
// tool_call block 时发的，一次响应里的几个调用会在**任何东西跑起来之前**全部到达，按它
// 计数数字一步就跳满了——而设备要看的正是「它在推进」。代价是并行工具组：executor 会把
// 一组的 Args 一次性发完（agent/tool_executor.go 的 executeToolCallsParallel 第一阶段），
// 那一组会让数字跳几格。那是事实（那几个调用确实同时在跑），不是需要抹平的偏差。
//
// 计数记在 sessionRun 上而不是 AgentState 里：AgentState 的每个 setSessionState 调用点
// 都是整份重建的，放进去就要求每一处都记着把计数带过来——十几处里漏一处，数字就归零。
// 起止由 beginTurn / endTurn 两个共同的出入口管住（runTurn 与 startCommand 都走它们）。
func (d *desktopApp) countToolCall(id string) {
	d.mu.Lock()
	if r := d.getRun(id); r != nil {
		r.toolCalls++
	}
	d.mu.Unlock()
}

func (d *desktopApp) setSessionState(id string, st AgentState) {
	st.SessionID = id
	// The sidebar's marker is filled HERE, from the ask table — never taken from the caller
	// (see waitingSentence in asks.go for the race that makes a caller-supplied field wrong).
	st.Waiting = d.waitingSentence(id, st)
	d.mu.Lock()
	r := d.getRun(id)
	r.state = st
	d.mu.Unlock()
	d.emitState(st)
	// The menu bar follows the DISPLAYED session's state and, on top of it, how many turns are in
	// flight — so it has to be repainted for a state change in ANY session, not just the current
	// one: a background session starting or finishing is exactly what changes that number.
	d.refreshTray()
	// The device link shows the same state (badge.go), and this is the one road every
	// state change travels — a turn's progress that the device missed would leave it
	// reporting a session as idle while it is working.
	d.notifyLink()
}

// emitState pushes one session's state to the frontend (see setSessionState).
func (d *desktopApp) emitState(st AgentState) {
	if d.app != nil {
		d.app.Event.Emit("agent:state", st)
	}
}

// trayLabel is the menu bar's text: today's spend and how full the displayed session's context is,
// joined with " · ".
//
// Either half can be missing (nothing spent yet, nothing measured), and when BOTH are the label
// falls back to the session's short state — the bar must keep saying something rather than flipping
// to an empty box next to the icon.
func trayLabel(st AgentState, cost, context string) string {
	parts := make([]string, 0, 2)
	if cost != "" {
		parts = append(parts, cost)
	}
	if context != "" {
		parts = append(parts, context)
	}
	if len(parts) == 0 {
		return st.Label
	}
	return strings.Join(parts, " · ")
}

// refreshTray repaints the tray for the DISPLAYED session — the only surface where a state is
// not per session. The icon carries the state; the two numbers are what the window cannot tell you
// while it is hidden.
//
// The numbers go to THREE places, and each covers a surface the others cannot reach: SetLabel is
// the macOS menu bar's text (a Linux tray host ignores it), the status MENU ITEM is where a Linux
// user reads them (a macOS menu shows it too, which is a bonus), and desktop_status.json is what a
// bar widget outside this process draws (barstatus.go). The three are set from the same strings
// and numbers so they can never disagree.
func (d *desktopApp) refreshTray() {
	if d.tray == nil {
		return
	}
	st := d.currentState()
	estimate, _, window := d.contextUsage(st.SessionID)
	spent := d.todayCost()
	cost := formatCost(spent)
	context := trayContext(estimate, window)
	d.tray.SetLabel(trayLabel(st, cost, context))
	if d.trayStatus != nil {
		d.trayStatus.SetLabel(trayStatusItemText(st, cost, context))
	}
	applyTrayIcon(d.tray, st.Status)
	writeDesktopStatus(st, spent, contextPercent(estimate, window))
}

// trayStatusItemText is the label of the tray menu's status item: the state word plus the same two
// numbers the bar label carries.
//
// The state word is kept even when the numbers are present: on Linux this item is what a reader
// looks at to know what the app is doing, and "¥0.386 · 42%" alone would not say whether it is
// working or idle.
func trayStatusItemText(st AgentState, cost, context string) string {
	readout := trayLabel(st, cost, context) // the numbers, or st.Label when there are none
	if readout == st.Label {
		return "运行状态: " + st.Label
	}
	return "运行状态: " + st.Label + " · " + readout
}

// reflectActive re-reports a session's state at the moment it becomes the displayed one:
// the menu bar has to follow the switch, and re-sending the event costs nothing (the same
// payload the session's own changes already send). LoadSession routes through
// setSessionState because it also settles the loaded run's history; this is the lightweight
// half, for a switch between sessions that are both already in the frontend's cache.
func (d *desktopApp) reflectActive(id string) {
	if id == "" {
		return
	}
	st := d.stateFor(id)
	st.SessionID = id
	d.emitState(st)
	d.refreshTray()
}

func (d *desktopApp) stateFor(id string) AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ownerOfLocked(id); r != nil {
		return r.state
	}
	return AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
}

// currentState returns the displayed session's state, NAMED by that session: the frontend
// files what it receives by id, so a payload without one could only be applied to whatever
// happened to be on screen when it landed.
func (d *desktopApp) currentState() AgentState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := AgentState{Status: StatusIdle, Label: "空闲", Detail: "就绪"}
	if r := d.ownerOfLocked(d.activeID); r != nil {
		st = r.state
	}
	st.SessionID = d.activeID
	return st
}

// startSimulatedTurn runs the canned turn the desktop falls back to when no agent is available
// (no config, or a bootstrap that failed). It reports whether it STARTED: the simulation is a
// per-APP singleton, so a request that arrives while one is in flight starts nothing — and the
// caller has to say so, because a silent return here is the same invisible failure as a
// dropped message (the frontend already drew the bubble it asked about).
func (d *desktopApp) startSimulatedTurn(ctx context.Context, id, _ string) bool {
	d.mu.Lock()
	if d.simCh != nil {
		select {
		case <-d.simCh:
		default:
			d.mu.Unlock()
			return false
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
	return true
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

// mergeTrailingUserMessage folds a trailing user message from the history into the text
// of the turn about to be sent, and returns the shortened history. An interrupted (or
// killed) session can end with a user message and no reply, and a provider must never
// see two consecutive user messages.
//
// The trailing message's <system-reminder> block is DROPPED rather than carried over.
// Session conversion re-attaches the block that message was stored with — deliberately,
// historyHasReminder relies on the prefix — so merging the content verbatim would embed
// the PREVIOUS turn's block in this turn's user text: the model would read a stale plan
// id, stale diagnostics and a stale branch as if they described now, and because the
// merged text is recorded as the new message, the block would then walk forward through
// the session turn after turn. This turn builds its own block for the current state
// (see agent/systemreminder.UnwrapUserMessage).
//
// A trailing message that is nothing BUT a block is left alone, and that is a
// deliberate trade rather than an oversight. Two shapes look identical here: a
// reminder the loop injected mid-run (`injectLoopReminders` appends one as a user
// message, so a Stop landing right there leaves it trailing), which is stale
// scaffolding the next run rebuilds; and a trailing artifact reminder, which
// ConvertSessionToLLMMessages appends as its own user message ON PURPOSE so that a
// reload cannot drop it (see the comment there — an /research or /review finding must
// still reach the model after a restart). There is no marker that separates them, and
// dropping the message would silently break the second to tidy up the first, so the
// no-user-text case keeps the behaviour it had.
//
// The trailing message's IMAGE parts are left behind too — this merge rebuilds a prompt
// from text, and only this turn's own @-references contribute images. An image attached
// to an interrupted prompt is therefore not re-sent by the turn that merges it; its
// record keeps the bytes, so a later reload (where the message is no longer trailing, and
// nothing merges) brings it back into the request. Carrying it here instead would mean
// rebuilding the parts array for a merged message — a behaviour for the interrupted-turn
// path, not a detail of the merge.
func mergeTrailingUserMessage(history []llm.Message, text string) ([]llm.Message, string) {
	n := len(history)
	if n == 0 || history[n-1].Role != "user" {
		return history, text
	}
	trailing := history[n-1].Content
	if unwrapped := systemreminder.UnwrapUserMessage(trailing); strings.TrimSpace(unwrapped) != "" {
		trailing = unwrapped
	}
	return history[:n-1], trailing + "\n" + text
}

// runHistory returns the session's in-memory conversation history (nil when the
// session is unknown). Resolved through ownerOfLocked: the child of a live compaction is the
// conversation the run holds, so asking for it by the child's id answers with the history the
// agent is actually sending (the compacted one), not with nil.
func (d *desktopApp) runHistory(id string) []llm.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ownerOfLocked(id); r != nil {
		return r.history
	}
	return nil
}

// setRunHistory replaces the session's in-memory conversation history — used
// after a compaction, which hands the run a shorter one.
func (d *desktopApp) setRunHistory(id string, history []llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ownerOfLocked(id); r != nil {
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

	// toolCalls is how many tool calls THIS TURN has started executing — the number
	// the device shows for the session (「第 N 个工具调用」). Guarded by d.mu, zeroed
	// by beginTurn and endTurn, and never touched by a state change (see countToolCall).
	//
	// It lives here rather than in AgentState because every setSessionState call site
	// builds a whole AgentState from scratch: a field there would have to be carried
	// across a dozen of them, and one miss would silently zero the number.
	toolCalls int

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

	// treeStale records that this session's TREE-DERIVED state no longer points at the tree
	// the session works in (a folder change, or a project edit that moved every member), and
	// that it must be re-pointed at the start of the next turn. See beginTurn for the members
	// it covers; they are deliberately re-pointed together, from one flag and one resolved
	// directory, so none of them can end up describing a tree another has left.
	//
	// It is set from the UI goroutine and applied in beginTurn, which is the only place that
	// can prove no turn of this session is mid-flight — re-pointing a store or a policy while
	// a turn reads it is a data race. Guarded by d.mu.
	treeStale bool

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
	//
	// It is read for more than the move: while it is set, the child it names is owned by THIS
	// run (see ownerOfLocked), which is what keeps a second turn from starting on a
	// conversation this one is still writing. followCompaction consumes it, and the ownership
	// goes with it.
	compaction *compactionRecord

	// cronJobName / cronSilent describe the SCHEDULED turn this run is executing,
	// for the completion notification only. They are set by the cron handler
	// before the stream starts, cronSilent just before the terminal event (the
	// reply is known by then), and cleared by endTurn. Guarded by d.mu.
	cronJobName string
	cronSilent  bool
}

// delegatorCache is this process's ONE sending-side cache, built on first use and kept in step with
// the peers config leaf (see node.DelegatorCache).
func (d *desktopApp) delegatorCache(cfg *config.Config) *node.DelegatorCache {
	if cfg == nil {
		return nil
	}
	d.delegatorMu.Lock()
	if d.delegator == nil {
		d.delegator = node.NewDelegatorCache(cfg.Node.Name, node.DiscoveryOf(cfg), logger.New("desktop"), d.peerDiscovered)
	}
	cache := d.delegator
	d.delegatorMu.Unlock()
	return cache
}

// senderFor returns the sending side as the CONCRETE type, for the callers that need more than the
// tool layer's contract: the node board shows each peer's card and the address this machine dials,
// and RemoteDelegator is deliberately blind to both.
func (d *desktopApp) senderFor(cfg *config.Config) *node.Sender {
	cache := d.delegatorCache(cfg)
	if cache == nil {
		return nil
	}
	// The cache is asked with no app lock held: it does its own locking, and the rule this codebase
	// follows is one lock at a time (a disk read under mu is how the desktop's own turn goroutines
	// would start queueing behind the sidebar's).
	return cache.ForSender(context.Background(), cfg.Peers)
}

// delegatorFor returns the sending side for this configuration, rebuilt when the peer list
// changes (see node.DelegatorCache). Nil means this process cannot delegate right now.
func (d *desktopApp) delegatorFor(cfg *config.Config) tools.RemoteDelegator {
	cache := d.delegatorCache(cfg)
	if cache == nil {
		return nil
	}
	return cache.For(context.Background(), cfg.Peers)
}
