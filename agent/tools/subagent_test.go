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

func (m *mockRunner) RunSubagent(_ context.Context, args SubagentArgs) (string, string, error) {
	m.calledArgs = args
	return m.result, "", m.err
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

func TestSubagentTool_Description_IncludesToolNames(t *testing.T) {
	runner := &mockRunner{
		toolNames: []string{"ReadFile", "Grep", "Glob", "Bash"},
	}
	tool := NewSubagentTool(runner)
	desc := tool.Description()

	if !strings.Contains(desc, "ReadFile, Grep, Glob, Bash") {
		t.Errorf("Description() should contain available tool names, got:\n%s", desc)
	}
	if !strings.Contains(desc, "Available tools for allowed_tools:") {
		t.Error("Description() should contain 'Available tools for allowed_tools:' header")
	}
}

func TestSubagentTool_ExecuteContext_Success(t *testing.T) {
	runner := &mockRunner{
		result:         "task completed successfully",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	result, err := tool.ExecuteContext(context.Background(), `{"prompt":"find all TODO comments"}`)
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

	_, err := tool.ExecuteContext(context.Background(),
		`{"prompt":"search code","allowed_tools":["ReadFile","Grep"],"max_iterations":10}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calledArgs.AllowedTools) != 2 || runner.calledArgs.AllowedTools[0] != "ReadFile" || runner.calledArgs.AllowedTools[1] != "Grep" {
		t.Errorf("allowed_tools not passed correctly: %v", runner.calledArgs.AllowedTools)
	}
	if runner.calledArgs.MaxIterations != 10 {
		t.Errorf("max_iterations not passed correctly: %d", runner.calledArgs.MaxIterations)
	}
}

func TestSubagentTool_ExecuteContext_WithWorktreeBranch(t *testing.T) {
	runner := &mockRunner{
		result:         "done",
		maxOutputChars: 16384,
	}
	tool := NewSubagentTool(runner)

	_, err := tool.ExecuteContext(context.Background(),
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

	result, err := tool.ExecuteContext(context.Background(), `{"prompt":"do something"}`)
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

	result, err := tool.ExecuteContext(context.Background(), `{"prompt":"big task"}`)
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
	result := tool.truncateOutput(input)
	if result != input {
		t.Errorf("should not truncate short output, got: %s", result)
	}
}

func TestSubagentTool_TruncateOutput_ExactBoundary(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 10}
	tool := NewSubagentTool(runner)

	input := "1234567890" // exactly 10 chars
	result := tool.truncateOutput(input)
	if result != input {
		t.Errorf("should not truncate at exact boundary, got: %s", result)
	}
}

func TestSubagentTool_TruncateOutput_Truncates(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 10}
	tool := NewSubagentTool(runner)

	input := "12345678901" // 11 chars, exceeds limit
	result := tool.truncateOutput(input)
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
	result := tool.truncateOutput(input)
	if result != input {
		t.Error("maxOutputChars=0 should mean no truncation")
	}
}

func TestSubagentTool_InvalidJSON(t *testing.T) {
	runner := &mockRunner{maxOutputChars: 16384}
	tool := NewSubagentTool(runner)

	_, err := tool.ExecuteContext(context.Background(), `{invalid json}`)
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
	if _, ok := props["max_iterations"]; !ok {
		t.Error("should have 'max_iterations' property")
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
		MaxIterations:  10,
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
	if parsed.MaxIterations != 10 {
		t.Errorf("max_iterations mismatch: %d", parsed.MaxIterations)
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