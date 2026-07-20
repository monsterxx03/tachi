package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bashStub returns a non-parallel stub Bash tool (like the real one) that
// echoes the command instead of executing it.
func bashStub() *stubTool {
	return &stubTool{
		name:     "Bash",
		desc:     "Run a command",
		parallel: false, // real Bash is non-parallel → sequential path → policy check applies
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
			if err := json.Unmarshal([]byte(args), &m); err != nil {
				return "", err
			}
			cmd, _ := m["command"].(string)
			return "ran: " + cmd, nil
		},
	}
}

// toolResultContains reports whether any ToolResult event matches isError and
// contains substr.
func toolResultContains(events []AgentEvent, isError bool, substr string) bool {
	for _, e := range events {
		if e.Type == AgentEventToolResult && e.ToolIsError == isError && strings.Contains(e.ToolResult, substr) {
			return true
		}
	}
	return false
}

// countConfirmations returns how many AgentEventToolConfirmation events occurred.
func countConfirmations(events []AgentEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == AgentEventToolConfirmation {
			n++
		}
	}
	return n
}

func TestAgentLoop_BashPolicyDeny(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git status && rm -rf /"}`),
			textSeq("blocked, will report to user"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Deny: []string{"rm -rf *"}}, permission.Rules{}))

	result, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "clean up", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotNil(t, result)
	assert.Equal(t, "blocked, will report to user", result.Response)
	assert.True(t, toolResultContains(events, true, "blocked by permission rule"),
		"deny error should be fed back to the LLM as a tool error; events: %v", events)
}

func TestAgentLoop_BashPolicyAsk_NonInteractiveDenied(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
			textSeq("cannot run that here"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(bashStub())
	a.SetPermissionMode(PermissionModeSkip) // channel / subagent / tachi -p
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	result, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "push it", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotNil(t, result)
	assert.Equal(t, "cannot run that here", result.Response)
	assert.True(t, toolResultContains(events, true, "requires interactive approval"),
		"ask in non-interactive mode should be denied with guidance")
	assert.Equal(t, 0, countConfirmations(events), "no interactive prompt in Skip mode")
}

func TestAgentLoop_BashPolicyAsk_NonInteractiveAutoApprove(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
			textSeq("pushed"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(bashStub())
	a.SetPermissionMode(PermissionModeSkip)
	a.SetAutoApprovePolicyAsks(true) // ACP "allow all" path
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	result, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "push it", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotNil(t, result)
	assert.Equal(t, "pushed", result.Response)
	assert.True(t, toolResultContains(events, false, "ran: git push origin main"),
		"auto-approved ask should execute")
}

func TestAgentLoop_BashPolicyAsk_TUIAllowOnce(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
			textSeq("pushed"),
		},
	}

	a := newTestAgent(mp)
	a.SetPermissionMode(PermissionModeTUI) // newTestAgent defaults to Skip mode
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	ch := a.RunConversationStream(t.Context(), nil, "push it", "", llm.ChatOptions{MaxTokens: 4096})
	var result *RunResult
	var events []AgentEvent
	for e := range ch {
		events = append(events, e)
		if e.Type == AgentEventToolConfirmation {
			assert.Contains(t, e.ToolDiff, "git push origin main")
			assert.Contains(t, e.ToolDiff, "git push*")
			a.ConfirmTool(ConfirmAllowOnce)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, "pushed", result.Response)
	assert.Equal(t, 1, countConfirmations(events))
	assert.True(t, toolResultContains(events, false, "ran: git push origin main"))
}

func TestAgentLoop_BashPolicyAsk_TUIAllowAlwaysRemembersExact(t *testing.T) {
	cmdArgs := `{"command":"git push origin main"}`
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", cmdArgs),
			toolCallSeq("Bash", "call-2", cmdArgs), // same command again
			textSeq("both pushed"),
		},
	}

	a := newTestAgent(mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	ch := a.RunConversationStream(t.Context(), nil, "push twice", "", llm.ChatOptions{MaxTokens: 4096})
	var result *RunResult
	var events []AgentEvent
	for e := range ch {
		events = append(events, e)
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(ConfirmAllowAlways)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, "both pushed", result.Response)
	assert.Equal(t, 1, countConfirmations(events),
		"second identical command should skip confirmation (session-exact remember)")
}

func TestAgentLoop_BashPolicyAsk_TUIDenyCancelsTurn(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
		},
	}

	a := newTestAgent(mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	ch := a.RunConversationStream(t.Context(), nil, "push it", "", llm.ChatOptions{MaxTokens: 4096})
	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(ConfirmDeny)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, "cancelled", result.ExitReason)
}

func TestAgentLoop_BashPolicyNoRules_Unaffected(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"rm -rf /"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(bashStub())
	// No policy at all (nil) — pre-feature behavior: everything executes.
	result, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "do it", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response)
	assert.True(t, toolResultContains(events, false, "ran: rm -rf /"))
	assert.Equal(t, 0, countConfirmations(events))
}

func TestNewPermissionPolicyFromConfig(t *testing.T) {
	assert.Nil(t, NewPermissionPolicyFromConfig(nil, "", nil), "nil config → nil policy")

	// Default config (no user rules) still gets the built-in deny rules.
	cfg := config.DefaultConfig()
	p := NewPermissionPolicyFromConfig(cfg, "", nil)
	require.NotNil(t, p, "built-in deny rules should produce a non-nil policy")
	d, _ := p.CheckBash("rm -rf /")
	assert.Equal(t, permission.DecisionDeny, d, "built-in rules should deny root deletion")
	d, _ = p.CheckBash("git status")
	assert.Equal(t, permission.DecisionAllow, d, "built-in rules should not affect normal commands")

	// User rules stack on top of built-ins (builtins match first on overlap).
	cfg.Permissions.Bash.Deny = []string{"git clone *"}
	p = NewPermissionPolicyFromConfig(cfg, "", nil)
	require.NotNil(t, p)
	d, rule := p.CheckBash("git clone https://example.com/r.git")
	assert.Equal(t, permission.DecisionDeny, d)
	assert.Equal(t, "git clone *", rule)
	d, rule = p.CheckBash("rm -rf /tmp/x")
	assert.Equal(t, permission.DecisionDeny, d)
	assert.Equal(t, "rm -rf /*", rule, "overlapping command hits the builtin rule first")

	// disable_builtin_deny (global) → no built-ins; nil policy when no user rules.
	cfg2 := config.DefaultConfig()
	cfg2.Permissions.Bash.DisableBuiltinDeny = true
	assert.Nil(t, NewPermissionPolicyFromConfig(cfg2, "", nil))

	// With builtins disabled, the previously-denied command is allowed.
	cfg2.Permissions.Bash.Deny = []string{"example *"}
	p2 := NewPermissionPolicyFromConfig(cfg2, "", nil)
	require.NotNil(t, p2)
	d, _ = p2.CheckBash("rm -rf /")
	assert.Equal(t, permission.DecisionAllow, d, "builtins disabled → root deletion allowed")
}
