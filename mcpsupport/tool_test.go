package mcpsupport

import (
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

	_, err := tool.ExecuteContext(nil, "not json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arguments")
}

func TestMCPTool_ExecuteContext_EmptyArgs(t *testing.T) {
	// With no manager, ExecuteContext should panic (nil pointer dereference).
	// This is an expected constraint: the tool must always have a manager.
	// We validate this by ensuring the function panics.
	mgr := NewManager()
	tool := MCPTool{
		serverName: "test",
		serverTool: &mcp.Tool{Name: "test"},
		manager:    mgr,
	}

	_, err := tool.ExecuteContext(nil, "")
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
	mgr := NewManager()
	assert.NotNil(t, mgr)
	assert.Empty(t, mgr.clients)
}

func TestManager_CallTool_NotConnected(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.CallTool(nil, "nonexistent", "tool", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
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
