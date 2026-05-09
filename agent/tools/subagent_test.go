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
	calledPrompt   string
	calledTools    []string
	calledMaxIters int
}

func (m *mockRunner) RunSubagent(_ context.Context, prompt string, allowedTools []string, maxIterations int) (string, error) {
	m.calledPrompt = prompt
	m.calledTools = allowedTools
	m.calledMaxIters = maxIterations
	return m.result, m.err
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
	if runner.calledPrompt != "find all TODO comments" {
		t.Errorf("prompt not passed correctly: %s", runner.calledPrompt)
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
	if len(runner.calledTools) != 2 || runner.calledTools[0] != "ReadFile" || runner.calledTools[1] != "Grep" {
		t.Errorf("allowed_tools not passed correctly: %v", runner.calledTools)
	}
	if runner.calledMaxIters != 10 {
		t.Errorf("max_iterations not passed correctly: %d", runner.calledMaxIters)
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
	if props["allowed_tools"].Type != "array" {
		t.Error("allowed_tools should be array type")
	}
}
