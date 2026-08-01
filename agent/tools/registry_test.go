package tools

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWriteTool(t *testing.T) {
	tool := WriteTool{}
	content := "Test content"
	_, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_write.txt", "content": "Test content"}`)
	if err != nil {
		t.Fatalf("WriteTool.Execute failed: %v", err)
	}
	defer os.Remove("/tmp/test_write.txt")

	// Verify file was written
	data, err := os.ReadFile("/tmp/test_write.txt")
	if err != nil {
		t.Fatalf("Failed to read written file: %v", err)
	}
	if string(data) != content {
		t.Errorf("Expected %q, got %q", content, string(data))
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubTool{
		name: "test",
		desc: "A test tool",
		props: map[string]PropertySchema{
			"arg1": {Type: "string", Description: "First argument"},
		},
		required: []string{"arg1"},
		executeFn: func(ctx context.Context, args string) (string, error) {
			return "success", nil
		},
	})

	// Test invoking registered tool
	tr := reg.Invoke(context.TODO(), "test", `{"arg1": "value1"}`)
	if tr.Status != ToolResultSuccess {
		t.Errorf("Invoke failed: %v", tr.Err)
	}
	if tr.Output != "success" {
		t.Errorf("Expected 'success', got %q", tr.Output)
	}

	// Test unknown tool
	tr = reg.Invoke(context.TODO(), "unknown", "{}")
	if tr.Status != ToolResultError {
		t.Error("Expected error for unknown tool")
	}

	// Test missing required argument
	tr = reg.Invoke(context.TODO(), "test", "{}")
	if tr.Status != ToolResultError {
		t.Error("Expected error for missing required argument")
	}

	// Test Unregister
	if !reg.Unregister("test") {
		t.Error("Expected Unregister to return true for registered tool")
	}
	tr = reg.Invoke(context.TODO(), "test", `{"arg1": "value1"}`)
	if tr.Status != ToolResultError {
		t.Error("Expected error after unregistering tool")
	}

	// Test Unregister non-existent tool
	if reg.Unregister("nonexistent") {
		t.Error("Expected Unregister to return false for unknown tool")
	}
}

func TestRegistryMCPToolRegistrationOrder(t *testing.T) {
	reg := NewRegistry()

	// Register some built-in tools first
	reg.Register(&stubTool{name: "Bash"})
	reg.Register(&stubTool{name: "ReadFile"})
	reg.Register(&stubTool{name: "EditFile"})

	// Register MCP tools — order should be preserved
	reg.Register(&stubTool{
		name: "mcp__postgres__query",
		desc: "Query a postgres database",
	})
	reg.Register(&stubTool{
		name: "mcp__postgres__list_tables",
		desc: "List all tables",
	})
	reg.Register(&stubTool{
		name: "mcp__github__create_pr",
		desc: "Create a pull request",
	})

	// GetToolNames: built-ins sorted, MCP tools in registration order
	names := reg.GetToolNames()
	builtinNames := names[:3]
	mcpNames := names[3:]

	assert.Equal(t, []string{"Bash", "EditFile", "ReadFile"}, builtinNames)
	assert.Equal(t, []string{
		"mcp__postgres__query",
		"mcp__postgres__list_tables",
		"mcp__github__create_pr",
	}, mcpNames)

	// GetSchemas: same ordering
	schemas := reg.GetSchemas()
	assert.Equal(t, "Bash", schemas[0].Name)
	assert.Equal(t, "EditFile", schemas[1].Name)
	assert.Equal(t, "ReadFile", schemas[2].Name)
	assert.Equal(t, "mcp__postgres__query", schemas[3].Name)
	assert.Equal(t, "mcp__postgres__list_tables", schemas[4].Name)
	assert.Equal(t, "mcp__github__create_pr", schemas[5].Name)

	// Register a new MCP tool later — should append at the end
	reg.Register(&stubTool{
		name: "mcp__filesystem__write",
		desc: "Write a file",
	})

	names = reg.GetToolNames()
	mcpNames = names[3:]
	assert.Equal(t, []string{
		"mcp__postgres__query",
		"mcp__postgres__list_tables",
		"mcp__github__create_pr",
		"mcp__filesystem__write",
	}, mcpNames, "Newly registered MCP tool should append at the end")
}

func TestRegistryMCPToolUnregisterOrder(t *testing.T) {
	reg := NewRegistry()

	reg.Register(&stubTool{name: "Bash"})
	reg.Register(&stubTool{name: "mcp__a__tool"})
	reg.Register(&stubTool{name: "mcp__b__tool"})
	reg.Register(&stubTool{name: "mcp__c__tool"})

	// Remove middle MCP tool, order of remaining should be preserved
	reg.Unregister("mcp__b__tool")
	names := reg.GetToolNames()
	assert.Equal(t, []string{"Bash", "mcp__a__tool", "mcp__c__tool"}, names)

	// Remove non-MCP tool — mcpOrder unaffected
	reg.Unregister("Bash")
	assert.Equal(t, []string{"mcp__a__tool", "mcp__c__tool"}, reg.GetToolNames())
}

func TestRegistryMCPToolOrderDeterministic(t *testing.T) {
	reg := NewRegistry()

	reg.Register(&stubTool{name: "mcp__z__tool"})
	reg.Register(&stubTool{name: "mcp__a__tool"})
	reg.Register(&stubTool{name: "mcp__m__tool"})

	// Should be in registration order, not alphabetical
	names := reg.GetToolNames()
	assert.Equal(t, []string{"mcp__z__tool", "mcp__a__tool", "mcp__m__tool"}, names,
		"MCP tools should be in registration order, not alphabetical")
}

func TestRegistryMCPToolIdempotentRegister(t *testing.T) {
	reg := NewRegistry()

	reg.Register(&stubTool{name: "mcp__postgres__query"})
	reg.Register(&stubTool{name: "mcp__github__pr"})

	// Re-register same tool — should not duplicate in mcpOrder
	reg.Register(&stubTool{name: "mcp__postgres__query"})

	mcpOrder := reg.getMCPOrder()
	assert.Equal(t, []string{"mcp__postgres__query", "mcp__github__pr"}, mcpOrder,
		"Re-registering same MCP tool should not create duplicate in mcpOrder")
}

func TestRegistryNoMCPTools(t *testing.T) {
	reg := NewRegistry()

	reg.Register(&stubTool{name: "Bash"})
	reg.Register(&stubTool{name: "EditFile"})

	names := reg.GetToolNames()
	assert.Equal(t, []string{"Bash", "EditFile"}, names)
	assert.Empty(t, reg.getMCPOrder())
}

func TestRegistryOnlyMCPTools(t *testing.T) {
	reg := NewRegistry()

	reg.Register(&stubTool{name: "mcp__b__tool"})
	reg.Register(&stubTool{name: "mcp__a__tool"})

	names := reg.GetToolNames()
	assert.Equal(t, []string{"mcp__b__tool", "mcp__a__tool"}, names)
}

func TestRegistryIsParallel(t *testing.T) {
	reg := NewRegistry()

	// Unknown tool returns false
	if reg.IsParallel("nonexistent") {
		t.Error("Expected IsParallel to return false for unknown tool")
	}

	// Register a parallel tool
	reg.Register(&stubTool{
		name:      "parallel_tool",
		parallel:  true,
		executeFn: func(ctx context.Context, args string) (string, error) { return "", nil },
	})
	if !reg.IsParallel("parallel_tool") {
		t.Error("Expected IsParallel to return true for parallel tool")
	}

	// Register a non-parallel tool
	reg.Register(&stubTool{name: "serial_tool", parallel: false})
	if reg.IsParallel("serial_tool") {
		t.Error("Expected IsParallel to return false for non-parallel tool")
	}
}
