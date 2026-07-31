// Package permission implements rule-based permission policies for tool
// execution. The first (and currently only) policy target is the Bash tool:
// commands are classified as allow / ask / deny based on glob rules from the
// global config and the project-level .tachi/permissions.yaml.
//
// This is a guardrail, not a sandbox: it exists to prevent accidents and to
// give users control points over autonomous command execution.
package permission

import (
	"path/filepath"
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
// an agent should never run: raw disk overwrite, and system shutdown. They
// are prepended to the user's global deny rules by
// NewPermissionPolicyFromConfig and can be turned off only via
// permissions.bash.disable_builtin_deny in the GLOBAL config (a project-level
// permissions.yaml cannot weaken them).
//
// Root/home deletion (`rm -rf /`, `rm -rf /etc`, `rm -rf ~`, `rm -rf $HOME`,
// ...) is NOT in this glob list: it is enforced by a structured argument
// check (checkBuiltinRmDangerous) that only targets the root directory
// itself, its direct children, and the home directory — while letting common
// cleanup of deeper paths (e.g. `rm -rf /tmp/xxx/*`, `rm -rf /var/log/*`)
// through to user policy. A plain glob like "rm -rf /*" would also match
// those deeper deletions because '*' spans '/' in wildcardMatch.
//
// Deliberately NOT included: git push --force (legitimate on feature
// branches), rm -rf of relative paths (common cleanup), curl|sh install
// patterns — those are user-policy decisions, add them to your own rules.
var BuiltinDenyRules = []string{
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

// Policy evaluates Bash commands against allow/ask/deny rules, plus the
// built-in structured rm guard and session-scoped exact-command approvals
// ("always allow" in the TUI). Safe for concurrent use.
type Policy struct {
	deny  []string
	ask   []string
	allow []string

	// builtinRm enables the structured root/home deletion guard for `rm`
	// invocations (see checkBuiltinRmDangerous). Default on; disabled via
	// NewPolicyNoBuiltins (permissions.bash.disable_builtin_deny).
	builtinRm bool

	mu           sync.Mutex
	sessionExact map[string]struct{}
}

// NewPolicy merges global and project-level rules into a Policy.
// deny/ask are unioned from both sources — a project can always tighten.
// allow is taken from the GLOBAL source only: project-level allow is dropped
// so a cloned repository can never loosen the user's ask tripwires
// (deny stays absolute; ask can only be added, never exempted, by projects).
// The built-in structured rm guard (root/home deletion) is enabled.
func NewPolicy(global, project Rules) *Policy {
	return newPolicy(global, project, true)
}

// NewPolicyNoBuiltins is NewPolicy with the built-in structured rm guard
// disabled. Use it only when the user opted out of ALL built-in protection
// via permissions.bash.disable_builtin_deny in the GLOBAL config — note that
// BuiltinDenyRules (disk/shutdown globs) are prepended by the caller, so this
// only disables the rm argument check.
func NewPolicyNoBuiltins(global, project Rules) *Policy {
	return newPolicy(global, project, false)
}

func newPolicy(global, project Rules, builtinRm bool) *Policy {
	p := &Policy{sessionExact: make(map[string]struct{}), builtinRm: builtinRm}
	p.deny = append(append([]string{}, global.Deny...), project.Deny...)
	p.ask = append(append([]string{}, global.Ask...), project.Ask...)
	p.allow = append([]string{}, global.Allow...) // project allow intentionally dropped
	return p
}

// Empty reports whether the policy has no rules at all — including the
// built-in rm guard. Callers can use this as a fast path to skip policy
// checks entirely (default behavior: allow). A policy built with NewPolicy
// (built-in guard enabled) is never Empty.
func (p *Policy) Empty() bool {
	return len(p.deny) == 0 && len(p.ask) == 0 && len(p.allow) == 0 && !p.builtinRm
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
		// User deny rules first — they may be broader or more specific than
		// the built-in guard, and the matched rule name is shown to the user.
		if rule := matchSegment(p.deny, seg); rule != "" {
			return DecisionDeny, rule
		}
		// Built-in structured rm guard: a fallback that catches root/home
		// deletion even when no user rule covers it.
		if p.builtinRm {
			if rule, hit := checkBuiltinRmDangerous(seg); hit {
				return DecisionDeny, rule
			}
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

// checkBuiltinRmDangerous inspects an `rm` command segment (already split by
// splitShellCommands) for absolutely dangerous deletion targets: the root
// directory itself, its direct children, and the home directory (~ and
// $HOME/${HOME} forms, including their direct children). It returns the
// offending target for display and true when the segment must be denied.
//
// This is the structured replacement for a glob like "rm -rf /*", which —
// because '*' in wildcardMatch spans '/' — would also blanket-deny harmless
// cleanup of deeper paths such as "rm -rf /tmp/xxx/*" or "rm -rf /var/log/*".
// Deeper paths are deliberately left to user policy (global/project rules).
//
// Quoting is handled via the mvdan/sh syntax tree, so `rm -rf "/tmp"` is
// still caught, and `$HOME` / `${HOME}` / `~/docs` resolve to the home
// guard rather than being treated as opaque words. A leading "sudo " prefix
// is ignored (mirroring matchSegment).
func checkBuiltinRmDangerous(seg string) (string, bool) {
	if rest, ok := strings.CutPrefix(seg, "sudo "); ok {
		seg = rest
	}

	prog, err := syntax.NewParser().Parse(strings.NewReader(seg), "")
	if err != nil {
		return "", false // unparseable segments are handled conservatively upstream
	}

	var call *syntax.CallExpr
	syntax.Walk(prog, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && call == nil {
			call = c
			return false
		}
		return true
	})
	if call == nil || len(call.Args) == 0 {
		return "", false
	}
	if wordLiteral(call.Args[0]) != "rm" {
		return "", false
	}

	for _, arg := range call.Args[1:] {
		if target := shellWordString(arg); dangerousRmTarget(target) {
			return "rm -rf " + target + " (builtin)", true
		}
	}
	return "", false
}

// dangerousRmTarget reports whether a single `rm` argument targets the root
// directory, a root-level directory (direct child of /), or the home
// directory (~, $HOME/${HOME}) — including their direct children. Paths
// deeper than one component (e.g. /tmp/xxx, /var/log/*, ~/.cache/foo) are
// not considered absolutely dangerous and fall through to user policy.
//
// Path components are compared after filepath.Clean, so traversal forms
// like /tmp/../etc or /./etc are normalized to /etc and still denied.
func dangerousRmTarget(p string) bool {
	// Home directory forms.
	switch {
	case p == "~", p == "$HOME", p == "${HOME}":
		return true // home itself
	case strings.HasPrefix(p, "~/"):
		return isSingleComponent(strings.TrimPrefix(p, "~/"))
	case strings.HasPrefix(p, "$HOME/"):
		return isSingleComponent(strings.TrimPrefix(p, "$HOME/"))
	case strings.HasPrefix(p, "${HOME}/"):
		return isSingleComponent(strings.TrimPrefix(p, "${HOME}/"))
	}

	// Absolute paths: normalize, then require exactly one component below /.
	clean := filepath.Clean(p)
	if clean == "/" {
		return true // the root directory itself
	}
	if rest, ok := strings.CutPrefix(clean, "/"); ok {
		return isSingleComponent(rest)
	}
	return false
}

// isSingleComponent reports whether s is a non-empty path component that
// contains no further '/'.
func isSingleComponent(s string) bool {
	return s != "" && !strings.Contains(s, "/")
}

// shellWordString renders a shell word to a best-effort literal string:
// plain literals (including '~', quoted strings) are concatenated, simple
// parameter expansions render as $NAME / ${NAME}, and anything dynamic
// (command substitutions, arithmetic) is dropped. Callers treat the result
// as opaque; ambiguous words simply fail the dangerous-target check.
func shellWordString(w *syntax.Word) string {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				switch ip := inner.(type) {
				case *syntax.Lit:
					sb.WriteString(ip.Value)
				case *syntax.ParamExp:
					sb.WriteString(paramExpString(ip))
				}
			}
		case *syntax.ParamExp:
			sb.WriteString(paramExpString(p))
		}
	}
	return sb.String()
}

// paramExpString renders a simple $NAME / ${NAME} expansion literally.
// Complex expansions (${a:-b}, ${#a}, ...) render as "" and are ignored.
func paramExpString(p *syntax.ParamExp) string {
	simple := p.Param != nil && p.Flags == nil &&
		!p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.NestedParam == nil && p.Index == nil &&
		len(p.Modifiers) == 0 && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Exp == nil
	if !simple {
		return ""
	}
	if p.Short {
		return "$" + p.Param.Value
	}
	return "${" + p.Param.Value + "}"
}

// wordLiteral returns the word's value only when it is a single plain
// literal (the common case for a command name), otherwise "".
func wordLiteral(w *syntax.Word) string {
	if len(w.Parts) == 1 {
		if l, ok := w.Parts[0].(*syntax.Lit); ok {
			return l.Value
		}
	}
	return ""
}
