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
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
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
	"skill": func(m *Model) tea.Cmd {
		return m.handleSkillCommand()
	},
	"transcript": func(m *Model) tea.Cmd {
		return m.handleTranscriptCommand()
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
			m.logger.Log("MCP: manual OAuth failed for %q: %v", srv.Name, err)
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
		m.logger.Log("MCP: failed to connect %q: %v", srv.Name, err)
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
		Provider:                 report.Session.Provider,
		Model:                    report.Session.Model,
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
	expanded := ExpandAtReferences(text)

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
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, expanded.Text, m.systemPrompt, m.chatOpts)

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
		metas := store.List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skills reloaded — %d skill(s) found", len(metas)),
		})
		return nil

	default:
		// /skill <name> — activate a specific skill
		return m.sendSkillMessage(sub, "")
	}
}

// sendSkillMessage activates a skill and sends its instructions as a user message.
// skillName is the skill to activate. extraArgs are additional text from the
// command line (e.g., "main.go" from "/code-review main.go").
func (m *Model) sendSkillMessage(skillName string, extraArgs string) tea.Cmd {
	// Prevent duplicate activation within the same session.
	if m.agent.IsSkillActive(skillName) {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skill **%s** is already active in this session.", skillName),
		})
		return nil
	}

	msg, err := m.agent.ActivateSkill(skillName, extraArgs)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skill **%s** not found. Use `/skill` to see available skills.", skillName),
		})
		return nil
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
