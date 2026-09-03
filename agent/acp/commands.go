package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// ACPSlashCommand represents a slash command available in ACP mode.
// Unlike TUI commands (which mutate the TUI Model), ACP commands work
// with ACPSession — they compute results and stream them back as
// SessionUpdate notifications.
type ACPSlashCommand struct {
	Name        string
	Description string
	InputHint   string // optional: hint for argument input (e.g. "query to search for")
	Handler     func(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error)
}

// ---------------------------------------------------------------------------
// Command handler registry
// ---------------------------------------------------------------------------

var acpCommandHandlers = map[string]func(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error){
	"commit":     handleACPCommit,
	"review":     handleACPReview,
	"init":       handleACPInit,
	"compact":    handleACPCompact,
	"usage":      handleACPUsage,
	"mcp":        handleACPMCP,
	"skill":      handleACPSkill,
	"transcript": handleACPTranscript,
	"research":   handleACPResearch,
}

// buildACPAvailableCommands builds the ACP AvailableCommand list from the
// shared cmds.Registry filtered for ACP mode, plus dynamic skill commands.
func buildACPAvailableCommands(aiAgent *agent.AIAgent) []acp.AvailableCommand {
	result := make([]acp.AvailableCommand, 0, len(acpCommandHandlers)+16)

	// Static commands from shared registry
	for _, def := range cmds.ForMode(cmds.ModeACP) {
		if _, ok := acpCommandHandlers[def.Name]; !ok {
			continue
		}
		ac := acp.AvailableCommand{
			Name:        def.Name,
			Description: def.Description,
		}
		if def.InputHint != "" {
			ac.Input = &acp.AvailableCommandInput{
				Unstructured: &acp.UnstructuredCommandInput{Hint: def.InputHint},
			}
		}
		result = append(result, ac)
	}

	// Dynamic skill commands
	if aiAgent != nil {
		if store := aiAgent.SkillStore(); store != nil {
			for _, meta := range store.List() {
				if !meta.Enabled {
					continue // disabled skills are not activatable
				}
				ac := acp.AvailableCommand{
					Name:        meta.Name,
					Description: meta.Description,
					Input: &acp.AvailableCommandInput{
						Unstructured: &acp.UnstructuredCommandInput{
							Hint: "optional instruction for this skill",
						},
					},
				}
				result = append(result, ac)
			}
		}
	}

	return result
}

// parseSlashCommand checks if the message is a slash command.
// Returns the matching command and the argument portion (text after the
// command name). Returns (nil, "") for normal messages.
func parseSlashCommand(msg string, aiAgent *agent.AIAgent) (*ACPSlashCommand, string) {
	msg = strings.TrimSpace(msg)
	if msg == "" || msg[0] != '/' {
		return nil, ""
	}

	// Split into command name and remainder
	parts := strings.SplitN(msg, " ", 2)
	cmdName := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	// Exact match first
	if cmd := findACPCommand(cmdName); cmd != nil {
		return cmd, args
	}

	// Prefix match (e.g. "mcp" matches "mcp list")
	if cmd := findACPCommandByPrefix(cmdName); cmd != nil {
		return cmd, args
	}

	// Check if it's a skill name
	if aiAgent != nil {
		if store := aiAgent.SkillStore(); store != nil {
			if _, ok := store.ResolveCommand(cmdName); ok {
				return &ACPSlashCommand{
					Name:    cmdName,
					Handler: makeACPSkillHandler(cmdName),
				}, args
			}
		}
	}

	// Not a known command — let it flow through as normal LLM input
	return nil, ""
}

// findACPCommand finds a command by exact name match from the shared registry.
func findACPCommand(name string) *ACPSlashCommand {
	def := cmds.Find(name)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeACP) {
		return nil
	}
	h, ok := acpCommandHandlers[name]
	if !ok {
		return nil
	}
	return &ACPSlashCommand{
		Name:        def.Name,
		Description: def.Description,
		InputHint:   def.InputHint,
		Handler:     h,
	}
}

// findACPCommandByPrefix finds a command that is a prefix of the input
// (e.g., "mcp" matches "mcp list").
func findACPCommandByPrefix(input string) *ACPSlashCommand {
	def := cmds.FindByPrefix(input)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeACP) {
		return nil
	}
	h, ok := acpCommandHandlers[def.Name]
	if !ok {
		return nil
	}
	return &ACPSlashCommand{
		Name:        def.Name,
		Description: def.Description,
		InputHint:   def.InputHint,
		Handler:     h,
	}
}

// makeACPSkillHandler returns a handler function for dynamic skill commands.
func makeACPSkillHandler(skillName string) func(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	return func(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
		return handleACPSkillActivate(ctx, sess, conn, skillName, args)
	}
}

// ---------------------------------------------------------------------------
// Helper utilities
// ---------------------------------------------------------------------------

// sendTextUpdate sends a text-only SessionUpdate to the client. Each call is
// its own logical message (slash feedback, command output) — always start a
// fresh message ID so it never merges with surrounding agent text.
func sendTextUpdate(ctx context.Context, conn *acp.AgentSideConnection, sessID acp.SessionId, text string) {
	_ = conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessID,
		Update:    agentMessageTextChunk(text, nextMessageID()),
	})
}

// loadSessionHistory loads the session's messages and converts them to LLM
// format, delegating to agent.LoadSessionHistory.
//
// The ACP session's sessMgr is the same *session.Manager instance that was
// handed to the agent via SetSessionManager (see agent.go newSession /
// LoadSession / resume paths), and ACPSession.ProviderType prefers
// agent.Provider().Name() — so the agent-side helper sees identical inputs.
//
// Errors are logged and treated as "no history": callers use the result as
// optional context for a one-off turn, where a failed load should degrade
// rather than abort. This preserves the behaviour of the three hand-rolled
// copies this replaces.
func loadSessionHistory(ctx context.Context, sess *ACPSession) []llm.Message {
	if sess == nil || sess.agent == nil {
		return nil
	}
	history, err := sess.agent.LoadSessionHistory()
	if err != nil {
		logger.FromContext(ctx).Error(ctx, "ACP: load session history failed", err)
		return nil
	}
	return history
}

// ---------------------------------------------------------------------------
// /commit handler
// ---------------------------------------------------------------------------

// acpOneoffSessionID returns the ID of the tachi session backing this ACP
// session. Used both to anchor one-off transcripts (/commit, /review) under
// the session directory and to scope per-session MCP tool discovery in the
// /mcp list. Returns "" when no session has been created yet.
func acpOneoffSessionID(sess *ACPSession) string {
	if sess.sessMgr != nil {
		if cur := sess.sessMgr.Current(); cur != nil {
			return cur.ID
		}
	}
	return ""
}

func handleACPCommit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /commit handler start")

	aiAgent := sess.agent

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, sess.additionalDirs, agent.ModeAuto, sess.ID)

	eventCh := aiAgent.RunCommitOneOff(ctx, systemPrompt, acpOneoffSessionID(sess), config.DefaultMaxTokens, "")

	stopReason, _, _ := streamToACP(ctx, sess, conn, eventCh)

	// /commit is a one-off task — its messages should not persist in the
	// session history cache. Clear it so the next Prompt reloads from disk
	// and gets the correct conversation history without the commit turn.
	sess.history = nil

	return stopReason, nil
}

// ---------------------------------------------------------------------------
// /review handler
// ---------------------------------------------------------------------------

func handleACPReview(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /review handler start")

	aiAgent := sess.agent
	cfg := sess.cfg

	// Resolve review provider and model from config (or fall back to main).
	reviewProvider := aiAgent.Provider()
	if rp := aiAgent.ReviewProvider(); rp != nil {
		reviewProvider = rp
	}

	// Parameter defaults/overrides come from the shared resolver (same as the
	// TUI side); only the provider lookup is agent-specific. Unconfigured
	// thinking dimensions follow the session's live config.
	ropts := cmds.ResolveReviewOptions(cfg)
	thinking, effort := cmds.ResolveReviewThinking(ropts,
		aiAgent.Config.Resolved.Thinking, aiAgent.Config.Resolved.ThinkingEffort)

	systemPrompt := buildSystemPromptForCwd(cfg, sess.cwd, sess.additionalDirs, agent.ModeAuto, sess.ID)
	opts := llm.ChatOptions{
		MaxTokens:      config.DefaultMaxTokens,
		Thinking:       thinking,
		ThinkingEffort: effort,
	}

	// The shared orchestrator resolves rounds, assigns per-round providers
	// (fail-fast on unresolvable adversarial models) and creates the report
	// directory. Single-round reviews flow through the same path — this
	// frontend never branches on round count. The report dir is anchored at
	// sess.cwd (the session working directory the round's Bash/WriteFile
	// tools resolve against) — NOT the process CWD, which ACP clients are
	// not required to align with (Zed starts the agent from the editor
	// binary's directory).
	orch, err := cmds.NewReviewOrchestratorFromCommand("/review "+args, ropts,
		func(rounds int) ([]llm.Provider, error) {
			if rounds == 1 {
				return []llm.Provider{reviewProvider}, nil
			}
			return aiAgent.ResolveAdversarialRoundModels(sess.cfg, reviewProvider, rounds)
		}, sess.cwd)
	if err != nil {
		sendTextUpdate(ctx, conn, acp.SessionId(sess.ID), err.Error())
		return acp.StopReasonEndTurn, err
	}

	// Synchronous round loop driven by the shared orchestrator: each round
	// gets a fresh isolated fork → stream to the client. defer Close keeps
	// the fork alive until the closure returns (streamToACP blocks until the
	// channel closes), so only one fork exists at a time — and a panic
	// mid-round can't leak the fork.
	stopReason := acp.StopReasonEndTurn
	var lastOutPath string // last round's orchestrator-owned report path (multi-round only)
	runErr := orch.Run(func(spec cmds.RoundSpec) error {
		if spec.OutPath != "" {
			lastOutPath = spec.OutPath
		}
		forked := aiAgent.Fork(agent.ForkConfig{
			Provider:      spec.Provider,
			MaxIterations: ropts.MaxIterations,
			AllowedTools:  ropts.AllowedTools,
			Logger:        aiAgent.Logger(),
		})
		defer forked.Close()

		eventCh := forked.Agent().RunOneOffStream(ctx, spec.Provider, systemPrompt, spec.Prompt, opts,
			agent.WithOneOffMeta(&agent.OneOffMeta{Kind: spec.Kind, SessionID: acpOneoffSessionID(sess)}))
		var err error
		stopReason, _, err = streamToACP(ctx, sess, conn, eventCh)
		// A broken round (API error / budget exhaustion) must terminate the
		// chain — streamToACP carries the failure back as an error while
		// keeping the legacy EndTurn stop reason for single-round callers.
		if err != nil {
			return err
		}
		// Client-initiated stop (disconnect/cancel) also terminates the chain.
		if stopReason != acp.StopReasonEndTurn {
			return cmds.ErrStopReview
		}
		return nil
	})

	// /review is a one-off task — its messages should not persist in the
	// session history cache (also on early termination). Clear it so the
	// next Prompt reloads from disk.
	sess.history = nil

	if runErr != nil {
		if errors.Is(runErr, cmds.ErrStopReview) {
			return stopReason, nil
		}
		return acp.StopReasonEndTurn, runErr
	}

	// Register the final round's report as a session artifact (only when
	// the file exists; a missing session manager is logged, not skipped).
	// The next Prompt reloads history from disk, where the artifact
	// reminder is carried by ConvertSessionToLLMMessages.
	if lastOutPath != "" {
		if _, statErr := os.Stat(lastOutPath); statErr != nil {
			logger.FromContext(ctx).Warn(ctx, "ACP: review artifact: report missing on disk, not registered", "path", lastOutPath, "err", statErr)
		} else if sm := aiAgent.SessionManager(); sm != nil {
			if err := sm.AppendArtifact(session.ArtifactRef{
				Kind:  session.ArtifactKindReview,
				Title: fmt.Sprintf("代码审查（%d 轮）", orch.TotalRounds()),
				Path:  lastOutPath,
			}); err != nil {
				logger.FromContext(ctx).Warn(ctx, "ACP: review artifact: append failed", "err", err)
			}
		} else {
			logger.FromContext(ctx).Warn(ctx, "ACP: review artifact: no session manager, not registered")
		}
	}
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /init handler
// ---------------------------------------------------------------------------

func handleACPInit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /init handler start")

	// Build history from session
	history := loadSessionHistory(ctx, sess)

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, sess.additionalDirs, agent.ModeAuto, sess.ID)

	eventCh := sess.agent.RunConversationStream(ctx, history, cmds.InitPromptTemplate, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	stopReason, _, _ := streamToACP(ctx, sess, conn, eventCh)
	return stopReason, nil
}

// ---------------------------------------------------------------------------
// /compact handler
// ---------------------------------------------------------------------------

func handleACPCompact(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /compact handler start")

	sessionID := acp.SessionId(sess.ID)
	sm := sess.sessMgr
	if sm == nil || !sm.HasCurrent() {
		sendTextUpdate(ctx, conn, sessionID, "No active session to compact.")
		return acp.StopReasonEndTurn, nil
	}

	// Build the conversation history to compact: prefer the in-memory cache,
	// fall back to loading the current session from disk. Passing nil history
	// would ask the LLM to summarize a conversation it cannot see.
	history := sess.history
	if len(history) == 0 {
		history = loadSessionHistory(ctx, sess)
	}
	if len(history) == 0 {
		sendTextUpdate(ctx, conn, sessionID, "Nothing to compact yet.")
		return acp.StopReasonEndTurn, nil
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, sess.additionalDirs, agent.ModeAuto, sess.ID)

	// Disk-loaded history has no system message; prepend it so the compact
	// LLM call sees the same environment context as the live conversation
	// (the compact instruction asks for working directory, branch, etc.).
	if history[0].Role != "system" {
		history = append([]llm.Message{{Role: "system", Content: systemPrompt}}, history...)
	}

	// Run compact turn — use DrainCompactEvents approach (simple, reliable).
	// WithNoTools hides every tool for this run; the registry is untouched, so
	// there is nothing to restore on any exit path (see agent/toolview.go).
	eventCh := sess.agent.RunConversationStream(ctx, history,
		cmds.BuildCompactInstruction(), systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens},
		agent.WithNoTools(),
	)

	summary, err := agent.DrainCompactEvents(eventCh)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, "Compact failed: "+err.Error())
		return acp.StopReasonEndTurn, nil
	}
	if summary == "" {
		sendTextUpdate(ctx, conn, sessionID, "Compact produced no summary.")
		return acp.StopReasonEndTurn, nil
	}

	// Create new session from summary
	_, err = sess.agent.CompleteCompact(sm, systemPrompt, summary)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, "Compact failed: "+err.Error())
		return acp.StopReasonEndTurn, nil
	}

	// Invalidate the cached history — it still holds the pre-compact messages.
	// The next Prompt reloads from disk, picking up the new compact session
	// (same pattern as /commit and /review).
	sess.history = nil

	sendTextUpdate(ctx, conn, sessionID,
		"Conversation compacted. New session created with summary of previous context.")
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /usage handler
// ---------------------------------------------------------------------------

func handleACPUsage(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /usage handler start")

	sessionID := acp.SessionId(sess.ID)

	// Check context cancellation for pure-computation commands
	select {
	case <-ctx.Done():
		return acp.StopReasonCancelled, nil
	default:
	}

	sm := sess.sessMgr
	if sm == nil || !sm.HasCurrent() {
		sendTextUpdate(ctx, conn, sessionID, "No active session.")
		return acp.StopReasonEndTurn, nil
	}

	report, err := agent.ComputeSessionUsage(sm, sess.agent.UsageRecorder(), sess.agent.ContextWindow(), sess.cfg)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, "Usage: "+err.Error())
		return acp.StopReasonEndTurn, nil
	}

	info := agent.BuildUsageReportInfo(report, sess.agent.LastInputEstimate(), sess.agent.LastTokenBreakdown(), sess.cfg.Debug.PPROF.Addr())

	text := cmds.FormatUsageReport(info)
	sendTextUpdate(ctx, conn, sessionID, text)
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /mcp handler
// ---------------------------------------------------------------------------

func handleACPMCP(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, fmt.Sprintf("ACP: /mcp handler start args=%q", args))

	sessionID := acp.SessionId(sess.ID)

	parts := strings.Fields(args)
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	name := ""
	if len(parts) > 1 {
		name = parts[1]
	}

	switch sub {
	case "list", "":
		return handleACPMCPList(ctx, sess, conn)
	case "reconnect":
		return handleACPMCPReconnect(ctx, sess, conn, name)
	case "profile":
		return handleACPMCPProfile(ctx, sess, conn, name)
	default:
		sendTextUpdate(ctx, conn, sessionID, "Usage: /mcp list | /mcp reconnect <name> | /mcp profile [<name>]")
		return acp.StopReasonEndTurn, nil
	}
}

// handleACPMCPProfile handles `/mcp profile [name]`. Without a name it lists
// the available profiles (mcp.<name>.json in global + project scope) and
// marks the active one. With a name it switches the active profile at
// runtime — in-memory only, reverts on restart.
//
// The switch runs synchronously in the prompt goroutine (like /commit): it
// blocks until the reconcile finishes, bounded by the per-server connect
// timeouts plus margin. Client-side cancellation of the prompt propagates
// through ctx into in-flight connections. Concurrent switches are rejected
// by the agent's in-flight lock.
func handleACPMCPProfile(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, name string) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	// The session's cwd scopes the project-level .tachi/mcp.<name>.json
	// lookup (empty cwd → global scope only, same as LoadMCPServers).
	workDir := sess.cwd
	available := config.ListMCPProfiles(workDir)

	if name == "" {
		sendTextUpdate(ctx, conn, sessionID, agent.FormatMCPProfileList(available, sess.cfg.ActiveMCPProfile))
		return acp.StopReasonEndTurn, nil
	}

	if !slices.Contains(available, name) {
		content := fmt.Sprintf("MCP profile **%s** not found.", name)
		if len(available) > 0 {
			content += fmt.Sprintf(" Available: %s", strings.Join(available, ", "))
		} else {
			content += " No mcp.<name>.json files exist yet."
		}
		sendTextUpdate(ctx, conn, sessionID, content)
		return acp.StopReasonEndTurn, nil
	}

	if name == sess.cfg.ActiveMCPProfile {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("MCP profile **%s** is already active", name))
		return acp.StopReasonEndTurn, nil
	}

	sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Switching MCP profile to **%s**...", name))

	// sess.agent's manager and cfg are the same objects SwitchMCPProfile
	// mutates (both come from NewAIAgentWithConfig / t.cfg), so subsequent
	// /mcp list calls reflect the new server set.
	res, err := sess.agent.SwitchMCPProfile(ctx, name, workDir)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to switch MCP profile to **%s**: %v", name, err))
		return acp.StopReasonEndTurn, nil
	}
	sendTextUpdate(ctx, conn, sessionID, agent.FormatMCPSwitchResult(res))
	return acp.StopReasonEndTurn, nil
}

func handleACPMCPList(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	servers := sess.cfg.MCPServers
	if len(servers) == 0 {
		sendTextUpdate(ctx, conn, sessionID, "No MCP servers configured.")
		return acp.StopReasonEndTurn, nil
	}

	infos := cmds.BuildMCPServerInfos(servers, sess.mcpMgr, acpOneoffSessionID(sess))
	sendTextUpdate(ctx, conn, sessionID, cmds.FormatMCPList(infos))
	return acp.StopReasonEndTurn, nil
}

func handleACPMCPReconnect(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, name string) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	if name == "" {
		sendTextUpdate(ctx, conn, sessionID, "Usage: /mcp reconnect <name>")
		return acp.StopReasonEndTurn, nil
	}

	mgr := sess.mcpMgr
	if mgr == nil {
		sendTextUpdate(ctx, conn, sessionID, "No MCP manager available.")
		return acp.StopReasonEndTurn, nil
	}

	// Unregister old tools for this server
	prefix := fmt.Sprintf("mcp__%s__", name)
	for _, s := range sess.agent.ToolSchemas() {
		if strings.HasPrefix(s.Name, prefix) {
			sess.agent.UnregisterTool(s.Name)
		}
	}

	sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Attempting to reconnect MCP server **%s**...", name))

	if mgr.IsConnected(name) {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("MCP server **%s** is already connected.", name))
	} else {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf(
			"MCP server **%s** is disconnected. To reconnect, configure it in mcp.json and start a new session.", name))
	}

	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /skill handler
// ---------------------------------------------------------------------------

func handleACPSkill(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, fmt.Sprintf("ACP: /skill handler start args=%q", args))

	sessionID := acp.SessionId(sess.ID)

	store := sess.agent.SkillStore()
	if store == nil {
		sendTextUpdate(ctx, conn, sessionID, "Skill system not available.")
		return acp.StopReasonEndTurn, nil
	}

	parts := strings.Fields(args)

	if len(parts) == 0 || parts[0] == "list" {
		return handleACPSkillList(ctx, sess, conn, store)
	}

	// /skill <name> [args] — activate a specific skill
	skillName := parts[0]
	extraArgs := ""
	if len(parts) > 1 {
		extraArgs = strings.Join(parts[1:], " ")
	}
	return handleACPSkillActivate(ctx, sess, conn, skillName, extraArgs)
}

func handleACPSkillList(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, store *skill.Store) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	metas := store.List()
	if len(metas) == 0 {
		sendTextUpdate(ctx, conn, sessionID,
			fmt.Sprintf("No skills found. Create a skill by adding a `SKILL.md` file in `.tachi/skills/<name>/`, `.claude/skills/<name>/`, `.cursor/skills/<name>/`, or `%s/<name>/`.", config.GlobalSkillsDir()))
		return acp.StopReasonEndTurn, nil
	}

	text := cmds.FormatSkillList(metas)
	sendTextUpdate(ctx, conn, sessionID, text)
	return acp.StopReasonEndTurn, nil
}

func handleACPSkillActivate(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, skillName, extraArgs string) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	if sess.agent.IsSkillActive(skillName) {
		sendTextUpdate(ctx, conn, sessionID,
			fmt.Sprintf("Skill **%s** is already active in this session.", skillName))
		return acp.StopReasonEndTurn, nil
	}

	msg, err := sess.agent.ActivateSkill(skillName, extraArgs)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID,
			fmt.Sprintf("Skill **%s** not found. Use /skill to see available skills.", skillName))
		return acp.StopReasonEndTurn, nil
	}

	// Run as normal conversation turn with skill activation message
	history := loadSessionHistory(ctx, sess)

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, sess.additionalDirs, agent.ModeAuto, sess.ID)
	eventCh := sess.agent.RunConversationStream(ctx, history, msg, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	stopReason, _, _ := streamToACP(ctx, sess, conn, eventCh)
	return stopReason, nil
}

// ---------------------------------------------------------------------------
// /transcript handler
// ---------------------------------------------------------------------------

func handleACPTranscript(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /transcript handler start")

	sessionID := acp.SessionId(sess.ID)

	sm := sess.sessMgr
	if sm == nil || !sm.HasCurrent() {
		sendTextUpdate(ctx, conn, sessionID, "No active session — start a conversation first.")
		return acp.StopReasonEndTurn, nil
	}

	msgs, err := sm.LoadMessages()
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to load session messages: %v", err))
		return acp.StopReasonEndTurn, nil
	}
	if len(msgs) == 0 {
		sendTextUpdate(ctx, conn, sessionID, "No messages in current session yet — send a message first.")
		return acp.StopReasonEndTurn, nil
	}

	curr := sm.Current()
	// Sub-agent sidecar messages are optional — a load failure is non-fatal.
	subagents, _ := sm.LoadSubagentMessages(curr.ID)
	// API request records (system prompt + tool schemas) are optional too.
	apiReqs, _ := sm.LoadAPIRequests(curr.ID)
	data := render.BuildReportDataFromMessagesWithRequests(curr, msgs, subagents, apiReqs)
	html, err := render.GenerateHTML(data)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to generate transcript HTML: %v", err))
		return acp.StopReasonEndTurn, nil
	}

	// Save to a temp file and return the path (no browser opening in ACP mode)
	tmpDir := os.TempDir()
	filename := filepath.Join(tmpDir, fmt.Sprintf("tachi-transcript-%s.html", curr.ID[:8]))
	if err := fileutil.WriteFileShared(filename, []byte(html)); err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to save transcript: %v", err))
		return acp.StopReasonEndTurn, nil
	}

	sendTextUpdate(ctx, conn, sessionID,
		fmt.Sprintf("📋 Transcript Report\n\nSession: %s\nSaved to: %s", curr.Title, filename))
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /research handler
// ---------------------------------------------------------------------------

func handleACPResearch(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, fmt.Sprintf("ACP: /research handler start args=%q", args))

	sessionID := acp.SessionId(sess.ID)

	cfg := sess.cfg
	if cfg == nil {
		sendTextUpdate(ctx, conn, sessionID, "No configuration available.")
		return acp.StopReasonEndTurn, nil
	}

	// Progress callbacks may fire concurrently from parallel sub-agents —
	// serialise SessionUpdate writes.
	var progressMu sync.Mutex
	report, _, err := sess.agent.RunDeepResearch(ctx, cfg, args,
		func(topic string, depth, breadth int) {
			sendTextUpdate(ctx, conn, sessionID,
				fmt.Sprintf("🔬 **Deep Research Started**\n\n**Topic**: %s\n**Depth**: %d | **Breadth**: %d\n\nSearching...",
					topic, depth, breadth))
		},
		func(text string) {
			progressMu.Lock()
			sendTextUpdate(ctx, conn, sessionID, text)
			progressMu.Unlock()
		})
	if errors.Is(err, agent.ErrResearchUsage) {
		sendTextUpdate(ctx, conn, sessionID, "Usage: `/research <topic> [--depth N] [--breadth N] [--format report|answer]`")
		return acp.StopReasonEndTurn, nil
	}
	if errors.Is(err, agent.ErrResearchUnavailable) {
		sendTextUpdate(ctx, conn, sessionID, "Deep Research is not available.")
		return acp.StopReasonEndTurn, nil
	}
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("❌ Research failed: %v", err))
		return acp.StopReasonEndTurn, nil
	}

	sendTextUpdate(ctx, conn, sessionID,
		fmt.Sprintf("✅ **Research Complete**\n\n---\n\n%s", report))

	// Invalidate the in-memory history cache so the NEXT Prompt reloads from
	// disk, where the artifact reminder registered by RunDeepResearch is
	// carried by ConvertSessionToLLMMessages. Without this, a live session
	// with a populated cache would never see the artifact.
	sess.history = nil
	return acp.StopReasonEndTurn, nil
}
