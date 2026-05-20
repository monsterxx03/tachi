package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent/mcp"
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
			Content: "No MCP servers configured in ~/.tachi/config.yaml",
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

// buildMCPServerItems collects current server state into display items.
func (m *Model) buildMCPServerItems() []MCPServerItem {
	items := make([]MCPServerItem, 0, len(m.mcpServers))
	for _, srv := range m.mcpServers {
		enabled := srv.IsEnabled()
		connected := false
		if m.mcpManager != nil {
			connected = m.mcpManager.IsConnected(srv.Name)
		}

		typeStr := string(srv.Type)

		// Gather tools for this server
		prefix := fmt.Sprintf("mcp__%s__", srv.Name)
		var tools []MCPToolItem
		for _, schema := range m.agent.ToolSchemas() {
			if strings.HasPrefix(schema.Name, prefix) {
				params := make([]MCPParamItem, 0, len(schema.Parameters.Properties))
				reqSet := make(map[string]bool, len(schema.Parameters.Required))
				for _, r := range schema.Parameters.Required {
					reqSet[r] = true
				}
				for pName, p := range schema.Parameters.Properties {
					params = append(params, MCPParamItem{
						Name:        pName,
						Type:        p.Type,
						Description: p.Description,
						Required:    reqSet[pName],
					})
				}
				tools = append(tools, MCPToolItem{
					Name:        strings.TrimPrefix(schema.Name, prefix),
					Description: schema.Description,
					Parameters:  params,
				})
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
			m.unregisterMCPTools(srv.Name)
		}
		m.refreshMCPServerItems()
		m.mcpView.SetMessage(fmt.Sprintf("✓ %s disabled", name))
		return nil
	}

	// Enable asynchronously
	enabled := true
	srv.Enabled = &enabled

	if m.mcpManager == nil {
		m.mcpView.SetMessage(fmt.Sprintf("✓ %s enabled (no manager)", name))
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

	m.unregisterMCPTools(name)
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

	ch := make(chan string, 1)
	go func() {
		defer close(ch)

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
			if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); !ok {
				ch <- fmt.Sprintf("OAuth failed: %v", err)
			}
			return
		}

		ch <- fmt.Sprintf("OAuth OK for %s — reconnecting...", srv.Name)

		reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
		defer reconnectCancel()

		tools, err := m.mcpManager.Reconnect(reconnectCtx, srv)
		if err != nil {
			ch <- fmt.Sprintf("Reconnect failed: %v", err)
			return
		}

		for _, t := range tools {
			m.agent.RegisterTool(t)
		}

		ch <- fmt.Sprintf("✓ %s connected with %d tool(s)", srv.Name, len(tools))
	}()

	return readNextMCPOverlayMsg(ch)
}

// mcpOverlayConnectAndRegister connects and registers tools, then sends result.
func (m *Model) mcpOverlayConnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to connect %s: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
	}

	ch <- fmt.Sprintf("✓ %s connected with %d tool(s)", srv.Name, len(tools))
}

// mcpOverlayReconnectAndRegister reconnects and registers tools, then sends result.
func (m *Model) mcpOverlayReconnectAndRegister(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect %s: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
	}

	ch <- fmt.Sprintf("✓ %s reconnected with %d tool(s)", srv.Name, len(tools))
}