//go:build integration

// Package run drives the real tachi binary through its -p (pipe) mode:
// exec.Command + pipes, asserting on the NDJSON AgentEvent stream, exit
// codes, and the mock server's request log. This is the only layer that
// exercises the true process boundary (main.go flag parsing, config loading,
// --allowed-tools).
package run

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
)

// Result captures one binary invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error // non-ExitError failures (binary not found, ...)
}

// Binary runs the tachi binary with the given args. workdir becomes the
// process working directory (tool side effects land there). Pass
// "--home", home as part of args for state isolation.
func Binary(bin, workdir string, args ...string) Result {
	cmd := exec.Command(bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		res.ExitCode = 0
		return res
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res
	}
	res.Err = err
	return res
}

// Event mirrors main.go's streamEvent NDJSON line.
type Event struct {
	Type       string `json:"type"`
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	ExitReason string `json:"exit_reason,omitempty"`
	Iterations int    `json:"iterations_used,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Usage mirrors main.go's usageJSON.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ParseNDJSON decodes the -o json-stream output into events, ignoring blank
// lines. Malformed lines fail the parse loudly (a format drift in the
// binary's output should never pass silently).
func ParseNDJSON(s string) []Event {
	var out []Event
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			panic("run: failed to parse NDJSON event line: " + err.Error() + "\nline: " + line)
		}
		out = append(out, ev)
	}
	return out
}

// TextDelta accumulates the streamed text from text_delta events — the
// -o json-stream equivalent of the human-readable final answer.
func TextDelta(events []Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == "text_delta" {
			sb.WriteString(ev.Content)
		}
	}
	return sb.String()
}
