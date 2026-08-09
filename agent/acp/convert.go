package acp

import (
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// convertContentBlocks converts ACP ContentBlock slice to a plain text user message
// and extracts image parts for multi-modal input.
func convertContentBlocks(blocks []acp.ContentBlock) (string, []llm.ContentPart) {
	var sb strings.Builder
	var images []llm.ContentPart

	for _, block := range blocks {
		switch {
		case block.Text != nil:
			sb.WriteString(block.Text.Text)
		case block.Image != nil:
			// Image block — extract as ContentPart for multi-modal LLM input.
			images = append(images, llm.ContentPart{
				Type:      llm.ContentPartImage,
				MediaType: block.Image.MimeType,
				Data:      block.Image.Data,
			})
			// Also add a text placeholder so the message makes sense as plain text.
			sb.WriteString("[图片]")
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
			fmt.Fprintf(&sb, "[@file: %s]\n", block.ResourceLink.Uri)
		}
	}
	return sb.String(), images
}

// extractPathFromURI extracts a filesystem path from a file:// URI.
func extractPathFromURI(uri string) string {
	if after, ok := strings.CutPrefix(uri, "file://"); ok {
		return after
	}
	return uri
}

// buildSystemPromptForCwd constructs the system prompt for ACP mode with a
// specific working directory, additional workspace roots, and session mode.
// In plan mode, the plan mode prompt is appended.
func buildSystemPromptForCwd(cfg *config.Config, cwd string, additionalDirs []string, mode string, sessionID string) string {
	prompt := agent.BuildSystemPromptWithRoots(cfg.Language, cwd, additionalDirs, sessionID)
	if mode == agent.ModePlan {
		prompt += "\n\n" + agent.BuildPlanModePrompt()
	}
	return prompt
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
