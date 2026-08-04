package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file guards the tool schema constraints added to improve LLM call
// accuracy: enum values, JSON types, and numeric bounds. If a tool's
// properties drift from these contracts, models lose the hard constraints
// that prevent malformed tool calls (e.g. misspelled operations, out-of-range
// limits).

func TestGrepToolSchemaConstraints(t *testing.T) {
	p := GrepTool{}.Properties()

	om, ok := p["output_mode"]
	require.True(t, ok, "output_mode property missing")
	assert.Equal(t, "string", om.Type)
	assert.Equal(t, []string{"files_with_matches", "content", "count"}, om.Enum)
	assert.Equal(t, "files_with_matches", om.Default)

	mr, ok := p["max_results"]
	require.True(t, ok, "max_results property missing")
	assert.Equal(t, "integer", mr.Type)
	require.NotNil(t, mr.Minimum)
	require.NotNil(t, mr.Maximum)
	assert.Equal(t, 1.0, *mr.Minimum)
	assert.Equal(t, 1000.0, *mr.Maximum)
}

func TestLSPToolSchemaConstraints(t *testing.T) {
	p := (&LSPTool{}).Properties()

	op, ok := p["operation"]
	require.True(t, ok, "operation property missing")
	assert.Equal(t, []string{
		"goToDefinition", "findReferences", "hover", "documentSymbol",
		"workspaceSymbol", "goToImplementation", "prepareCallHierarchy",
		"incomingCalls", "outgoingCalls",
	}, op.Enum)
}

func TestCronToolSchemaConstraints(t *testing.T) {
	p := (&CronTool{}).Properties()

	action, ok := p["action"]
	require.True(t, ok, "action property missing")
	assert.Equal(t, []string{"list", "create", "get", "update", "delete", "pause", "resume"}, action.Enum)

	typ, ok := p["type"]
	require.True(t, ok, "type property missing")
	assert.Equal(t, []string{"oneshot", "recurring"}, typ.Enum)
	assert.Equal(t, "oneshot", typ.Default)

	notify, ok := p["notify"]
	require.True(t, ok, "notify property missing")
	assert.Equal(t, []string{"always", "when_relevant"}, notify.Enum)
	assert.Equal(t, "always", notify.Default)
}

func TestSkillToolSchemaConstraints(t *testing.T) {
	p := (&SkillTool{}).Properties()

	op, ok := p["operation"]
	require.True(t, ok, "operation property missing")
	assert.Equal(t, []string{"list", "view", "create", "update", "delete"}, op.Enum)

	src, ok := p["source"]
	require.True(t, ok, "source property missing")
	assert.Equal(t, []string{"project", "global"}, src.Enum)
	assert.Equal(t, "project", src.Default)
}

func TestReadToolSchemaTypes(t *testing.T) {
	p := NewReadTool().Properties()

	offset, ok := p["offset"]
	require.True(t, ok, "offset property missing")
	assert.Equal(t, "integer", offset.Type)
	// Negative offsets are valid (tail reads), so no Minimum constraint.
	assert.Nil(t, offset.Minimum)

	limit, ok := p["limit"]
	require.True(t, ok, "limit property missing")
	assert.Equal(t, "integer", limit.Type)
	require.NotNil(t, limit.Minimum)
	assert.Equal(t, 1.0, *limit.Minimum)
}

func TestBashToolSchemaConstraints(t *testing.T) {
	p := BashTool{}.Properties()

	timeout, ok := p["timeout"]
	require.True(t, ok, "timeout property missing")
	assert.Equal(t, "integer", timeout.Type)
	require.NotNil(t, timeout.Minimum)
	require.NotNil(t, timeout.Maximum)
	assert.Equal(t, 1.0, *timeout.Minimum)
	assert.Equal(t, 600000.0, *timeout.Maximum)
	assert.Equal(t, 120000, timeout.Default)
}

func TestWebSearchToolSchemaConstraints(t *testing.T) {
	p := (&WebSearchTool{}).Properties()

	num, ok := p["num"]
	require.True(t, ok, "num property missing")
	assert.Equal(t, "integer", num.Type)
	require.NotNil(t, num.Minimum)
	require.NotNil(t, num.Maximum)
	assert.Equal(t, 1.0, *num.Minimum)
	assert.Equal(t, 10.0, *num.Maximum)
	assert.Equal(t, 5, num.Default)
}

func TestMCPSearchToolsSchemaConstraints(t *testing.T) {
	// Properties() does not touch the searcher/tracker, so a zero-value tool is fine.
	p := (&MCPSearchToolsTool{}).Properties()

	mr, ok := p["max_results"]
	require.True(t, ok, "max_results property missing")
	assert.Equal(t, "integer", mr.Type)
	require.NotNil(t, mr.Minimum)
	require.NotNil(t, mr.Maximum)
	assert.Equal(t, 1.0, *mr.Minimum)
	assert.Equal(t, 20.0, *mr.Maximum)
	assert.Equal(t, 5, mr.Default)
}

func TestSubagentToolSchemaAllowedToolsEnum(t *testing.T) {
	runner := &mockRunner{toolNames: []string{"ReadFile", "Grep", "Glob", "Bash", "WebSearch"}}
	tool := NewSubagentTool(runner)

	p := tool.Properties()
	at, ok := p["allowed_tools"]
	require.True(t, ok, "allowed_tools property missing")
	assert.Equal(t, "array", at.Type)

	items, ok := at.Items.(map[string]any)
	require.True(t, ok, "items should be a map")
	assert.Equal(t, "string", items["type"])
	enum, ok := items["enum"].([]any)
	require.True(t, ok, "items.enum should be []any")
	assert.Equal(t, []any{"ReadFile", "Grep", "Glob", "Bash", "WebSearch"}, enum)
}

func TestSubagentToolSchemaAllowedToolsEmptyEnum(t *testing.T) {
	// With no available tools, allowed_tools must NOT carry an enum at all —
	// an empty enum ("enum": []) would be invalid JSON Schema.
	tool := NewSubagentTool(&mockRunner{}) // toolNames == nil

	p := tool.Properties()
	at, ok := p["allowed_tools"]
	require.True(t, ok, "allowed_tools property missing")

	items, ok := at.Items.(map[string]any)
	require.True(t, ok, "items should be a map")
	assert.Equal(t, "string", items["type"])
	_, hasEnum := items["enum"]
	assert.False(t, hasEnum, "empty tool list must not produce an enum key")
}

func TestSchemaToPropsJSONPassthrough(t *testing.T) {
	max := 42.0
	props := map[string]PropertySchema{
		"mode":  {Type: "string", Description: "mode", Enum: []string{"a", "b"}, Default: "a"},
		"count": {Type: "integer", Description: "count", Minimum: new(1.0), Maximum: &max},
	}

	out := schemaToPropsJSON(props)
	assert.Equal(t, []string{"a", "b"}, out["mode"].(map[string]any)["enum"])
	assert.Equal(t, "a", out["mode"].(map[string]any)["default"])
	assert.Equal(t, 1.0, out["count"].(map[string]any)["minimum"])
	assert.Equal(t, 42.0, out["count"].(map[string]any)["maximum"])
}
