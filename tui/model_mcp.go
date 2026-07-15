package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
)

// mcpOverlayMsg delivers an async status message to the MCP overlay.
type mcpOverlayMsg struct {
	content string
	nextCh  <-chan string
}

// readNextMCPOverlayMsg reads the next message from ch and returns an mcpOverlayMsg.
func readNextMCPOverlayMsg(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return mcpOverlayMsg{content: content, nextCh: ch}
	}
}

// enterMCPOverlay builds server items and opens the overlay.
func (m *Model) enterMCPOverlay() tea.Cmd {
	if len(m.mcpServers) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP servers configured in ~/.tachi/mcp.json or .tachi/mcp.json",
		})
		return nil
	}

	items := m.buildMCPServerItems()
	m.mcpView.SetServers(items)
	m.mcpView.SetSize(m.width, m.height)
	m.mcpView.SetMessage("")
	m.setState(stateManagingMCP)
	return nil
}

// exitMCPOverlay dismisses the overlay and returns to idle.
func (m *Model) exitMCPOverlay() {
	m.setState(stateIdle)
	m.layout()
}

// buildMCPServerItems collects current server state into display items,
// including both registered and deferred (not yet loaded) MCP tools.
func (m *Model) buildMCPServerItems() []MCPServerItem {
	items := make([]MCPServerItem, 0, len(m.mcpServers))
	for _, srv := range m.mcpServers {
		enabled := srv.IsEnabled()
		connected := false
		if m.mcpManager != nil {
			connected = m.mcpManager.IsConnected(srv.Name)
		}

		typeStr := string(srv.Type)
		prefix := fmt.Sprintf("mcp__%s__", srv.Name)
		seen := make(map[string]bool) // dedup by tool name

		// Build helper to convert schema + DeferredTool to MCPToolItem
		schemaToItem := func(name, desc string, params tools.ParametersSchema) MCPToolItem {
			itemParams := make([]MCPParamItem, 0, len(params.Properties))
			reqSet := make(map[string]bool, len(params.Required))
			for _, r := range params.Required {
				reqSet[r] = true
			}
			for pName, p := range params.Properties {
				itemParams = append(itemParams, MCPParamItem{
					Name:        pName,
					Type:        p.Type,
					Description: p.Description,
					Required:    reqSet[pName],
				})
			}
			return MCPToolItem{
				Name:        name,
				Description: desc,
				Parameters:  itemParams,
			}
		}

		var tools []MCPToolItem

		// 1. Collect registered tools (already loaded into LLM)
		for _, schema := range m.agent.ToolSchemas() {
			if after, ok := strings.CutPrefix(schema.Name, prefix); ok {
				toolName := after
				item := schemaToItem(toolName, schema.Description, schema.Parameters)
				item.Deferred = false
				tools = append(tools, item)
				seen[schema.Name] = true
			}
		}

		// 2. Collect deferred tools (not yet loaded — from deferred pool)
		if dp := m.agent.DeferredPool(); dp != nil {
			for _, dt := range dp.All() {
				if strings.HasPrefix(dt.Name, prefix) && !seen[dt.Name] {
					toolName := strings.TrimPrefix(dt.Name, prefix)
					item := schemaToItem(toolName, dt.Description, dt.Schema.Parameters)
					item.Deferred = true
					tools = append(tools, item)
					seen[dt.Name] = true
				}
			}
		}

		items = append(items, MCPServerItem{
			Name:      srv.Name,
			Type:      typeStr,
			Enabled:   enabled,
			Connected: connected,
			ToolCount: len(tools),
			Tools:     tools,
			HasOAuth:  srv.HasOAuth(),
			Profile:   srv.Profile,
		})
	}
	return items
}

// refreshMCPServerItems rebuilds server data and re-injects into the view,
// preserving selection position.
func (m *Model) refreshMCPServerItems() {
	oldSel := m.mcpView.SelectedServer()
	items := m.buildMCPServerItems()
	m.mcpView.SetServers(items)
	// Try to restore selection
	if oldSel != "" {
		for i := range items {
			if items[i].Name == oldSel {
				m.mcpView.selIdx = i
				break
			}
		}
	}
	m.mcpView.SetSize(m.width, m.height)
}

// handleKeyManagingMCP dispatches actions from the overlay.
func (m *Model) handleKeyManagingMCP(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	act := m.mcpView.HandleKey(msg.String())

	switch act {
	case MCPActionDismiss:
		m.exitMCPOverlay()
		return m, nil

	case MCPActionToggle:
		name := m.mcpView.SelectedServer()
		if name == "" {
			return m, nil
		}
		return m, m.mcpOverlayToggle(name)

	case MCPActionReconnect:
		name := m.mcpView.SelectedServer()
		if name == "" || m.mcpManager == nil {
			return m, nil
		}
		return m, m.mcpOverlayReconnect(name)

	case MCPActionAuth:
		name := m.mcpView.SelectedServer()
		if name == "" || m.mcpManager == nil {
			return m, nil
		}
		return m, m.mcpOverlayAuth(name)
	}

	return m, nil
}

// mcpOverlayToggle handles toggle from within the overlay.
func (m *Model) mcpOverlayToggle(name string) tea.Cmd {
	idx := -1
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	srv := &m.mcpServers[idx]
	wasEnabled := srv.Enabled
	if wasEnabled == nil || *wasEnabled {
		// Disable
		disabled := false
		srv.Enabled = &disabled
		if m.mcpManager != nil {
			_ = m.mcpManager.Disconnect(srv.Name)
			m.agent.UnregisterMCPServer(srv.Name)
		}
		m.refreshMCPServerItems()
		m.mcpView.SetMessage(fmt.Sprintf("● %s disabled", name))
		return nil
	}

	// Enable asynchronously
	enabled := true
	srv.Enabled = &enabled

	if m.mcpManager == nil {
		m.mcpView.SetMessage(fmt.Sprintf("● %s enabled (no manager)", name))
		m.refreshMCPServerItems()
		return nil
	}

	m.mcpView.SetMessage(fmt.Sprintf("Enabling %s...", name))

	ch := make(chan string, 1)
	go m.mcpOverlayConnectAndRegister(srv, ch)
	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayReconnect handles reconnect from within the overlay.
func (m *Model) mcpOverlayReconnect(name string) tea.Cmd {
	if m.mcpManager == nil {
		m.mcpView.SetMessage("No MCP manager available")
		return nil
	}

	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	if !srv.IsEnabled() {
		m.mcpView.SetMessage(fmt.Sprintf("%s is disabled — toggle first", name))
		return nil
	}

	m.agent.UnregisterMCPServer(name)
	m.mcpView.SetMessage(fmt.Sprintf("Reconnecting %s...", name))

	ch := make(chan string, 1)
	go m.mcpOverlayReconnectAndRegister(srv, ch)
	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayAuth starts the OAuth flow from within the overlay.
func (m *Model) mcpOverlayAuth(name string) tea.Cmd {
	if m.mcpManager == nil {
		m.mcpView.SetMessage("No MCP manager available")
		return nil
	}

	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.mcpView.SetMessage(fmt.Sprintf("Server %s not found", name))
		return nil
	}

	if srv.Type != config.MCPTransportHTTP {
		m.mcpView.SetMessage(fmt.Sprintf("OAuth only for HTTP servers (%s is stdio)", name))
		return nil
	}

	m.mcpView.SetMessage(fmt.Sprintf("Starting OAuth for %s...", name))

	ch := make(chan string, 8)
	go func() {
		defer close(ch)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		statusFn := func(msg string) {
			select {
			case ch <- msg:
			case <-ctx.Done():
			}
		}

		if err := mcp.RunOAuthFlow(ctx, srv, statusFn); err != nil {
			m.logger.Logf(context.Background(), "MCP: OAuth flow failed for %q: %v", srv.Name, err)
			if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); !ok {
				ch <- fmt.Sprintf("OAuth failed: %v", err)
			}
			return
		}

		// Collect all servers that share the same token (same host).
		tokenKey := srv.TokenStorageName()
		var siblings []*config.MCPServerConfig
		for i := range m.mcpServers {
			s := &m.mcpServers[i]
			if s.Name == srv.Name {
				continue
			}
			if s.IsEnabled() && s.Type == config.MCPTransportHTTP && s.TokenStorageName() == tokenKey {
				siblings = append(siblings, s)
			}
		}

		ch <- fmt.Sprintf("OAuth OK for %s — reconnecting...", srv.Name)

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		mcpTools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			ch <- fmt.Sprintf("Reconnect failed: %v", err)
			return
		}

		count := m.agent.AddDeferredMCPTools(mcpTools)
		ch <- fmt.Sprintf("● %s connected with %d tool(s)", srv.Name, count)

		// Reconnect sibling servers sharing the same OAuth token.
		for _, sib := range siblings {
			sibCtx, sibCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
			sibTools, sibErr := m.mcpManager.Reconnect(sibCtx, sib)
			sibCancel()
			if sibErr != nil {
				m.logger.Logf(context.Background(), "MCP: sibling reconnect %q failed: %v", sib.Name, sibErr)
				continue
			}
			sibCount := m.agent.AddDeferredMCPTools(sibTools)
			ch <- fmt.Sprintf("● %s connected with %d tool(s) (shared token)", sib.Name, sibCount)
		}
	}()

	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayConnectAndRegister connects to a server and adds its tools to the
// deferred pool (not directly registered), so the LLM learns about them
// via the <available-deferred-tools> system reminder and MCPSearchTools.
func (m *Model) mcpOverlayConnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	mcpTools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to connect %s: %v", srv.Name, err)
		return
	}

	count := m.agent.AddDeferredMCPTools(mcpTools)

	ch <- fmt.Sprintf("● %s connected with %d tool(s) — MCPSearchTools 可搜索加载", srv.Name, count)
}

// mcpOverlayReconnectAndRegister reconnects to a server and adds its tools to the
// deferred pool (not directly registered), so the LLM learns about them
// via the <available-deferred-tools> system reminder and MCPSearchTools.
func (m *Model) mcpOverlayReconnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	mcpTools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect %s: %v", srv.Name, err)
		return
	}

	count := m.agent.AddDeferredMCPTools(mcpTools)

	ch <- fmt.Sprintf("● %s reconnected with %d tool(s) — MCPSearchTools 可搜索加载", srv.Name, count)
}
