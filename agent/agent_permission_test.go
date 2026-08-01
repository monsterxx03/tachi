package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/monsterxx03/tachi/agent/hooks"
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

	a := newTestAgent(t,mp)
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

	a := newTestAgent(t,mp)
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

	a := newTestAgent(t,mp)
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

	a := newTestAgent(t,mp)
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

	a := newTestAgent(t,mp)
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

	a := newTestAgent(t,mp)
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
	assert.Equal(t, ExitReasonCancelled, result.ExitReason)
}

// captureHookEvents installs a hook dispatcher that records permission
// request/result events as compact strings, so tests can assert that the
// Herdr-style "blocked" notification fires around a bash ask prompt.
func captureHookEvents(a *AIAgent) *[]string {
	var mu sync.Mutex
	got := make([]string, 0, 4)
	d := hooks.NewDispatcher(nil)
	d.RegisterCallback(hooks.EventPermissionRequest, "test", func(_ context.Context, _ string, payload []byte) {
		var p hooks.Payload
		if err := json.Unmarshal(payload, &p); err != nil {
			return
		}
		mu.Lock()
		got = append(got, fmt.Sprintf("request tool=%s args=%s", p.ToolName, p.ToolArgs))
		mu.Unlock()
	})
	d.RegisterCallback(hooks.EventPermissionResult, "test", func(_ context.Context, _ string, payload []byte) {
		var p hooks.Payload
		if err := json.Unmarshal(payload, &p); err != nil {
			return
		}
		mu.Lock()
		got = append(got, fmt.Sprintf("result tool=%s approved=%v", p.ToolName, p.Approved))
		mu.Unlock()
	})
	a.Config.HookDispatcher = d
	return &got
}

func TestAgentLoop_BashPolicyAsk_TUIHooksDispatch(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
			textSeq("pushed"),
		},
	}

	a := newTestAgent(t, mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))
	got := captureHookEvents(a)

	ch := a.RunConversationStream(t.Context(), nil, "push it", "", llm.ChatOptions{MaxTokens: 4096})
	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(ConfirmAllowOnce)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, "pushed", result.Response)
	assert.Equal(t, []string{
		"request tool=Bash args={\"command\":\"git push origin main\"}",
		"result tool=Bash approved=true",
	}, *got, "permission hooks should fire around the bash ask prompt")
}

func TestAgentLoop_BashPolicyAsk_TUIDenyDispatchesDeniedResult(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
		},
	}

	a := newTestAgent(t, mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))
	got := captureHookEvents(a)

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
	assert.Equal(t, ExitReasonCancelled, result.ExitReason)
	assert.Equal(t, []string{
		"request tool=Bash args={\"command\":\"git push origin main\"}",
		"result tool=Bash approved=false",
	}, *got, "denied bash ask should still report permission_request + denied result")
}

func TestAgentLoop_BashPolicyNoRules_Unaffected(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"rm -rf /"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(t,mp)
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

	// User rules stack on top of built-ins.
	cfg.Permissions.Bash.Deny = []string{"git clone *"}
	p = NewPermissionPolicyFromConfig(cfg, "", nil)
	require.NotNil(t, p)
	d, rule := p.CheckBash("git clone https://example.com/r.git")
	assert.Equal(t, permission.DecisionDeny, d)
	assert.Equal(t, "git clone *", rule)
	// Root deletion is still caught by the built-in structured guard (with
	// no user rule for it, the guard reports itself).
	d, rule = p.CheckBash("rm -rf /")
	assert.Equal(t, permission.DecisionDeny, d)
	assert.Equal(t, "rm -rf / (builtin)", rule)
	// Deeper absolute-path deletion (e.g. /tmp/x) is NOT builtin-guarded —
	// it falls through to user policy (Allow here, no matching user rule).
	d, rule = p.CheckBash("rm -rf /tmp/x")
	assert.Equal(t, permission.DecisionAllow, d, "deeper absolute-path deletion is user policy now")
	assert.Equal(t, "", rule)

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

func TestNewPermissionPolicyFromConfig_ProjectAllowIgnored(t *testing.T) {
	// Project file adds an ask rule AND an allow rule that would exempt a
	// global ask — the allow must be ignored (projects can only tighten).
	root := t.TempDir()
	dir := filepath.Join(root, ".tachi")
	require.NoError(t, os.MkdirAll(dir, 0755))
	content := `permissions:
  bash:
    ask: ["npm *"]
    allow: ["git status*", "npm test*"]
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(content), 0644))

	cfg := config.DefaultConfig()
	cfg.Permissions.Bash.Ask = []string{"git *"}
	p := NewPermissionPolicyFromConfig(cfg, root, nil)
	require.NotNil(t, p)

	// Project ask is honored (tightening works).
	if d, _ := p.CheckBash("npm install"); d != permission.DecisionAsk {
		t.Error("project ask should be honored")
	}
	// Project allow must NOT exempt the global ask rule.
	if d, _ := p.CheckBash("git status"); d != permission.DecisionAsk {
		t.Error("project allow must be ignored — git status should still hit global ask")
	}
	// Project allow must NOT exempt its own project ask either.
	if d, _ := p.CheckBash("npm test"); d != permission.DecisionAsk {
		t.Error("project allow must be ignored — npm test should still hit project ask")
	}
}
