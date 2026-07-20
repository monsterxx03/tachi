// Package permission implements rule-based permission policies for tool
// execution. The first (and currently only) policy target is the Bash tool:
// commands are classified as allow / ask / deny based on glob rules from the
// global config and the project-level .tachi/permissions.yaml.
//
// This is a guardrail, not a sandbox: it exists to prevent accidents and to
// give users control points over autonomous command execution.
package permission

import (
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// Decision is the outcome of a policy check.
type Decision int

const (
	// DecisionAllow lets the call proceed without user interaction.
	DecisionAllow Decision = iota
	// DecisionAsk requires interactive user approval before execution.
	DecisionAsk
	// DecisionDeny blocks the call; an error is returned to the LLM.
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionAsk:
		return "ask"
	case DecisionDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Rules holds glob patterns for Bash command classification.
// Only the '*' wildcard is supported (any, possibly empty, byte sequence).
// Matching is case-sensitive.
type Rules struct {
	Deny  []string
	Ask   []string
	Allow []string
}

// BuiltinDenyRules are deny rules for absolutely dangerous commands — things
// an agent should never run: root/home deletion, raw disk overwrite, and
// system shutdown. They are prepended to the user's global deny rules by
// NewPermissionPolicyFromConfig and can be turned off only via
// permissions.bash.disable_builtin_deny in the GLOBAL config (a project-level
// permissions.yaml cannot weaken them).
//
// Deliberately NOT included: git push --force (legitimate on feature
// branches), rm -rf of relative paths (common cleanup), curl|sh install
// patterns — those are user-policy decisions, add them to your own rules.
var BuiltinDenyRules = []string{
	// Root/home deletion — both -rf/-fr flag orders, the classic
	// "rm -rf / *" typo form, tilde and $HOME variants.
	"rm -rf /", "rm -rf /*", "rm -rf / *",
	"rm -fr /", "rm -fr /*", "rm -fr / *",
	"rm -rf ~", "rm -rf ~/*", "rm -rf ~ *",
	"rm -fr ~", "rm -fr ~/*", "rm -fr ~ *",
	"rm -rf $HOME", "rm -rf $HOME/*",
	"rm -rf ${HOME}", "rm -rf ${HOME}/*",

	// Raw disk overwrite (device-prefixed to keep /dev/null legit).
	"dd *of=/dev/sd*", "dd *of=/dev/hd*", "dd *of=/dev/vd*",
	"dd *of=/dev/xvd*", "dd *of=/dev/nvme*", "dd *of=/dev/mmcblk*",
	"dd *of=/dev/disk*", "dd *of=/dev/loop*",
	"mkfs*", "wipefs", "wipefs *",
	"> /dev/sd*", ">> /dev/sd*",
	"> /dev/hd*", ">> /dev/hd*",
	"> /dev/vd*", ">> /dev/vd*",
	"> /dev/nvme*", ">> /dev/nvme*",
	"> /dev/mmcblk*", ">> /dev/mmcblk*",
	"> /dev/disk*", ">> /dev/disk*",

	// System shutdown/reboot, including sudo and systemctl forms.
	"shutdown", "shutdown *", "sudo shutdown", "sudo shutdown *",
	"reboot", "reboot *", "sudo reboot", "sudo reboot *",
	"halt", "halt *", "sudo halt", "sudo halt *",
	"poweroff", "poweroff *", "sudo poweroff", "sudo poweroff *",
	"systemctl reboot*", "systemctl poweroff*", "systemctl halt*",
	"init 0", "init 6", "sudo init 0", "sudo init 6",
}

// Policy evaluates Bash commands against allow/ask/deny rules, plus
// session-scoped exact-command approvals ("always allow" in the TUI).
// Safe for concurrent use.
type Policy struct {
	deny  []string
	ask   []string
	allow []string

	mu           sync.Mutex
	sessionExact map[string]struct{}
}

// NewPolicy merges global and project-level rules into a Policy.
// Both sources are unioned: project rules can only tighten (add deny/ask) or
// exempt (add allow); precedence per segment is deny > allow > ask, so a
// project allow can never override a global ask/deny.
func NewPolicy(global, project Rules) *Policy {
	p := &Policy{sessionExact: make(map[string]struct{})}
	p.deny = append(append([]string{}, global.Deny...), project.Deny...)
	p.ask = append(append([]string{}, global.Ask...), project.Ask...)
	p.allow = append(append([]string{}, global.Allow...), project.Allow...)
	return p
}

// Empty reports whether the policy has no rules at all. Callers can use this
// as a fast path to skip policy checks entirely (default behavior: allow).
func (p *Policy) Empty() bool {
	return len(p.deny) == 0 && len(p.ask) == 0 && len(p.allow) == 0
}

// AllowExactSession records an exact command string as approved for the rest
// of the session (the "always allow" choice in the TUI). Matching is exact:
// a composite command is remembered as a whole and never widened to a prefix.
func (p *Policy) AllowExactSession(command string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionExact[normalize(command)] = struct{}{}
}

// CheckBash evaluates command against the policy and returns the decision
// plus the matched rule (or a short reason) for display to the user/LLM.
//
// Semantics:
//   - The command is parsed into simple command segments (pipelines, &&/||/;
//     lists, and command substitutions) with mvdan.cc/sh — quoting is
//     respected, so `echo "a;b"` is a single segment.
//   - Redirects are checked as pseudo-segments (e.g. `> /dev/sda`).
//   - A leading "sudo " prefix is stripped per segment, so `sudo rm -rf /`
//     matches rules written for `rm -rf /`.
//   - Per segment: deny match → Deny; allow match → exempt from ask;
//     ask match → Ask.
//   - Whole command: any Deny → Deny; else any Ask → Ask; else Allow.
//   - Unparseable commands → Ask (conservative).
//   - Exact session approvals short-circuit to Allow.
func (p *Policy) CheckBash(command string) (Decision, string) {
	cmd := normalize(command)
	if cmd == "" {
		return DecisionAllow, ""
	}

	p.mu.Lock()
	_, approved := p.sessionExact[cmd]
	p.mu.Unlock()
	if approved {
		return DecisionAllow, ""
	}

	segments, err := splitShellCommands(cmd)
	if err != nil {
		return DecisionAsk, "command could not be parsed"
	}

	askRule := ""
	for _, seg := range segments {
		if rule := matchSegment(p.deny, seg); rule != "" {
			return DecisionDeny, rule
		}
		if matchSegment(p.allow, seg) != "" {
			continue // explicitly exempted from ask rules
		}
		if rule := matchSegment(p.ask, seg); rule != "" {
			askRule = rule // keep scanning: a later segment may still deny
		}
	}
	if askRule != "" {
		return DecisionAsk, askRule
	}
	return DecisionAllow, ""
}

// matchSegment matches a command segment against patterns. A leading "sudo "
// prefix is also stripped and the remainder matched, so rules like "rm -rf /*"
// cover the "sudo rm -rf /" form without duplicating every rule.
func matchSegment(patterns []string, seg string) string {
	if rule := matchAny(patterns, seg); rule != "" {
		return rule
	}
	if rest, ok := strings.CutPrefix(seg, "sudo "); ok {
		if rule := matchAny(patterns, rest); rule != "" {
			return rule
		}
	}
	return ""
}

// normalize collapses all whitespace runs to single spaces so that
// session-exact matching is stable against insignificant formatting.
func normalize(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

// matchAny returns the first pattern that matches s, or "" if none match.
func matchAny(patterns []string, s string) string {
	for _, pat := range patterns {
		if wildcardMatch(pat, s) {
			return pat
		}
	}
	return ""
}

// splitShellCommands parses a shell command line into simple command
// segments: every command invocation in pipelines, lists, and command
// substitutions, plus redirects as their own pseudo-segments.
func splitShellCommands(command string) ([]string, error) {
	parser := syntax.NewParser()
	prog, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, err
	}

	printer := syntax.NewPrinter()
	var segments []string
	syntax.Walk(prog, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			var sb strings.Builder
			_ = printer.Print(&sb, n)
			if s := strings.TrimSpace(sb.String()); s != "" {
				segments = append(segments, s)
			}
		case *syntax.Redirect:
			// Print as "> /path" (operator, space, target) so user rules
			// like "*> /dev/*" match naturally.
			var sb strings.Builder
			sb.WriteString(n.Op.String())
			if n.Word != nil {
				sb.WriteString(" ")
				_ = printer.Print(&sb, n.Word)
			}
			if s := strings.TrimSpace(sb.String()); s != "" {
				segments = append(segments, s)
			}
		}
		return true
	})
	return segments, nil
}

// wildcardMatch reports whether s matches the glob pattern, where '*'
// matches any (possibly empty) sequence of bytes. No other metacharacters
// ('?', '[', ']') are special. Case-sensitive.
func wildcardMatch(pattern, s string) bool {
	px, sx := 0, 0
	star, starMatch := -1, 0
	for sx < len(s) {
		if px < len(pattern) && pattern[px] == '*' {
			star = px
			starMatch = sx
			px++
			continue
		}
		if px < len(pattern) && pattern[px] == s[sx] {
			px++
			sx++
			continue
		}
		if star != -1 {
			px = star + 1
			starMatch++
			sx = starMatch
			continue
		}
		return false
	}
	// Trailing '*' in pattern matches the empty remainder.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}
