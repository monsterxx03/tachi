package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tasks"
)

// mockRunner implements SubagentRunner for testing.
type mockRunner struct {
	result         string
	id             string
	err            error
	toolNames      []string
	maxOutputChars int
	calledArgs     SubagentArgs
}

func (m *mockRunner) RunSubagent(_ context.Context, args SubagentArgs) (string, string, *SubagentResult, error) {
	m.calledArgs = args
	return m.result, m.id, nil, m.err
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

// TestSubagentTool_TruncateOutput_CountsRunesNotBytes 校验阈值按**字符**计而非字节：
// 一个汉字占 3 字节，按字节判会让中文内容在 1/3 处就被截断，且按字节切会留下半截字符。
func TestSubagentTool_TruncateOutput_CountsRunesNotBytes(t *testing.T) {
	t.Run("恰好等于阈值不截断（字节数已远超阈值）", func(t *testing.T) {
		tool := NewSubagentTool(&mockRunner{maxOutputChars: 10})
		input := strings.Repeat("中", 10) // 10 字符 / 30 字节
		if got := tool.truncateOutput(input, 0); got != input {
			t.Errorf("10 个汉字 = 10 字符，不该截断（按字节判会误截）: %q", got)
		}
	})

	t.Run("超限时按字符边界截断", func(t *testing.T) {
		tool := NewSubagentTool(&mockRunner{maxOutputChars: 10})
		got := tool.truncateOutput(strings.Repeat("中", 12), 0)

		want := strings.Repeat("中", 10) + "\n\n⚠️ [Output truncated at 10 chars]"
		if got != want {
			t.Errorf("截断结果 = %q, want %q", got, want)
		}
		if !utf8.ValidString(got) {
			t.Error("按字符边界切不应产出非法 UTF-8")
		}
	})
}

// TestSubagentTool_SpillOversizedToFile 校验配了 resultBaseDir 时超限结果落盘而非截断，
// 且落盘的是**完整内容** —— 这正是与截断路径的关键差别（截断丢的内容无法取回）。
func TestSubagentTool_SpillOversizedToFile(t *testing.T) {
	dir := t.TempDir()
	tool := NewSubagentTool(&mockRunner{maxOutputChars: 10})
	tool.SetResultBaseDir(dir)

	input := "0123456789ABCDEFGHIJ" // 20 chars，超 10 的阈值
	result := tool.truncateOutput(input, 0)

	if !strings.Contains(result, "[SUBAGENT OUTPUT TOO LARGE]") {
		t.Fatalf("超限应返回落盘指针，got: %q", result)
	}
	if !strings.Contains(result, "20 chars exceeds limit (10 chars)") {
		t.Errorf("指针应写明原始长度与阈值，got: %q", result)
	}
	if strings.Contains(result, input) {
		t.Error("指针里不应内联完整内容（那就等于没省下 context）")
	}

	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("应恰好落盘一个文件, files=%v err=%v", files, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	if string(got) != input {
		t.Errorf("落盘内容 = %q, want %q（必须是完整内容，不能是截断后的）", got, input)
	}
	if !strings.Contains(result, files[0].Name()) {
		t.Errorf("指针里的路径应指向落盘文件，got: %q", result)
	}
}

// TestSubagentTool_SpillFallsBackToTruncation 校验落盘失败时退回截断，而不是让工具直接失败。
func TestSubagentTool_SpillFallsBackToTruncation(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSubagentTool(&mockRunner{maxOutputChars: 10})
	tool.SetResultBaseDir(blocker) // 指向普通文件 → 建目录失败 → 落盘失败

	result := tool.truncateOutput("12345678901", 0)
	if !strings.Contains(result, "⚠️ [Output truncated at 10 chars]") {
		t.Errorf("落盘失败应退回截断，got: %q", result)
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

// The identity is recorded on the CALL's context, not on the tool: several
// SubAgent calls can run in one batch, and a shared "last one" field links one
// call's transcript record to another call's sub-agent file.
func TestSubagentTool_RecordsItsIDPerCall(t *testing.T) {
	tool := NewSubagentTool(&mockRunner{result: "done", id: "sub-1", maxOutputChars: 16384})
	ctx := WithSubagentMetaCarrier(context.Background())

	if _, err := tool.ExecuteContext(ctx, `{"prompt": "look"}`); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	id, _ := SubagentMetaFromCtx(ctx)
	if id != "sub-1" {
		t.Errorf("meta id = %q, want sub-1", id)
	}

	// A different call gets its own carrier; nothing leaks between them.
	other := WithSubagentMetaCarrier(context.Background())
	if id, _ := SubagentMetaFromCtx(other); id != "" {
		t.Errorf("a fresh carrier should be empty, got %q", id)
	}
}

// asyncMockRunner implements both runner interfaces; Start keeps the callback so
// the test can finish the work when it chooses.
type asyncMockRunner struct {
	mockRunner
	done func(SubagentOutcome)
}

func (m *asyncMockRunner) StartSubagent(_ context.Context, args SubagentArgs, done func(SubagentOutcome)) (string, error) {
	m.calledArgs = args
	m.done = done
	return m.id, nil
}

// A dispatch returns immediately and the outcome arrives through the queue —
// the same door a background process uses.
func TestSubagentDispatchReturnsAndPostsTheOutcome(t *testing.T) {
	runner := &asyncMockRunner{mockRunner: mockRunner{id: "sub-async", maxOutputChars: 16384}}
	tool := NewSubagentTool(runner)
	q := tasks.New()
	tool.SetAsync(true, q)

	ctx := WithSubagentMetaCarrier(context.Background())
	out, err := tool.ExecuteContext(ctx, `{"prompt": "look at X"}`)
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if !strings.Contains(out, "sub-async") || !strings.Contains(out, "running") {
		t.Fatalf("dispatch result should name the task and its state, got %q", out)
	}
	if id, _ := SubagentMetaFromCtx(ctx); id != "sub-async" {
		t.Errorf("meta id = %q, want sub-async", id)
	}
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("nothing should be posted before the sub-agent finishes, got %v", got)
	}

	runner.done(SubagentOutcome{
		ID:     "sub-async",
		Result: "the answer",
		Stats:  &SubagentResult{IterCount: 2, Duration: time.Second},
	})

	events := q.Pending()
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != tasks.KindSubagent || e.Name != "sub-async" {
		t.Errorf("event = %+v, want the sub-agent", e)
	}
	if !strings.Contains(e.Stdout, "the answer") {
		t.Errorf("event output = %q, want the result", e.Stdout)
	}
	if e.Command != "look at X" {
		t.Errorf("event task = %q, want the prompt", e.Command)
	}
	if e.Status != tasks.StatusExited {
		t.Errorf("status = %s, want exited", e.Status)
	}
}

// A failed sub-agent still reports: the partial result is the useful part.
func TestSubagentDispatchReportsFailure(t *testing.T) {
	runner := &asyncMockRunner{mockRunner: mockRunner{id: "sub-fail", maxOutputChars: 16384}}
	tool := NewSubagentTool(runner)
	q := tasks.New()
	tool.SetAsync(true, q)

	if _, err := tool.ExecuteContext(WithSubagentMetaCarrier(context.Background()), `{"prompt": "x"}`); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	runner.done(SubagentOutcome{ID: "sub-fail", Result: "half done", Err: fmt.Errorf("budget exhausted")})

	events := q.Pending()
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	if events[0].Status != tasks.StatusError || events[0].Error == "" {
		t.Errorf("event = %+v, want an error status and reason", events[0])
	}
	if !strings.Contains(events[0].Stdout, "half done") {
		t.Errorf("event output = %q, want the partial result", events[0].Stdout)
	}
}

// No queue means no place to report, so the tool must stay synchronous even when
// the caller asks for async: a result with nowhere to go is worse than a slower
// turn.
func TestSubagentAsyncNeedsAQueue(t *testing.T) {
	runner := &asyncMockRunner{mockRunner: mockRunner{result: "done", id: "sub", maxOutputChars: 16384}}
	tool := NewSubagentTool(runner)
	tool.SetAsync(true, nil)

	if tool.Description() == subagentBackgroundDescription {
		t.Error("async needs a queue; without one the tool must stay synchronous")
	}
	if _, err := tool.ExecuteContext(context.Background(), `{"prompt": "x"}`); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if runner.done != nil {
		t.Error("the synchronous path must have been used")
	}
}

// The registry hands the tool result the per-call identity, so a tool_result
// record can link to the right sub-agent file even when a batch ran several.
func TestRegistryInvokeCarriesTheSubagentIdentity(t *testing.T) {
	r := NewRegistry()
	r.Register(NewSubagentTool(&mockRunner{result: "done", id: "sub-2", maxOutputChars: 16384}))

	res := r.Invoke(context.Background(), ToolNameSubAgent, `{"prompt": "x"}`)
	if res.Status != ToolResultSuccess {
		t.Fatalf("Invoke status = %v (err %v)", res.Status, res.Err)
	}
	if res.SubagentID != "sub-2" {
		t.Errorf("SubagentID = %q, want sub-2", res.SubagentID)
	}
}

// wait is the model's escape hatch for sequential work: it chooses to block in
// this turn instead of being woken later.
func TestSubagentWaitRunsInThisTurn(t *testing.T) {
	runner := &asyncMockRunner{mockRunner: mockRunner{result: "inline result", id: "sub-w", maxOutputChars: 16384}}
	tool := NewSubagentTool(runner)
	q := tasks.New()
	tool.SetAsync(true, q)

	out, err := tool.ExecuteContext(WithSubagentMetaCarrier(context.Background()), `{"prompt": "x", "wait": true}`)
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if !strings.Contains(out, "inline result") {
		t.Errorf("wait=true must return the result in this call, got %q", out)
	}
	if runner.done != nil {
		t.Error("wait=true must not go through the dispatcher")
	}
	if got := q.Pending(); len(got) != 0 {
		t.Errorf("a waited sub-agent has nothing to report later, got %v", got)
	}
}

// The field exists only where the choice exists: a blocking entry point would
// advertise a decision the model does not have.
func TestSubagentWaitIsOnlyAdvertisedWhenAsync(t *testing.T) {
	syncTool := NewSubagentTool(&mockRunner{maxOutputChars: 16384})
	if _, ok := syncTool.Properties()["wait"]; ok {
		t.Error("a blocking SubAgent tool must not advertise wait")
	}

	asyncTool := NewSubagentTool(&mockRunner{maxOutputChars: 16384})
	asyncTool.SetAsync(true, tasks.New())
	if _, ok := asyncTool.Properties()["wait"]; !ok {
		t.Error("an async SubAgent tool must advertise wait")
	}
}
