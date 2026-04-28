// Package systemreminder provides a mechanism for injecting dynamic
// <system-reminder> tags at the top of user messages. These carry
// transient contextual information (current date, token usage warnings,
// iteration budget) that's not suitable for the static system prompt.
package systemreminder

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// Context carries the dynamic state available when generating reminders.
// All fields are zero-valued when not applicable; individual reminders
// decide which fields to examine.
type Context struct {
	// IsFirstMessage is true when this is the first user message in a
	// brand-new conversation (no prior messages except the system prompt).
	IsFirstMessage bool

	// IterationsLeft is the number of agent-loop iterations remaining.
	// Zero means the budget is exactly exhausted.
	IterationsLeft int

	// MaxIterations is the configured iteration budget ceiling.
	MaxIterations int

	// InputTokens is the cumulative input token count from the most recent
	// API response (includes all prior messages in the context window).
	InputTokens int64

	// ContextWindow is the model's maximum context window size.
	ContextWindow int64

	// Now is the current time, provided by the caller so tests can
	// inject a deterministic clock.
	Now time.Time
}

// Reminder generates one or more reminder lines given the current context.
// It returns nil or an empty slice when no reminder is needed.
type Reminder interface {
	Generate(ctx Context) []string
}

// Collector aggregates a set of Reminders and formats active ones into a
// single <system-reminder>...</system-reminder> block.
type Collector struct {
	reminders []Reminder
}

// NewCollector creates a Collector that consults the given reminders.
// When called with no arguments the collector always produces empty output.
func NewCollector(reminders ...Reminder) *Collector {
	return &Collector{reminders: reminders}
}

// Collect queries every registered reminder and, if any produce output,
// wraps the combined lines in <system-reminder> tags.
// Returns an empty string when no reminders are active.
func (c *Collector) Collect(ctx Context) string {
	var parts []string
	for _, r := range c.reminders {
		parts = append(parts, r.Generate(ctx)...)
	}
	if len(parts) == 0 {
		return ""
	}
	block := "<system-reminder>\n" + strings.Join(parts, "\n") + "\n</system-reminder>"
	debuglog.Log("systemreminder: injecting %d reminder(s):\n%s", len(parts), block)
	return block
}

// WrapUserMessage prepends the <system-reminder> block (if any) to the
// given user message content. This is a convenience helper so callers
// don't need to manually check emptiness.
func (c *Collector) WrapUserMessage(userMessage string, ctx Context) string {
	block := c.Collect(ctx)
	if block == "" {
		return userMessage
	}
	return block + "\n" + userMessage
}

// ---- Built-in reminders -----------------------------------------------------

// DateReminder injects the current date on the first message of a brand-new
// conversation. This gives the model temporal awareness without hard-coding
// a date in the system prompt (which would go stale across long sessions).
type DateReminder struct{}

func (DateReminder) Generate(ctx Context) []string {
	if !ctx.IsFirstMessage {
		return nil
	}
	line := fmt.Sprintf("Current date: %s", ctx.Now.Format("Monday, January 2, 2006"))
	debuglog.Log("systemreminder: DateReminder firing: %q", line)
	return []string{line}
}

// IterationWarningReminder warns when the agent loop is running low on
// iterations so the model knows to finish its work efficiently.
// Threshold is the remaining-iteration count at or below which the warning fires.
type IterationWarningReminder struct {
	Threshold int
}

func (r IterationWarningReminder) Generate(ctx Context) []string {
	if ctx.MaxIterations <= 0 || ctx.IterationsLeft <= 0 {
		return nil
	}
	if r.Threshold <= 0 {
		return nil
	}
	if ctx.IterationsLeft > r.Threshold {
		return nil
	}
	line := fmt.Sprintf(
		"Iteration budget: %d of %d iterations remaining. Complete your work as efficiently as possible.",
		ctx.IterationsLeft, ctx.MaxIterations,
	)
	debuglog.Log("systemreminder: IterationWarningReminder firing (threshold=%d): %q", r.Threshold, line)
	return []string{line}
}

// TokenWarningReminder warns when the input token count exceeds a percentage
// of the context window, so the model can adjust its output verbosity.
// ThresholdPct is the usage percentage at or above which the warning fires.
type TokenWarningReminder struct {
	ThresholdPct int
}

func (r TokenWarningReminder) Generate(ctx Context) []string {
	if ctx.ContextWindow <= 0 || ctx.InputTokens <= 0 {
		return nil
	}
	if r.ThresholdPct <= 0 {
		return nil
	}
	pct := float64(ctx.InputTokens) / float64(ctx.ContextWindow) * 100
	if pct < float64(r.ThresholdPct) {
		return nil
	}
	line := fmt.Sprintf(
		"Context window usage: %.0f%% (%d / %d input tokens). Be concise and minimize unnecessary output.",
		pct, ctx.InputTokens, ctx.ContextWindow,
	)
	debuglog.Log("systemreminder: TokenWarningReminder firing (threshold=%d%%): %q", r.ThresholdPct, line)
	return []string{line}
}

// GitReminder injects the current git repository status on the first message
// of a brand-new conversation. It runs git commands to gather branch info and
// status, giving the model awareness of the git context without hard-coding it
// in the system prompt.
type GitReminder struct{}

func (GitReminder) Generate(ctx Context) []string {
	if !ctx.IsFirstMessage {
		return nil
	}
	// Only fire if we're inside a git repository.
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil
	}

	var lines []string

	// Current branch (including detached HEAD state).
	branchOut, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		branch := strings.TrimSpace(string(branchOut))
		if branch == "HEAD" {
			// Detached HEAD, show short commit hash.
			commitOut, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
			if err == nil {
				lines = append(lines, fmt.Sprintf("Git HEAD: detached at %s", strings.TrimSpace(string(commitOut))))
			}
		} else {
			lines = append(lines, fmt.Sprintf("Git branch: %s", branch))
		}
	}

	// Short status (porcelain).
	statusOut, err := exec.Command("git", "status", "--porcelain").Output()
	if err == nil {
		statusLines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
		if len(statusLines) > 0 && statusLines[0] != "" {
			// Limit to at most 30 lines to avoid blowing up the context.
			if len(statusLines) > 30 {
				statusLines = append(statusLines[:30], "... (truncated)")
			}
			lines = append(lines, "Git status:")
			for _, s := range statusLines {
				lines = append(lines, fmt.Sprintf("  %s", s))
			}
		} else {
			lines = append(lines, "Git status: clean")
		}
	}

	if len(lines) == 0 {
		return nil
	}

	debuglog.Log("systemreminder: GitReminder firing: %d lines", len(lines))
	return lines
}
