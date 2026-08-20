package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

// BuildMCPServerInfos assembles the display data for all configured MCP servers.
// mgr may be nil (tools/status info will be omitted). sessionID selects the
// per-session discovered set shown as "loaded"; pass "" to show none as loaded.
func BuildMCPServerInfos(servers []config.MCPServerConfig, mgr *mcp.Manager, sessionID string) []MCPServerInfo {
	// Collect tool info from MCP manager (if available).
	var poolToolsByServer map[string][]*mcp.DeferredTool
	var discovered map[string]bool
	if mgr != nil {
		discovered = make(map[string]bool)
		if set := mgr.SetIfExists(sessionID); set != nil {
			for _, name := range set.List() {
				discovered[name] = true
			}
		}
		poolToolsByServer = make(map[string][]*mcp.DeferredTool)
		for _, dt := range mgr.Pool().All() {
			poolToolsByServer[dt.ServerName] = append(poolToolsByServer[dt.ServerName], dt)
		}
	}

	infos := make([]MCPServerInfo, 0, len(servers))
	for _, srv := range servers {
		enabled := srv.IsEnabled()

		status := "⚪ Disabled"
		if enabled {
			if mgr != nil {
				if mgr.IsConnected(srv.Name) {
					status = "🟢 Connected"
				} else {
					status = "🔴 Disconnected"
				}
			} else {
				status = "Enabled"
			}
		}

		transport := "?"
		switch srv.Type {
		case config.MCPTransportStdio:
			transport = fmt.Sprintf("`stdio` — `%s`", srv.Command)
		case config.MCPTransportHTTP:
			transport = fmt.Sprintf("`http` — `%s`", srv.URL)
		}

		oauth := ""
		if srv.Type == config.MCPTransportHTTP && (srv.HasOAuth() || hasTokenOnDisk(srv.TokenStorageName())) {
			oauth = oauthStatusString(&srv)
		}

		var tools []MCPToolInfo
		toolsPending := false
		if dts, ok := poolToolsByServer[srv.Name]; ok && len(dts) > 0 {
			for _, dt := range dts {
				toolName := strings.TrimPrefix(dt.Name, "mcp__"+srv.Name+"__")
				tools = append(tools, MCPToolInfo{
					Name:       toolName,
					Discovered: discovered[dt.Name],
				})
			}
		} else if poolToolsByServer != nil {
			toolsPending = true
		}

		infos = append(infos, MCPServerInfo{
			Name:         srv.Name,
			Status:       status,
			Transport:    transport,
			OAuth:        oauth,
			Tools:        tools,
			ToolsPending: toolsPending,
		})
	}

	return infos
}

// oauthStatusString returns a human-readable OAuth status for the server.
func oauthStatusString(srv *config.MCPServerConfig) string {
	store, err := mcp.NewFileTokenStore(srv.TokenStorageName())
	if err != nil {
		return "⚠️ error"
	}

	token, err := store.GetToken(context.Background())
	if err != nil || token == nil {
		return "❌ not authenticated"
	}

	if token.IsExpired() {
		if token.RefreshToken != "" {
			return "🔄 expired (has refresh_token)"
		}
		return "❌ expired"
	}

	expiresIn := ""
	if !token.ExpiresAt.IsZero() {
		remaining := time.Until(token.ExpiresAt)
		if remaining > 24*time.Hour {
			expiresIn = fmt.Sprintf(", expires in %dd", int(remaining.Hours()/24))
		} else if remaining > time.Hour {
			expiresIn = fmt.Sprintf(", expires in %dh", int(remaining.Hours()))
		} else {
			expiresIn = fmt.Sprintf(", expires in %dm", int(remaining.Minutes()))
		}
	}

	return fmt.Sprintf("✅ authenticated%s", expiresIn)
}

// hasTokenOnDisk checks if there's any persisted token or DCR info for the storage key.
func hasTokenOnDisk(storageKey string) bool {
	store, err := mcp.NewFileTokenStore(storageKey)
	if err != nil {
		return false
	}
	if _, err := store.GetToken(context.Background()); err == nil {
		return true
	}
	if _, err := store.GetDCRInfo(context.Background()); err == nil {
		return true
	}
	return false
}
