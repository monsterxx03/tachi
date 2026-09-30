package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// mcpImageMaxCount bounds how many images one MCP result may attach. Images
// bypass the text result limit — truncateToolOutput measures characters, and
// image bytes are not characters — so this cap is what keeps a server from
// pushing an unbounded number of them into a single request. A screenshot (the
// common case) is one.
const mcpImageMaxCount = 4

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

		// JSON Schema allows "type" to be an array union (e.g. ["null","array"])
		// or to be replaced by a composition (pydantic emits
		// anyOf:[{type:integer},{type:null}] for Optional[int]), but LLM tool
		// APIs only accept a single string type. Resolve both forms so the
		// property keeps its real type instead of reaching the LLM as an empty
		// one (see propertyType).
		if typ := propertyType(propMap); typ != "" {
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
// the useful type while dropping the nullable marker). A bare "null" counts as
// absent too: a parameter whose only accepted type is null has no value to
// pass, and sending "type":"null" is as unhelpful as sending "". Returns ""
// when the keyword is missing or contains no usable type.
func typeKeyword(v any) string {
	switch t := v.(type) {
	case string:
		if t == "null" {
			return ""
		}
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

// composedTypeKeywords are the JSON Schema keywords that replace a "type"
// outright with a union of subschemas: anyOf/oneOf directly, allOf because a
// narrowing declaration (a $ref plus the constrained type) puts it there.
var composedTypeKeywords = []string{"anyOf", "oneOf", "allOf"}

// propertyType resolves the model-facing "type" of an MCP property schema. A
// plain "type" keyword wins; otherwise the type is read out of a composition,
// recursively, taking each union's first non-null member. Servers do serve a
// parameter as anyOf:[{type:integer},{type:null}] with no top-level "type"
// (pydantic's Optional[int]), and one property left typeless is not a local
// problem: an empty "type" is invalid JSON Schema, so the provider rejects the
// whole request — every tool in the list goes down with it. Returns "" when no
// usable type is named, and callers must then omit the keyword.
func propertyType(propMap map[string]any) string {
	if typ := typeKeyword(propMap["type"]); typ != "" {
		return typ
	}
	for _, keyword := range composedTypeKeywords {
		branches, ok := propMap[keyword].([]any)
		if !ok {
			continue
		}
		for _, branch := range branches {
			b, ok := branch.(map[string]any)
			if !ok {
				continue
			}
			if typ := propertyType(b); typ != "" {
				return typ
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

	// Images ride the same side channel the Read tool uses, so a screenshot
	// reaches the model instead of being dropped (see extractMCPImages).
	images, dropNote := extractMCPImages(result.Content)
	tools.AddImageParts(ctx, images)
	if dropNote != "" {
		output += dropNote
	}

	// Apply result size limit: save oversized results to disk and return preview.
	// Runs after image extraction: truncation replaces the text, and the images
	// attached above are already out of its reach (it measures characters).
	output = t.manager.truncateToolOutput(ctx, output, t.manager.ToolResultMaxChars(), t.manager.ToolResultFileDir(), t.Name())

	return output, nil
}

// decodedImageSize returns the exact number of bytes a standard-base64 string
// decodes to.
//
// base64.StdEncoding.DecodedLen returns an UPPER BOUND rather than the real
// length — from the encoded length alone it cannot know the padding — and it
// over-counts by one or two bytes whenever the last group is partial. An image
// sitting exactly at llm.MaxImageSize is precisely the image worth keeping, so
// the budget is measured against the true size.
func decodedImageSize(data string) int64 {
	padding := 0
	for i := len(data) - 1; i >= 0 && data[i] == '='; i-- {
		padding++
	}
	return int64(len(data)/4*3 - padding)
}

// extractMCPImages pulls image content out of an MCP tool result as LLM
// content parts, plus a note naming whatever it had to leave behind.
//
// MCP servers declare images as a content item beside their text — a
// screenshot tool answers "Took a screenshot" plus the PNG — but the text-only
// result path drops every non-text item, so the bytes never reached the model.
//
// Images are sized by their DECODED length against llm.MaxImageSize, the single
// budget shared with the Read tool and @-file expansion: base64 inflates by 4/3,
// so measuring the encoded string would refuse an image those paths accept.
// Dropped images are always reported, never silently swallowed — a model that
// is told a screenshot arrived but cannot see it will otherwise invent one.
func extractMCPImages(content []mcp.Content) (parts []llm.ContentPart, dropNote string) {
	var dropped []string

	for _, item := range content {
		img, ok := item.(mcp.ImageContent)
		if !ok || img.Data == "" {
			continue
		}
		if len(parts) >= mcpImageMaxCount {
			dropped = append(dropped, fmt.Sprintf("one %s image over the %d-image limit", img.MIMEType, mcpImageMaxCount))
			continue
		}

		size := decodedImageSize(img.Data)
		if size > llm.MaxImageSize {
			dropped = append(dropped, fmt.Sprintf("one %s image (%s, over the %s limit)",
				img.MIMEType, strutil.HumanBytes(size), strutil.HumanBytes(llm.MaxImageSize)))
			continue
		}

		parts = append(parts, llm.ContentPart{
			Type:      llm.ContentPartImage,
			MediaType: img.MIMEType,
			Data:      img.Data,
		})
	}

	if len(dropped) > 0 {
		var sb strings.Builder
		sb.WriteString("\n[MCP returned images that were not attached: ")
		sb.WriteString(strings.Join(dropped, ", "))
		sb.WriteString("]")
		dropNote = sb.String()
	}
	return parts, dropNote
}
