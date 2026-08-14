package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// --- /commit ---

// handleCommitCommand runs a one-off LLM turn that drafts a commit message
// and commits the current repo changes via the Bash tool (no direct exec
// here — the model drives git itself). It runs in a clean context without
// conversation history, using the dedicated commit provider when configured
// (commit_provider), otherwise the thread's main provider.
//
// Thinking is disabled for /commit: the commit task is simple and avoiding
// thinking saves tokens/latency (same as TUI/ACP).
//
// The one-off run holds the thread's cached-agent lock for its duration
// (same as /research), so a concurrent message on this thread waits for the
// commit to finish before starting its own turn.
func (m *Manager) handleCommitCommand(ctx context.Context, threadID string) (string, error) {
	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Global one-off concurrency cap: reject with a hint instead of silently
	// queueing behind the cached-agent lock.
	if !m.oneoffSem.TryAcquire() {
		return "", fmt.Errorf("已有 %d 个长任务（/commit、/review）在运行，请稍后再试", m.oneoffSem.Len())
	}
	defer m.oneoffSem.Release()

	// Register so /stop and /new can cancel this run mid-flight.
	ctx, done := m.registerOneoff(threadID, ctx)
	defer done()

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return "", fmt.Errorf("acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)

	workDir := m.effectiveThreadWorkDir(ca, threadID)
	// Bind the thread's working directory so the run's Bash tool resolves
	// relative paths against it (same as runAgentTurn).
	ctx = wdctx.WithDir(ctx, workDir)

	aiAgent := ca.agent
	sessionID := m.threadSessionID(threadID)
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, workDir, sessionID)

	eventCh := aiAgent.RunCommitOneOff(ctx, systemPrompt, sessionID, config.DefaultMaxTokens, "")

	text, err, incomplete := m.drainOneOffEvents(ctx, eventCh, aiAgent)
	if err != nil {
		return text, err
	}
	if incomplete {
		text += "\n\n⚠️ 提交过程未完整完成（部分输出可能缺失）。请检查 git 状态确认提交是否成功。"
	}
	return text, nil
}

// --- /review ---

// handleReviewCommand runs a code review of the current repo changes in an
// isolated fork with limited tools (Bash, ReadFile, WriteFile, Glob, Grep).
// The forked agent does NOT inherit conversation history — it gets a clean
// prompt to review git diff output.
//
// "/review N" (N ≥ 2) runs N sequential adversarial rounds in isolated forks:
// Reviewer → Challenger → Judge (role cycle, final round fixed as Judge).
// Without a round count, /review stays the single-round code review.
// See docs/2026-07-30-adversarial-review-design.md.
//
// The shared cmds.ReviewOrchestrator owns all orchestration state (round
// resolution, provider assignment with fail-fast, report directory, round
// bookkeeping) — this handler only drives the synchronous round loop, the
// same pattern as ACP (agent/acp/commands.go handleACPReview).
func (m *Manager) handleReviewCommand(ctx context.Context, threadID, args string) (string, error) {
	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Global one-off concurrency cap: reject with a hint instead of silently
	// queueing behind the cached-agent lock.
	if !m.oneoffSem.TryAcquire() {
		return "", fmt.Errorf("已有 %d 个长任务（/commit、/review）在运行，请稍后再试", m.oneoffSem.Len())
	}
	defer m.oneoffSem.Release()

	// Register so /stop and /new can cancel this run mid-flight.
	ctx, done := m.registerOneoff(threadID, ctx)
	defer done()

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return "", fmt.Errorf("acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)

	// Ensure the thread has a session before running: the first message on a
	// thread may be /review itself, and runAgentTurn's prepareThreadSession
	// never runs for slash commands. Without it the artifact registration in
	// appendReviewArtifact would find no session.
	resolved := m.getProviderForThread(threadID)
	if sm, _ := m.prepareThreadSession(threadID, resolved); sm != nil {
		ca.agent.SetSessionManager(sm)
	}

	workDir := m.effectiveThreadWorkDir(ca, threadID)
	// Bind the thread's working directory so each round's Bash/WriteFile
	// tools resolve relative paths against it — this MUST match the baseDir
	// passed to NewReviewOrchestratorFromCommand below, otherwise the
	// orchestrator's on-disk verification and the LLM's WriteFile would
	// disagree about where reports land.
	ctx = wdctx.WithDir(ctx, workDir)

	aiAgent := ca.agent

	// Resolve review provider and model from config (or fall back to main).
	reviewProvider := aiAgent.Provider()
	if rp := aiAgent.ReviewProvider(); rp != nil {
		reviewProvider = rp
	}

	// Parameter defaults/overrides come from the shared resolver (same as
	// the TUI/ACP side); only the provider lookup is agent-specific.
	ropts := cmds.ResolveReviewOptions(m.cfg)
	thinking, effort := cmds.ResolveReviewThinking(ropts,
		aiAgent.Config.Resolved.Thinking, aiAgent.Config.Resolved.ThinkingEffort)

	sessionID := m.threadSessionID(threadID)
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, workDir, sessionID)
	opts := llm.ChatOptions{
		MaxTokens:      config.DefaultMaxTokens,
		Thinking:       thinking,
		ThinkingEffort: effort,
	}

	// The shared orchestrator resolves rounds, assigns per-round providers
	// (fail-fast on unresolvable adversarial models) and creates the report
	// directory. Single-round reviews flow through the same path — this
	// frontend never branches on round count. The report dir is anchored at
	// the thread's working directory (the base the round's Bash/WriteFile
	// tools resolve against) — NOT the process CWD.
	orch, err := cmds.NewReviewOrchestratorFromCommand("/review "+args, ropts,
		func(rounds int) ([]llm.Provider, error) {
			if rounds == 1 {
				return []llm.Provider{reviewProvider}, nil
			}
			return aiAgent.ResolveAdversarialRoundModels(m.cfg, reviewProvider, rounds)
		}, workDir)
	if err != nil {
		return "", err
	}

	// Proactive progress so the user knows the review is running (a
	// multi-round review can take minutes).
	if orch.IsMultiRound() {
		m.sendToThread(ctx, threadID,
			fmt.Sprintf("🔍 开始代码审查 — **%d 轮对抗式审查**（Reviewer → Challenger → Judge）...", orch.TotalRounds()), "")
	} else {
		m.sendToThread(ctx, threadID, "🔍 开始代码审查...", "")
	}

	// Streaming callback for channel implementations that show real-time
	// tool-call progress (e.g. Discord status embeds). The callback rides on
	// ctx from the message/interaction handler — drainOneOffEvents forwards it.
	//
	// Multi-round delivery model (avoids duplicating text the user already
	// saw): each round's full text is pushed via sendToThread as it completes,
	// and the final reply only carries the status + report directory. Single-
	// round has no intermediate push — the LLM text IS the reply, and the
	// reply must carry it in full: the text slash-command path and the Discord
	// interaction path both deliver reply.Content verbatim, with no separate
	// streaming/embed on non-Discord channels (WeChat etc.), so trimming it
	// would silently drop the review output there.

	var out strings.Builder // single-round text only
	incompleteRounds := 0
	var lastOutPath string // last round's orchestrator-owned report path (multi-round only)
	runErr := orch.Run(func(spec cmds.RoundSpec) error {
		if spec.OutPath != "" {
			lastOutPath = spec.OutPath
		}
		// Per-round banner + report path hint (multi-round only).
		banner := fmt.Sprintf("── 第 %d 轮（%d/%d）— %s ──", spec.Round, spec.Round, orch.TotalRounds(), cmds.RoleName(spec.Role))
		if orch.IsMultiRound() && spec.OutPath != "" {
			m.sendToThread(ctx, threadID, banner+"\n报告输出: "+spec.OutPath, "")
		}

		forked := aiAgent.Fork(agent.ForkConfig{
			Provider:      spec.Provider,
			MaxIterations: ropts.MaxIterations,
			AllowedTools:  ropts.AllowedTools,
			Logger:        aiAgent.Logger(),
		})
		defer forked.Close()

		eventCh := forked.Agent().RunOneOffStream(ctx, spec.Provider, systemPrompt, spec.Prompt, opts,
			agent.WithOneOffMeta(&agent.OneOffMeta{Kind: spec.Kind, SessionID: sessionID}))

		text, err, incomplete := m.drainOneOffEvents(ctx, eventCh, forked.Agent())
		if err != nil {
			return err
		}
		if incomplete {
			incompleteRounds++
		}
		if text == "" {
			return nil
		}
		if orch.IsMultiRound() {
			// Push the round's full text as it completes (real-time delivery;
			// also means a later failure never loses rounds the user has
			// already seen — B7). The final reply only summarises.
			m.sendToThread(ctx, threadID, banner+"\n"+text, "")
		} else {
			// Single-round: the LLM text IS the review output.
			out.WriteString(text + "\n\n")
		}
		return nil
	})

	if runErr != nil {
		// Multi-round: completed rounds were already pushed in full; the
		// error reply adds status (current round + report dir), not text.
		//
		// The error DETAIL is deliberately NOT embedded here — the callers
		// (handleSlashCommand / Discord interaction path) append "❌ <err>"
		// once. Embedding it would duplicate the error in the reply.
		if orch.IsMultiRound() {
			dir := orch.ReportDir()
			// Next() increments current before running a round, so on
			// failure CurrentRound() is the failed round and done =
			// current-1 is the number of completed rounds (0 when the
			// first round fails; guarded below).
			done := orch.CurrentRound() - 1
			if done < 0 {
				done = 0
			}
			return fmt.Sprintf("审查在第 %d 轮失败（已完成 %d 轮）\n报告目录: `%s`",
				orch.CurrentRound(), done, dir), runErr
		}
		return strings.TrimSpace(out.String()), runErr
	}

	// Success line. Multi-round: status reflects any incomplete rounds and
	// points at the report directory (text already pushed per round).
	if orch.IsMultiRound() {
		// Register the final round's report as a session artifact. Incomplete
		// rounds (drainOneOffEvents' incomplete flag) still splice into
		// ca.history so the user can ask about what DID complete, but skip
		// the disk registration — a truncated report isn't advertised as a
		// durable artifact.
		if lastOutPath != "" {
			m.appendReviewArtifact(threadID, ca, orch.TotalRounds(), lastOutPath, incompleteRounds == 0)
		}
		dir, _ := filepath.Rel(workDir, orch.ReportDir())
		if dir == "" || strings.HasPrefix(dir, "..") {
			dir = orch.ReportDir()
		}
		status := fmt.Sprintf("✅ 审查完成（%d 轮）", orch.TotalRounds())
		if incompleteRounds > 0 {
			status = fmt.Sprintf("⚠️ 审查完成（%d 轮，其中 %d 轮未完整完成）", orch.TotalRounds(), incompleteRounds)
		}
		return status + "。报告目录: `" + dir + "`", nil
	}

	// Single-round: append a short completion marker so an empty/plain reply
	// still signals the review happened. The orchestrator owns the
	// single-round report path too, so register it as a followable artifact.
	if lastOutPath != "" {
		m.appendReviewArtifact(threadID, ca, 1, lastOutPath, true)
	}
	result := strings.TrimSpace(out.String())
	return result + "\n\n✅ 审查完成（1 轮）", nil
}

// appendReviewArtifact registers the final review report as a session
// artifact. Best-effort: failures are logged, never surfaced as a review
// error. persist=false (incomplete rounds) skips the disk write but still
// splices into ca.history. Caller holds the cached agent's lock; the file
// must exist on disk.
func (m *Manager) appendReviewArtifact(threadID string, ca *cachedAgent, rounds int, reportPath string, persist bool) {
	if _, statErr := os.Stat(reportPath); statErr != nil {
		m.logger.Warn(context.Background(), "channel: review artifact: report missing on disk, not registered", "path", reportPath, "err", statErr)
		return
	}
	ref := session.ArtifactRef{
		Kind:  session.ArtifactKindReview,
		Title: fmt.Sprintf("代码审查（%d 轮）", rounds),
		Path:  reportPath,
	}

	if persist {
		sm := m.newSessionManager()
		if sm != nil {
			sess, err := sm.FindByThreadID(threadID)
			if err != nil || sess == nil {
				m.logger.Warn(context.Background(), "channel: review artifact: session not found", "thread", threadID, "err", err)
			} else if err := sm.AppendArtifactTo(sess.ID, ref); err != nil {
				m.logger.Warn(context.Background(), "channel: review artifact: append failed", "err", err)
			}
		}
	}

	m.spliceArtifactIntoCache(ca, ref)
}

// --- helpers for one-off LLM commands (/commit, /review) ---

// effectiveThreadWorkDir returns the working directory a one-off command
// should run in: the session's persisted WorkingDir wins (it survives
// restarts, e.g. after /cd + restart), falling back to the cached agent's
// workDir and finally ".".
func (m *Manager) effectiveThreadWorkDir(ca *cachedAgent, threadID string) string {
	if threadID != "" {
		sm := m.newSessionManager()
		if sm != nil {
			if sess, err := sm.FindByThreadID(threadID); err == nil && sess != nil && sess.WorkingDir != "" {
				return sess.WorkingDir
			}
		}
	}
	if ca != nil && ca.workDir != "" {
		return ca.workDir
	}
	return "."
}

// threadSessionID is shared with the ambient pipeline (ambient.go) — one-off
// transcripts (/commit, /review, ambient) all anchor under the session
// directory via this helper.

// drainOneOffEvents consumes an agent event stream for a one-off LLM run
// (/commit, /review round). Unlike runAgentTurn's drainEvents call, one-off
// runs have no thread activation: steer, AskUser waiting and attachment
// collection are all skipped (ta == nil). The channel's streaming callback
// (if the handler ctx carried one) is forwarded so Discord can show live
// tool-call progress embeds.
//
// The third return value (incomplete) reports whether the run ended
// abnormally — an error event occurred but drainEvents still returned nil
// because partial text was produced (its result normalization is tuned for
// regular conversation, where partial output beats a hard failure). One-off
// callers use it to mark the round/commit as incomplete instead of claiming
// success ("✅ 审查完成" when a round died midway is a lie).
func (m *Manager) drainOneOffEvents(ctx context.Context, ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent) (string, error, bool) {
	onTextDelta := streamingCallbackFromCtx(ctx)

	// Tee the event stream to spot error events that drainEvents swallows.
	// The goroutine only exits when ch closes (one-off runs have no
	// AskUser/Steer early-return paths), so the channel close establishes a
	// happens-before edge: by the time drainEvents returns, incomplete is final.
	tee := make(chan agent.AgentEvent, 8)
	var incomplete bool
	go func() {
		defer close(tee)
		for e := range ch {
			switch e.Type {
			case agent.AgentEventError:
				incomplete = true
			case agent.AgentEventTurnComplete:
				if e.Result != nil && e.Result.Error != nil {
					incomplete = true
				}
			}
			tee <- e
		}
	}()

	text, err := m.drainEvents(ctx, tee, aiAgent, nil, nil, onTextDelta, true)
	return text, err, incomplete
}
