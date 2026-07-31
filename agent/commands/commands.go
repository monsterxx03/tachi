// Package commands provides shared slash command definitions, lookup, and
// formatting utilities used across TUI, channel, and ACP modes.
//
// Each mode retains its own handler signatures and dispatch mechanisms —
// this package extracts the common metadata and presentation logic that
// was previously duplicated three times.
package commands

import (
	"strconv"
	"strings"

	"slices"
)

// Mode identifies which frontends support a command.
type Mode string

const (
	ModeTUI     Mode = "tui"
	ModeChannel Mode = "channel"
	ModeACP     Mode = "acp"
)

// Def describes a slash command's metadata. All modes share this registry
// for help text, autocomplete, and command existence checks.
type Def struct {
	Name        string // without leading "/", e.g. "commit"
	Description string
	InputHint   string // optional argument hint (e.g. "list | <name>")
	Modes       []Mode // which modes support this command
}

// Registry contains all known slash commands across all modes.
// Order is significant: it determines autocomplete/help listing order.
var Registry = []Def{
	{Name: "new", Description: "Start new conversation", Modes: []Mode{ModeTUI, ModeChannel}},
	{Name: "quit", Description: "Exit tachi", Modes: []Mode{ModeTUI}},
	{Name: "model", Description: "Switch provider/model", InputHint: "[name]", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "commit", Description: "Generate commit message and commit via git", Modes: []Mode{ModeTUI, ModeACP}},
	{Name: "compact", Description: "Compress conversation history into a summary", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "init", Description: "Generate .tachi.md project context file", Modes: []Mode{ModeTUI, ModeACP}},
	{Name: "mcp", Description: "Manage MCP servers (list, toggle, reconnect, auth)", InputHint: "list | toggle | reconnect | auth <name>", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "sessions", Description: "Browse and reload previous sessions", Modes: []Mode{ModeTUI}},
	{Name: "usage", Description: "Show token usage, cost, and tool call statistics", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "review", Description: "Code review current repo changes via agent fork (correctness, quality, efficiency)", InputHint: "[rounds]", Modes: []Mode{ModeTUI, ModeACP}},
	{Name: "skill", Description: "List or activate skills", InputHint: "list | reload | <name>", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "transcript", Description: "Generate session transcript report", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "dream", Description: "Run AutoDream memory consolidation now", Modes: []Mode{ModeTUI}},
	{Name: "cron", Description: "List cron jobs", Modes: []Mode{ModeChannel}},
	{Name: "cd", Description: "Change working directory for this thread", InputHint: "<directory>", Modes: []Mode{ModeChannel}},
	{Name: "stop", Description: "Stop the current LLM turn", Modes: []Mode{ModeChannel}},
	{Name: "research", Description: "Deep research on a topic. Usage: /research <topic> [--depth 2] [--breadth 3]", InputHint: "<topic>", Modes: []Mode{ModeTUI, ModeChannel, ModeACP}},
	{Name: "restart", Description: "Restart Tachi via systemctl (requires systemd)", Modes: []Mode{ModeChannel}},
}

// ForMode returns the subset of commands available in the given mode.
func ForMode(mode Mode) []Def {
	var out []Def
	for _, d := range Registry {
		if slices.Contains(d.Modes, mode) {
			out = append(out, d)
		}
	}
	return out
}

// Find returns the command definition with the exact name, or nil.
func Find(name string) *Def {
	for i := range Registry {
		if Registry[i].Name == name {
			return &Registry[i]
		}
	}
	return nil
}

// FindByPrefix returns a command whose name is a prefix of the input
// (e.g. "mcp" matches "mcp list"). Exact matches are included.
// Returns nil if no command matches.
func FindByPrefix(input string) *Def {
	for i := range Registry {
		if input == Registry[i].Name || len(input) > len(Registry[i].Name) && input[:len(Registry[i].Name)+1] == Registry[i].Name+" " {
			return &Registry[i]
		}
	}
	return nil
}

// MatchPrefix returns all commands whose name starts with the given prefix.
// If prefix is empty, returns all commands.
func MatchPrefix(prefix string) []Def {
	if prefix == "" {
		out := make([]Def, len(Registry))
		copy(out, Registry)
		return out
	}
	var out []Def
	for _, d := range Registry {
		if len(d.Name) >= len(prefix) && d.Name[:len(prefix)] == prefix {
			out = append(out, d)
		}
	}
	return out
}

// MatchPrefixForMode returns commands that start with prefix and are
// available in the given mode. If prefix is empty, returns all for that mode.
func MatchPrefixForMode(prefix string, mode Mode) []Def {
	var out []Def
	for _, d := range Registry {
		if prefix != "" && (len(d.Name) < len(prefix) || d.Name[:len(prefix)] != prefix) {
			continue
		}
		if slices.Contains(d.Modes, mode) {
			out = append(out, d)
		}
	}
	return out
}

// ResearchArgs holds parsed arguments from a /research command.
type ResearchArgs struct {
	Topic   string // The research topic / query
	Depth   int    // Research depth (default: 2)
	Breadth int    // Search breadth per level (default: 3)
}

// ParseResearchArgs parses /research command arguments.
// Input format: <topic> [--depth N] [--breadth N]
//
// Returns parsed args with defaults applied for optional fields.
// The returned topic is non-empty when parsing succeeds.
func ParseResearchArgs(input string) ResearchArgs {
	args := ResearchArgs{}

	parts := strings.Fields(input)
	if len(parts) == 0 {
		return args
	}

	// Extract flags
	var topicParts []string
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "--depth" && i+1 < len(parts):
			if d, err := strconv.Atoi(parts[i+1]); err == nil && d > 0 {
				args.Depth = d
			}
			i++ // skip value
		case parts[i] == "--breadth" && i+1 < len(parts):
			if b, err := strconv.Atoi(parts[i+1]); err == nil && b > 0 {
				args.Breadth = b
			}
			i++ // skip value
		default:
			topicParts = append(topicParts, parts[i])
		}
	}

	args.Topic = strings.Join(topicParts, " ")
	return args
}

// ---------------------------------------------------------------------------
// Adversarial review shared logic (round resolution, model assignment,
// fail-fast check, prompt/path building) lives in review.go.
// ---------------------------------------------------------------------------
