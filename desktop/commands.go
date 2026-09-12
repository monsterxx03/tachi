package main

// Slash commands for the desktop.
//
// The command SET is the shared registry (agent/commands), so the desktop can
// never drift from the TUI/ACP/channel: what differs per frontend is only how a
// command is executed and where its output goes. Here a command runs as a TURN —
// the session goes busy, Stop cancels it, tool calls and text stream into the
// transcript through the same event path as a normal reply — which is why the
// three handlers below are thin: each one reuses the agent's own runner for the
// job (/commit → RunCommitOneOff, /review → the shared orchestrator + forks,
// /compact → a tool-less conversation turn + CompleteCompact).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"encoding/json"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/shutil"
)

// CommandVO describes a slash command for the composer's "/" palette.
type CommandVO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputHint   string `json:"inputHint,omitempty"`
}

// commandRun is everything a handler needs: the session's run, the turn's
// context, the raw arguments, and the channel its events are pumped into.
// commandRun is one command's execution context. scope and reviewedMsg are the desktop's
// review context: which files the review covers, and which turn asked for it (see
// agent.ReviewOrigin).
type commandRun struct {
	desk *desktopApp
	run  *sessionRun
	id   string
	ctx  context.Context
	args string
	// scope limits the command to specific paths (currently only /review uses it: the
	// turn-level "review these changes" entry knows which files the turn touched).
	scope []string
	// reviewedMsg is the conversation message the run was started for ("" for a typed
	// command). It is recorded with the run so a restart can still pair the two.
	reviewedMsg string
	ech         chan<- agent.AgentEvent
}

// desktopCommandHandlers maps a command name to its desktop implementation.
// Only names in this map are offered to the frontend (see ListCommands), so a
// registry entry without an implementation is simply not advertised.
var desktopCommandHandlers = map[string]func(*commandRun) error{
	"compact":       runCompactCommand,
	commandReview:   runReviewCommand,
	commandCommit:   runCommitCommand,
	"sh":            runShellCommand,
}

// The commands that run as one-off forks. Named because more than the tables below needs them:
// a finished run is announced by WHAT it was (see oneOffDoneBody in notify.go).
const (
	commandReview = "review"
	commandCommit = "commit"
)

// commandLane says where a command's UI events go, and it is a property of the COMMAND
// rather than of "being a command": /review and /commit run one-off forks, whose process
// belongs in the side-channel panel, while /compact rewrites this very conversation (its
// summary IS its output) and /sh echoes a command the user wanted to see here. Declared in
// one place so the split cannot drift per call site.
var commandLane = map[string]runLane{
	"compact":      laneTranscript,
	"sh":           laneTranscript,
	commandReview:  laneOneOff,
	commandCommit:  laneOneOff,
}

// laneFor answers the table above, defaulting to the transcript: a command whose lane
// nobody declared keeps the behaviour it had.
func laneFor(name string) runLane {
	if lane, ok := commandLane[name]; ok {
		return lane
	}
	return laneTranscript
}

// ListCommands returns the slash commands the desktop supports, in registry
// order (which is what the palette shows).
func (s *AgentService) ListCommands() []CommandVO {
	defs := cmds.MatchPrefixForMode("", cmds.ModeDesktop)
	out := make([]CommandVO, 0, len(defs))
	for _, def := range defs {
		if _, ok := desktopCommandHandlers[def.Name]; !ok {
			continue
		}
		out = append(out, CommandVO{Name: def.Name, Description: def.Description, InputHint: def.InputHint})
	}
	return out
}

// RunCommand runs a slash command for the displayed session. It returns "" when
// the command started, or a human-readable reason when it did not — the
// frontend shows that as a notice in the transcript instead of pretending a
// turn happened.
func (s *AgentService) RunCommand(text string) string {
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
	if body == "" {
		return "空命令"
	}
	def := cmds.FindByPrefix(body)
	if def == nil {
		return fmt.Sprintf("未知命令：/%s", strings.Fields(body)[0])
	}
	handler, ok := desktopCommandHandlers[def.Name]
	if !ok {
		return fmt.Sprintf("desktop 暂不支持 /%s", def.Name)
	}
	args := strings.TrimSpace(strings.TrimPrefix(body, def.Name))
	// A typed command has no turn behind it: the review it starts covers the whole tree.
	return s.desk.startCommand(def.Name, args, nil, "", laneFor(def.Name), handler)
}

// ReviewChanges runs a review SCOPED to the files a turn changed: the desktop's
// turn-level entry, one click next to the diff chip. It runs as a turn — the session
// goes busy, Stop cancels it, and the reviewer's ReportFinding calls are counted for the
// frontend's one-line anchor — but its process goes to the side-channel lane: the
// conversation keeps one line about the review, and the panel shows the rest.
//
// reviewedMsg is the id of the turn whose footer was clicked. It is recorded WITH the run
// (agent.ReviewOrigin) because that pairing is otherwise only in the frontend's memory: a
// restart would lose it, and the turn could no longer say 已评审 N 条.
//
// Returns "" when the run started, or a reason the UI shows instead.
func (s *AgentService) ReviewChanges(sessionID string, paths []string, reviewedMsg string) string {
	if len(paths) == 0 {
		return "这一轮没有可评审的改动"
	}
	d := s.desk
	d.mu.Lock()
	active := d.activeID
	d.mu.Unlock()
	if active == "" {
		return "没有活跃会话"
	}
	if sessionID != "" && sessionID != active {
		return "只能评审当前会话的改动"
	}
	if notice := s.nothingToReview(active, paths); notice != "" {
		return notice
	}
	return d.startCommand(commandReview, "", paths, reviewedMsg, laneFor(commandReview), desktopCommandHandlers[commandReview])
}

// nothingToReview explains why a review would have nothing to look at ("" when it would).
//
// A review reads the WORKING TREE — its prompt tells the fork to run `git diff HEAD --
// <paths>` — and the file list is all that survives a turn (no snapshot of the content is
// kept anywhere). So once those changes are committed the diff is empty, and the reviewer
// would dutifully report "no changes", which the panel then renders as 「最近一次评审没有
// 报告问题」: a review that saw nothing, dressed up as a review that found nothing.
//
// Saying so instead is the honest version. The judgement is GetTurnDiff's — the same call
// the diff panel makes — so the button and the panel can never disagree about whether
// there is anything there.
func (s *AgentService) nothingToReview(sessionID string, paths []string) string {
	diff := s.GetTurnDiff(sessionID, paths)
	if len(diff.Files) > 0 {
		return ""
	}
	if diff.Note != "" {
		// GetTurnDiff already explained it in the user's words (no workspace, not a git
		// repository, paths outside it) — every one of those means no baseline to diff
		// against, so the review would be blind for the same reason.
		//
		// The ignored case is the one where "no baseline" is the wrong half of the story: the
		// file exists and is new, git simply hides it, and a reviewer reading the tree would
		// see it. Saying so is the difference between "nothing to review" and "I cannot see it".
		if diff.Ignored > 0 {
			return diff.Note + "；评审同样看不到被忽略的文件，因此没有开始评审"
		}
		return diff.Note + "；没有可对照的基线，评审看不到改动，因此没有开始评审"
	}
	return "这些文件在工作树里已经没有未提交的差异了（可能已经提交）——评审只能看未提交的改动，所以没有开始评审"
}

// startCommand runs a command with a turn's lifecycle. The event source is the
// handler's own run rather than a conversation call, but everything around it is
// shared with startTurn: the busy flag, the cancellable context (Stop works),
// the steer channel, the transcript stream, and the post-turn session hand-off.
//
// Sub-run boundaries are hidden: a handler may run several LLM calls (a
// multi-round /review), and each one ends with its own turn_complete. Only the
// LAST one is forwarded, so the transcript's "running" state and the turn footer
// describe the command as a whole instead of finish-and-restart per round.
//
// Returns "" when the command started, or the reason it did not — the frontend
// shows that instead of a turn that never happened.
// emitOneOff reports a side-channel run's lifecycle on the one-off lane.
//
// It carries FACTS ONLY — that a run of this kind started, and how it ended (findings
// counted, duration, iterations, why it failed). The content is not sent: the run writes
// its own record file as it goes, and the panel reads that. One source of truth, and no
// second streaming implementation to keep in step with the transcript's.
func (d *desktopApp) emitOneOff(id string, payload map[string]any) {
	if d.app == nil {
		return
	}
	payload["sessionId"] = id
	d.app.Event.Emit("agent:oneoff", payload)
}

func (d *desktopApp) startCommand(name, args string, scope []string, reviewedMsg string, lane runLane, handler func(*commandRun) error) string {
	d.mu.Lock()
	id := d.activeID
	if id == "" {
		d.mu.Unlock()
		return refuseNoSession
	}
	r := d.getRun(id)
	d.mu.Unlock()
	if r.agent == nil {
		if d.cfg == nil {
			return "agent 不可用（未配置，模拟模式）"
		}
		pr, err := d.prepareSession(context.Background(), id)
		if err != nil || pr.agent == nil {
			return "agent 尚未就绪，稍后重试"
		}
		r = pr
	}

	// Commands take the turn scaffolding but not the steer channel: each command
	// run is a self-contained prompt (a summary request, a review round), and
	// injecting a queued chat message into one would only confuse it. The run
	// still owns one, so nothing else changes.
	ctx, cancel, _, turnDone, ok := d.beginTurn(id, r)
	if !ok {
		return "当前会话有回合在运行，命令未执行"
	}
	_ = cancel // held by the run (r.turnCancel); Stop is the user-facing path

	if lane == laneOneOff {
		d.emitOneOff(id, map[string]any{"kind": name, "phase": "start"})
	}

	go func() {
		endReason := "error"
		defer func() {
			d.endTurn(id, endReason)
			close(turnDone)
		}()
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "执行", Detail: "/" + name})

		events := make(chan agent.AgentEvent, 64)
		startedAt := time.Now()
		go func() {
			defer close(events)
			if err := handler(&commandRun{
				desk: d, run: r, id: id, ctx: ctx, args: args, scope: scope,
				reviewedMsg: reviewedMsg, ech: events,
			}); err != nil {
				events <- agent.AgentEvent{Type: agent.AgentEventError, Result: &agent.RunResult{Error: err}}
			}
		}()

		// Sub-runs report their own completion; the command as a whole completes
		// once, at the end (see the doc comment). A failure anywhere keeps the
		// turn in the error state the handler/loop already reported.
		var lastComplete *agent.AgentEvent
		failed := false
		findings := 0
		for ev := range events {
			switch ev.Type {
			case agent.AgentEventTurnComplete:
				lastComplete = &ev // forwarded once the whole command is done
				continue
			case agent.AgentEventError:
				failed = true
				if ev.Result != nil && (ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled) {
					endReason = "interrupted"
				}
			case agent.AgentEventToolCallStart:
				// The one number the conversation's anchor shows about a review. Counted
				// here rather than read back from the record: this is the live signal, and
				// the file would have to be parsed while it is still being written.
				if ev.ToolName == tools.ToolNameReportFinding {
					findings++
				}
			}
			d.handleEventIn(id, ev, lane)
		}
		if !failed {
			endReason = "complete"
			if lastComplete != nil {
				d.handleEventIn(id, *lastComplete, lane)
			} else {
				// A command that calls no LLM (/sh) has no run to report, but the
				// transcript still has to stop showing "running" and the footer
				// still wants the wall-clock time.
				d.handleEventIn(id, agent.AgentEvent{
					Type:   agent.AgentEventTurnComplete,
					Result: &agent.RunResult{Duration: time.Since(startedAt)},
				}, lane)
			}
		}
		if lane == laneOneOff {
			// The run's outcome, for the anchor and the panel. `interrupted` is not an
			// error: a stop the user asked for ends the run the same way it ends a turn.
			payload := map[string]any{"kind": name, "phase": "end", "findings": findings}
			if lastComplete != nil && lastComplete.Result != nil {
				payload["durationMs"] = lastComplete.Result.Duration.Milliseconds()
				payload["iterations"] = lastComplete.Result.IterationsUsed
			}
			if endReason != "complete" {
				payload["phase"] = "error"
				switch {
				case endReason == "interrupted":
					payload["interrupted"] = true
					payload["error"] = "已停止"
				case lastComplete != nil && lastComplete.Result != nil && lastComplete.Result.Error != nil:
					payload["error"] = lastComplete.Result.Error.Error()
				default:
					payload["error"] = "运行失败，见日志"
				}
			}
			d.emitOneOff(id, payload)
			// …and the same fact as a notification. A side-channel run has nothing on screen to
			// watch, so when the window is not focused this is the ONLY signal that it is over —
			// and it names the run and its outcome, which is why it is not notifyTurnDone.
			d.notify.notifyOneOffDone(d.sessionTitle(id), name, findings, endReason)
		}
	}()
	return ""
}

// ---------------------------------------------------------------------------
// /compact
// ---------------------------------------------------------------------------

// runCompactCommand compacts the conversation on demand: the same shape as the
// TUI's /compact — a normal conversation turn (the model has to see the whole
// history) with every tool hidden, then the shared CompleteCompact hand-off that
// the auto-compaction path also uses.
func runCompactCommand(c *commandRun) error {
	r := c.run
	history := c.desk.runHistory(c.id)
	if len(history) == 0 {
		return errors.New("对话历史为空，无需压缩")
	}
	// One prompt for both the summarising turn and the hand-off: the session
	// cannot move under way, and CompleteCompact must record what was sent.
	systemPrompt := c.desk.systemPromptFor(c.id)

	stream := r.agent.RunConversationStream(c.ctx, history, cmds.BuildCompactInstruction(),
		systemPrompt, llm.ChatOptions{MaxTokens: c.desk.cfg.MaxTokens}, agent.WithNoTools())

	var summary strings.Builder
	for ev := range stream {
		if ev.Type == agent.AgentEventTextDelta {
			summary.WriteString(ev.TextDelta)
		}
		if ev.Type == agent.AgentEventError {
			c.ech <- ev
			return nil // the loop already reported why
		}
		c.ech <- ev
	}
	if strings.TrimSpace(summary.String()) == "" {
		return errors.New("压缩未产生摘要")
	}

	newHistory, err := r.agent.CompleteCompact(r.sm, systemPrompt, summary.String())
	if err != nil {
		return err
	}
	c.desk.setRunHistory(c.id, newHistory)
	// Hand the move to the post-turn machinery: the compacted child session is
	// the conversation from here on, and followCompaction re-homes the run (and
	// the frontend) once the command's turn ends.
	if cur := r.sm.Current(); cur != nil {
		d := c.desk
		d.mu.Lock()
		r.compaction = &compactionRecord{SessionID: cur.ID, OldMsgCount: len(history)}
		d.mu.Unlock()
	}

	// The notice deliberately carries no summary: the summary IS this command's
	// streamed output, already in the transcript above it.
	if c.desk.app != nil {
		c.desk.app.Event.Emit("agent:compact_done", map[string]any{"sessionId": c.id, "oldCount": len(history)})
	}
	return nil
}

// ---------------------------------------------------------------------------
// /review [rounds]
// ---------------------------------------------------------------------------

// runReviewCommand runs the adversarial review the other frontends run: the
// shared orchestrator resolves rounds and per-round providers, and each round
// gets a fresh fork (so a round cannot see or pollute the previous one) whose
// one-off stream is forwarded into the transcript.
func runReviewCommand(c *commandRun) error {
	a := c.run.agent
	cfg := c.desk.cfg

	reviewProvider := a.Provider()
	if rp := a.ReviewProvider(); rp != nil {
		reviewProvider = rp
	}
	ropts := cmds.ResolveReviewOptions(cfg)
	// A scoped run (ReviewChanges) reviews exactly the files the turn touched; the plain
	// /review command leaves this empty and reviews the whole working tree.
	ropts.Scope = c.scope
	thinking, effort := cmds.ResolveReviewThinking(ropts, a.Config.Resolved.Thinking, a.Config.Resolved.ThinkingEffort)
	opts := llm.ChatOptions{MaxTokens: config.DefaultMaxTokens, Thinking: thinking, ThinkingEffort: effort}

	// The report directory is anchored at the session's working directory — the
	// root the round's Bash/WriteFile tools resolve against.
	orch, err := cmds.NewReviewOrchestratorFromCommand("/review "+c.args, ropts,
		func(rounds int) ([]llm.Provider, error) {
			if rounds == 1 {
				return []llm.Provider{reviewProvider}, nil
			}
			return a.ResolveAdversarialRoundModels(cfg, reviewProvider, rounds)
		}, c.desk.sessionWorkDir(c.id))
	if err != nil {
		return err
	}

	return orch.Run(func(spec cmds.RoundSpec) error {
		forked := a.Fork(agent.ForkConfig{
			Provider:      spec.Provider,
			MaxIterations: ropts.MaxIterations,
			AllowedTools:  ropts.AllowedTools,
			ForReview:     true,
			Logger:        a.Logger(),
		})
		defer forked.Close()

		stream := forked.Agent().RunOneOffStream(c.ctx, spec.Provider, c.desk.systemPromptFor(c.id), spec.Prompt, opts,
			agent.WithOneOffMeta(agent.OneOffMetaForReview(spec.Kind, c.id, spec.OutPath,
				agent.ReviewOrigin{ReviewedMsg: c.reviewedMsg, Paths: c.scope})))
		for ev := range stream {
			c.ech <- ev
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// /commit
// ---------------------------------------------------------------------------

// runCommitCommand reuses the agent's one-off commit runner: it prompts the
// model to inspect the diff and commit it, hiding every tool except Bash. The
// run is one-off — it never appends to the session — so the conversation is left
// exactly as it was, with only the transcript showing what happened.
func runCommitCommand(c *commandRun) error {
	stream := c.run.agent.RunCommitOneOff(c.ctx, c.desk.systemPromptFor(c.id), c.id, c.desk.cfg.MaxTokens, "")
	for ev := range stream {
		c.ech <- ev
	}
	return nil
}

// ---------------------------------------------------------------------------
// /sh
// ---------------------------------------------------------------------------

// runShellCommand runs a shell command in the session's working directory and
// echoes the result — no LLM involved.
//
// The output is rendered as a TOOL CARD, the same shape the agent's own Bash
// calls take: a local command is then visually distinct from something the model
// said, and it inherits the card's affordances (exit status, duration,
// collapsible full output). That is also why the card body does not repeat the
// command (unlike shutil.FormatShellResult, which the chat frontends use where
// the reply has to be self-contained) — it is already the card's title.
//
// The result is deliberately NOT appended to the conversation: /sh is a local
// escape hatch, and the model should not start reasoning about a command it
// never ran.
func runShellCommand(c *commandRun) error {
	command := strings.TrimSpace(c.args)
	if command == "" {
		// A usage line, not an error: the turn ends normally with the hint in
		// the transcript, exactly like the TUI's /sh does.
		c.ech <- agent.AgentEvent{
			Type:      agent.AgentEventTextDelta,
			TextDelta: "用法：/sh <command> — 在会话工作目录执行 shell 命令并回显输出（不经过模型，目录切换不持久）",
		}
		return nil
	}

	// The Bash arg shape, so tools.ToolArgsSummary renders the same title the
	// agent's own Bash calls get.
	args, _ := json.Marshal(map[string]string{"command": command})
	c.ech <- agent.AgentEvent{
		Type:           agent.AgentEventToolCallStart,
		ToolName:       tools.ToolNameBash,
		ToolArgs:       string(args),
		ToolAutoExpand: true, // the user asked for this output: show it, don't fold it
	}

	// Bounded by the shared timeout AND by the turn's context, so the stop button
	// kills a runaway command just like it stops a turn.
	ctx, cancel := context.WithTimeout(c.ctx, shutil.DefaultShellTimeout)
	defer cancel()
	start := time.Now()
	out, code, _ := shutil.Shell(ctx, c.desk.sessionWorkDir(c.id), command)

	note := ""
	switch {
	case errors.Is(c.ctx.Err(), context.Canceled):
		note = "⏹ 已终止"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		note = "⏱️ 超时，进程已终止"
	case code != 0:
		note = fmt.Sprintf("(exit %d)", code)
	}
	c.ech <- agent.AgentEvent{
		Type:         agent.AgentEventToolResult,
		ToolName:     tools.ToolNameBash,
		ToolResult:   shellEcho(out, note),
		ToolIsError:  note != "",
		ToolDuration: time.Since(start),
	}
	return nil
}

// shellEcho is the tool card body for /sh: the command's combined output plus the
// same trailers the chat frontends print (exit code, timeout, cancellation), or
// an explicit "no output" so the card never looks empty for a silent command.
func shellEcho(out, note string) string {
	switch {
	case note != "":
		if strings.TrimSpace(out) == "" {
			return note
		}
		return out + "\n" + note
	case strings.TrimSpace(out) == "":
		return "(no output)"
	}
	return out
}
