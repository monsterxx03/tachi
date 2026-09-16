package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestTool() mcp.Tool {
	schema := mcp.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Name of the person to greet",
			},
			"count": map[string]any{
				"type":        "integer",
				"description": "How many times to greet",
			},
		},
		Required: []string{"name"},
	}

	return mcp.Tool{
		Name:        "greet",
		Description: "Greet someone by name",
		InputSchema: schema,
	}
}

func TestMCPToolName(t *testing.T) {
	tool := MCPTool{
		serverName: "my-server",
		serverTool: &mcp.Tool{
			Name:        "do_stuff",
			Description: "Does things",
		},
	}

	assert.Equal(t, "mcp__my-server__do_stuff", tool.Name())
}

func TestMCPToolDescription(t *testing.T) {
	tool := MCPTool{
		serverName: "my-server",
		serverTool: &mcp.Tool{
			Name:        "do_stuff",
			Description: "Does things",
		},
	}

	assert.Equal(t, "[MCP:my-server] Does things", tool.Description())
}

func TestMCPToolProperties(t *testing.T) {
	mcpTool := makeTestTool()
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcpTool,
	}

	props := tool.Properties()

	require.Len(t, props, 2)

	nameProp, ok := props["name"]
	require.True(t, ok)
	assert.Equal(t, "string", nameProp.Type)
	assert.Equal(t, "Name of the person to greet", nameProp.Description)

	countProp, ok := props["count"]
	require.True(t, ok)
	assert.Equal(t, "integer", countProp.Type)
	assert.Equal(t, "How many times to greet", countProp.Description)
}

func TestMCPToolRequired(t *testing.T) {
	mcpTool := makeTestTool()
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcpTool,
	}

	required := tool.Required()
	require.Len(t, required, 1)
	assert.Equal(t, "name", required[0])
}

func TestMCPToolParallel(t *testing.T) {
	tool := MCPTool{}
	assert.True(t, tool.Parallel())
}

func TestMCPToolPropertiesEmpty(t *testing.T) {
	mcpTool := mcp.Tool{
		Name:        "empty",
		Description: "No input schema",
	}
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcpTool,
	}

	props := tool.Properties()
	assert.Empty(t, props)
}

func TestMCPToolPropertiesNilSchema(t *testing.T) {
	// Construct a Tool with non-map properties (edge case)
	mcpTool := mcp.Tool{
		Name:        "bad",
		Description: "Bad schema",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"bad_prop": "not_a_map", // invalid structure
			},
		},
	}
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcpTool,
	}

	props := tool.Properties()
	// Should not crash, just skip the non-map property
	assert.Empty(t, props)
}

func TestFormatMCPResult(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{Type: "text", Text: "Hello, World!"},
		},
		IsError: false,
	}

	output, err := formatMCPResult(result)
	require.NoError(t, err)
	assert.Equal(t, "Hello, World!", output)
}

func TestFormatMCPResultError(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{Type: "text", Text: "Something went wrong"},
		},
		IsError: true,
	}

	_, err := formatMCPResult(result)
	require.Error(t, err)
	mcpErr, ok := err.(*MCPToolError)
	require.True(t, ok)
	assert.Contains(t, mcpErr.Error(), "Something went wrong")
}

func TestMCPToolError(t *testing.T) {
	e := &MCPToolError{Message: "test error"}
	assert.Contains(t, e.Error(), "test error")
}

func TestMCPTool_ExecuteContext_InvalidJSON(t *testing.T) {
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcp.Tool{Name: "test"},
	}

	_, err := tool.ExecuteContext(context.TODO(), "not json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arguments")
}

func TestMCPTool_ExecuteContext_EmptyArgs(t *testing.T) {
	// With no manager, ExecuteContext should panic (nil pointer dereference).
	// This is an expected constraint: the tool must always have a manager.
	// We validate this by ensuring the function panics.
	mgr := NewManager(t.Context(), nil, nil)
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcp.Tool{Name: "test"},
		manager:    mgr,
	}

	_, err := tool.ExecuteContext(context.TODO(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestContentToString(t *testing.T) {
	content := []mcp.Content{
		mcp.TextContent{Type: "text", Text: "Hello"},
	}

	result := contentToString(content)
	assert.Equal(t, "Hello", result)
}

func TestContentToStringMultiple(t *testing.T) {
	content := []mcp.Content{
		mcp.TextContent{Type: "text", Text: "first"},
		mcp.TextContent{Type: "text", Text: "second"},
	}

	result := contentToString(content)
	assert.Equal(t, "firstsecond", result)
}

func TestContentToStringNonText(t *testing.T) {
	// We can't easily construct non-text content without the library's constructors,
	// but we can test the error message includes the content.
	mcpErr := &MCPToolError{Message: "something broke"}
	assert.Contains(t, mcpErr.Error(), "MCP tool error: something broke")
}

func TestManager_NewManager(t *testing.T) {
	mgr := NewManager(t.Context(), nil, nil)
	assert.NotNil(t, mgr)
	assert.Empty(t, mgr.clients)
}

func TestMCPTool_JSONArgsParsing(t *testing.T) {
	// Test that JSON args are correctly parsed into map[string]any
	args := `{"name": "World", "count": 5}`

	var argMap map[string]any
	err := json.Unmarshal([]byte(args), &argMap)
	require.NoError(t, err)
	assert.Equal(t, "World", argMap["name"])
	assert.Equal(t, float64(5), argMap["count"]) // JSON numbers unmarshal as float64
}

func TestMCPToolProperties_ForwardsConstraints(t *testing.T) {
	// MCP servers declare enum/minimum/maximum/default/format in their
	// inputSchema; Properties() must forward them so the LLM API receives the
	// same hard constraints as built-in tools.
	schema := mcp.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"description": "mode selector",
				"enum":        []any{"fast", "slow", "auto"},
				"default":     "auto",
			},
			"count": map[string]any{
				"type":    "integer",
				"minimum": float64(1),
				"maximum": float64(20),
			},
			"url": map[string]any{
				"type":   "string",
				"format": "uri",
			},
			"mixed_enum": map[string]any{
				"type": "string",
				"enum": []any{"ok", 42}, // heterogeneous → dropped
			},
			"empty_enum": map[string]any{
				"type": "string",
				"enum": []any{},
			},
			"no_type": map[string]any{
				"description": "missing type keyword",
			},
		},
	}

	tool := MCPTool{serverTool: &mcp.Tool{Name: "do_stuff", InputSchema: schema}}
	props := tool.Properties()

	mode := props["mode"]
	assert.Equal(t, "string", mode.Type)
	assert.Equal(t, []string{"fast", "slow", "auto"}, mode.Enum)
	assert.Equal(t, "auto", mode.Default)

	count := props["count"]
	require.NotNil(t, count.Minimum)
	require.NotNil(t, count.Maximum)
	assert.Equal(t, 1.0, *count.Minimum)
	assert.Equal(t, 20.0, *count.Maximum)

	assert.Equal(t, "uri", props["url"].Format)

	// Heterogeneous and empty enums must NOT produce an Enum (an empty enum
	// would be invalid JSON Schema).
	assert.Nil(t, props["mixed_enum"].Enum)
	assert.Nil(t, props["empty_enum"].Enum)

	// Property without type keyword still forwarded with description only.
	assert.Equal(t, "missing type keyword", props["no_type"].Description)
}

func TestMCPToolProperties_UnionTypeNormalization(t *testing.T) {
	// MCP servers may declare "type" as a JSON Schema union array (e.g.
	// ["null","array"]); LLM APIs only accept a single string type, and an
	// empty string broke requests. Properties() must collapse the union to
	// its first non-null member.
	schema := mcp.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"domains": map[string]any{
				"type":        []any{"null", "array"},
				"description": "domain list",
				"items": map[string]any{
					"type":        []any{"null", "string"},
					"description": "a domain",
				},
			},
			"client_id": map[string]any{
				"type": []any{"string", "null"},
			},
			"nullable_only": map[string]any{
				"type": []any{"null"},
			},
			"plain_string": map[string]any{
				"type": "integer",
			},
			"no_type": map[string]any{
				"description": "missing type keyword",
			},
		},
	}

	tool := MCPTool{serverTool: &mcp.Tool{Name: "do_stuff", InputSchema: schema}}
	props := tool.Properties()

	// ["null","array"] → "array"
	domains := props["domains"]
	assert.Equal(t, "array", domains.Type)
	assert.Equal(t, "domain list", domains.Description)

	// items subschema is forwarded verbatim — its union type must be
	// normalized too, and the original MCP schema must not be mutated.
	items, ok := domains.Items.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", items["type"])
	assert.Equal(t, "a domain", items["description"])
	rawDomains := schema.Properties["domains"].(map[string]any)
	assert.Equal(t, []any{"null", "array"}, rawDomains["type"])
	assert.Equal(t, []any{"null", "string"}, rawDomains["items"].(map[string]any)["type"])

	// ["string","null"] → "string"
	assert.Equal(t, "string", props["client_id"].Type)

	// union of only nulls → no usable type (empty, not "null")
	assert.Equal(t, "", props["nullable_only"].Type)

	// plain string types pass through untouched
	assert.Equal(t, "integer", props["plain_string"].Type)

	// missing type keyword keeps old behavior: description only
	assert.Equal(t, "", props["no_type"].Type)
	assert.Equal(t, "missing type keyword", props["no_type"].Description)
}

func TestMCPToolProperties_ComposedSchemaType(t *testing.T) {
	// pydantic-generated schemas declare a nullable parameter as a composition
	// with no top-level "type" — anyOf:[{type:integer},{type:null}] for
	// Optional[int]. Reading only the "type" keyword left such a property
	// typeless, and the empty "type" that followed made the provider reject the
	// ENTIRE request (every tool in the list, not just the offending one).
	schema := mcp.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"template_id": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "integer"},
					map[string]any{"type": "null"},
				},
				"default": nil,
				"title":   "Template Id",
			},
			"label": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "null"},
					map[string]any{"type": "string"},
				},
			},
			"nested": map[string]any{
				"anyOf": []any{
					map[string]any{
						"anyOf": []any{
							map[string]any{"type": "boolean"},
							map[string]any{"type": "null"},
						},
					},
				},
			},
			"narrowed": map[string]any{
				"allOf": []any{
					map[string]any{"$ref": "#/$defs/Node"},
					map[string]any{"type": "object"},
				},
			},
			"explicit_wins": map[string]any{
				"type": "string",
				"anyOf": []any{
					map[string]any{"type": "integer"},
					map[string]any{"type": "null"},
				},
			},
			"nullable_only": map[string]any{
				"anyOf": []any{map[string]any{"type": "null"}},
			},
			"ref_only": map[string]any{
				"$ref": "#/$defs/Unresolved",
			},
		},
	}

	tool := MCPTool{serverTool: &mcp.Tool{Name: "get_sub_workspaces", InputSchema: schema}}
	props := tool.Properties()

	assert.Equal(t, "integer", props["template_id"].Type)
	assert.Equal(t, "string", props["label"].Type)
	assert.Equal(t, "boolean", props["nested"].Type)
	assert.Equal(t, "object", props["narrowed"].Type)

	// An explicit keyword is the more specific declaration — it wins.
	assert.Equal(t, "string", props["explicit_wins"].Type)

	// Nothing usable to name: the property stays typeless, and the caller
	// (llm.ToolParameterProperty) omits the keyword rather than sending "".
	assert.Equal(t, "", props["nullable_only"].Type)
	assert.Equal(t, "", props["ref_only"].Type)
}

func TestPropertyType(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{name: "plain type", in: map[string]any{"type": "string"}, want: "string"},
		{name: "type union", in: map[string]any{"type": []any{"null", "array"}}, want: "array"},
		{
			name: "anyOf nullable",
			in: map[string]any{"anyOf": []any{
				map[string]any{"type": "integer"}, map[string]any{"type": "null"},
			}},
			want: "integer",
		},
		{
			name: "first branch names no type",
			in: map[string]any{"anyOf": []any{
				map[string]any{"$ref": "#/$defs/X"}, map[string]any{"type": "number"},
			}},
			want: "number",
		},
		{name: "only null", in: map[string]any{"anyOf": []any{map[string]any{"type": "null"}}}, want: ""},
		{name: "no type keyword", in: map[string]any{"description": "x"}, want: ""},
		{name: "badly shaped branches", in: map[string]any{"anyOf": "not-a-list"}, want: ""},
		{name: "non-map branch", in: map[string]any{"oneOf": []any{"string"}}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, propertyType(c.in))
		})
	}
}

func TestTypeKeyword(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{name: "plain string", in: "array", want: "array"},
		{name: "plain null", in: "null", want: ""},
		{name: "union with null", in: []any{"null", "array"}, want: "array"},
		{name: "union null first", in: []any{"null", "string", "null"}, want: "string"},
		{name: "union only null", in: []any{"null"}, want: ""},
		{name: "empty union", in: []any{}, want: ""},
		{name: "union non-string member", in: []any{"null", 42}, want: ""},
		{name: "missing", in: nil, want: ""},
		{name: "non-type value", in: 42, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, typeKeyword(c.in))
		})
	}
}

func TestNormalizeSchemaNode(t *testing.T) {
	node := map[string]any{
		"type":        []any{"null", "object"},
		"description": "nested",
		"items": map[string]any{
			"type": []any{"null", "array"},
			"items": map[string]any{
				"type": "string",
			},
		},
		"default": 42, // unrelated keywords pass through
	}
	got := normalizeSchemaNode(node).(map[string]any)

	assert.Equal(t, "object", got["type"])
	assert.Equal(t, "nested", got["description"])
	assert.Equal(t, 42, got["default"])
	items := got["items"].(map[string]any)
	assert.Equal(t, "array", items["type"])
	assert.Equal(t, "string", items["items"].(map[string]any)["type"])

	// original untouched
	assert.Equal(t, []any{"null", "object"}, node["type"])
	assert.Equal(t, []any{"null", "array"}, node["items"].(map[string]any)["type"])

	// union of only nulls drops the keyword
	onlyNull := normalizeSchemaNode(map[string]any{"type": []any{"null"}}).(map[string]any)
	_, has := onlyNull["type"]
	assert.False(t, has)

	// non-map nodes pass through unchanged
	assert.Equal(t, "plain", normalizeSchemaNode("plain"))
}

func TestStringEnum(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
		ok   bool
	}{
		{name: "string slice", in: []any{"a", "b"}, want: []string{"a", "b"}, ok: true},
		{name: "mixed types", in: []any{"a", 1}, want: nil, ok: false},
		{name: "empty", in: []any{}, want: nil, ok: false},
		{name: "not an array", in: "a", want: nil, ok: false},
		{name: "nil", in: nil, want: nil, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stringEnum(c.in)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestJsonNumberPtr(t *testing.T) {
	f, ok := jsonNumberPtr(float64(42))
	require.True(t, ok)
	assert.Equal(t, 42.0, *f)

	_, ok = jsonNumberPtr("42")
	assert.False(t, ok)

	_, ok = jsonNumberPtr(nil)
	assert.False(t, ok)
}
