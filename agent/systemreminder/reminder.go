// Package systemreminder provides a mechanism for injecting dynamic
// <system-reminder> tags at the top of user messages. These carry
// transient contextual information (current date, token usage warnings,
// iteration budget) that's not suitable for the static system prompt.
package systemreminder

import (
	"fmt"
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

	// LastMessageDate is the calendar date (YYYY-MM-DD) of the most recent
	// user message that was processed. It's empty for brand-new conversations.
	// Used by reminders that need to know when a new day has started.
	LastMessageDate string

	// IsToolResult is true when the reminder block is being injected after
	// tool results in the agent loop (not attached to a real user message).
	// Reminders that are only meaningful for user-facing messages (e.g.,
	// DateReminder) can skip when this is set.
	IsToolResult bool

	// CurrentPrompt is the current user input. Set by WrapUserMessage before
	// calling Collect so MemoryRecallReminder can use it as a search query.
	CurrentPrompt string

	// SkipRecall prevents memory recall (e.g. "tachi run" non-interactive mode).
	SkipRecall bool

	// ToolNames lists the names of tools that were executed in the current
	// turn (in order of execution). Empty when the reminder is not being
	// injected at a tool-result boundary. Reminders that only apply to
	// specific tool invocations can inspect this field.
	ToolNames []string

	// SessionID is the current session's ID. Used by PlanTrackingReminder
	// to filter plan files belonging to the current session.
	SessionID string
}

// Reminder generates one or more reminder lines given the current context.
// It returns nil or an empty slice when no reminder is needed.
type Reminder interface {
	Generate(ctx Context) []string
}

// TaggedReminder is an optional interface that Reminders can implement
// to declare their own XML wrapper tag. When a Reminder implements this,
// its output is wrapped in <tag>...</tag> instead of the default
// <system-reminder> block.
type TaggedReminder interface {
	Reminder
	WrapperTag() string // e.g. "relevant-memories"
}

// Collector aggregates a set of Reminders and formats active ones into a
// single <system-reminder>...< /system-reminder> block.
type Collector struct {
	reminders []Reminder
	logger    *debuglog.Logger
}

// NewCollector creates a Collector that consults the given reminders.
// When called with no arguments the collector always produces empty output.
func NewCollector(reminders ...Reminder) *Collector {
	return &Collector{reminders: reminders, logger: debuglog.DefaultLogger}
}

// AddReminder appends a reminder to the collector without rebuilding.
func (c *Collector) AddReminder(r Reminder) {
	c.reminders = append(c.reminders, r)
}

// SetLogger overrides the collector's logger. Channel callers use this to inject
// a channel-specific logger so debug output is tagged with the correct source.
func (c *Collector) SetLogger(l *debuglog.Logger) {
	c.logger = l
}

// reminderGroup holds accumulated output parts and a human-readable name
// of the reminders that fired, for logging.
type reminderGroup struct {
	parts     []string
	firedName string
}

// collectGroups iterates all registered reminders and groups their output
// by wrapper tag. Returns the default (untagged) group and a tag→group map.
func (c *Collector) collectGroups(ctx Context) (*reminderGroup, map[string]*reminderGroup) {
	defaultG := &reminderGroup{}
	taggedG := make(map[string]*reminderGroup)

	for _, r := range c.reminders {
		generated := r.Generate(ctx)
		if len(generated) == 0 {
			continue
		}

		// Extract the short type name for logging.
		typeName := fmt.Sprintf("%T", r)
		if idx := strings.LastIndex(typeName, "."); idx >= 0 {
			typeName = typeName[idx+1:]
		}

		if tr, ok := r.(TaggedReminder); ok {
			tag := tr.WrapperTag()
			g := taggedG[tag]
			if g == nil {
				g = &reminderGroup{firedName: tag}
				taggedG[tag] = g
			} else {
				g.firedName += "+" + typeName
			}
			g.parts = append(g.parts, generated...)
		} else {
			if defaultG.firedName != "" {
				defaultG.firedName += ", "
			}
			defaultG.firedName += typeName
			defaultG.parts = append(defaultG.parts, generated...)
		}
	}

	return defaultG, taggedG
}

// buildBlock wraps reminder output parts in a <tag>...</tag> block.
func buildBlock(tag string, parts []string) string {
	return "<" + tag + ">\n" + strings.Join(parts, "\n") + "\n</" + tag + ">"
}

// Collect queries every registered reminder and groups output by wrapper tag.
// Reminders that implement TaggedReminder get their own <tag>...</tag> block;
// all others are combined into the default <system-reminder> block.
// Returns an empty string when no reminders are active or c is nil.
func (c *Collector) Collect(ctx Context) string {
	if c == nil {
		return ""
	}

	defaultG, taggedG := c.collectGroups(ctx)

	var blocks []string

	// Build <system-reminder> block from default-group reminders.
	if len(defaultG.parts) > 0 {
		blocks = append(blocks, buildBlock("system-reminder", defaultG.parts))
		c.logger.Log("systemreminder: firing reminder(s): %s", defaultG.firedName)
	}

	// Build tagged blocks (e.g. <relevant-memories>).
	for tag, g := range taggedG {
		blocks = append(blocks, buildBlock(tag, g.parts))
		c.logger.Log("systemreminder: firing tagged reminder(s): %s", g.firedName)
	}

	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n")
}

// WrapUserMessage prepends the <system-reminder> block (if any) to the
// given user message content. This is a convenience helper so callers
// don't need to manually check emptiness. Safe to call on a nil Collector.
func (c *Collector) WrapUserMessage(userMessage string, ctx Context) string {
	if c == nil {
		return userMessage
	}
	// Inject current prompt so MemoryRecallReminder can use it as search query
	ctx.CurrentPrompt = userMessage
	block := c.Collect(ctx)
	if block == "" {
		return userMessage
	}
	return block + "\n" + userMessage
}
