package agent

import (
	"context"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/stretchr/testify/assert"
)

func TestRegisterToolsForSubagent_AllTools(t *testing.T) {
	// Create a parent agent with some tools registered
	parent := NewAIAgent(nil, "test-model", 50)
	parent.RegisterTools() // registers ReadFile, WriteFile, EditFile, Glob, Grep, Bash, AskUserQuestion

	// Also register a SubAgent tool (which should be excluded from child)
	mockRunner := &mockSubagentRunner{toolNames: []string{"ReadFile"}, maxOutput: 16384}
	parent.RegisterTool(agenttools.NewSubagentTool(mockRunner))

	// Create a child agent and register tools for subagent (no filter)
	child := NewAIAgent(nil, "test-model", 50)
	child.RegisterToolsForSubagent(parent, nil)

	childSchemas := child.ToolSchemas()
	childNames := make(map[string]bool)
	for _, s := range childSchemas {
		childNames[s.Name] = true
	}

	// Should have standard tools
	assert.True(t, childNames["ReadFile"], "ReadFile should be inherited")
	assert.True(t, childNames["WriteFile"], "WriteFile should be inherited")
	assert.True(t, childNames["EditFile"], "EditFile should be inherited")
	assert.True(t, childNames["Glob"], "Glob should be inherited")
	assert.True(t, childNames["Grep"], "Grep should be inherited")
	assert.True(t, childNames["Bash"], "Bash should be inherited")

	// Should NOT have AskUserQuestion or SubAgent
	assert.False(t, childNames[agenttools.ToolNameAskUser], "AskUserQuestion should be excluded")
	assert.False(t, childNames[agenttools.ToolNameSubAgent], "SubAgent should be excluded")
}

func TestRegisterToolsForSubagent_Filtered(t *testing.T) {
	parent := NewAIAgent(nil, "test-model", 50)
	parent.RegisterTools()

	child := NewAIAgent(nil, "test-model", 50)
	child.RegisterToolsForSubagent(parent, []string{"ReadFile", "Grep", "Glob"})

	childSchemas := child.ToolSchemas()
	childNames := make(map[string]bool)
	for _, s := range childSchemas {
		childNames[s.Name] = true
	}

	// Should only have the allowed tools
	assert.True(t, childNames["ReadFile"], "ReadFile should be registered")
	assert.True(t, childNames["Grep"], "Grep should be registered")
	assert.True(t, childNames["Glob"], "Glob should be registered")

	// Should NOT have tools not in the whitelist
	assert.False(t, childNames["WriteFile"], "WriteFile should not be registered")
	assert.False(t, childNames["EditFile"], "EditFile should not be registered")
	assert.False(t, childNames["Bash"], "Bash should not be registered")
	assert.False(t, childNames[agenttools.ToolNameAskUser], "AskUserQuestion should always be excluded")
	assert.False(t, childNames[agenttools.ToolNameSubAgent], "SubAgent should always be excluded")
}

func TestRegisterToolsForSubagent_AskUserAlwaysExcluded(t *testing.T) {
	parent := NewAIAgent(nil, "test-model", 50)
	parent.RegisterTools()

	// Even if explicitly allowed, AskUserQuestion should be excluded
	child := NewAIAgent(nil, "test-model", 50)
	child.RegisterToolsForSubagent(parent, []string{agenttools.ToolNameAskUser, "ReadFile"})

	childSchemas := child.ToolSchemas()
	childNames := make(map[string]bool)
	for _, s := range childSchemas {
		childNames[s.Name] = true
	}

	assert.True(t, childNames["ReadFile"], "ReadFile should be registered")
	assert.False(t, childNames[agenttools.ToolNameAskUser], "AskUserQuestion should always be excluded")
}

func TestSubagentMaxIterations_Default(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	assert.Equal(t, defaultSubagentMaxIterations, agent.SubagentMaxIterations())
}

func TestSubagentMaxIterations_Configured(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	agent.subagentMaxIterations = 20
	assert.Equal(t, 20, agent.SubagentMaxIterations())
}

func TestSubagentMaxConcurrency_Default(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	assert.Equal(t, defaultSubagentMaxConcurrency, agent.SubagentMaxConcurrency())
}

func TestSubagentMaxConcurrency_Configured(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	agent.subagentMaxConcurrency = 8
	assert.Equal(t, 8, agent.SubagentMaxConcurrency())
}

func TestSubagentMaxOutputChars_Default(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	assert.Equal(t, defaultSubagentMaxOutputChars, agent.SubagentMaxOutputChars())
}

func TestSubagentMaxOutputChars_Configured(t *testing.T) {
	agent := NewAIAgent(nil, "test", 50)
	agent.subagentMaxOutputChars = 8192
	assert.Equal(t, 8192, agent.SubagentMaxOutputChars())
}

func TestSubagentProvider_FallbackToMain(t *testing.T) {
	mainProvider := &mockStreamProvider{name: "main-provider"}
	agent := NewAIAgent(mainProvider, "main-model", 50)

	assert.Equal(t, mainProvider, agent.SubagentProvider())
	assert.Equal(t, "main-model", agent.SubagentModel())
}

func TestSubagentProvider_Dedicated(t *testing.T) {
	mainProvider := &mockStreamProvider{name: "main-provider"}
	subProvider := &mockStreamProvider{name: "sub-provider"}
	agent := NewAIAgent(mainProvider, "main-model", 50)
	agent.subagentProvider = subProvider
	agent.subagentModel = "sub-model"

	assert.Equal(t, subProvider, agent.SubagentProvider())
	assert.Equal(t, "sub-model", agent.SubagentModel())
}

func TestSubagentExecutor_AvailableToolNames(t *testing.T) {
	parent := NewAIAgent(nil, "test-model", 50)
	parent.RegisterTools()

	mockRunner := &mockSubagentRunner{toolNames: []string{"ReadFile"}, maxOutput: 16384}
	parent.RegisterTool(agenttools.NewSubagentTool(mockRunner))

	executor := NewSubagentExecutor(parent)
	names := executor.AvailableToolNames()

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}

	assert.True(t, nameSet["ReadFile"])
	assert.True(t, nameSet["Bash"])
	assert.False(t, nameSet[agenttools.ToolNameAskUser], "AskUserQuestion should be excluded from available names")
	assert.False(t, nameSet[agenttools.ToolNameSubAgent], "SubAgent should be excluded from available names")
}

func TestSubagentExecutor_MaxOutputChars(t *testing.T) {
	parent := NewAIAgent(nil, "test-model", 50)
	parent.subagentMaxOutputChars = 8192
	executor := NewSubagentExecutor(parent)
	assert.Equal(t, 8192, executor.MaxOutputChars())
}

// mockSubagentRunner is a simple mock for SubagentRunner used in tests.
type mockSubagentRunner struct {
	toolNames []string
	maxOutput int
}

func (m *mockSubagentRunner) RunSubagent(_ context.Context, _ agenttools.SubagentArgs) (string, error) {
	return "", nil
}
func (m *mockSubagentRunner) AvailableToolNames() []string { return m.toolNames }
func (m *mockSubagentRunner) MaxOutputChars() int          { return m.maxOutput }
