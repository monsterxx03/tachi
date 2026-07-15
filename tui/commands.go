package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// InitPromptTemplate is the prompt sent to LLM to generate .tachi.md.
// Deprecated: use cmds.InitPromptTemplate instead.
var InitPromptTemplate = cmds.InitPromptTemplate

type Command struct {
	Name        string
	Description string
	handler     func(*Model) tea.Cmd
}

var commandHandlers = map[string]func(*Model) tea.Cmd{
	"new": func(m *Model) tea.Cmd {
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		m.agent.StoreSessionMemory()
		m.history = nil
		m.chatview.Clear()
		m.agent.ClearSession()
		// Reset cost and usage so statusbar shows clean state.
		m.totalUsage = llm.Usage{}
		m.sessionCost = 0
		m.statusbar.SetUsage(nil)
		m.statusbar.SetCost(0)
		m.statusbar.SetSessionInfo("", "")
		return nil
	},
	"quit": func(m *Model) tea.Cmd {
		m.agent.StoreSessionMemory()
		return tea.Quit
	},
	"model": func(m *Model) tea.Cmd {
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
	"commit": func(m *Model) tea.Cmd {
		return m.sendCommitCommand()
	},
	"compact": func(m *Model) tea.Cmd {
		return m.handleCompactCommand()
	},
	"init": func(m *Model) tea.Cmd {
		return m.sendInitCommand()
	},
	"mcp": func(m *Model) tea.Cmd {
		return m.handleMCPCommand()
	},
	"sessions": func(m *Model) tea.Cmd {
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
			if idx := slices.IndexFunc(sessions, func(s *session.Session) bool {
				return s.ID == curr.ID
			}); idx >= 0 {
				m.sessionSelIdx = idx
			}
		}
		// Ensure the pre-selected session is visible
		m.clampSessionScroll()
		m.setState(stateSelectingSession)
		m.layout()
		return nil
	},
	"usage": func(m *Model) tea.Cmd {
		return m.handleUsageCommand()
	},
	"review": func(m *Model) tea.Cmd {
		return m.sendReviewCommand()
	},
	"skill": func(m *Model) tea.Cmd {
		return m.handleSkillCommand()
	},
	"transcript": func(m *Model) tea.Cmd {
		return m.handleTranscriptCommand()
	},
	"dream": func(m *Model) tea.Cmd {
		return m.handleDreamCommandDispatch()
	},
	"research": func(m *Model) tea.Cmd {
		return m.handleResearchCommand()
	},
}

func matchCommands(prefix string) []Command {
	stripped := strings.TrimPrefix(prefix, "/")
	defs := cmds.MatchPrefixForMode(stripped, cmds.ModeTUI)
	var out []Command
	for _, d := range defs {
		if h, ok := commandHandlers[d.Name]; ok {
			out = append(out, Command{
				Name:        "/" + d.Name,
				Description: d.Description,
				handler:     h,
			})
		}
	}
	return out
}

func findCommand(name string) *Command {
	if !strings.HasPrefix(name, "/") {
		return nil
	}
	stripped := strings.TrimPrefix(name, "/")
	def := cmds.Find(stripped)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeTUI) {
		return nil
	}
	h, ok := commandHandlers[stripped]
	if !ok {
		return nil
	}
	return &Command{
		Name:        "/" + def.Name,
		Description: def.Description,
		handler:     h,
	}
}

// findCommandByPrefix matches commands that are prefixes of the input
// (e.g., "/mcp" matches "/mcp list", "/mcp toggle foo").
// Exact matches are preferred; this is used as a fallback.
func findCommandByPrefix(input string) *Command {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	stripped := strings.TrimPrefix(input, "/")
	def := cmds.FindByPrefix(stripped)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeTUI) {
		return nil
	}
	h, ok := commandHandlers[def.Name]
	if !ok {
		return nil
	}
	return &Command{
		Name:        "/" + def.Name,
		Description: def.Description,
		handler:     h,
	}
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
			m.agent.UnregisterMCPServer(srv.Name)
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

	// Full cleanup of registry, deferred pool, and discovered set,
	// then reconnect asynchronously
	m.agent.UnregisterMCPServer(name)

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
			m.logger.Logf(context.Background(), "MCP: OAuth flow failed for %q: %v", srv.Name, err)
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

		mcpTools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			ch <- fmt.Sprintf("Reconnect failed for **%s**: %v", srv.Name, err)
			return
		}

		count := m.agent.AddDeferredMCPTools(mcpTools)

		ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s) — 使用 MCPSearchTools 搜索并加载", srv.Name, count)
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
			m.logger.Logf(context.Background(), "MCP: manual OAuth failed for %q: %v", srv.Name, err)
			msgs = append(msgs, fmt.Sprintf("OAuth authorization failed for **%s**: %v", srv.Name, err))
			return
		}

		msgs = append(msgs, fmt.Sprintf("OAuth authorization successful for **%s**! Reconnecting...", srv.Name))

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		mcpTools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("Reconnect failed for **%s**: %v", srv.Name, err))
			return
		}

		count := m.agent.AddDeferredMCPTools(mcpTools)

		msgs = append(msgs, fmt.Sprintf("MCP server **%s** connected with %d tool(s) — 使用 MCPSearchTools 搜索并加载", srv.Name, count))
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

// connectAndRegisterMCP connects to a server and adds its tools to the
// deferred pool (not directly registered), so the LLM learns about them
// via the <available-deferred-tools> system reminder and MCPSearchTools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) connectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	mcpTools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		m.logger.Logf(context.Background(), "MCP: failed to connect %q: %v", srv.Name, err)
		ch <- fmt.Sprintf("Failed to connect to **%s**: %v", srv.Name, err)
		return
	}

	count := m.agent.AddDeferredMCPTools(mcpTools)

	ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s) — 使用 MCPSearchTools 搜索并加载", srv.Name, count)
}

// reconnectAndRegisterMCP reconnects to a server and adds its tools to the
// deferred pool (not directly registered), so the LLM learns about them
// via the <available-deferred-tools> system reminder and MCPSearchTools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) reconnectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	mcpTools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect to **%s**: %v", srv.Name, err)
		return
	}

	count := m.agent.AddDeferredMCPTools(mcpTools)

	ch <- fmt.Sprintf("MCP server **%s** reconnected with %d tool(s) — 使用 MCPSearchTools 搜索并加载", srv.Name, count)
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

	// Convert tool call stats to shared type
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
		EstimatedInputTokens:     m.totalUsage.LastInputTokens,
		Cost:                     report.Cost,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
	}
	// Populate token breakdown from the agent
	info.EstBreakdown = m.agent.LastTokenBreakdown()

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: cmds.FormatUsageReport(info),
	})
	return nil
}

// handleTranscriptCommand generates an HTML transcript report for the current
// session, opens it in the default browser, and shows the result in the chat view.
// If the browser cannot be opened, it displays the file path instead.
func (m *Model) handleTranscriptCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No active session — start a conversation first",
		})
		return nil
	}

	// Load messages for the current session
	msgs, err := sm.LoadMessages()
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to load session messages: %v", err),
		})
		return nil
	}
	if len(msgs) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No messages in current session yet — send a message first",
		})
		return nil
	}

	curr := sm.Current()

	// Build report data from session messages
	data := render.BuildReportDataFromMessages(curr, msgs)
	html, err := render.GenerateHTML(data)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to generate transcript HTML: %v", err),
		})
		return nil
	}

	path, err := render.OpenInBrowser(html, curr.ID)
	if err != nil {
		// Browser couldn't be opened — show the file path in chat
		m.chatview.AddMessage(chatMessage{
			Role: "assistant",
			Content: fmt.Sprintf(
				"**📋 Transcript Report**\n\nBrowser could not be opened automatically.\n\nReport saved to:\n`%s`",
				path,
			),
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role: "assistant",
		Content: fmt.Sprintf(
			"**📋 Transcript Report**\n\nSession: `%s`\nOpened: `%s`",
			curr.Title, path,
		),
	})
	return nil
}

// handleDreamCommandDispatch parses /dream subcommands and dispatches.
//
//	/dream or /dream run  → trigger AutoDream
//	/dream status         → show current orchestrator status
func (m *Model) handleDreamCommandDispatch() tea.Cmd {
	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "status":
		return m.handleDreamStatusCommand()
	default:
		// /dream or /dream run — trigger dream.
		return m.handleDreamCommand()
	}
}

// handleDreamStatusCommand shows the current dream orchestrator status.
func (m *Model) handleDreamStatusCommand() tea.Cmd {
	if m.dreamOrch == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "🧠 **当前没有正在运行的 AutoDream**\n\n使用 `/dream` 触发新的记忆整合。",
		})
		return nil
	}

	status := m.dreamOrch.Status()
	if status.Running == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "🧠 **AutoDream 空闲中**\n\n没有正在运行的 domain，可能是等待 goroutine 启动或已结束。",
		})
		return nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("🧠 **AutoDream 状态** — %d 个 domain 正在处理：\n\n", status.Running))

	for i, d := range status.Domains {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("**%s**", d.Domain))
		if d.Root != "" {
			b.WriteString(fmt.Sprintf(" — `%s`", d.Root))
		}
		b.WriteString("\n")

		runningSince := time.Since(d.StartedAt).Round(time.Second)
		b.WriteString(fmt.Sprintf("- 状态：运行中（已进行 %v）\n", runningSince))
		b.WriteString(fmt.Sprintf("- 处理中：%d 个 session\n", d.ActiveCount))

		last := d.LastState
		if !last.LastDreamAt.IsZero() {
			lastDreamAgo := time.Since(last.LastDreamAt).Round(time.Minute)
			b.WriteString(fmt.Sprintf("- 上次完成：%v 前\n", lastDreamAgo))
			b.WriteString(fmt.Sprintf("- 上次结果：%d sessions, %d facts, %d superseded, %d pruned\n",
				last.SessionsDreamed, last.FactsAdded, last.FactsSuperseded, last.FactsPruned))
		} else {
			b.WriteString("- 上次完成：首次运行\n")
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: b.String(),
	})
	return nil
}

// handleDreamCommand triggers AutoDream memory consolidation synchronously
// (not via SystemScheduler). It lists all sessions, runs the dream
// orchestrator, and streams progress/results back to the chat view asynchronously.
func (m *Model) handleDreamCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No session manager available — start a conversation first",
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
			Content: "No sessions found — nothing to consolidate yet",
		})
		return nil
	}

	provider := m.agent.Provider()
	if provider == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No LLM provider configured — cannot run dream",
		})
		return nil
	}

	// Check if dream is already running.
	if m.dreamOrch != nil {
		if s := m.dreamOrch.Status(); s.Running > 0 {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "🧠 **AutoDream 正在运行中**\n\n请等待当前 dream 完成后再触发，或使用 `/dream status` 查看进度。",
			})
			return nil
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("🧠 **AutoDream 已触发** — 正在整合 %d 个 session 的记忆...\n\n使用 `/dream status` 查看实时进度。", len(sessions)),
	})

	ch := make(chan string, 5) // buffer 5 for status + sentinel
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)

	// Store orchestrator reference before goroutine starts so /dream status can query it.
	m.dreamOrch = dream.NewOrchestrator(dream.Config{
		Logger:        m.logger,
		MaxConcurrent: m.cfg.Dream.MaxConcurrent,
	})
	o := m.dreamOrch // local reference for goroutine

	go func() {
		defer cancel()

		cfg := m.cfg
		var dreamProvider string
		maxIter := 30
		maxMessageChars := 2000
		if cfg != nil {
			dreamProvider = cfg.Dream.Provider
			if cfg.Dream.SubagentMaxIter > 0 {
				maxIter = cfg.Dream.SubagentMaxIter
			}
			if cfg.Dream.MaxMessageChars > 0 {
				maxMessageChars = cfg.Dream.MaxMessageChars
			}
		}

		var providers []config.ProviderConfig
		if cfg != nil {
			providers = cfg.Providers
		}

		runFn := func(ctx context.Context, plan dream.Plan) (dream.State, error) {
			// Use a fresh session manager so Load(id) doesn't mutate
			// the TUI's current-session pointer.
			dreamSM, smErr := session.NewManager(nil)
			loadMessages := func(id string) ([]session.Message, error) {
				if smErr != nil {
					return nil, smErr
				}
				if _, err := dreamSM.Load(id); err != nil {
					return nil, err
				}
				return dreamSM.LoadMessages()
			}

			return dream.RunDream(ctx, plan, dream.RunConfig{
				FallbackProvider: provider,
				DreamProvider:    dreamProvider,
				Providers:        providers,
				MaxIter:          maxIter,
				MaxTokens:        m.chatOpts.MaxTokens,
				MaxMessageChars:  maxMessageChars,
				Logger:           m.logger,
			}, loadMessages)
		}

		if err := o.Run(ctx, sessions, runFn); err != nil {
			ch <- fmt.Sprintf("🧠 **Dream 失败**: %v", err)
		} else {
			ch <- "🧠 **Dream 完成** — 记忆已整合"
		}

		// Signal completion so the TUI can clean up the orchestrator reference.
		ch <- dreamDoneSentinel
		close(ch)
	}()

	return readNextDreamStatus(ch)
}

// dreamDoneSentinel is a sentinel message sent through the dream status channel
// to signal that the orchestrator has completed and should be cleaned up.
// It contains a null byte which cannot appear in normal status messages.
const dreamDoneSentinel = "\x00"

// readNextDreamStatus reads the next message from the channel and returns a
// dreamStatusMsg. If the channel is closed, returns nil.
func readNextDreamStatus(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return dreamStatusMsg{content: content, nextCh: ch}
	}
}

// readNextResearchStatus reads the next message from the channel and returns a
// researchStatusMsg. When the channel is closed (research complete), returns
// researchDoneMsg so the model can reset isResearching and allow input.
func readNextResearchStatus(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return researchDoneMsg{}
		}
		return researchStatusMsg{content: content, nextCh: ch}
	}
}

// handleResearchCommand handles /research <topic> [--depth N] [--breadth N]
func (m *Model) handleResearchCommand() tea.Cmd {
	parsed := cmds.ParseResearchArgs(m.subcommandInput)
	if parsed.Topic == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/research <topic> [--depth N] [--breadth N]`",
		})
		return nil
	}

	cfg := m.cfg
	if cfg == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No configuration found. Run `tachi init` first.",
		})
		return nil
	}

	if parsed.Depth <= 0 {
		parsed.Depth = cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = cfg.DeepResearch.DefaultBreadth
	}

	engine, err := m.agent.NewDeepResearch(cfg)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to create research engine: %v", err),
		})
		return nil
	}
	if engine == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Deep Research is not available (engine creation returned nil).",
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "user",
		Content: fmt.Sprintf("/research %s", parsed.Topic),
	})
	m.chatview.AddMessage(chatMessage{
		Role: "assistant",
		Content: fmt.Sprintf("🔬 **深度研究已启动**\n\n**主题**: %s\n**深度**: %d | **广度**: %d\n\n正在生成搜索查询、并行搜索、提取信息... 这可能需要几分钟，请稍候。\n\n进度消息会实时显示在此处。",
			parsed.Topic, parsed.Depth, parsed.Breadth),
	})

	ch := make(chan string, 100)
	researchCtx, researchCancel := context.WithTimeout(context.Background(), cfg.DeepResearch.Timeout+time.Minute)

	m.isResearching = true
	m.cancelFunc = researchCancel

	go func() {
		defer researchCancel()
		defer close(ch)

		report, runErr := engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			select {
			case ch <- msg:
			default:
			}
		})
		if runErr != nil {
			ch <- fmt.Sprintf("❌ **研究失败**: %v", runErr)
			return
		}

		ch <- fmt.Sprintf("✅ **研究完成**\n\n---\n\n%s", report)
	}()

	return readNextResearchStatus(ch)
}

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

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	// Set up steer channel so pending input can be injected at tool-call boundaries.
	m.steerRespCh = make(chan string)
	m.agent.SetSteerChannel(m.steerRespCh)

	// Attach images (if any) for the next RunConversationStream call.
	if len(expanded.Images) > 0 {
		m.agent.SetPendingImages(expanded.Images)
	}

	m.streamGen++
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, expanded.Text, m.effectiveSystemPrompt(), m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
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

	// Save tool registry: /commit should only use the Bash tool.
	// Save all tools, then unregister everything except Bash.
	m.savedTools = m.agent.SaveToolRegistry()
	for _, name := range m.agent.ToolNames() {
		if name != tools.ToolNameBash {
			m.agent.UnregisterTool(name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	commitProvider := m.agent.CommitProvider()
	commitModel := m.agent.Model()

	// Disable thinking for /commit: the commit message task is simple and
	// avoiding thinking saves tokens/latency.
	commitOpts := m.chatOpts
	thinkingDisabled := false
	commitOpts.Thinking = &thinkingDisabled

	m.streamGen++
	m.eventCh = m.agent.RunOneOffStream(ctx, commitProvider, m.systemPrompt,
		cmds.CommitUserPrompt(commitModel), commitOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// sendReviewCommand uses Fork() to create an isolated child agent with limited
// tools (Bash, ReadFile, Glob, Grep), then runs a code review of the current
// repo changes. The forked agent does NOT inherit conversation history or
// session context — it gets a clean prompt to review git diff output.
func (m *Model) sendReviewCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/review"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Save conversation history so we can restore it after the one-off
	// review run completes (the forked agent doesn't touch m.history, but
	// setting savedHistory marks this as a one-off for TurnComplete handling).
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)

	// Resolve review config — fall back to defaults when not configured.
	rc := m.resolveReviewConfig()

	// Fork a child agent with configurable tools.
	forked := m.agent.Fork(agent.ForkConfig{
		Provider:      rc.provider,
		MaxIterations: rc.maxIterations,
		AllowedTools:  rc.allowedTools,
		Logger:        m.agent.Logger(),
	})
	m.forkedAgent = forked

	// Apply thinking config: if review config explicitly sets thinking,
	// override chatOpts; otherwise inherit (default: disabled).
	reviewOpts := m.chatOpts
	if rc.thinking != nil {
		reviewOpts.Thinking = rc.thinking
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	m.eventCh = forked.Agent().RunOneOffStream(ctx, rc.provider,
		m.systemPrompt, cmds.ReviewUserPrompt(), reviewOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// reviewResolved holds the resolved review configuration after applying
// config defaults and provider resolution.
type reviewResolved struct {
	provider      llm.Provider
	maxIterations int
	allowedTools  []string
	thinking      *bool
}

// resolveReviewConfig reads the review config from m.cfg and resolves the
// provider, falling back to the main provider with sensible defaults.
func (m *Model) resolveReviewConfig() reviewResolved {
	// Determine allowed tools (slice can't use `default` tag, handle in code).
	var allowedTools []string
	if m.cfg != nil && len(m.cfg.Review.AllowedTools) > 0 {
		allowedTools = m.cfg.Review.AllowedTools
	} else {
		allowedTools = cmds.DefaultReviewAllowedTools()
	}

	// MaxIterations and Thinking are populated by defaults.Set() from struct tags.
	maxIter := cmds.DefaultReviewMaxIterations
	thinking := new(bool)
	if m.cfg != nil {
		maxIter = m.cfg.Review.MaxIterations
		thinking = m.cfg.Review.Thinking
	}

	// Use pre-resolved review provider from agent (if configured), or fall
	// back to main provider.
	provider := m.agent.Provider()
	if rp := m.agent.ReviewProvider(); rp != nil {
		provider = rp
	}

	return reviewResolved{
		provider:      provider,
		maxIterations: maxIter,
		allowedTools:  allowedTools,
		thinking:      thinking,
	}
}

// sendInitCommand sends the init prompt to LLM to generate .tachi.md
func (m *Model) sendInitCommand() tea.Cmd {
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/init"})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
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

	// 3.5 Store current session memory before compaction
	m.agent.StoreCompactMemory()

	// 4. Clear tools so the LLM doesn't call tools during compact.
	// Prompt also instructs "不要调用任何工具" as a double safeguard.
	m.savedTools = m.agent.SaveToolRegistry()
	m.agent.ClearToolRegistry()

	// 5. Build compact instruction (no history — LLM sees history as context)
	instruction := cmds.BuildCompactInstruction()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	// Use RunConversationStream so the LLM sees the current session as
	// structured history (role alternation, tool calls, etc.).
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, instruction, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// formatCompactSummary formats the compact result for display in the chatview.
func formatCompactSummary(summary string, oldMsgCount int) string {
	var sb strings.Builder
	sb.WriteString("🔍 **对话已压缩**\n\n")
	sb.WriteString(fmt.Sprintf("旧消息数: %d 条\n", oldMsgCount))
	sb.WriteString("\n---\n\n")
	sb.WriteString(summary)
	sb.WriteString("\n\n---\n")
	return sb.String()
}

// rollbackCompact restores the pre-compact state (history + tools) and displays
// an error in the chatview. Used when the compact LLM call fails or
// FinalizeCompact returns an error.
func (m *Model) rollbackCompact(errMsg string) {
	m.history = m.savedHistory
	m.savedHistory = nil
	if m.savedTools != nil {
		m.agent.RestoreToolRegistry(m.savedTools)
		m.savedTools = nil
	}
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

	if m.savedTools != nil {
		m.agent.RestoreToolRegistry(m.savedTools)
		m.savedTools = nil
	}

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
func (m *Model) handleSkillCommand() tea.Cmd {
	store := m.agent.SkillStore()
	if store == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Skill system not available",
		})
		return nil
	}

	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "", "list":
		metas := store.List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: cmds.FormatSkillList(metas),
		})
		return nil

	case "reload":
		// Re-create the store to pick up new/modified skills
		m.agent.ReloadSkills()
		m.refreshSkillCompletions()
		metas := m.agent.SkillStore().List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skills reloaded — %d skill(s) found", len(metas)),
		})
		return nil

	default:
		// /skill <name> [args] — activate a specific skill
		extraArgs := ""
		if len(parts) > 2 {
			extraArgs = strings.Join(parts[2:], " ")
		}
		return m.sendSkillMessage(sub, extraArgs)
	}
}

// sendSkillMessage activates a skill and sends its instructions as a user message.
// skillName is the skill to activate. extraArgs are additional text from the
// command line (e.g., "main.go" from "/code-review main.go").
// If the skill is already active in this session, only a short directive
// message is injected (the full skill body is already in context).
func (m *Model) sendSkillMessage(skillName string, extraArgs string) tea.Cmd {
	var msg string
	if m.agent.IsSkillActive(skillName) {
		// Skill body already in conversation context — send directive only.
		msg = skill.BuildDirectiveMessage(skillName, extraArgs)
	} else {
		var err error
		msg, err = m.agent.ActivateSkill(skillName, extraArgs)
		if err != nil {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("Skill **%s** not found. Use `/skill` to see available skills.", skillName),
			})
			return nil
		}
	}

	// Add the activation message as a system-style user message
	m.chatview.AddMessage(chatMessage{
		Role:    "user",
		Content: fmt.Sprintf("/%s %s", skillName, extraArgs),
	})

	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	m.streamGen++
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, msg, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}
