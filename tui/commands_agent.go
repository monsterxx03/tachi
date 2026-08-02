package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/llm"
)

// ------- Agent-driven commands (trigger LLM conversations) -------

func (m *Model) sendMessage(text string) tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: text})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Expand @path references: inject file/directory contents into the
	// message sent to the LLM, but keep the TUI display unexpanded.
	// Images are extracted as structured ContentParts for multi-modal input.
	expanded := m.ExpandAtReferences(text)

	ctx := m.startTurn()

	// Set up steer channel so pending input can be injected at tool-call boundaries.
	m.steerCh = make(chan agent.SteerInput)
	var ropts []agent.RunOption
	ropts = append(ropts, agent.WithSteerChannel(m.steerCh))
	if len(expanded.Images) > 0 {
		ropts = append(ropts, agent.WithPendingImages(expanded.Images))
	}

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, expanded.Text, m.effectiveSystemPrompt(), m.chatOpts, ropts...)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// currentSessionID returns the active tachi session ID ("" if none), used to
// anchor one-off transcripts (/commit, /review) under the session directory.
func (m *Model) currentSessionID() string {
	if sm := m.agent.SessionManager(); sm != nil {
		if cur := sm.Current(); cur != nil {
			return cur.ID
		}
	}
	return ""
}

// sendCommitCommand 使用干净的对话上下文（不继承历史）把任务说明发给 LLM，
// 由模型用 Bash 工具自行执行 git 并提交（不在此处 exec 任何命令）。
// 如果配置了 commit_provider，使用专用 provider；否则回退到主 provider。
func (m *Model) sendCommitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/commit"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Save conversation history so we can restore it after the one-off
	// commit run completes (RunOneOffStream overwrites m.history).
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)

	ctx := m.startTurn()

	// /commit only needs Bash — the tool view hides everything else for the
	// duration of this run without touching the registry. Keep the TUI's
	// configured MaxTokens; the shared runner handles provider + thinking.
	m.eventCh = m.agent.RunCommitOneOff(ctx, m.systemPrompt, m.currentSessionID(), m.chatOpts.MaxTokens, "")

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// sendReviewCommand uses Fork() to create an isolated child agent with limited
// tools (Bash, ReadFile, Glob, Grep), then runs a code review of the current
// repo changes. The forked agent does NOT inherit conversation history or
// session context — it gets a clean prompt to review git diff output.
//
// With "/review N" (N ≥ 2), the command runs N sequential rounds in isolated
// forks: Reviewer → Challenger → Judge (role cycle, final round fixed as
// Judge). Without a round count, /review stays the single-round code review.
// See docs/2026-07-30-adversarial-review-design.md.
//
// The shared cmds.ReviewOrchestrator owns all orchestration state (round
// resolution, provider assignment with fail-fast, report directory, round
// bookkeeping); this frontend only constructs it and drives Next()/Complete()
// via startReviewRound / the TurnComplete branch.
func (m *Model) sendReviewCommand() tea.Cmd {
	display := m.subcommandInput
	if display == "" {
		display = "/review"
	}
	m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false
	// Defensive: clear any stale badge from an abnormal exit before the new
	// run starts (startReviewRound re-sets it for multi-round runs).
	m.statusbar.ClearReviewBadge()

	// Save conversation history so we can restore it after the one-off
	// review run completes (the forked agent doesn't touch m.history, but
	// setting savedHistory marks this as a one-off for TurnComplete handling).
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)

	// Round resolution, provider assignment (fail-fast on unresolvable
	// adversarial models) and report-dir creation all live in the shared
	// orchestrator; any failure aborts before round 1.
	orch, err := cmds.NewReviewOrchestratorFromCommand(m.subcommandInput,
		cmds.ResolveReviewOptions(m.cfg), m.resolveReviewProviders, "")
	if err != nil {
		m.savedHistory = nil
		m.chatview.AddMessage(chatMessage{Role: "error", Content: err.Error()})
		m.setState(stateIdle)
		// Defensive: normally already nil at this point, but never leave a
		// stale cancel/event channel behind on a re-entrant path.
		m.cancelFunc = nil
		m.eventCh = nil
		return nil
	}

	m.reviewOrch = orch
	m.isReviewing = true
	return m.startReviewRound() // starts round 1 (contains statusbar.Tick + nextEvent)
}

// resolveReviewProviders resolves per-round providers: single-round reviews
// use review.provider (or the main provider); multi-round goes through the
// adversarial model assignment (models modulo-cycled, judge fixed on the
// final round) with the fail-fast check — any configured but unresolvable
// name aborts before round 1 (silently falling back to the main model would
// make "multi-model adversarial review" a lie).
func (m *Model) resolveReviewProviders(rounds int) ([]llm.Provider, error) {
	provider := m.agent.Provider()
	if rp := m.agent.ReviewProvider(); rp != nil {
		provider = rp
	}
	if rounds == 1 {
		return []llm.Provider{provider}, nil
	}
	return m.agent.ResolveAdversarialRoundModels(m.cfg, provider, rounds)
}

// startReviewRound asks the shared orchestrator for the next round's spec,
// forks a fresh isolated agent for it, and starts its one-off stream. Every
// round gets a new ctx and a streamGen bump so late events from the previous
// round expire.
func (m *Model) startReviewRound() tea.Cmd {
	spec, ok := m.reviewOrch.Next()
	if !ok {
		return nil // defensive — Complete returns done on the final round
	}
	orch := m.reviewOrch

	forked := m.agent.Fork(agent.ForkConfig{
		Provider:      spec.Provider,
		MaxIterations: orch.Options().MaxIterations,
		AllowedTools:  orch.Options().AllowedTools,
		Logger:        m.agent.Logger(),
	})
	m.forkedAgent = forked

	// Reset per-round streaming state (the previous round's bubble was sealed
	// by FinishStreaming in the TurnComplete branch; round 1 was already
	// reset in sendReviewCommand — idempotent).
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()

	if orch.IsMultiRound() {
		roleName := cmds.RoleName(spec.Role)
		m.chatview.AppendTextDelta(fmt.Sprintf(
			"\n══════════ Round %d/%d — %s (%s) ══════════\n",
			spec.Round, orch.TotalRounds(), roleName, spec.Provider.Model()))
		// Statusbar indicator: current role + round position (n/m).
		m.statusbar.SetReviewBadge(fmt.Sprintf("⚔️ %s %d/%d",
			roleName, spec.Round, orch.TotalRounds()))
	}

	// Apply thinking config: review.thinking / review.thinking_level pin
	// their dimension when configured; unconfigured dimensions follow the
	// current session's thinking (which itself falls back to the
	// provider/model default — runAgentLoop applies that when we pass nil/
	// empty through to the fork).
	thinking, effort := cmds.ResolveReviewThinking(orch.Options(),
		m.agent.Config.Thinking, m.agent.Config.ThinkingEffort)
	reviewOpts := m.chatOpts
	reviewOpts.Thinking = thinking
	reviewOpts.ThinkingEffort = effort

	ctx := m.startTurn()
	m.eventCh = forked.Agent().RunOneOffStream(ctx, spec.Provider,
		m.systemPrompt, spec.Prompt, reviewOpts,
		agent.WithOneOffMeta(&agent.OneOffMeta{Kind: spec.Kind, SessionID: m.currentSessionID()}))
	return tea.Batch(m.statusbar.Tick(), m.nextEvent())
}

// showReviewReportHint renders the per-round on-disk verification hint (the
// verification itself is done by the shared orchestrator's Complete).
// Single-round reviews have no orchestrator-owned path and show nothing.
func (m *Model) showReviewReportHint(report cmds.RoundReport) {
	if report.Path == "" {
		return
	}
	if report.Saved {
		m.chatview.AddMessage(chatMessage{Role: "oneoff_note", Content: "💾 报告已保存: " + report.Path})
	} else {
		m.chatview.AddMessage(chatMessage{Role: "error",
			Content: fmt.Sprintf("⚠️ 第 %d 轮未成功保存报告，后续轮次将跳过它", report.Round)})
	}
}

// sendInitCommand sends the init prompt to LLM to generate .tachi.md
func (m *Model) sendInitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/init"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx := m.startTurn()

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, cmds.InitPromptTemplate, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// handleCompactCommand handles the /compact slash command.
// It appends a compact instruction to the current conversation so the LLM
// can summarize using its existing context window (no history re-embedding).
// After the turn completes, a new session is created with the summary.
func (m *Model) handleCompactCommand() tea.Cmd {
	// 1. Pre-checks
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "没有活跃的 session 可以压缩",
		})
		return nil
	}
	if len(m.history) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "对话历史为空，无需压缩",
		})
		return nil
	}

	// 2. Show user intent and set state
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/compact"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// 3. Save state for rollback
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)
	m.isCompacting = true

	// 4. Build compact instruction (no history — LLM sees history as context)
	instruction := cmds.BuildCompactInstruction()

	ctx := m.startTurn()

	// Use RunConversationStream so the LLM sees the current session as
	// structured history (role alternation, tool calls, etc.).
	// WithNoTools hides every tool for this run — the prompt also says
	// "不要调用任何工具" as a double safeguard.
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, instruction, m.systemPrompt, m.chatOpts,
		agent.WithNoTools())

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// formatCompactSummary formats the compact result for display in the chatview.
func formatCompactSummary(summary string, oldMsgCount int) string {
	var sb strings.Builder
	sb.WriteString("🔍 **对话已压缩**\n\n")
	fmt.Fprintf(&sb, "旧消息数: %d 条\n", oldMsgCount)
	sb.WriteString("\n---\n\n")
	sb.WriteString(summary)
	sb.WriteString("\n\n---\n")
	return sb.String()
}

// rollbackCompact restores the pre-compact history and displays an error in
// the chatview. Used when the compact LLM call fails or FinalizeCompact
// returns an error. The tool set needs no rollback — /compact runs under a
// per-run tool view that expires with the run (see agent/toolview.go).
func (m *Model) rollbackCompact(errMsg string) {
	m.history = m.savedHistory
	m.savedHistory = nil
	m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
	m.chatview.FinishStreaming()
	m.setState(stateIdle)
	m.cancelFunc = nil
	m.eventCh = nil
}

// abortCompactForSwitch cleans up state after a compact-for-model-switch
// operation fails. Unlike rollbackCompact, it does NOT restore savedHistory
// (the current history is kept as-is since the switch was never applied) and
// it clears the pendingSwitchProvider so the model stays on the original provider.
func (m *Model) abortCompactForSwitch(errMsg string) {
	m.compactForSwitch = false
	m.pendingSwitchProvider = nil
	m.savedHistory = nil

	m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
	m.chatview.FinishStreaming()
	m.syncSessionInfo()
	m.setState(stateIdle)
	m.pendingQueue = nil
	m.chatview.RemovePendingItems()
	m.statusbar.SetPendingCount(0)
	m.cancelFunc = nil
	m.eventCh = nil
}

// handleSkillCommand handles the /skill slash command.
// /skill              → list all available skills
// /skill <name>       → activate a specific skill
// /skill reload       → re-scan skill directories
