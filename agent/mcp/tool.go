package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
)

// MCPTool wraps an MCP server tool as a Tachi Tool implementation.
type MCPTool struct {
	serverName string
	serverTool *mcp.Tool
	manager    *Manager
}

// Name returns the tool name, prefixed with "mcp__<server>__" to avoid conflicts.
func (t MCPTool) Name() string {
	return fmt.Sprintf("mcp__%s__%s", t.serverName, t.serverTool.Name)
}

// ServerName returns the MCP server this tool belongs to.
func (t MCPTool) ServerName() string { return t.serverName }

// ToolName returns the original MCP-level tool name (without prefix).
func (t MCPTool) ToolName() string { return t.serverTool.Name }

// Description returns the tool description.
func (t MCPTool) Description() string {
	return fmt.Sprintf("[MCP:%s] %s", t.serverName, t.serverTool.Description)
}

// IsDestructive returns true if the MCP tool may modify system state.
// It reads the tool's annotations from the MCP protocol:
//   - If ReadOnlyHint is true → not destructive
//   - If DestructiveHint is explicitly false → not destructive
//   - Otherwise → conservative default (assume destructive)
func (t MCPTool) IsDestructive() bool {
	a := t.serverTool.Annotations
	if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
		return false
	}
	if a.DestructiveHint != nil && !*a.DestructiveHint {
		return false
	}
	// No annotations or DestructiveHint=true → assume destructive.
	return true
}

// Properties converts the MCP input schema to the Tachi PropertySchema format.
func (t MCPTool) Properties() map[string]tools.PropertySchema {
	result := make(map[string]tools.PropertySchema)

	if t.serverTool.InputSchema.Properties == nil {
		return result
	}

	for name, propRaw := range t.serverTool.InputSchema.Properties {
		propMap, ok := propRaw.(map[string]any)
		if !ok {
			continue
		}

		ps := tools.PropertySchema{}

		if typ, ok := propMap["type"].(string); ok {
			ps.Type = typ
		}

		if desc, ok := propMap["description"].(string); ok {
			ps.Description = desc
		}

		if items, ok := propMap["items"]; ok {
			ps.Items = items
		}

		result[name] = ps
	}

	return result
}

// Required returns the list of required parameter names.
func (t MCPTool) Required() []string {
	return t.serverTool.InputSchema.Required
}

// Parallel returns true if the underlying tool supports parallel execution.
func (t MCPTool) Parallel() bool {
	// MCP tools default to true for parallel execution
	return true
}

// ExecuteContext calls the MCP tool via the manager.
func (t MCPTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var argMap map[string]any
	if args == "" {
		argMap = make(map[string]any)
	} else if err := json.Unmarshal([]byte(args), &argMap); err != nil {
		return "", fmt.Errorf("invalid arguments for MCP tool %s: %w", t.serverTool.Name, err)
	}

	result, err := t.manager.CallTool(ctx, t.serverName, t.serverTool.Name, argMap)
	if err != nil {
		return "", fmt.Errorf("MCP tool %s failed: %w", t.serverTool.Name, err)
	}

	output, formatErr := formatMCPResult(result)
	if formatErr != nil {
		return "", formatErr
	}

	// Apply result size limit: save oversized results to disk and return preview.
	output = t.manager.truncateToolOutput(ctx, output, t.manager.ToolResultMaxChars(), t.manager.ToolResultFileDir(), t.Name())

	return output, nil
}
