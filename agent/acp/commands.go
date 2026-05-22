package acp

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
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
// Command registry
// ---------------------------------------------------------------------------

var acpCommands = []ACPSlashCommand{
	{
		Name:        "commit",
		Description: "Generate commit message for staged changes and commit via git",
		Handler:     handleACPCommit,
	},
	{
		Name:        "init",
		Description: "Generate .tachi.md project context file",
		Handler:     handleACPInit,
	},
	{
		Name:        "compact",
		Description: "Compress conversation history into a summary and start fresh",
		Handler:     handleACPCompact,
	},
	{
		Name:        "usage",
		Description: "Show token usage, cost, and tool call statistics",
		Handler:     handleACPUsage,
	},
	{
		Name:        "mcp",
		Description: "Manage MCP servers (list, reconnect)",
		InputHint:   "list | reconnect <name>",
		Handler:     handleACPMCP,
	},
	{
		Name:        "skill",
		Description: "List or activate skills",
		InputHint:   "list | <name> [args]",
		Handler:     handleACPSkill,
	},
	{
		Name:        "transcript",
		Description: "Generate session transcript report",
		Handler:     handleACPTranscript,
	},
}

// buildACPAvailableCommands builds the ACP AvailableCommand list from the
// static command registry, plus dynamic skill commands from the skill store.
func buildACPAvailableCommands(aiAgent *agent.AIAgent) []acp.AvailableCommand {
	result := make([]acp.AvailableCommand, 0, len(acpCommands)+16)

	// Static commands
	for _, cmd := range acpCommands {
		ac := acp.AvailableCommand{
			Name:        cmd.Name,
			Description: cmd.Description,
		}
		if cmd.InputHint != "" {
			ac.Input = &acp.AvailableCommandInput{
				Unstructured: &acp.UnstructuredCommandInput{Hint: cmd.InputHint},
			}
		}
		result = append(result, ac)
	}

	// Dynamic skill commands
	if aiAgent != nil {
		if store := aiAgent.SkillStore(); store != nil {
			for _, meta := range store.List() {
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

// findACPCommand finds a command by exact name match.
func findACPCommand(name string) *ACPSlashCommand {
	for _, cmd := range acpCommands {
		if cmd.Name == name {
			return &cmd
		}
	}
	return nil
}

// findACPCommandByPrefix finds a command that is a prefix of the input
// (e.g., "/mcp" matches "/mcp list").
func findACPCommandByPrefix(input string) *ACPSlashCommand {
	for _, cmd := range acpCommands {
		if input == cmd.Name || strings.HasPrefix(input, cmd.Name+" ") {
			return &cmd
		}
	}
	return nil
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
// /commit handler
// ---------------------------------------------------------------------------

func handleACPCommit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /commit handler start")

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

	systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)

	eventCh := aiAgent.RunOneOffStream(ctx, commitProvider, systemPrompt,
		agent.CommitUserPrompt(model), opts)

	return streamToACP(ctx, sess, conn, eventCh), nil
}

// ---------------------------------------------------------------------------
// /init handler
// ---------------------------------------------------------------------------

func handleACPInit(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /init handler start")

	// Build history from session
	var history []llm.Message
	if sess.sessMgr != nil {
		msgs, err := sess.sessMgr.LoadMessages()
		if err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
			if convErr == nil {
				history = llmMsgs
			} else {
				debuglog.DefaultLogger.Log("ACP: ConvertSessionToLLMMessages failed: %v", convErr)
			}
		}
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)

	eventCh := sess.agent.RunConversationStream(ctx, history, agent.InitPromptTemplate, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	return streamToACP(ctx, sess, conn, eventCh), nil
}

// ---------------------------------------------------------------------------
// /compact handler
// ---------------------------------------------------------------------------

func handleACPCompact(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /compact handler start")

	sessionID := acp.SessionId(sess.ID)
	sm := sess.sessMgr
	if sm == nil || !sm.HasCurrent() {
		sendTextUpdate(ctx, conn, sessionID, "No active session to compact.")
		return acp.StopReasonEndTurn, nil
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
	systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)
	eventCh := sess.agent.RunConversationStream(ctx, nil,
		agent.BuildCompactInstruction(), systemPrompt,
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

	sendTextUpdate(ctx, conn, sessionID,
		"Conversation compacted. New session created with summary of previous context.")
	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /usage handler
// ---------------------------------------------------------------------------

func handleACPUsage(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /usage handler start")

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

	text := formatUsageReportACP(report)
	sendTextUpdate(ctx, conn, sessionID, text)
	return acp.StopReasonEndTurn, nil
}

// formatUsageReportACP formats a usage report as plain text (not Markdown).
func formatUsageReportACP(report *agent.SessionUsageReport) string {
	var sb strings.Builder
	sb.WriteString("📊 Session Usage\n\n")

	// Session info
	sb.WriteString(fmt.Sprintf("Session: %s\n", report.Session.ID))
	provider := report.Session.Provider
	if provider == "" {
		provider = "(unknown)"
	}
	sb.WriteString(fmt.Sprintf("Provider: %s\n", provider))
	sb.WriteString(fmt.Sprintf("Model: %s\n", report.Session.Model))
	title := report.Session.Title
	if title == "" {
		title = "(untitled)"
	}
	sb.WriteString(fmt.Sprintf("Title: %s\n\n", title))

	// Token usage
	u := report.Usage
	sb.WriteString("Token Usage\n")
	sb.WriteString(fmt.Sprintf("  Input tokens: %s\n", agent.FormatTokens(u.InputTokens)))
	if u.CacheReadInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Cache read:  %s\n", agent.FormatTokens(u.CacheReadInputTokens)))
	}
	if u.CacheCreationInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Cache created: %s\n", agent.FormatTokens(u.CacheCreationInputTokens)))
	}
	sb.WriteString(fmt.Sprintf("  Output tokens: %s\n", agent.FormatTokens(u.OutputTokens)))
	sb.WriteString(fmt.Sprintf("  Total tokens:  %s\n", agent.FormatTokens(u.InputTokens+u.OutputTokens)))
	if report.ContextWindow > 0 && u.InputTokens > 0 {
		pct := float64(u.InputTokens) / float64(report.ContextWindow) * 100
		sb.WriteString(fmt.Sprintf("  Context: %s / %s (%.0f%%)\n", agent.FormatTokens(u.InputTokens), agent.FormatTokens(report.ContextWindow), pct))
	}

	// Cost
	sb.WriteString("\nCost\n")
	if report.Cost <= 0 {
		sb.WriteString("  No pricing data available\n")
	} else {
		sb.WriteString(fmt.Sprintf("  Total cost: ¥%.4f\n", report.Cost))
	}

	// Tool calls
	sb.WriteString("\nTool Calls\n")
	names := slices.Sorted(maps.Keys(report.ToolCalls))
	for _, name := range names {
		st := report.ToolCalls[name]
		line := fmt.Sprintf("  - %s: %d call(s)", name, st.Count)
		if st.ErrCount > 0 {
			line += fmt.Sprintf(" (%d failed)", st.ErrCount)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("\n  Total: %d main + %d subagent = %d call(s)\n",
		report.MainCount, report.SubCount, report.MainCount+report.SubCount))

	return sb.String()
}

// ---------------------------------------------------------------------------
// /mcp handler
// ---------------------------------------------------------------------------

func handleACPMCP(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /mcp handler start args=%q", args)

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

	mgr := sess.mcpMgr
	if mgr == nil {
		sendTextUpdate(ctx, conn, sessionID, "No MCP manager available.")
		return acp.StopReasonEndTurn, nil
	}

	// List servers by iterating MCP tools in the agent's registry.
	toolSchemas := sess.agent.ToolSchemas()
	var mcpToolNames []string
	for _, s := range toolSchemas {
		if strings.HasPrefix(s.Name, "mcp__") {
			mcpToolNames = append(mcpToolNames, s.Name)
		}
	}

	var sb strings.Builder
	sb.WriteString("MCP Servers\n\n")

	// Group by server name
	serverTools := make(map[string][]string)
	for _, tn := range mcpToolNames {
		// Name format: mcp__<server>__<tool>
		parts := strings.SplitN(tn, "__", 3)
		if len(parts) >= 3 {
			server := parts[1]
			toolName := parts[2]
			serverTools[server] = append(serverTools[server], toolName)
		}
	}

	if len(serverTools) == 0 {
		sb.WriteString("  No MCP tools registered.\n")
	} else {
		servers := slices.Sorted(maps.Keys(serverTools))
		for _, srv := range servers {
			connected := mgr.IsConnected(srv)
			status := "🔴 Disconnected"
			if connected {
				status = "🟢 Connected"
			}
			tools := serverTools[srv]
			fmt.Fprintf(&sb, "  - %s (%s) — %d tool(s)\n", srv, status, len(tools))
			for _, t := range tools {
				fmt.Fprintf(&sb, "      • %s\n", t)
			}
		}
	}

	sendTextUpdate(ctx, conn, sessionID, sb.String())
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
			"MCP server **%s** is disconnected. To reconnect, please configure it in config.yaml and start a new session.", name))
	}

	return acp.StopReasonEndTurn, nil
}

// ---------------------------------------------------------------------------
// /skill handler
// ---------------------------------------------------------------------------

func handleACPSkill(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, args string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /skill handler start args=%q", args)

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
			"No skills found. Create a skill by adding a `SKILL.md` file in `.tachi/skills/<name>/` or `~/.tachi/skills/<name>/`.")
		return acp.StopReasonEndTurn, nil
	}

	var sb strings.Builder
	sb.WriteString("Available Skills:\n\n")
	for _, meta := range metas {
		sourceTag := ""
		if meta.Source == "project" {
			sourceTag = " 🏠"
		}
		sb.WriteString(fmt.Sprintf("  - %s%s\n", meta.Name, sourceTag))
		sb.WriteString(fmt.Sprintf("    %s\n", meta.Description))
		if len(meta.Tags) > 0 {
			sb.WriteString(fmt.Sprintf("    Tags: %s\n", strings.Join(meta.Tags, ", ")))
		}
	}
	sb.WriteString(fmt.Sprintf("\n%d skill(s) total", len(metas)))

	sendTextUpdate(ctx, conn, sessionID, sb.String())
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
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
			if convErr == nil {
				history = llmMsgs
			} else {
				debuglog.DefaultLogger.Log("ACP: ConvertSessionToLLMMessages failed: %v", convErr)
			}
		}
	}

	systemPrompt := buildSystemPromptForCwd(sess.cfg.Language, sess.cwd)
	eventCh := sess.agent.RunConversationStream(ctx, history, msg, systemPrompt,
		llm.ChatOptions{MaxTokens: config.DefaultMaxTokens})

	return streamToACP(ctx, sess, conn, eventCh), nil
}

// ---------------------------------------------------------------------------
// /transcript handler
// ---------------------------------------------------------------------------

func handleACPTranscript(ctx context.Context, sess *ACPSession, conn *acp.AgentSideConnection, _ string) (acp.StopReason, error) {
	debuglog.DefaultLogger.Log("ACP: /transcript handler start")

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
	data := render.BuildReportDataFromMessages(curr, msgs)
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
