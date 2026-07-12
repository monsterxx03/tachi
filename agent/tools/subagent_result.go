package tools

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SubagentResult wraps the result of a sub-agent execution with statistics.
type SubagentResult struct {
	Output    string
	ShortID   string
	IterCount int
	Duration  time.Duration
	// ToolCallSummary maps tool name to call count, e.g. {"Glob": 2, "ReadFile": 5}
	ToolCallSummary ToolCallCount
}

// ToolCallCount is a convenience type for building tool call summaries.
type ToolCallCount map[string]int

// Add increments the count for the given tool name.
func (t ToolCallCount) Add(name string) {
	t[name]++
}

// String returns a human-readable summary like "Glob(2), ReadFile(5)".
func (t ToolCallCount) String() string {
	if len(t) == 0 {
		return ""
	}
	names := make([]string, 0, len(t))
	for name := range t {
		names = append(names, name)
	}
	sort.Strings(names)
	var result strings.Builder
	for i, name := range names {
		if i > 0 {
			result.WriteString(", ")
		}
		result.WriteString(name)
		if count := t[name]; count > 1 {
			result.WriteString("(" + strconv.Itoa(count) + ")")
		}
	}
	return result.String()
}

// FormatSubagentStats formats subagent execution stats for display.
func FormatSubagentStats(d time.Duration, iterCount int, toolSummary string) string {
	stats := fmt.Sprintf("_Sub-agent completed in %s, %d iterations_",
		formatDuration(d), iterCount)
	if toolSummary != "" {
		stats += fmt.Sprintf(", tools: %s", toolSummary)
	}
	return "\n\n---\n" + stats
}

// SubagentStatsCarrier is an optional interface for tools that expose
// sub-agent execution statistics after invocation.
type SubagentStatsCarrier interface {
	LastSubagentStats() (int, time.Duration)
}

// formatDuration formats a time.Duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := d.Seconds() - float64(minutes*60)
	return fmt.Sprintf("%dm%.0fs", minutes, seconds)
}
