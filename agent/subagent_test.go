package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/agent/subagent"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
)

func TestSubagentExecutor_MaxOutputChars(t *testing.T) {
	// MaxOutputChars with cfg value
	cfg := config.SubagentConfig{MaxOutputChars: 8192}
	a := &mockAgent{}
	executor := subagent.NewExecutor(a, cfg)
	assert.Equal(t, 8192, executor.MaxOutputChars())

	// MaxOutputChars default fallback
	cfg2 := config.SubagentConfig{MaxOutputChars: 0}
	executor2 := subagent.NewExecutor(a, cfg2)
	assert.Equal(t, subagent.DefaultMaxOutputChars, executor2.MaxOutputChars())
}

func TestSubagentExecutor_MaxIterations(t *testing.T) {
	cfg := config.SubagentConfig{MaxIterations: 20}
	a := &mockAgent{}
	executor := subagent.NewExecutor(a, cfg)

	// MaxIterations is read from subagent.max_iterations config (not from LLM args)
	assert.NotNil(t, executor)
}

func TestSubagentExecutor_MaxConcurrency(t *testing.T) {
	cfg := config.SubagentConfig{MaxConcurrency: 8}
	a := &mockAgent{}
	executor := subagent.NewExecutor(a, cfg)
	assert.NotNil(t, executor)

	// Default fallback
	cfg2 := config.SubagentConfig{MaxConcurrency: 0}
	executor2 := subagent.NewExecutor(a, cfg2)
	assert.NotNil(t, executor2)
}

func TestSubagentExecutor_AvailableToolNames(t *testing.T) {
	a := &mockAgent{toolNames: []string{"ReadFile", "Bash", "Grep", agenttools.ToolNameAskUser, agenttools.ToolNameSubAgent}}
	executor := subagent.NewExecutor(a, config.SubagentConfig{})

	names := executor.AvailableToolNames()
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}

	assert.True(t, nameSet["ReadFile"])
	assert.True(t, nameSet["Bash"])
	assert.False(t, nameSet[agenttools.ToolNameAskUser], "AskUserQuestion should be excluded")
	assert.False(t, nameSet[agenttools.ToolNameSubAgent], "SubAgent should be excluded")
}

func TestSubagentProvider_FallbackToMain(t *testing.T) {
	mainProvider := &mockStreamProvider{name: "main-provider"}
	agent := newBareTestAgent(t, mainProvider, 50)

	// Falls back to the main provider (usage-wrapped by construction).
	got := agent.SubagentProvider()
	if got == nil || got.Name() != "main-provider" {
		t.Errorf("SubagentProvider = %v, want the main provider (usage-wrapped)", got)
	}
}

func TestSubagentProvider_Dedicated(t *testing.T) {
	mainProvider := &mockStreamProvider{name: "main-provider"}
	subProvider := &mockStreamProvider{name: "sub-provider"}
	agent := newBareTestAgent(t, mainProvider, 50)
	agent.Config.SubagentProvider = subProvider

	assert.Equal(t, subProvider, agent.SubagentProvider())
}

func TestGetTool(t *testing.T) {
	agent := newBareTestAgent(t, nil, 50)
	agent.RegisterTools()

	tool := agent.GetTool("ReadFile")
	assert.NotNil(t, tool)
	assert.Equal(t, "ReadFile", tool.Name())

	tool = agent.GetTool("NonExistent")
	assert.Nil(t, tool)
}

func TestNewChildAgent(t *testing.T) {
	parent := newBareTestAgent(t, nil, 50)
	parent.RegisterTools()

	provider := &mockStreamProvider{name: "child-provider"}
	allowedTools := []string{"ReadFile", "Grep", "Glob"}

	child := parent.NewChildAgent(logger.Default(), provider, 10, allowedTools, "session-123")

	assert.NotNil(t, child, "NewChildAgent should return a non-nil ChildAgent")
}

func TestChildAdapter_Run(t *testing.T) {
	parent := newBareTestAgent(t, nil, 50)
	parent.RegisterTools()

	provider := &mockStreamProvider{
		name:      "child-provider",
		sequences: [][]llm.StreamEvent{textSeq("hello from child")},
	}
	allowedTools := []string{"ReadFile", "Grep", "Glob"}

	child := parent.NewChildAgent(logger.Default(), provider, 10, allowedTools, "session-123")

	ch := child.Run(t.Context(), provider, "You are a test agent.", "Say hello", llm.ChatOptions{MaxTokens: 100})

	gotText := false
	gotComplete := false
	for event := range ch {
		switch event.Type {
		case subagent.StreamEventTextDelta:
			gotText = true
		case subagent.StreamEventTurnComplete:
			gotComplete = true
		case subagent.StreamEventError:
			t.Logf("unexpected error: %v", event.Error)
		}
	}

	assert.True(t, gotText, "should get text delta")
	assert.True(t, gotComplete, "should get turn complete")
}

// mockAgent implements subagent.Agent for testing the executor.
type mockAgent struct {
	toolNames []string
}

func (m *mockAgent) SubagentProvider() llm.Provider      { return nil }
func (m *mockAgent) ParentSessionID() string             { return "" }
func (m *mockAgent) Logger() *logger.Logger              { return logger.Default() }
func (m *mockAgent) ToolNames() []string                 { return m.toolNames }
func (m *mockAgent) GetTool(name string) agenttools.Tool { return nil }
func (m *mockAgent) NewChildAgent(logger *logger.Logger, provider llm.Provider, maxIterations int, allowedTools []string, subagentSessionID string) subagent.ChildAgent {
	return nil
}
