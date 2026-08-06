package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

// --- /mcp ---

// handleMCPAuth starts the OAuth2 flow for an HTTP MCP server.
//
// Usage: /mcp auth <server>
//
// Runs in a background goroutine and skips the browser-open step entirely
// (channel mode is headless). startManualFlow binds a local callback listener
// on the configured callback_host (e.g. 10.x.x.x) and sends the authorization
// URL to the user via sendToThread. The OAuth provider redirects the user's
// browser to that address; the listener catches the code and completes the
// exchange automatically — no paste-back required.
//
// On success the token is persisted to disk; the next agent turn will
// automatically connect to the server using the stored credentials.
func (m *Manager) handleMCPAuth(threadID, serverName string) (string, error) {
	if serverName == "" {
		return "Usage: `/mcp auth <server>` — authorize an HTTP MCP server", nil
	}

	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Find the server config.
	var srv *config.MCPServerConfig
	for i := range m.cfg.MCPServers {
		if m.cfg.MCPServers[i].Name == serverName {
			srv = &m.cfg.MCPServers[i]
			break
		}
	}
	if srv == nil {
		return fmt.Sprintf("MCP server **%s** not found. Use `/mcp` to list configured servers.", serverName), nil
	}
	if srv.Type != config.MCPTransportHTTP {
		return fmt.Sprintf("OAuth is only supported for HTTP MCP servers. **%s** uses stdio transport.", serverName), nil
	}

	// Run the OAuth flow in a background goroutine so we can return an
	// immediate acknowledgement. RunManualOAuthFlow skips the browser-open
	// attempt; startManualFlow binds on the configured callback_host and
	// sends the authorization URL to the user via statusFn.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		statusFn := func(msg string) {
			m.sendToThread(context.Background(), threadID, msg, "")
		}

		if err := mcp.RunManualOAuthFlow(ctx, srv, statusFn); err != nil {
			m.logger.Error(context.Background(), "channel: /mcp auth flow failed", err, "name", serverName)
			m.sendToThread(context.Background(), threadID,
				fmt.Sprintf("❌ OAuth failed for **%s**: %v", serverName, err), "")
			return
		}

		m.logger.Info(context.Background(), "channel: /mcp auth flow succeeded", "name", serverName)
		m.sendToThread(context.Background(), threadID,
			fmt.Sprintf("✅ OAuth authorization successful for **%s**!\n\nSend a message to start using the server's tools.", serverName), "")
	}()

	return fmt.Sprintf("🔐 Starting OAuth authorization for **%s**...\n\nThe authorization URL will be sent to you in a moment.", serverName), nil
}

// handleMCPList returns a markdown-formatted list of configured MCP servers
// with their discovered (loaded) tools when the shared MCP manager is available.
func (m *Manager) handleMCPList() (string, error) {
	servers := m.cfg.MCPServers
	if len(servers) == 0 {
		return "No MCP servers configured.", nil
	}

	mgr := m.initSharedMCP()
	infos := cmds.BuildMCPServerInfos(servers, mgr)
	return cmds.FormatMCPList(infos), nil
}

// --- /usage ---

// handleUsageCommand returns usage stats for the session associated with the ThreadID.
func (m *Manager) handleUsageCommand(threadID string) (string, error) {
	if threadID == "" {
		return "No active session (no thread ID).", nil
	}

	sm := m.newSessionManager()
	if sm == nil {
		return "Session manager unavailable.", nil
	}

	_, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /usage find session failed", err, "thread", threadID)
		return "Failed to find session.", nil
	}
	if !sm.HasCurrent() {
		return "No session found for this thread. Send a message first to start a session.", nil
	}

	// Resolve context window; cost now comes from the usage ledger (single
	// source of truth) — no price resolution needed here.
	contextWindow := m.defaultResolvedProvider.ContextWindow

	report, err := agent.ComputeSessionUsage(sm, agent.GlobalUsageRecorder(), contextWindow)
	if err != nil {
		return fmt.Sprintf("Failed to compute usage: %v", err), nil
	}

	// Read the estimate and its breakdown together: a turn may be running on
	// this thread, and two separate reads could mix values from two estimates.
	estTokens, estBreakdown := m.getAgentEstimateWithBreakdown(threadID)

	info := agent.BuildUsageReportInfo(report, estTokens, estBreakdown, m.cfg.Debug.PPROF.Addr())

	return cmds.FormatUsageReport(info), nil
}
