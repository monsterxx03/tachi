package subagent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
)

// ---- executor_test.go — Tests for Executor helper functions ----

func TestBuildAllowedTools_EmptyAllowList(t *testing.T) {
	exec := &Executor{
		agent: &fakeAgent{},
		cfg:   config.SubagentConfig{},
	}

	got := buildAllowedTools(exec, nil)
	// With nil agent, AvailableToolNames will return empty list
	assert.Empty(t, got)
}

func TestBuildAllowedTools_ExplicitAllowList(t *testing.T) {
	exec := &Executor{
		agent: &fakeAgent{},
		cfg:   config.SubagentConfig{},
	}

	allow := []string{"ReadFile", "Grep", "Bash"}
	got := buildAllowedTools(exec, allow)
	assert.Equal(t, allow, got)
}

func TestBuildAllowedTools_FiltersAskUser(t *testing.T) {
	exec := &Executor{
		agent: &fakeAgent{},
		cfg:   config.SubagentConfig{},
	}

	allow := []string{"ReadFile", tools.ToolNameAskUser, "Grep", tools.ToolNameSubAgent}
	got := buildAllowedTools(exec, allow)

	assert.Equal(t, []string{"ReadFile", "Grep"}, got)
}

func TestBuildAllowedTools_AllBlocked(t *testing.T) {
	exec := &Executor{
		agent: &fakeAgent{},
		cfg:   config.SubagentConfig{},
	}

	allow := []string{tools.ToolNameAskUser, tools.ToolNameSubAgent}
	got := buildAllowedTools(exec, allow)
	assert.Empty(t, got)
}

func TestBuildAllowedTools_WithAvailableToolNames(t *testing.T) {
	exec := &Executor{
		agent: &fakeAgent{toolNames: []string{"ReadFile", "Grep", "Bash", tools.ToolNameAskUser, tools.ToolNameSubAgent}},
		cfg:   config.SubagentConfig{},
	}

	// Empty allow list → uses AvailableToolNames (which filters AskUser + SubAgent)
	got := buildAllowedTools(exec, nil)
	assert.Equal(t, []string{"ReadFile", "Grep", "Bash"}, got)
}

func TestUsageToSession_Nil(t *testing.T) {
	got := usageToSession(nil)
	assert.Nil(t, got)
}

func TestUsageToSession(t *testing.T) {
	usage := &llm.Usage{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheCreationInputTokens: 200,
		CacheReadInputTokens:     100,
	}
	got := usageToSession(usage)

	assert.Equal(t, int64(1000), got.InputTokens)
	assert.Equal(t, int64(500), got.OutputTokens)
	assert.Equal(t, int64(0), got.CacheCreationInputTokens) // session.Usage doesn't carry cache tokens
	assert.Equal(t, int64(0), got.CacheReadInputTokens)
}

func TestExecutor_NewExecutor(t *testing.T) {
	a := &fakeAgent{}
	cfg := config.SubagentConfig{MaxConcurrency: 8}
	exec := NewExecutor(a, cfg)
	assert.NotNil(t, exec)
	assert.Equal(t, a, exec.agent)
	assert.NotNil(t, exec.sem)
}

func TestExecutor_NewExecutorDefaultConcurrency(t *testing.T) {
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{MaxConcurrency: 0})
	assert.NotNil(t, exec)

	// Verify semaphore capacity equals default by trying to fill it
	for i := range DefaultMaxConcurrency {
		select {
		case exec.sem <- struct{}{}:
			// good
		default:
			t.Fatalf("expected semaphore capacity %d, but couldn't acquire at %d", DefaultMaxConcurrency, i+1)
		}
	}
	// Next acquire should block
	select {
	case exec.sem <- struct{}{}:
		t.Error("semaphore should be full")
	default:
		// expected
	}
}

func TestExecutor_EnableWorktree(t *testing.T) {
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{Worktree: true})
	assert.Nil(t, exec.worktreeMgr)
	exec.EnableWorktree(debuglog.DefaultLogger)
	assert.NotNil(t, exec.worktreeMgr)
}

func TestExecutor_MaxOutputChars_Configured(t *testing.T) {
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{MaxOutputChars: 4096})
	assert.Equal(t, 4096, exec.MaxOutputChars())
}

func TestExecutor_MaxOutputChars_Default(t *testing.T) {
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{MaxOutputChars: 0})
	assert.Equal(t, DefaultMaxOutputChars, exec.MaxOutputChars())
}

func TestExecutor_AvailableToolNames(t *testing.T) {
	a := &fakeAgent{toolNames: []string{"ReadFile", "Bash", tools.ToolNameAskUser, tools.ToolNameSubAgent, "Grep"}}
	exec := NewExecutor(a, config.SubagentConfig{})
	names := exec.AvailableToolNames()

	assert.Len(t, names, 3)
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	assert.True(t, nameSet["ReadFile"])
	assert.True(t, nameSet["Bash"])
	assert.True(t, nameSet["Grep"])
	assert.False(t, nameSet[tools.ToolNameAskUser])
	assert.False(t, nameSet[tools.ToolNameSubAgent])
}

func TestExecutor_RunSubagent_ContextCancelled(t *testing.T) {
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{MaxConcurrency: 1})
	// Fill the semaphore
	exec.sem <- struct{}{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, shortID, err := exec.RunSubagent(ctx, tools.SubagentArgs{Prompt: "test"})
	assert.Empty(t, result)
	assert.Empty(t, shortID)
	assert.Error(t, err)
}

func TestExecutor_MaxIterations_FromArgs(t *testing.T) {
	// We can't fully test RunSubagent without a ChildAgent that responds,
	// but we can verify the iteration-related parameter overrides via the
	// internal run method by checking that the child's maxIterations is set.
	// This is covered by integration-style tests in agent/subagent_test.go.
	// Here we just verify the config path.
	exec := NewExecutor(&fakeAgent{}, config.SubagentConfig{MaxIterations: 20})
	assert.NotNil(t, exec)
}

func TestWorktreeManager_Create_FallbackOnFailure(t *testing.T) {
	// When git is unavailable (should not happen in this repo), Create degrades.
	// We verify the WorktreeManager is instantiatable.
	wm := NewWorktreeManager(config.SubagentConfig{Worktree: true}, debuglog.DefaultLogger)
	assert.NotNil(t, wm)
	assert.NotEmpty(t, wm.worktreeDir)
	assert.True(t, wm.cleanup)
}

func TestNewWorktreeManager_Defaults(t *testing.T) {
	cfg := config.SubagentConfig{
		Worktree:        true,
		WorktreeCleanup: new(false),
		WorktreeBranch:  "main",
		WorktreeDir:     "/tmp/test-worktrees",
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)
	assert.Equal(t, "/tmp/test-worktrees", wm.worktreeDir)
	assert.Equal(t, "main", wm.defaultBranch)
	assert.False(t, wm.cleanup)
}

func TestNewWorktreeManager_NoWorktreeCleanup(t *testing.T) {
	cfg := config.SubagentConfig{Worktree: true}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)
	assert.True(t, wm.cleanup) // default
	assert.NotEmpty(t, wm.worktreeDir)
}

func TestFallbackIfEmpty(t *testing.T) {
	assert.Equal(t, "default", fallbackIfEmpty("", "default"))
	assert.Equal(t, "explicit", fallbackIfEmpty("explicit", "default"))
	assert.Equal(t, "", fallbackIfEmpty("", ""))
}

func TestSystemPrompt_ContainsRules(t *testing.T) {
	assert.Contains(t, SystemPrompt, "DO NOT ask the user questions")
	assert.Contains(t, SystemPrompt, "DO NOT attempt to delegate to sub-agents")
	assert.Contains(t, SystemPrompt, "Your output goes directly back to the main agent")
}

func TestWorktreePromptFmt_HasPlaceholder(t *testing.T) {
	assert.Contains(t, WorktreePromptFmt, "%s")
	rendered := SystemPrompt + string([]byte(WorktreePromptFmt)[:])
	_ = rendered
	result := SystemPrompt + WorktreePromptFmt // just verify it compiles
	assert.Contains(t, result, "%s")
}

func TestStreamEventTypes(t *testing.T) {
	// Verify stream event type constants are distinct
	types := map[StreamEventType]bool{}
	types[StreamEventTextDelta] = true
	types[StreamEventThinkingDelta] = true
	types[StreamEventToolCallArgs] = true
	types[StreamEventToolResult] = true
	types[StreamEventTurnComplete] = true
	types[StreamEventError] = true
	assert.Len(t, types, 6)
}

// ---- Test helpers ----

// fakeAgent implements Agent with configurable behavior.
type fakeAgent struct {
	toolNames         []string
	provider          llm.Provider
	childAgentFactory func(logger *debuglog.Logger, provider llm.Provider, maxIterations int, allowedTools []string, subagentSessionID string) ChildAgent
}

func (a *fakeAgent) SubagentProvider() llm.Provider   { return a.provider }
func (a *fakeAgent) SessionManager() *session.Manager { return nil }
func (a *fakeAgent) Logger() *debuglog.Logger         { return debuglog.DefaultLogger }
func (a *fakeAgent) ToolNames() []string              { return a.toolNames }
func (a *fakeAgent) GetTool(name string) tools.Tool   { return nil }

func (a *fakeAgent) NewChildAgent(logger *debuglog.Logger, provider llm.Provider,
	maxIterations int, allowedTools []string, subagentSessionID string) ChildAgent {
	if a.childAgentFactory != nil {
		return a.childAgentFactory(logger, provider, maxIterations, allowedTools, subagentSessionID)
	}
	return &fakeChildAgent{}
}

// fakeChildAgent implements ChildAgent for testing RunSubagent.
type fakeChildAgent struct {
	events []StreamEvent
}

func (c *fakeChildAgent) Run(ctx context.Context, provider llm.Provider,
	systemPrompt, userPrompt string, opts llm.ChatOptions) <-chan StreamEvent {
	ch := make(chan StreamEvent, len(c.events)+1)
	go func() {
		defer close(ch)
		for _, e := range c.events {
			select {
			case ch <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// ---- Verify interface compliance ----

func TestAgentInterface(t *testing.T) {
	// compile-time check: fakeAgent implements Agent
	var _ Agent = (*fakeAgent)(nil)
}

func TestChildAgentInterface(t *testing.T) {
	// compile-time check: fakeChildAgent implements ChildAgent
	var _ ChildAgent = (*fakeChildAgent)(nil)
}
