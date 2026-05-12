package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

// InitPromptTemplate is the prompt sent to LLM to generate .tachi.md
const InitPromptTemplate = `Create (or improve) a .tachi.md file at the repo root. This file is read by future coding agent instances — write for agents, not humans. Keep it under 200 lines, terse and dense.

What to include:
1. Build, lint, test commands (including how to run a single test).
2. High-level architecture — the "big picture" that requires reading multiple files to discover. Use compact formats (tables, one-liners, signatures) over prose.

Rules:
- If .tachi.md exists, read it first and improve it in-place.
- No generic advice ("write tests", "be helpful", "don't hardcode secrets").
- No listing every file/dir — focus on relationships and non-obvious design decisions.
- No made-up sections ("Common Tasks", "Tips", "Support").
- If .cursor/rules/, .cursorrules, or .github/copilot-instructions.md exist, extract their key constraints.
- If README.md exists, extract its essential info.
- Use the WriteFile tool to write the result.

Gather context first:
  git status
  git branch --show-current
  git log --oneline -5
  find . -maxdepth 1 -name 'Makefile' -o -name 'go.mod' -o -name 'package.json' -o -name 'README.md' -o -name '.cursorrules' -o -name 'CLAUDE.md' 2>/dev/null
  ls -la

Then read key files, understand the architecture, and produce the .tachi.md.`

type Command struct {
	Name        string
	Description string
	handler     func(*Model) tea.Cmd
}

var commands = []Command{
	{
		Name:        "/new",
		Description: "Start new conversation",
		handler: func(m *Model) tea.Cmd {
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.history = nil
			m.chatview.Clear()
			m.agent.ClearSession()
			m.statusbar.SetSessionInfo("", "")
			return nil
		},
	},
	{
		Name:        "/quit",
		Description: "Exit tachi",
		handler:     func(m *Model) tea.Cmd { return tea.Quit },
	},
	{
		Name:        "/model",
		Description: "Switch provider/model",
		handler: func(m *Model) tea.Cmd {
			cfg := m.cfg
			if cfg == nil {
				freshCfg, err := config.Load()
				if err != nil {
					m.chatview.AddMessage(chatMessage{
						Role:    "assistant",
						Content: "No providers configured in ~/.tachi/config.yaml",
					})
					return nil
				}
				cfg = freshCfg
				m.cfg = cfg
			}
			if len(cfg.Providers) == 0 {
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: "No providers configured in ~/.tachi/config.yaml",
				})
				return nil
			}
			m.providerItems = cfg.Providers
			m.providerSelIdx = 0
			m.setState(stateSelectingModel)
			m.layout()
			return nil
		},
	},
	{
		Name:        "/commit",
		Description: "Ask LLM to write commit message and commit via Bash (git)",
		handler: func(m *Model) tea.Cmd {
			return m.sendCommitCommand()
		},
	},
	{
		Name:        "/init",
		Description: "Generate .tachi.md project context file via LLM",
		handler: func(m *Model) tea.Cmd {
			return m.sendInitCommand()
		},
	},
	{
		Name:        "/mcp",
		Description: "Manage MCP servers (list, toggle, reconnect, auth)",
		handler: func(m *Model) tea.Cmd {
			return m.handleMCPCommand()
		},
	},
	{
		Name:        "/sessions",
		Description: "Browse and reload previous sessions",
		handler: func(m *Model) tea.Cmd {
			sm := m.agent.SessionManager()
			if sm == nil {
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: "No session manager available",
				})
				return nil
			}
			sessions, err := sm.List()
			if err != nil {
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: fmt.Sprintf("Failed to list sessions: %v", err),
				})
				return nil
			}
			if len(sessions) == 0 {
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: "No sessions found",
				})
				return nil
			}
			m.sessionList = sessions
			m.sessionSelIdx = 0
			m.sessionScrollOff = 0
			// Pre-select the current session if it's in the list
			if curr := sm.Current(); curr != nil {
				for i, s := range sessions {
					if s.ID == curr.ID {
						m.sessionSelIdx = i
						break
					}
				}
			}
			// Ensure the pre-selected session is visible
			m.clampSessionScroll()
			m.setState(stateSelectingSession)
			m.layout()
			return nil
		},
	},
	{
		Name:        "/usage",
		Description: "Show session ID, token usage, cost, and tool call counts",
		handler: func(m *Model) tea.Cmd {
			return m.handleUsageCommand()
		},
	},
}

func matchCommands(prefix string) []Command {
	if prefix == "/" {
		out := make([]Command, len(commands))
		copy(out, commands)
		return out
	}
	var out []Command
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.Name, prefix) {
			out = append(out, cmd)
		}
	}
	return out
}

func findCommand(name string) *Command {
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i]
		}
	}
	return nil
}

// findCommandByPrefix matches commands that are prefixes of the input
// (e.g., "/mcp" matches "/mcp list", "/mcp toggle foo").
// Exact matches are preferred; this is used as a fallback.
func findCommandByPrefix(input string) *Command {
	for i := range commands {
		if input == commands[i].Name || strings.HasPrefix(input, commands[i].Name+" ") {
			return &commands[i]
		}
	}
	return nil
}

// mcpCommandTimeout is the timeout for MCP connect/reconnect operations
// triggered by slash commands.
const mcpCommandTimeout = 10 * time.Second

// handleMCPCommand dispatches to the appropriate subcommand handler based on
// the raw input stored in m.subcommandInput.
func (m *Model) handleMCPCommand() tea.Cmd {
	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	arg := ""
	if len(parts) > 2 {
		arg = parts[2]
	}

	switch sub {
	case "toggle":
		return m.mcpToggle(arg)
	case "reconnect":
		return m.mcpReconnect(arg)
	case "auth":
		return m.mcpAuth(arg)
	default:
		// "list" or bare "/mcp" — open the overlay
		return m.enterMCPOverlay()
	}
}

// mcpList shows all configured MCP servers with their status.
func (m *Model) mcpList() tea.Cmd {
	if len(m.mcpServers) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP servers configured in ~/.tachi/config.yaml",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("**MCP Servers:**\n\n")

	for _, srv := range m.mcpServers {
		enabled := srv.IsEnabled()
		connected := false
		if m.mcpManager != nil {
			connected = m.mcpManager.IsConnected(srv.Name)
		}

		status := "⚪ Disabled"
		if enabled {
			if connected {
				status = "🟢 Connected"
			} else {
				status = "🔴 Disconnected"
			}
		}

		transport := "?"
		switch srv.Type {
		case config.MCPTransportStdio:
			transport = fmt.Sprintf("stdio `%s`", srv.Command)
		case config.MCPTransportHTTP:
			transport = fmt.Sprintf("http `%s`", srv.URL)
		}

		fmt.Fprintf(&sb, "- **%s** (%s)\n  Transport: %s\n",
			srv.Name, status, transport)
		if srv.Profile != "" {
			fmt.Fprintf(&sb, "  Profile: `%s`\n", srv.Profile)
		}
		if srv.HasOAuth() {
			oauthStatus := "no token"
			if connected && m.mcpManager != nil {
				if h := m.mcpManager.GetOAuthHandler(srv.Name); h != nil {
					oauthStatus = "configured"
				}
			}
			fmt.Fprintf(&sb, "  OAuth: %s\n", oauthStatus)
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: sb.String(),
	})
	return nil
}

// mcpToggle enables or disables an MCP server by name.
func (m *Model) mcpToggle(name string) tea.Cmd {
	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/mcp toggle <name>` — specify a server name",
		})
		return nil
	}

	// Find the server config
	idx := -1
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** not found in config", name),
		})
		return nil
	}

	srv := &m.mcpServers[idx]
	wasEnabled := srv.Enabled
	if wasEnabled == nil || *wasEnabled {
		// Currently enabled → disable it
		disabled := false
		srv.Enabled = &disabled

		// Disconnect and unregister tools
		if m.mcpManager != nil {
			_ = m.mcpManager.Disconnect(srv.Name)
			m.unregisterMCPTools(srv.Name)
		}

		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** disabled", name),
		})
		return nil
	}

	// Currently disabled → enable it asynchronously
	enabled := true
	srv.Enabled = &enabled

	if m.mcpManager == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** enabled (no manager available)", name),
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Enabling MCP server **%s**...", name),
	})

	ch := make(chan string, 1)
	go m.connectAndRegisterMCP(srv, ch)
	return readNextMCPStatus(ch)
}

// mcpReconnect reconnects to a disconnected MCP server by name.
func (m *Model) mcpReconnect(name string) tea.Cmd {
	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/mcp reconnect <name>` — specify a server name",
		})
		return nil
	}

	if m.mcpManager == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP manager available",
		})
		return nil
	}

	// Find server config
	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** not found in config", name),
		})
		return nil
	}

	if !srv.IsEnabled() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** is disabled. Use `/mcp toggle %s` to enable it first", name, name),
		})
		return nil
	}

	// Unregister old tools, then reconnect asynchronously
	m.unregisterMCPTools(name)

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Reconnecting to MCP server **%s**...", name),
	})

	ch := make(chan string, 1)
	go m.reconnectAndRegisterMCP(srv, ch)
	return readNextMCPStatus(ch)
}

// mcpAuth initiates or completes the OAuth2 flow for an HTTP MCP server.
// Usage: /mcp auth <name> [redirect-url]
// If redirect-url is provided, it completes the manual flow.
// Otherwise it starts the interactive flow (browser callback → manual fallback).
func (m *Model) mcpAuth(name string) tea.Cmd {
	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/mcp auth <name>` — authorize an MCP server, or `/mcp auth <name> <redirect-url>` to complete manual flow",
		})
		return nil
	}

	if m.mcpManager == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP manager available",
		})
		return nil
	}

	// Find server config
	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** not found in config", name),
		})
		return nil
	}

	if srv.Type != config.MCPTransportHTTP {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("OAuth is only supported for HTTP MCP servers. **%s** is stdio.", name),
		})
		return nil
	}

	// Check if there's a redirect URL arg (manual flow completion)
	parts := strings.Fields(m.subcommandInput)
	if len(parts) > 3 {
		redirectURL := strings.Join(parts[3:], " ")
		return m.completeManualOAuth(srv, redirectURL)
	}

	// Start interactive flow
	return m.startInteractiveOAuth(srv)
}

// startInteractiveOAuth runs the OAuth flow asynchronously and reports results.
// Intermediate messages (e.g. "Open this URL") are sent to the TUI immediately
// via the channel so the user sees them even while the flow is still running.
func (m *Model) startInteractiveOAuth(srv *config.MCPServerConfig) tea.Cmd {
	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Starting OAuth authorization for **%s**...", srv.Name),
	})

	ch := make(chan string, 1)
	go func() {
		defer close(ch)

		// errFn sends a message to TUI immediately — needed because
		// startManualFlow may block waiting for a callback and we must
		// surface the "Open this URL" prompt right away.
		errFn := func(msg string) {
			select {
			case ch <- msg:
			default:
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := mcp.RunOAuthFlow(ctx, srv, errFn); err != nil {
			m.logger.Log("MCP: OAuth flow failed for %q: %v", srv.Name, err)
			// When the browser flow fails and we fall back to manual flow,
			// errFn has already delivered the instructions. An OAuthRequiredError
			// here would just repeat the same info — skip it.
			if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); !ok {
				ch <- fmt.Sprintf("OAuth failed for **%s**: %v", srv.Name, err)
			}
			return
		}

		ch <- fmt.Sprintf("OAuth authorization successful for **%s**! Reconnecting...", srv.Name)

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		tools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			ch <- fmt.Sprintf("Reconnect failed for **%s**: %v", srv.Name, err)
			return
		}

		for _, t := range tools {
			m.agent.RegisterTool(t)
			m.logger.Log("MCP: registered tool %s", t.Name())
		}

		ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s) ✓", srv.Name, len(tools))
	}()

	return readNextMCPStatus(ch)
}

// completeManualOAuth finishes the manual OAuth flow with the pasted redirect URL,
// then reconnects the server.
func (m *Model) completeManualOAuth(srv *config.MCPServerConfig, redirectURL string) tea.Cmd {
	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Completing OAuth authorization for **%s**...", srv.Name),
	})

	ch := make(chan string, 1)
	go func() {
		var msgs []string
		defer func() {
			if len(msgs) > 0 {
				ch <- strings.Join(msgs, "\n\n")
			}
			close(ch)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer cancel()

		if err := mcp.CompleteManualAuth(ctx, srv, redirectURL); err != nil {
			m.logger.Log("MCP: manual OAuth failed for %q: %v", srv.Name, err)
			msgs = append(msgs, fmt.Sprintf("OAuth authorization failed for **%s**: %v", srv.Name, err))
			return
		}

		msgs = append(msgs, fmt.Sprintf("OAuth authorization successful for **%s**! Reconnecting...", srv.Name))

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		tools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("Reconnect failed for **%s**: %v", srv.Name, err))
			return
		}

		for _, t := range tools {
			m.agent.RegisterTool(t)
			m.logger.Log("MCP: registered tool %s", t.Name())
		}

		msgs = append(msgs, fmt.Sprintf("MCP server **%s** connected with %d tool(s) ✓", srv.Name, len(tools)))
	}()

	return readNextMCPStatus(ch)
}

// readNextMCPStatus reads the next message from the channel and returns a
// mcpStatusMsg. If the channel is closed, returns nil (no more messages).
// This enables a goroutine to stream multiple status updates to the TUI.
func readNextMCPStatus(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return mcpStatusMsg{content: content, nextCh: ch}
	}
}

// connectAndRegisterMCP connects to a server and registers its tools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) connectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		m.logger.Log("MCP: failed to connect %q: %v", srv.Name, err)
		ch <- fmt.Sprintf("Failed to connect to **%s**: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
		m.logger.Log("MCP: registered tool %s", t.Name())
	}

	ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s)", srv.Name, len(tools))
}

// reconnectAndRegisterMCP reconnects to a server and registers its tools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) reconnectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect to **%s**: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
		m.logger.Log("MCP: registered tool %s", t.Name())
	}

	ch <- fmt.Sprintf("MCP server **%s** reconnected with %d tool(s)", srv.Name, len(tools))
}

// unregisterMCPTools removes all tools belonging to a server from the
// agent's tool registry. Tool names follow the pattern mcp__<server>__<tool>.
func (m *Model) unregisterMCPTools(serverName string) {
	prefix := fmt.Sprintf("mcp__%s__", serverName)
	for _, schema := range m.agent.ToolSchemas() {
		if strings.HasPrefix(schema.Name, prefix) {
			m.agent.UnregisterTool(schema.Name)
		}
	}
}

// handleUsageCommand builds a usage report and displays it in the chat view.
func (m *Model) handleUsageCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No active session",
		})
		return nil
	}

	report, err := agent.ComputeSessionUsage(sm, m.resolveModelPrice(), m.agent.ContextWindow())
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to compute usage: %v", err),
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("**📊 Session Usage**\n\n")

	// Session info
	sb.WriteString(fmt.Sprintf("**Session:** `%s`\n", report.Session.ID))
	provider := report.Session.Provider
	if provider == "" {
		provider = "(unknown)"
	}
	sb.WriteString(fmt.Sprintf("**Provider:** %s\n", provider))
	sb.WriteString(fmt.Sprintf("**Model:** %s\n", report.Session.Model))
	title := report.Session.Title
	if title == "" {
		title = "(untitled)"
	}
	sb.WriteString(fmt.Sprintf("**Title:** %s\n\n", title))

	// Token usage
	u := report.Usage
	sb.WriteString("**Token Usage**\n")
	sb.WriteString(fmt.Sprintf("  Input tokens: %s\n", formatTokens(u.InputTokens)))
	if u.CacheReadInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache read:  %s\n", formatTokens(u.CacheReadInputTokens)))
	}
	if u.CacheCreationInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache created: %s\n", formatTokens(u.CacheCreationInputTokens)))
	}
	cacheMissInput := u.InputTokens - u.CacheReadInputTokens
	if cacheMissInput < 0 {
		cacheMissInput = 0
	}
	if cacheMissInput != u.InputTokens {
		sb.WriteString(fmt.Sprintf("  ↳ Cache miss:  %s\n", formatTokens(cacheMissInput)))
	}
	sb.WriteString(fmt.Sprintf("  Output tokens: %s\n", formatTokens(u.OutputTokens)))
	sb.WriteString(fmt.Sprintf("  Total tokens:  %s\n", formatTokens(u.InputTokens+u.OutputTokens)))
	if report.ContextWindow > 0 && u.InputTokens > 0 {
		pct := float64(u.InputTokens) / float64(report.ContextWindow) * 100
		sb.WriteString(fmt.Sprintf("  Context: %s / %s (%.0f%%)\n", formatTokens(u.InputTokens), formatTokens(report.ContextWindow), pct))
	}

	// Cost
	sb.WriteString("\n**Cost**\n")
	if report.Cost <= 0 {
		sb.WriteString("  No pricing data available\n")
	} else {
		sb.WriteString(fmt.Sprintf("  Total cost: **¥%.4f**\n", report.Cost))
	}

	// Tool calls
	sb.WriteString("\n**Tool Calls**\n")
	names := slices.Sorted(maps.Keys(report.ToolCalls))
	for _, name := range names {
		st := report.ToolCalls[name]
		line := fmt.Sprintf("  - **%s**: %d call(s)", name, st.Count)
		if st.ErrCount > 0 {
			line += fmt.Sprintf(" (%d failed)", st.ErrCount)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("\n  **Total:** %d main + %d subagent = **%d** call(s)\n",
		report.MainCount, report.SubCount, report.MainCount+report.SubCount))

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: sb.String(),
	})
	return nil
}
