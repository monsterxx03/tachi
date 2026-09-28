package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mockRunner implements SubagentRunner for testing.
type mockRunner struct {
	result         string
	err            error
	toolNames      []string
	maxOutputChars int
	calledArgs     SubagentArgs
}

func (m *mockRunner) RunSubagent(_ context.Context, args SubagentArgs) (string, string, *SubagentResult, error) {
	m.calledArgs = args
	return m.result, "", nil, m.err
}

func (m *mockRunner) AvailableToolNames() []string { return m.toolNames }
func (m *mockRunner) MaxOutputChars() int          { return m.maxOutputChars }

func TestSubagentTool_Name(t *testing.T) {
	tool := NewSubagentTool(&mockRunner{})
	if tool.Name() != ToolNameSubAgent {
		t.Errorf("expected Name() = %s, got %s", ToolNameSubAgent, tool.Name())
	}
}

func TestSubagentTool_Parallel(t *testing.T) {
	tool := NewSubagentTool(&mockRunner{})
	if !tool.Parallel() {
		t.Error("expected Parallel() = true")
	}
}

func TestSubagentTool_Required(t *testing.T) {
	tool := NewSubagentTool(&mockRunner{})
	required := tool.Required()
	if len(required) != 1 || required[0] != "prompt" {
		t.Errorf("expected Required() = [prompt], got %v", required)
	}
}

// The tool names a sub-agent may be given reach the model through the
// allowed_tools items.enum, and ONLY through it: the enum is what constrains the
// value, so prose repeating the list is a fact stated twice. This test used to
// require the names in Description() — that is exactly what was removed.
func TestSubagentTool_ToolNamesReachTheSchema(t *testing.T) {
	runner := &mockRunner{
		toolNames: []string{"ReadFile", "Grep", "Glob", "Bash"},
	}
	tool := NewSubagentTool(runner)

	items, ok := tool.Properties()["allowed_tools"].Items.(map[string]any)
	if !ok {
		t.Fatal("allowed_tools should carry a typed items schema")
	}
	enum, ok := items["enum"].([]any)
	if !ok {
		t.Fatal("allowed_tools items should carry an enum of the tool names")
	}
	got := make([]string, 0, len(enum))
	for _, v := range enum {
		got = append(got, v.(string))
	}
	if strings.Join(got, ", ") != "ReadFile, Grep, Glob, Bash" {
		t.Errorf("expected the runner's tool names in the enum, got %v", got)
	}
	if strings.Contains(tool.Description(), "ReadFile, Grep, Glob, Bash") {
		t.Error("the names belong in the enum, not repeated in the description")
	}
}

func TestSubagentTool_ExecuteContext_Success(t *testing.T) {
	runner := &mockRunner{
		result:         "task completed successfully",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	result, err := tool.ExecuteContext(t.Context(), `{"prompt":"find all TODO comments"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "task completed successfully" {
		t.Errorf("unexpected result: %s", result)
	}
	if runner.calledArgs.Prompt != "find all TODO comments" {
		t.Errorf("prompt not passed correctly: %s", runner.calledArgs.Prompt)
	}
}

func TestSubagentTool_ExecuteContext_WithAllowedTools(t *testing.T) {
	runner := &mockRunner{
		result:         "done",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	_, err := tool.ExecuteContext(t.Context(),
		`{"prompt":"search code","allowed_tools":["ReadFile","Grep"]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calledArgs.AllowedTools) != 2 || runner.calledArgs.AllowedTools[0] != "ReadFile" || runner.calledArgs.AllowedTools[1] != "Grep" {
		t.Errorf("allowed_tools not passed correctly: %v", runner.calledArgs.AllowedTools)
	}
}

func TestSubagentTool_ExecuteContext_WithWorktreeBranch(t *testing.T) {
	runner := &mockRunner{
		result:         "done",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	_, err := tool.ExecuteContext(t.Context(),
		`{"prompt":"search on branch","worktree_branch":"feat/experiment"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.calledArgs.WorktreeBranch != "feat/experiment" {
		t.Errorf("worktree_branch not passed correctly: %s", runner.calledArgs.WorktreeBranch)
	}
}

func TestSubagentTool_ExecuteContext_Error_NoPartialResult(t *testing.T) {
	runner := &mockRunner{
		result:         "",
		err:            fmt.Errorf("budget exhausted"),
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	result, err := tool.ExecuteContext(t.Context(), `{"prompt":"do something"}`)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != "" {
		t.Errorf("expected empty result on error with no partial output, got: %s", result)
	}
	if !strings.Contains(err.Error(), "sub-agent failed") {
		t.Errorf("error should wrap with 'sub-agent failed', got: %v", err)
	}
}

func TestSubagentTool_ExecuteContext_Error_WithPartialResult(t *testing.T) {
	runner := &mockRunner{
		result:         "partial findings here",
		err:            fmt.Errorf("iteration budget exhausted"),
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	result, err := tool.ExecuteContext(t.Context(), `{"prompt":"big task"}`)
	if err != nil {
		t.Fatalf("should not return error when partial result is available: %v", err)
	}
	if !strings.Contains(result, "partial findings here") {
		t.Error("result should contain partial output")
	}
	if !strings.Contains(result, "⚠️ Error:") {
		t.Error("result should contain error marker")
	}
	if !strings.Contains(result, "iteration budget exhausted") {
		t.Error("result should contain the original error message")
	}
}

func TestSubagentTool_TruncateOutput_NoTruncation(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 100}
	tool := NewSubagentTool(runner)

	input := "short output"
	result := tool.truncateOutput(input, 0)
	if result != input {
		t.Errorf("should not truncate short output, got: %s", result)
	}
}

func TestSubagentTool_TruncateOutput_ExactBoundary(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 10}
	tool := NewSubagentTool(runner)

	input := "1234567890" // exactly 10 chars
	result := tool.truncateOutput(input, 0)
	if result != input {
		t.Errorf("should not truncate at exact boundary, got: %s", result)
	}
}

func TestSubagentTool_TruncateOutput_Truncates(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 10}
	tool := NewSubagentTool(runner)

	input := "12345678901" // 11 chars, exceeds limit
	result := tool.truncateOutput(input, 0)
	if !strings.HasPrefix(result, "1234567890") {
		t.Errorf("truncated result should start with first 10 chars, got: %s", result)
	}
	if !strings.Contains(result, "⚠️ [Output truncated at 10 chars]") {
		t.Errorf("truncated result should contain truncation marker, got: %s", result)
	}
}

func TestSubagentTool_TruncateOutput_ZeroMaxChars(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 0}
	tool := NewSubagentTool(runner)

	input := strings.Repeat("x", 100000)
	result := tool.truncateOutput(input, 0)
	if result != input {
		t.Error("maxOutputChars=0 should mean no truncation")
	}
}

func TestSubagentTool_InvalidJSON(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 16384}
	tool := NewSubagentTool(runner)

	_, err := tool.ExecuteContext(t.Context(), `{invalid json}`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("error should mention invalid arguments, got: %v", err)
	}
}

func TestSubagentTool_Properties(t *testing.T) {
	tool := NewSubagentTool(&mockRunner{})
	props := tool.Properties()

	if _, ok := props["prompt"]; !ok {
		t.Error("should have 'prompt' property")
	}
	if _, ok := props["allowed_tools"]; !ok {
		t.Error("should have 'allowed_tools' property")
	}
	if _, ok := props["worktree_branch"]; !ok {
		t.Error("should have 'worktree_branch' property")
	}
	if props["allowed_tools"].Type != "array" {
		t.Error("allowed_tools should be array type")
	}
}

func TestSubagentArgs_Serialization(t *testing.T) {
	args := SubagentArgs{
		Prompt:         "test task",
		AllowedTools:   []string{"ReadFile", "Grep"},
		WorktreeBranch: "feat/test",
	}

	// Verify all fields are serializable
	data, err := marshalResult(args)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Verify round-trip
	var parsed SubagentArgs
	if err := parseArgs(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if parsed.Prompt != "test task" {
		t.Errorf("prompt mismatch: %s", parsed.Prompt)
	}
	if len(parsed.AllowedTools) != 2 {
		t.Errorf("allowed_tools length mismatch: %d", len(parsed.AllowedTools))
	}
	if parsed.WorktreeBranch != "feat/test" {
		t.Errorf("worktree_branch mismatch: %s", parsed.WorktreeBranch)
	}
}

func TestSubagentTool_LastSubagentID(t *testing.T) {
	runner := &mockRunner{
		result:         "done",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	// Before any invocation, ID should be empty
	if id := tool.LastSubagentID(); id != "" {
		t.Errorf("expected empty ID before invocation, got %q", id)
	}
}

func TestSubagentIDCarrier_InToolResult(t *testing.T) {
	// Verify that SubagentTool implements SubagentIDCarrier
	tool := NewSubagentTool(&mockRunner{maxOutputChars: 16384})
	var carrier SubagentIDCarrier = tool
	if carrier.LastSubagentID() != "" {
		t.Error("expected empty subagent ID before invocation")
	}
}
