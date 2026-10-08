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
	// DecisionAllowExempt is an allow the USER WROTE: an `allow` rule matched,
	// and the matched rule comes back with the decision. It is stronger than
	// DecisionAllow in exactly one way — an allow pattern means "no exception is
	// needed here", which on the ask path also means the JUDGMENT GATE is not
	// consulted (see agent/agent_permission.go's checkBashPermission).
	//
	// It is a separate value rather than a rule returned alongside
	// DecisionAllow because the two must not be confused by a caller that reads
	// only the decision: a bare Allow is "nothing covered this, so it runs by
	// default" — the population the judgment gate exists for — while this one is
	// the user's own answer about exactly this shape.
	DecisionAllowExempt
	// DecisionAsk requires interactive user approval before execution.
	DecisionAsk
	// DecisionDeny blocks the call; an error is returned to the LLM.
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionAllowExempt:
		return "allow (exempted by an allow rule)"
	case DecisionAsk:
		return "ask"
	case DecisionDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Rules holds glob patterns classifying one subject of a check: a Bash command
// segment, or the target path of a file tool. Only the '*' wildcard is
// supported (any, possibly empty, byte sequence) and it spans '/' — so a
// pattern like "~/.ssh/*" or "git push*" covers everything after the star.
// Matching is case-sensitive.
//
// Which text a rule is matched against is the check's business: CheckBash
// matches canonical command segments, CheckPath a canonical absolute path (see
// paths.go). The three lists mean the same thing in both: deny blocks, allow
// exempts from ask rules AND from the judgment gate (the decision comes back as
// DecisionAllowExempt), ask raises the question.
type Rules struct {
	Deny  []string
	Ask   []string
	Allow []string
}

// PolicyOption configures a Policy as it is built. Options exist so a caller
// that has more rule sets to hand over (the agent: the file tools' path rules)
// adds them to the SAME policy object — one session switch, one reset, one
// inherit — without every command-only caller and test growing an argument.
type PolicyOption func(*Policy)

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

// Policy evaluates Bash commands (CheckBash) and file tools' target paths
// (CheckPath) against allow/ask/deny rules, plus the built-in structured rm
// guard and a session-scoped "stop asking" switch (the frontends' 「本会话全部
// 允许」). Safe for concurrent use.
type Policy struct {
	deny  []string
	ask   []string
	allow []string

	// read, write, edit are the file tools' path rules, attached by
	// WithFileRules (see paths.go). Zero means nothing watches those calls, which
	// is the default: no file rules exist in a stock config.
	read  PathRules
	write PathRules
	edit  PathRules

	// builtinRm enables the structured root/home deletion guard for `rm`
	// invocations (see checkBuiltinRmDangerous). Default on; disabled via
	// NewPolicyNoBuiltins (permissions.bash.disable_builtin_deny).
	builtinRm bool

	mu sync.Mutex
	// allowAllAsks is the session-scoped approval: every ask rule is treated as
	// allowed until this policy goes away. Deny rules are decided ABOVE it in
	// CheckBash, so it can never turn "stop asking me" into "run what I forbade".
	allowAllAsks bool
}

// NewPolicy merges global and project-level rules into a Policy.
// deny/ask are unioned from both sources — a project can always tighten.
// allow is taken from the GLOBAL source only: project-level allow is dropped
// so a cloned repository can never loosen the user's ask tripwires
// (deny stays absolute; ask can only be added, never exempted, by projects).
// The built-in structured rm guard (root/home deletion) is enabled.
//
// The file tools' path rules are attached by passing WithFileRules; a policy
// built without it watches commands only, which is what a caller that has no
// file rules (or a command-only test) wants.
func NewPolicy(global, project Rules, opts ...PolicyOption) *Policy {
	return newPolicy(global, project, true, opts...)
}

// NewPolicyNoBuiltins is NewPolicy with the built-in structured rm guard
// disabled. Use it only when the user opted out of ALL built-in protection
// via permissions.bash.disable_builtin_deny in the GLOBAL config — note that
// BuiltinDenyRules (disk/shutdown globs) are prepended by the caller, so this
// only disables the rm argument check.
func NewPolicyNoBuiltins(global, project Rules, opts ...PolicyOption) *Policy {
	return newPolicy(global, project, false, opts...)
}

func newPolicy(global, project Rules, builtinRm bool, opts ...PolicyOption) *Policy {
	p := &Policy{builtinRm: builtinRm}
	merged := mergeLayer(global, project)
	p.deny, p.ask, p.allow = merged.Deny, merged.Ask, merged.Allow
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// mergeLayer unions one rule set across the global and project layers: deny and
// ask come from both (a project can only tighten), allow from the GLOBAL layer
// only. Both halves of the policy go through it, so "what a project may do to
// the user's rules" is answered once.
func mergeLayer(global, project Rules) Rules {
	return Rules{
		Deny:  append(append([]string{}, global.Deny...), project.Deny...),
		Ask:   append(append([]string{}, global.Ask...), project.Ask...),
		Allow: append([]string{}, global.Allow...),
	}
}

// Empty reports whether the policy has no rules at all — including the
// built-in rm guard, and including the file tools' path rules: a policy that
// only watches file calls is not empty, and a caller using this as "nothing
// watches anything" would otherwise skip the half the user did configure.
// A policy built with NewPolicy (built-in guard enabled) is never Empty.
func (p *Policy) Empty() bool {
	return len(p.deny) == 0 && len(p.ask) == 0 && len(p.allow) == 0 && !p.builtinRm &&
		pathRuleSetEmpty(p.read) && pathRuleSetEmpty(p.write) && pathRuleSetEmpty(p.edit)
}

// pathRuleSetEmpty reports whether one tool's path rules hold nothing.
func pathRuleSetEmpty(r PathRules) bool {
	return len(r.Deny) == 0 && len(r.Ask) == 0 && len(r.Allow) == 0
}

// sessionApproved reports the session-scoped 「本会话全部允许」 switch. It is
// read by both halves of the policy at the point where it may act — after every
// deny check of theirs has run — so that "stop asking me" can never become "do
// what I forbade".
func (p *Policy) sessionApproved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allowAllAsks
}

// AllowAllAsksForSession stops this session's ask rules from asking: every command
// they would have sent to the user is allowed instead, and so is every matching
// path of the file tools' ask rules. This is the frontends' 「本会话全部允许」
// (the TUI's `a`), and it is deliberately SESSION-scoped — the policy is built
// per agent, so it dies with the conversation.
//
// It does NOT disable deny rules, of either half. Both checks decide deny while
// they walk their subject and return before an ask is ever raised, so the switch
// cannot be reached for a forbidden command or path: "stop asking me" never
// becomes "run what I forbade".
func (p *Policy) AllowAllAsksForSession() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowAllAsks = true
}

// ResetSessionApprovals drops the session-scoped approvals, so the next command
// asks again. Called when a conversation ENDS (a new session is not the session the
// user approved). The user's rules are not state and are untouched — deny/ask/allow
// still describe the same commands.
func (p *Policy) ResetSessionApprovals() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowAllAsks = false
}

// InheritSessionApprovals copies the session-scoped approvals from prev onto p.
//
// A frontend whose session can move between trees (the desktop: one process,
// several sessions, each with its own working directory) has to REBUILD the
// policy when the tree changes, because the project half of the rules is read
// from <tree>/.tachi/permissions.yaml. Rebuilding would otherwise re-ask for
// every command the user had already waved through with 「本会话全部允许」 —
// a regression the user cannot see coming, since the card simply reappears.
//
// Only the APPROVALS carry over; the rules come from the new tree. That is the
// whole point: the tree changed, so which commands are forbidden changed with
// it, and deny is still decided before any ask is reached. prev may be nil
// (no policy to inherit from), in which case p is left as built.
func (p *Policy) InheritSessionApprovals(prev *Policy) {
	if p == nil || prev == nil {
		return
	}
	prev.mu.Lock()
	approved := prev.allowAllAsks
	prev.mu.Unlock()
	if !approved {
		return
	}
	p.AllowAllAsksForSession()
}

// AskNoRule is what the returned RULE slot carries when no rule was consulted at all: the command
// could not be parsed, so there is nothing to name and the policy asks conservatively (see
// CheckBash's "Unparseable commands → Ask").
//
// It is a distinct VALUE rather than a sentence on purpose. That slot is read by everything that
// shows or logs an ask — the TUI prompt, the desktop's confirmation card, the non-interactive
// refusal, the log fields — and those surfaces have to word it DIFFERENTLY from "a rule matched":
// a human-readable sentence sitting in a field named `rule` is exactly what made the desktop card
// claim 「命中 permissions.bash.ask 规则」 for a command that hit no rule at all, sending the reader
// looking for a rule that does not exist (issue #40).
const AskNoRule = "no-rule:unparseable"

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
//   - Per segment: deny match → Deny; allow match → the segment is exempt from
//     ask rules AND the whole command is reported as DecisionAllowExempt (the
//     rule that matched comes back with it); ask match → Ask.
//   - Whole command: any Deny → Deny; else any Ask → Ask; else AllowExempt when
//     an allow rule matched, else Allow.
//   - Unparseable commands → Ask (conservative).
//   - A session-wide approval (AllowAllAsksForSession) turns Ask into Allow —
//     and only Ask: deny is decided above this point, so a forbidden command
//     can never be reached by it.
func (p *Policy) CheckBash(command string) (Decision, string) {
	if strings.TrimSpace(command) == "" {
		return DecisionAllow, ""
	}

	// The command is parsed AS WRITTEN. It used to be whitespace-collapsed first
	// (normalize, for the exact-command session memory that is gone), which made every
	// multi-line command unparseable — a heredoc's delimiter stops starting a line, and
	// the body's quotes and backticks become live syntax — so writing a file with
	// `cat > f <<'EOF'` fell through to the conservative ask. The parser prints canonical
	// segments for matching, so nothing is lost by handing it the original text, and the
	// body of a heredoc is data: it does not appear in the segments and cannot trip a rule.
	segments, err := splitShellCommands(command)
	if err != nil {
		return DecisionAsk, AskNoRule
	}

	askRule := ""
	allowRule := ""
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
		if rule := matchSegment(p.allow, seg); rule != "" {
			// Explicitly exempted from ask rules — and, because the gate runs on
			// the same path, from the judgment too (the caller reads
			// DecisionAllowExempt). Remembered rather than returned here: a later
			// segment can still deny, and an ask rule on ANOTHER segment still
			// stands.
			allowRule = rule
			continue
		}
		if rule := matchSegment(p.ask, seg); rule != "" {
			askRule = rule // keep scanning: a later segment may still deny
		}
	}
	if askRule != "" {
		// The session-wide approval is applied HERE and not at the top of the
		// function on purpose: every deny check above has already run, so an
		// allowed session can never run a command the rules forbid.
		if p.sessionApproved() {
			return DecisionAllow, ""
		}
		return DecisionAsk, askRule
	}
	if allowRule != "" {
		return DecisionAllowExempt, allowRule
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

// matchAny returns the first pattern that matches s, or "" if none match.
func matchAny(patterns []string, s string) string {
	for _, pat := range patterns {
		if wildcardMatch(pat, s) {
			return pat
		}
	}
	return ""
}

// Segments returns the canonical command segments CheckBash matches rules
// against: every command invocation in pipelines, lists and command
// substitutions, plus redirects as their own pseudo-segments, printed in
// canonical form (quoting normalised, a heredoc's body absent).
//
// It is exported for the ONE other reader that must agree with the policy about
// what a command is: a judgment gate's state has to describe the command the
// rules were matched against, and a second parser there would be free to drift
// from this one. An error means the command could not be parsed at all — the
// same condition that makes CheckBash answer conservatively.
func Segments(command string) ([]string, error) {
	return splitShellCommands(command)
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
