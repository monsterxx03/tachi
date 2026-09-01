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

		// JSON Schema allows "type" to be an array union (e.g. ["null","array"]),
		// but LLM tool APIs only accept a single string type. Normalize so the
		// LLM never receives an empty/invalid type keyword.
		if typ := typeKeyword(propMap["type"]); typ != "" {
			ps.Type = typ
		}

		if desc, ok := propMap["description"].(string); ok {
			ps.Description = desc
		}

		if items, ok := propMap["items"]; ok {
			ps.Items = normalizeSchemaNode(items)
		}

		// Forward structured constraints declared by the MCP server. These
		// were previously dropped at registration; the LLM API now receives
		// the same hard constraints (enum/bounds/format/default) as built-in
		// tools, which matters most for MCP tools — typically the least
		// structured tools in the registry.
		if enum, ok := stringEnum(propMap["enum"]); ok {
			ps.Enum = enum
		}
		if format, ok := propMap["format"].(string); ok {
			ps.Format = format
		}
		if def, ok := propMap["default"]; ok {
			ps.Default = def
		}
		if min, ok := jsonNumberPtr(propMap["minimum"]); ok {
			ps.Minimum = min
		}
		if max, ok := jsonNumberPtr(propMap["maximum"]); ok {
			ps.Maximum = max
		}

		result[name] = ps
	}

	return result
}

// stringEnum extracts a string-only enum from a JSON-decoded value ([]any).
// Returns ok=false when the value is missing, not an array, or contains
// non-string elements — heterogeneous enums cannot be expressed in []string
// and are dropped rather than partially forwarded. Empty arrays also return
// false (an empty enum would be invalid JSON Schema).
func stringEnum(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// typeKeyword extracts a plain-string "type" from a JSON-decoded schema
// keyword. JSON Schema permits the union form (e.g. ["null","array"]), but
// LLM tool APIs only accept a single string type — so the union is collapsed
// to its first non-"null" member (["null","array"] → "array", which preserves
// the useful type while dropping the nullable marker). Returns "" when the
// keyword is missing or contains no usable type.
func typeKeyword(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, member := range t {
			if s, ok := member.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// normalizeSchemaNode recursively rewrites "type" keywords in a schema node
// that is forwarded verbatim to the LLM API (the "items" subschema),
// collapsing union forms into plain strings. The original MCP schema is left
// untouched — a copy is made. A union with no non-null member drops the
// keyword entirely rather than emitting "type": "".
func normalizeSchemaNode(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m)+1)
	for k, val := range m {
		switch k {
		case "type":
			if s := typeKeyword(val); s != "" {
				out[k] = s
			}
		case "items":
			out[k] = normalizeSchemaNode(val)
		default:
			out[k] = val
		}
	}
	return out
}

// jsonNumberPtr extracts a numeric bound from a JSON-decoded value.
// JSON numbers decode as float64; returns ok=false otherwise.
func jsonNumberPtr(v any) (*float64, bool) {
	f, ok := v.(float64)
	if !ok {
		return nil, false
	}
	return &f, true
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
