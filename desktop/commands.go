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

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// CommandVO describes a slash command for the composer's "/" palette.
type CommandVO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputHint   string `json:"inputHint,omitempty"`
}

// commandRun is everything a handler needs: the session's run, the turn's
// context, the raw arguments, and the channel its events are pumped into.
type commandRun struct {
	desk *desktopApp
	run  *sessionRun
	id   string
	ctx  context.Context
	args string
	ech  chan<- agent.AgentEvent
}

// desktopCommandHandlers maps a command name to its desktop implementation.
// Only names in this map are offered to the frontend (see ListCommands), so a
// registry entry without an implementation is simply not advertised.
var desktopCommandHandlers = map[string]func(*commandRun) error{
	"compact": runCompactCommand,
	"review":  runReviewCommand,
	"commit":  runCommitCommand,
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
	return s.desk.startCommand(def.Name, args, handler)
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
func (d *desktopApp) startCommand(name, args string, handler func(*commandRun) error) string {
	d.mu.Lock()
	id := d.activeID
	if id == "" {
		d.mu.Unlock()
		return "没有活跃会话"
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

	go func() {
		endReason := "error"
		defer func() {
			d.endTurn(id, endReason)
			close(turnDone)
		}()
		d.setSessionState(id, AgentState{Status: StatusBusy, Label: "执行", Detail: "/" + name})

		events := make(chan agent.AgentEvent, 64)
		go func() {
			defer close(events)
			if err := handler(&commandRun{desk: d, run: r, id: id, ctx: ctx, args: args, ech: events}); err != nil {
				events <- agent.AgentEvent{Type: agent.AgentEventError, Result: &agent.RunResult{Error: err}}
			}
		}()

		var lastComplete *agent.AgentEvent
		for ev := range events {
			switch ev.Type {
			case agent.AgentEventTurnComplete:
				lastComplete = &ev // forwarded once the whole command is done
				continue
			case agent.AgentEventError:
				if ev.Result != nil && (ev.Result.ExitReason == agent.ExitReasonInterrupted || ev.Result.ExitReason == agent.ExitReasonCancelled) {
					endReason = "interrupted"
				}
			}
			d.handleEvent(id, ev)
		}
		if lastComplete != nil && endReason != "error" {
			endReason = "complete"
			d.handleEvent(id, *lastComplete)
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

	stream := r.agent.RunConversationStream(c.ctx, history, cmds.BuildCompactInstruction(),
		c.desk.systemPrompt, llm.ChatOptions{MaxTokens: c.desk.cfg.MaxTokens}, agent.WithNoTools())

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

	newHistory, err := r.agent.CompleteCompact(r.sm, c.desk.systemPrompt, summary.String())
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
			Logger:        a.Logger(),
		})
		defer forked.Close()

		stream := forked.Agent().RunOneOffStream(c.ctx, spec.Provider, c.desk.systemPrompt, spec.Prompt, opts,
			agent.WithOneOffMeta(&agent.OneOffMeta{Kind: spec.Kind, SessionID: c.id}))
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
	stream := c.run.agent.RunCommitOneOff(c.ctx, c.desk.systemPrompt, c.id, c.desk.cfg.MaxTokens, "")
	for ev := range stream {
		c.ech <- ev
	}
	return nil
}
