package acp

import (
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
)

// convertContentBlocks converts ACP ContentBlock slice to a plain text user message.
func convertContentBlocks(blocks []acp.ContentBlock) string {
	var sb strings.Builder
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			sb.WriteString(block.Text.Text)
		case block.Resource != nil:
			// Inline embedded resources like Tachi's @-file format
			if block.Resource.Resource.TextResourceContents != nil {
				res := block.Resource.Resource.TextResourceContents
				path := extractPathFromURI(res.Uri)
				sb.WriteString("--- BEGIN UNTRUSTED FILE CONTENT: ")
				sb.WriteString(path)
				sb.WriteString(" ---\n")
				sb.WriteString(res.Text)
				sb.WriteString("\n--- END UNTRUSTED FILE CONTENT ---\n")
			}
		case block.ResourceLink != nil:
			// Resource links — just note the reference for the LLM to fetch if needed
			sb.WriteString(fmt.Sprintf("[@file: %s]\n", block.ResourceLink.Uri))
		}
	}
	return sb.String()
}

// extractPathFromURI extracts a filesystem path from a file:// URI.
func extractPathFromURI(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		return strings.TrimPrefix(uri, "file://")
	}
	return uri
}

// buildSystemPromptForCwd constructs the system prompt for ACP mode with a specific working directory.
func buildSystemPromptForCwd(language string, cwd string) string {
	return agent.BuildSystemPrompt(language, cwd)
}

// convertMCPServers converts editor-provided ACP MCP servers to Tachi's MCPServerConfig format.
// It applies the conflict policy: "client_wins" means editor servers override same-named config servers.
func convertMCPServers(acpServers []acp.McpServer, conflictPolicy string, existingServers []config.MCPServerConfig) []config.MCPServerConfig {
	// Build a set of existing server names for conflict detection
	existing := make(map[string]bool, len(existingServers))
	for _, s := range existingServers {
		existing[s.Name] = true
	}

	var result []config.MCPServerConfig
	for _, srv := range acpServers {
		// Only handle stdio transport (ACP spec requires all agents support it)
		if srv.Stdio == nil {
			continue
		}

		name := srv.Stdio.Name
		if name == "" {
			name = srv.Stdio.Command
		}

		// Apply conflict policy
		if existing[name] {
			if conflictPolicy != "client_wins" {
				continue // agent_wins: skip editor's server
			}
			// client_wins: editor's server will be connected (may shadow agent's)
		}

		// Convert env variables
		env := make(map[string]string, len(srv.Stdio.Env))
		for _, e := range srv.Stdio.Env {
			env[e.Name] = e.Value
		}

		result = append(result, config.MCPServerConfig{
			Name:    name,
			Command: srv.Stdio.Command,
			Args:    srv.Stdio.Args,
			Env:     env,
		})
	}

	return result
}
