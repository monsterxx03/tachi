package permission

import (
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The path half of the policy: the same three decisions and the same
// precedence as a Bash command (deny > allow as an exemption > ask > default
// allow), applied to the TARGET PATH of a file tool instead of to command
// segments. The two halves live in one Policy because they are one switch for the
// user: 「本会话全部允许」 and the session resets cover both, and a frontend that
// rebuilds the policy rebuilds both.
//
// Two forms of the same path meet here, and the reason is that a rule has to
// answer two questions that pull in opposite directions:
//
//   - What is actually being touched? Only the CANONICAL path knows (absolute,
//     cleaned, symlinks resolved as far as the path exists): `ln -s ~/.ssh /tmp/x`
//     must not turn a read of `/tmp/x/id_rsa` into a path no rule about `~/.ssh`
//     covers.
//   - Which rule did the user write? Only their TEXT knows. A pattern is
//     canonicalised for MATCHING, and the matched rule is quoted back to the
//     human as they wrote it — an ask that names `/private/etc/*` for a rule they
//     wrote as `/etc/*` sends them looking for a rule they never wrote, which is
//     the same failure as naming a rule that did not match at all (issue #40).
//
// Both sides are therefore canonicalised once, at build time, and compared
// canonically (PathPattern).

// CanonicalizePath is the ONE resolution both sides of a path check go through:
// the TARGET of a call and the literal prefix of a rule. It exists so the two are
// not resolved by two different functions and a rule cannot silently stop firing
// (see PathPattern).
//
// A path that cannot be resolved at all is returned as written: dropping a rule,
// or pointing it somewhere else, would be worse than one that matches nothing.
func CanonicalizePath(path string) string {
	canon, err := fileutil.CanonicalPath(path)
	if err != nil {
		return path
	}
	return canon
}

// FileTool names which file tool a path check is about. The three have separate
// rule sets because the same path is not the same risk: reading a key leaks it,
// overwriting one loses it.
type FileTool int

const (
	FileToolRead FileTool = iota
	FileToolWrite
	FileToolEdit
)

// String renders the tool for logs and ask previews. It spells the name the
// MODEL sees (agent/tools' ToolNameRead and friends), because that same word is
// what a user reads in the preview and greps for in the transcript.
func (t FileTool) String() string {
	switch t {
	case FileToolRead:
		return "ReadFile"
	case FileToolWrite:
		return "WriteFile"
	case FileToolEdit:
		return "EditFile"
	}
	return "file"
}

// PathPattern is one path rule in the two forms it is needed in: Text is what the
// user wrote, verbatim, and Canonical is that pattern anchored (a relative one
// joined to its layer's tree, `~` expanded) and with its literal prefix resolved,
// which is what the matcher compares against the target's canonical path.
//
// The two differ on a machine where a directory is reached through a symlink — on
// macOS `/etc` is `/private/etc`, so a rule written `/etc/*` would never fire
// against the canonical target `/private/etc/hosts`, and a deny that denies
// nothing is worse than no rule at all, because the reader believes the lock is
// there — and they differ for every `~` and every relative pattern, because Text
// stays as written.
//
// Text is what an ask preview and a refusal quote back, and it is deliberately the
// AUTHORED string rather than the expansion: the reader is looking at their own
// config file, where `~/.ssh/*` is what they typed. A rule named in a form that
// file does not contain is the issue-#40 failure again — the reader goes hunting
// for something they never wrote.
type PathPattern struct {
	Text      string
	Canonical string
}

// PathRules is one file tool's rules, split the way the check reads them.
type PathRules struct {
	Deny  []PathPattern
	Ask   []PathPattern
	Allow []PathPattern
}

// FileRules groups the three file tools' rule sets for ONE layer (the global
// config, or one tree's project file).
type FileRules struct {
	Read  PathRules
	Write PathRules
	Edit  PathRules
}

// WithFileRules attaches the file tools' path rules to a policy. It is an option
// rather than a constructor argument so that every existing caller — and every
// test that builds a command-only policy — keeps its one-argument-per-layer shape,
// while the agent that HAS the file rules passes them in the same call that builds
// the policy (one object, one session switch, one reset).
//
// The layering rule is the command rules': deny and ask are unioned from both
// layers (a project can only tighten), allow is taken from the GLOBAL layer only,
// so a cloned repository can never exempt a path from the user's ask tripwires.
func WithFileRules(global, project FileRules) PolicyOption {
	return func(p *Policy) {
		p.read = mergePathLayer(global.Read, project.Read)
		p.write = mergePathLayer(global.Write, project.Write)
		p.edit = mergePathLayer(global.Edit, project.Edit)
	}
}

// mergePathLayer is mergeLayer for the path half.
func mergePathLayer(global, project PathRules) PathRules {
	return PathRules{
		Deny:  append(append([]PathPattern{}, global.Deny...), project.Deny...),
		Ask:   append(append([]PathPattern{}, global.Ask...), project.Ask...),
		Allow: append([]PathPattern{}, global.Allow...),
	}
}

// PathRulesArmed reports whether this policy holds ANY path rule for tool — the
// cheap question a caller asks before resolving a path it could only conclude
// "allow" about.
//
// It exists because the answer has to be free: checkFilePermission runs on EVERY
// ReadFile / WriteFile / EditFile call, and resolving a path (symlinks, a stat)
// to then match it against nothing would be a cost the stock config pays for a
// feature it does not use. `CheckPath` on an unarmed tool is harmless but not
// free; this is what lets the caller skip it.
func (p *Policy) PathRulesArmed(tool FileTool) bool {
	if p == nil {
		return false
	}
	r := p.rulesFor(tool)
	return len(r.Deny) > 0 || len(r.Ask) > 0 || len(r.Allow) > 0
}

// rulesFor returns the tool's rule set. An unknown FileTool gets the zero
// PathRules (nothing matches, everything is allowed by default) rather than
// another tool's set: guessing would apply a rule the user wrote for a different
// action.
func (p *Policy) rulesFor(tool FileTool) PathRules {
	switch tool {
	case FileToolRead:
		return p.read
	case FileToolWrite:
		return p.write
	case FileToolEdit:
		return p.edit
	}
	return PathRules{}
}

// CheckPath evaluates the CANONICAL absolute path of one file tool call (see
// PathPattern for why it is that form), and returns the decision plus the matched
// rule AS THE USER WROTE IT (or "").
//
// Order, once per call: a deny pattern → Deny; an allow pattern → AllowExempt
// (allowed because the USER wrote a rule for it, and exempt from the ask rules —
// allow means "no exception needed here", never "only this may run"); an ask
// pattern → Ask. Nothing matching is Allow: the rule sets are tripwires, not
// whitelists.
//
// A session-wide approval (AllowAllAsksForSession) turns Ask into Allow and
// nothing else: deny is decided above it, so 「本会话全部允许」 can never become
// "write what I forbade".
func (p *Policy) CheckPath(tool FileTool, path string) (Decision, string) {
	if p == nil || strings.TrimSpace(path) == "" {
		return DecisionAllow, ""
	}
	r := p.rulesFor(tool)
	if rule := matchPath(r.Deny, path); rule != "" {
		return DecisionDeny, rule
	}
	if rule := matchPath(r.Allow, path); rule != "" {
		// The matched rule comes back AS THE USER WROTE IT (PathPattern.Text):
		// it is what a log line or a preview quotes, and the caller reads
		// AllowExempt as "the user already answered for this path" (the judgment
		// gate is not consulted, see checkFilePermission).
		return DecisionAllowExempt, rule
	}
	if rule := matchPath(r.Ask, path); rule != "" {
		if p.sessionApproved() {
			return DecisionAllow, ""
		}
		return DecisionAsk, rule
	}
	return DecisionAllow, ""
}

// matchPath returns the TEXT of the first pattern matching path, or "".
func matchPath(patterns []PathPattern, path string) string {
	for _, pat := range patterns {
		if pathMatch(pat.Canonical, path) {
			return pat.Text
		}
	}
	return ""
}

// pathMatch reports whether a canonical pattern matches a canonical absolute path.
//
// Two ways to match, and the second one is not an extra feature: a LITERAL pattern
// also covers everything under it, because `deny: ["/home/me/.ssh"]` names a
// directory and a rule that only matched the directory entry itself would be a
// trap — the reader sees a lock on the key and the key's siblings stay open. A
// pattern with a '*' keeps only the wildcard reading, so `/home/me/*/id_rsa`
// cannot silently widen into a prefix.
func pathMatch(pattern, path string) bool {
	if wildcardMatch(pattern, path) {
		return true
	}
	if strings.Contains(pattern, "*") {
		return false
	}
	return strings.HasPrefix(path, pattern+"/")
}

// ExpandPathRules makes a layer's path patterns absolute against anchor and
// returns them ready for matchPath: each one anchored (`~` expanded, a relative
// pattern joined to the layer's tree), with its literal prefix resolved, and with
// the text the user wrote kept verbatim beside it (see PathPattern).
//
// A leading `~` means the USER's home directory and expands against home, not
// against the anchor: the global config and a project file both write `~` for the
// same place, and a project file that could re-point `~` at its own tree would be
// a way to smuggle a rule about the user's home into "inside this repo". Everything
// else that is still relative is joined to anchor — $HOME for the global config,
// the repository root for a <tree>/.tachi/permissions.yaml — which is what makes
// `ask: ["secrets/**"]` in a project file mean "inside this repository".
//
// Empty patterns are dropped (a YAML list with a stray "" would otherwise match
// nothing while looking like a rule).
//
// home empty (the caller could not resolve it) leaves `~` patterns as written,
// i.e. matching nothing: the caller is expected to have warned, and inventing a
// home would be worse — a deny rule silently pointed at the wrong directory.
func ExpandPathRules(r Rules, anchor, home string) PathRules {
	return PathRules{
		Deny:  expandPatterns(r.Deny, anchor, home),
		Ask:   expandPatterns(r.Ask, anchor, home),
		Allow: expandPatterns(r.Allow, anchor, home),
	}
}

func expandPatterns(patterns []string, anchor, home string) []PathPattern {
	out := make([]PathPattern, 0, len(patterns))
	for _, pat := range patterns {
		s := strings.TrimSpace(pat)
		if s == "" {
			continue
		}
		// Text is the authored string, untouched — that is what the reader's own
		// config file contains (see PathPattern).
		expanded := expandPattern(s, anchor, home)
		out = append(out, PathPattern{Text: s, Canonical: canonicalPattern(expanded)})
	}
	return out
}

// canonicalPattern resolves the part of a pattern that is a path: for a literal
// pattern that is the whole thing, and for a wildcard one it is the directory the
// wildcard sits in (`/a/b*.go` resolves `/a` and keeps `b*.go` as text, `/a/*`
// resolves `/a`). A pattern that begins with a wildcard has nothing to resolve.
//
// It builds the result from strings rather than with filepath.Join, and that is
// load-bearing: Join CLEANS, so `Join("/a", "")+"*"` would come out as `/a*` — a
// rule that no longer covers the directory but every sibling whose name starts
// the same way (`/a-backup`, `/a2`). A deny that silently widens is a bug the
// reader cannot see, and an allow rule that widens is a hole.
//
// Like the target path, the resolution covers the prefix that EXISTS and leaves
// the rest as written, so a rule about a directory that is not there yet is
// resolved exactly as far as its own path can be.
func canonicalPattern(pattern string) string {
	star := strings.Index(pattern, "*")
	if star < 0 {
		return CanonicalizePath(pattern)
	}
	head, rest := pattern[:star], pattern[star:]
	cut := strings.LastIndex(head, "/")
	if cut < 0 {
		// No directory part to resolve. Only reachable for a relative pattern that
		// expansion has not anchored; a leading wildcard returned above.
		return pattern
	}
	dir, tail := head[:cut], head[cut+1:]
	if dir == "" {
		dir = "/" // absolute at the root: "/x*" is not a case, but "/*" is
	}
	resolved := CanonicalizePath(dir)
	if resolved == "/" {
		return resolved + tail + rest
	}
	return resolved + "/" + tail + rest
}

// expandPattern anchors one pattern. `~` is the user's home directory wherever
// the rule was written; anything else still relative is joined to anchor ($HOME
// for the global layer, the tree for a project file).
//
// A pattern that BEGINS with a wildcard is the exception, and deliberately so: a
// leading `*` already means "any prefix", so joining it to an anchor would turn
// `*/.ssh/*` into `$HOME/*/.ssh/*` — a rule that says less than it reads, on a
// config where the reader meant "anywhere". It is also the only form that can
// name a path outside the tree from a file whose anchor is unknown (and the one
// the desktop smoke needs, where the working directory is a fresh temp dir).
func expandPattern(pattern, anchor, home string) string {
	switch {
	case pattern == "~":
		if home == "" {
			return pattern
		}
		return filepath.Clean(home)
	case strings.HasPrefix(pattern, "~/"):
		if home == "" {
			return pattern
		}
		return filepath.Join(home, pattern[len("~/"):])
	case filepath.IsAbs(pattern):
		return filepath.Clean(pattern)
	case strings.HasPrefix(pattern, "*"):
		return filepath.Clean(pattern)
	case anchor == "":
		return filepath.Clean(pattern)
	default:
		return filepath.Join(anchor, pattern)
	}
}
