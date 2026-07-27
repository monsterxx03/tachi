package acp

import (
	"context"
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
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
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
	"model":      handleACPModel,
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

// sendTextUpdate sends a text-only SessionUpdate to the client.
func sendTextUpdate(ctx context.Context, conn *acp.AgentSideConnection, sessID acp.SessionId, text string) {
	_ = conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessID,
		Update:    acp.UpdateAgentMessageText(text),
	})
}

// resolveModelPrice resolves the effective price for the current provider+model.
func resolveModelPrice(sess *ACPSession) *llm.ModelPrice {
	return llm.ResolveModelPrice(sess.agent.Model(), nil, nil, nil, nil)
}

// ---------------------------------------------------------------------------
// /model handler
// ---------------------------------------------------------------------------

func handleACPModel(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	// List available models if no args
	if args == "" {
		cur := sess.resolveProviderName()
		var sb strings.Builder
		sb.WriteString("Available models:\n")
		for _, p := range sess.cfg.Providers {
			fmt.Fprintf(&sb, "  • %s (%s)", p.Name, p.Model)
			if p.Name == cur {
				sb.WriteString(" ← current")
			}
			sb.WriteString("\n")
		}
		sendTextUpdate(ctx, conn, sessionID, sb.String())
		return acp.StopReasonEndTurn, nil
	}

	// Switch to named provider
	if err := switchSessionModel(sess, args, nil); err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to switch model: %v", err))
		return acp.StopReasonEndTurn, err
	}

	sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Switched to model: %s", args))
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /commit handler
// ---------------------------------------------------------------------------

// acpOneoffSessionID returns the tachi session ID anchoring one-off
// transcripts (/commit, /review) to this ACP session's directory.
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

	// Save tool registry, leave only Bash
	savedTools := aiAgent.SaveToolRegistry()
	for _, name := range aiAgent.ToolNames() {
		if name != tools.ToolNameBash {
			aiAgent.UnregisterTool(name)
		}
	}
	defer func() {
		if savedTools != nil {
			aiAgent.RestoreToolRegistry(savedTools)
		}
	}()

	commitProvider := aiAgent.CommitProvider()
	model := aiAgent.Model()

	thinkingDisabled := false
	opts := llm.ChatOptions{
		MaxTokens: config.DefaultMaxTokens,
		Thinking:  &thinkingDisabled,
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, agent.ModeAuto, sess.ID)

	eventCh := aiAgent.RunOneOffStream(ctx, commitProvider, systemPrompt,
		cmds.CommitUserPrompt(model), opts,
		agent.OneOffMeta{Kind: "commit", SessionID: acpOneoffSessionID(sess)})

	stopReason, _ := streamToACP(ctx, sess, conn, eventCh)

	// /commit is a one-off task — its messages should not persist in the
	// session history cache. Clear it so the next Prompt reloads from disk
	// and gets the correct conversation history without the commit turn.
	sess.history = nil

	return stopReason, nil
}

// ---------------------------------------------------------------------------
// /review handler
// ---------------------------------------------------------------------------

func handleACPReview(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /review handler start")

	aiAgent := sess.agent
	cfg := sess.cfg

	// Resolve review provider and model from config (or fall back to main).
	reviewProvider := aiAgent.Provider()
	if rp := aiAgent.ReviewProvider(); rp != nil {
		reviewProvider = rp
	}

	// MaxIterations and Thinking are populated by defaults.Set() from struct tags.
	maxIter := cmds.DefaultReviewMaxIterations
	thinking := new(bool)
	if cfg != nil {
		maxIter = cfg.Review.MaxIterations
		thinking = cfg.Review.Thinking
	}

	// Resolve allowed tools (slice can't use `default` tag, handle in code).
	var allowedTools []string
	if cfg != nil && len(cfg.Review.AllowedTools) > 0 {
		allowedTools = cfg.Review.AllowedTools
	} else {
		allowedTools = cmds.DefaultReviewAllowedTools()
	}

	// Fork a child agent with configurable tools.
	forked := aiAgent.Fork(agent.ForkConfig{
		Provider:      reviewProvider,
		MaxIterations: maxIter,
		AllowedTools:  allowedTools,
		Logger:        aiAgent.Logger(),
	})
	defer forked.Close()

	systemPrompt := buildSystemPromptForCwd(cfg, sess.cwd, agent.ModeAuto, sess.ID)

	opts := llm.ChatOptions{
		MaxTokens: config.DefaultMaxTokens,
		Thinking:  thinking,
	}

	eventCh := forked.Agent().RunOneOffStream(ctx, reviewProvider,
		systemPrompt, cmds.ReviewUserPrompt(), opts,
		agent.OneOffMeta{Kind: "review", SessionID: acpOneoffSessionID(sess)})

	stopReason, _ := streamToACP(ctx, sess, conn, eventCh)

	// /review is a one-off task — its messages should not persist in the
	// session history cache. Clear it so the next Prompt reloads from disk
	// and gets the correct conversation history without the review turn.
	sess.history = nil

	return stopReason, nil
}

// ---------------------------------------------------------------------------
// /init handler
// ---------------------------------------------------------------------------

func handleACPInit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	logger.FromContext(ctx).Info(ctx, "ACP: /init handler start")

	// Build history from session
	var history []llm.Message
	if sess.sessMgr != nil {
		msgs, err := sess.sessMgr.LoadMessages()
		if err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.ProviderType())
			if convErr == nil {
				history = llmMsgs
			} else {
				logger.FromContext(ctx).Error(ctx, "ACP: ConvertSessionToLLMMessages failed", convErr)
			}
		}
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, agent.ModeAuto, sess.ID)

	eventCh := sess.agent.RunConversationStream(ctx, history, cmds.InitPromptTemplate, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	stopReason, _ := streamToACP(ctx, sess, conn, eventCh)
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
		if msgs, err := sm.LoadMessages(); err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.ProviderType())
			if convErr == nil {
				history = llmMsgs
			} else {
				logger.FromContext(ctx).Error(ctx, "ACP: /compact convert history failed", convErr)
			}
		}
	}
	if len(history) == 0 {
		sendTextUpdate(ctx, conn, sessionID, "Nothing to compact yet.")
		return acp.StopReasonEndTurn, nil
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, agent.ModeAuto, sess.ID)

	// Disk-loaded history has no system message; prepend it so the compact
	// LLM call sees the same environment context as the live conversation
	// (the compact instruction asks for working directory, branch, etc.).
	if history[0].Role != "system" {
		history = append([]llm.Message{{Role: "system", Content: systemPrompt}}, history...)
	}

	// Save and clear tools: compact shouldn't call any tools
	savedTools := sess.agent.SaveToolRegistry()
	sess.agent.ClearToolRegistry()
	defer func() {
		if savedTools != nil {
			sess.agent.RestoreToolRegistry(savedTools)
		}
	}()

	// Run compact turn — use DrainCompactEvents approach (simple, reliable)
	eventCh := sess.agent.RunConversationStream(ctx, history,
		cmds.BuildCompactInstruction(), systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens},
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
	_, err = agent.FinalizeCompact(sm, systemPrompt, summary)
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

	price := resolveModelPrice(sess)
	report, err := agent.ComputeSessionUsage(sm, price, sess.agent.ContextWindow())
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, "Usage: "+err.Error())
		return acp.StopReasonEndTurn, nil
	}

	// Convert to shared UsageReportInfo
	toolCalls := make(map[string]*cmds.ToolCallStat, len(report.ToolCalls))
	for name, st := range report.ToolCalls {
		toolCalls[name] = &cmds.ToolCallStat{Count: st.Count, ErrCount: st.ErrCount}
	}
	info := &cmds.UsageReportInfo{
		SessionID:                report.Session.ID,
		Provider:                 report.Session.ProviderName,
		Title:                    report.Session.Title,
		ContextWindow:            report.ContextWindow,
		InputTokens:              report.Usage.InputTokens,
		LastInputTokens:          report.Usage.LastInputTokens,
		CacheReadInputTokens:     report.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: report.Usage.CacheCreationInputTokens,
		OutputTokens:             report.Usage.OutputTokens,
		EstimatedInputTokens:     sess.agent.LastInputEstimate(),
		Cost:                     report.Cost,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
	}
	// Populate token breakdown from the session agent
	info.EstBreakdown = sess.agent.LastTokenBreakdown()

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
	default:
		sendTextUpdate(ctx, conn, sessionID, "Usage: /mcp list | /mcp reconnect <name>")
		return acp.StopReasonEndTurn, nil
	}
}

func handleACPMCPList(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection) (acp.StopReason, error) {
	sessionID := acp.SessionId(sess.ID)

	servers := sess.cfg.MCPServers
	if len(servers) == 0 {
		sendTextUpdate(ctx, conn, sessionID, "No MCP servers configured.")
		return acp.StopReasonEndTurn, nil
	}

	infos := cmds.BuildMCPServerInfos(servers, sess.mcpMgr)
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
			"No skills found. Create a skill by adding a `SKILL.md` file in `.tachi/skills/<name>/`, `.claude/skills/<name>/`, `.cursor/skills/<name>/`, or `~/.tachi/skills/<name>/`.")
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
	var history []llm.Message
	if sess.sessMgr != nil {
		msgs, err := sess.sessMgr.LoadMessages()
		if err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.ProviderType())
			if convErr == nil {
				history = llmMsgs
			} else {
				logger.FromContext(ctx).Error(ctx, "ACP: ConvertSessionToLLMMessages failed", convErr)
			}
		}
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg, sess.cwd, agent.ModeAuto, sess.ID)
	eventCh := sess.agent.RunConversationStream(ctx, history, msg, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	stopReason, _ := streamToACP(ctx, sess, conn, eventCh)
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
	data := render.BuildReportDataFromMessages(curr, msgs, subagents)
	html, err := render.GenerateHTML(data)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to generate transcript HTML: %v", err))
		return acp.StopReasonEndTurn, nil
	}

	// Save to a temp file and return the path (no browser opening in ACP mode)
	tmpDir := os.TempDir()
	filename := filepath.Join(tmpDir, fmt.Sprintf("tachi-transcript-%s.html", curr.ID[:8]))
	if err := os.WriteFile(filename, []byte(html), 0644); err != nil {
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

	parsed := cmds.ParseResearchArgs(args)
	if parsed.Topic == "" {
		sendTextUpdate(ctx, conn, sessionID, "Usage: `/research <topic> [--depth N] [--breadth N] [--format report|answer]`")
		return acp.StopReasonEndTurn, nil
	}

	cfg := sess.cfg
	if cfg == nil {
		sendTextUpdate(ctx, conn, sessionID, "No configuration available.")
		return acp.StopReasonEndTurn, nil
	}

	// Apply defaults from config if not specified
	if parsed.Depth <= 0 {
		parsed.Depth = cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = cfg.DeepResearch.DefaultBreadth
	}

	engine, err := sess.agent.NewDeepResearch(cfg)
	if err != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("Failed to create research engine: %v", err))
		return acp.StopReasonEndTurn, nil
	}
	if engine == nil {
		sendTextUpdate(ctx, conn, sessionID, "Deep Research is not available.")
		return acp.StopReasonEndTurn, nil
	}

	sendTextUpdate(ctx, conn, sessionID,
		fmt.Sprintf("🔬 **Deep Research Started**\n\n**Topic**: %s\n**Depth**: %d | **Breadth**: %d\n\nSearching...",
			parsed.Topic, parsed.Depth, parsed.Breadth))

	// Run with research-specific timeout, streaming progress via SessionUpdate.
	// Use a mutex to serialise progress callbacks, since the engine may call
	// them concurrently from multiple goroutines (parallel sub-agents).
	researchCtx, cancel := context.WithTimeout(ctx, cfg.DeepResearch.Timeout)
	defer cancel()

	var progressMu sync.Mutex
	report, runErr := engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
		progressMu.Lock()
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf(format, args...))
		progressMu.Unlock()
	})
	if runErr != nil {
		sendTextUpdate(ctx, conn, sessionID, fmt.Sprintf("❌ Research failed: %v", runErr))
		return acp.StopReasonEndTurn, nil
	}

	sendTextUpdate(ctx, conn, sessionID,
		fmt.Sprintf("✅ **Research Complete**\n\n---\n\n%s", report))
	return acp.StopReasonEndTurn, nil
}
