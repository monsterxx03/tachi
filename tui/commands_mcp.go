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
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

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
	case "toggle", "reconnect", "auth":
		if m.mcpSwitching {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "An MCP profile switch is in progress — try again in a moment",
			})
			return nil
		}
	}
	switch sub {
	case "toggle":
		return m.mcpToggle(arg)
	case "reconnect":
		return m.mcpReconnect(arg)
	case "auth":
		return m.mcpAuth(arg)
	case "profile":
		return m.mcpProfile(arg)
	default:
		// "list" or bare "/mcp" — open the overlay
		return m.enterMCPOverlay()
	}
}

// mcpProfile handles `/mcp profile [name]`. Without a name it lists the
// available profiles (mcp.<name>.json in global + project scope) and marks
// the active one. With a name it switches the active profile at runtime —
// in-memory only, reverts on restart (same semantics as /model).
func (m *Model) mcpProfile(name string) tea.Cmd {
	if m.mcpSwitching {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "An MCP profile switch is already in progress — try again in a moment",
		})
		return nil
	}

	workDir := config.FindProjectRoot()
	available := config.ListMCPProfiles(workDir)

	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: agent.FormatMCPProfileList(available, m.cfg.ActiveMCPProfile),
		})
		return nil
	}

	if !slices.Contains(available, name) {
		content := fmt.Sprintf("MCP profile **%s** not found.", name)
		if len(available) > 0 {
			content += fmt.Sprintf(" Available: %s", strings.Join(available, ", "))
		} else {
			content += " No mcp.<name>.json files exist yet."
		}
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: content})
		return nil
	}

	if name == m.cfg.ActiveMCPProfile {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP profile **%s** is already active", name),
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Switching MCP profile to **%s**...", name),
	})

	m.mcpSwitching = true
	ch := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		// Both channels always close, on every path: close(ch) terminates
		// the readNextMCPStatus chain (without it the reader goroutine would
		// leak), close(done) triggers the mcpProfileSwitchedMsg resync that
		// clears m.mcpSwitching. LIFO: ch closes before done, so the status
		// message is queued before the resync runs.
		defer close(done)
		defer close(ch)

		// No outer deadline here — SwitchMCPProfile derives the batch
		// budget from the servers' own per-server timeouts (mcp.json).
		res, err := m.agent.SwitchMCPProfile(context.Background(), name, workDir)
		if err != nil {
			m.logger.Error(context.Background(), "MCP: profile switch failed", err, "profile", name)
			ch <- fmt.Sprintf("Failed to switch MCP profile to **%s**: %v", name, err)
			return
		}
		ch <- agent.FormatMCPSwitchResult(res)
	}()
	// Batch: stream the status message AND notify the update loop to re-sync
	// m.mcpServers once the goroutine is done (msg handled on main goroutine).
	return tea.Batch(readNextMCPStatus(ch), waitMCPProfileSwitched(done))
}

// waitMCPProfileSwitched returns a tea.Cmd yielding mcpProfileSwitchedMsg
// once the switch goroutine has finished (its channel closes with it).
func waitMCPProfileSwitched(done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-done
		return mcpProfileSwitchedMsg{}
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
			m.logger.Error(context.Background(), "MCP: OAuth flow failed", err, "server", srv.Name)
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
			m.logger.Error(context.Background(), "MCP: manual OAuth failed", err, "server", srv.Name)
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
// via the <system-reminder> deferred-tools hint and MCPSearchTools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) connectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	mcpTools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		m.logger.Error(context.Background(), "MCP: failed to connect", err, "server", srv.Name)
		ch <- fmt.Sprintf("Failed to connect to **%s**: %v", srv.Name, err)
		return
	}

	count := m.agent.AddDeferredMCPTools(mcpTools)

	ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s) — 使用 MCPSearchTools 搜索并加载", srv.Name, count)
}

// reconnectAndRegisterMCP reconnects to a server and adds its tools to the
// deferred pool (not directly registered), so the LLM learns about them
// via the <system-reminder> deferred-tools hint and MCPSearchTools.
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

// handleThinkingCommand handles /thinking: shows the current session's
// thinking level, or sets a new one. The setting is per-session — it is
// persisted to the session's meta.json and only affects the current session.
//
//	/thinking              → show current level + valid options
//	/thinking <level>      → set level: none | low | medium | high | xhigh | max | default
