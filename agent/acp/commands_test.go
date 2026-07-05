package acp

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/monsterxx03/tachi/agent"
)

// expectedACPCmds lists all static commands we expect in buildACPAvailableCommands.
var expectedACPCmds = []struct {
	name     string
	hasInput bool
}{
	{name: "commit"},
	{name: "review"},
	{name: "init"},
	{name: "compact"},
	{name: "usage"},
	{name: "mcp", hasInput: true},
	{name: "skill", hasInput: true},
	{name: "transcript"},
	{name: "research", hasInput: true},
}

func TestBuildACPAvailableCommands_StaticCommands(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Collect all returned command names for duplicate checking
	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	// Check each expected command
	for _, ec := range expectedACPCmds {
		t.Run(ec.name, func(t *testing.T) {
			count, found := names[ec.name]
			assert.True(t, found, "command %q should be present in available commands", ec.name)
			assert.Equal(t, 1, count, "command %q should appear exactly once", ec.name)

			// Find the command and check fields
			for _, c := range cmds {
				if c.Name == ec.name {
					assert.NotEmpty(t, c.Description, "command %q should have a non-empty description", ec.name)
					if ec.hasInput {
						assert.NotNil(t, c.Input, "command %q should have Input set", ec.name)
						if c.Input != nil {
							assert.NotNil(t, c.Input.Unstructured, "command %q Input should have Unstructured", ec.name)
							assert.NotEmpty(t, c.Input.Unstructured.Hint, "command %q Input should have a non-empty Hint", ec.name)
						}
					} else {
						assert.Nil(t, c.Input, "command %q should NOT have Input set", ec.name)
					}
				}
			}
		})
	}
}

func TestBuildACPAvailableCommands_Count(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Should have exactly len(expectedACPCmds) commands (no skills configured)
	assert.Len(t, cmds, len(expectedACPCmds))
}

func TestBuildACPAvailableCommands_NoDuplicates(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	for name, count := range names {
		assert.Equal(t, 1, count, "command %q appears %d times — no duplicates expected", name, count)
	}
}

func TestBuildACPAvailableCommands_NilAgent(t *testing.T) {
	// Passing nil should not panic; returns empty static commands
	cmds := buildACPAvailableCommands(nil)
	assert.Len(t, cmds, len(expectedACPCmds),
		"nil agent should still return static commands (no skills)")
}
