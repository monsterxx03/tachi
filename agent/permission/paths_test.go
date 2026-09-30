package permission

import (
	"strings"
	"testing"
)

// pathRules is the conversion a policy build does, for patterns that are already
// absolute and free of `~`: no anchor to apply. It is what the tests below hand to
// WithFileRules.
func pathRules(r Rules) PathRules { return ExpandPathRules(r, "", "") }

// texts returns the user-written text of every pattern, which is what a check
// reports back and what these tests compare.
func texts(patterns []PathPattern) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p.Text)
	}
	return out
}

// canonicals returns the matching form of every pattern: anchored and with its
// literal prefix resolved, which is what the matcher compares.
func canonicals(patterns []PathPattern) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p.Canonical)
	}
	return out
}

// The three decisions on the path half, and — the part worth pinning — which
// pattern form reaches what: a literal path covers the directory's contents, a
// wildcard keeps its own meaning, and expansion is what makes `~` and
// tree-relative patterns one text the matcher can compare.
func TestCheckPathDecisions(t *testing.T) {
	files := FileRules{
		Read: pathRules(Rules{Ask: []string{"/repo/secrets/*"}}),
		Write: pathRules(Rules{
			Deny: []string{"/etc/*"},
			Ask:  []string{"/repo/config/*"},
		}),
		Edit: pathRules(Rules{}),
	}

	tests := []struct {
		name    string
		tool    FileTool
		path    string
		want    Decision
		wantRul string
	}{
		{"unmatched read runs", FileToolRead, "/repo/main.go", DecisionAllow, ""},
		{"ask rule matches the read", FileToolRead, "/repo/secrets/token.env", DecisionAsk, "/repo/secrets/*"},
		{"another tool's rules do not apply", FileToolRead, "/repo/config/app.yaml", DecisionAllow, ""},
		{"deny beats everything", FileToolWrite, "/etc/hosts", DecisionDeny, "/etc/*"},
		{"ask on the write half", FileToolWrite, "/repo/config/app.yaml", DecisionAsk, "/repo/config/*"},
		{"edit with no rules runs", FileToolEdit, "/etc/hosts", DecisionAllow, ""},
		{"unknown tool watches nothing", FileTool(99), "/etc/hosts", DecisionAllow, ""},
		{"empty path is not a rule match", FileToolWrite, "  ", DecisionAllow, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPolicy(Rules{}, Rules{}, WithFileRules(files, FileRules{}))
			// CheckPath's contract is the CANONICAL path (see PathPattern); the
			// caller resolves it with the same function the rules were expanded
			// with, which is what makes `/etc/*` fire on macOS.
			got, rule := p.CheckPath(tt.tool, CanonicalizePath(tt.path))
			if got != tt.want || rule != tt.wantRul {
				t.Fatalf("CheckPath(%s, %q) = (%v, %q), want (%v, %q)",
					tt.tool, tt.path, got, rule, tt.want, tt.wantRul)
			}
		})
	}
}

// Allow is an exemption, not a whitelist: a path that matches allow is not asked
// about, and a path that matches nothing at all runs too.
func TestCheckPathAllowExemptsFromAsk(t *testing.T) {
	p := NewPolicy(Rules{}, Rules{}, WithFileRules(FileRules{
		Write: pathRules(Rules{Ask: []string{"/repo/*"}, Allow: []string{"/repo/build/*"}}),
	}, FileRules{}))

	if d, _ := p.CheckPath(FileToolWrite, "/repo/build/out.bin"); d != DecisionAllow {
		t.Fatalf("an allow-exempted path should run, got %v", d)
	}
	if d, rule := p.CheckPath(FileToolWrite, "/repo/src/main.go"); d != DecisionAsk || rule != "/repo/*" {
		t.Fatalf("an ask rule should still ask, got (%v, %q)", d, rule)
	}
	if d, _ := p.CheckPath(FileToolWrite, "/elsewhere/x"); d != DecisionAllow {
		t.Fatalf("nothing matching runs, got %v", d)
	}
}

// The session switch is the frontends' 「本会话全部允许」: it silences ask rules
// on BOTH halves and can never reach a deny.
func TestCheckPathSessionApprovalSilencesAskOnly(t *testing.T) {
	p := NewPolicy(
		Rules{Ask: []string{"git push*"}},
		Rules{},
		WithFileRules(FileRules{
			Write: pathRules(Rules{Deny: []string{"/etc/*"}, Ask: []string{"/repo/config/*"}}),
		}, FileRules{}),
	)

	if d, rule := p.CheckPath(FileToolWrite, "/repo/config/app.yaml"); d != DecisionAsk || rule != "/repo/config/*" {
		t.Fatalf("before the approval the ask rule should ask, got (%v, %q)", d, rule)
	}

	p.AllowAllAsksForSession()

	if d, _ := p.CheckPath(FileToolWrite, "/repo/config/app.yaml"); d != DecisionAllow {
		t.Fatalf("an approved session should not ask, got %v", d)
	}
	if d, _ := p.CheckBash("git push origin main"); d != DecisionAllow {
		t.Fatalf("an approved session should not ask about the command half either, got %v", d)
	}
	// deny is decided above the switch, so it survives it.
	if d, _ := p.CheckPath(FileToolWrite, CanonicalizePath("/etc/hosts")); d != DecisionDeny {
		t.Fatalf("deny must survive a session approval, got %v", d)
	}
}

// A policy that only watches file calls is not Empty: Empty is the "nothing
// watches anything" fast path, and a caller acting on it would skip the half the
// user did configure.
func TestEmptyCountsFileRules(t *testing.T) {
	base := NewPolicyNoBuiltins(Rules{}, Rules{})
	if !base.Empty() {
		t.Fatal("a no-builtins policy with no rules should be empty")
	}
	withFiles := NewPolicyNoBuiltins(Rules{}, Rules{}, WithFileRules(FileRules{
		Read: pathRules(Rules{Ask: []string{"/repo/*"}}),
	}, FileRules{}))
	if withFiles.Empty() {
		t.Fatal("a policy holding file rules is not empty")
	}
}

// A project can only tighten: its deny and ask join the global ones, its allow is
// dropped (allow is global-only), for every file tool.
func TestWithFileRulesProjectLayerCanOnlyTighten(t *testing.T) {
	p := NewPolicy(Rules{}, Rules{}, WithFileRules(
		FileRules{Write: pathRules(Rules{Allow: []string{"/repo/build/*"}})},
		FileRules{Write: pathRules(Rules{Deny: []string{"/repo/.git/*"}, Allow: []string{"/repo/.git/*"}})},
	))

	if d, _ := p.CheckPath(FileToolWrite, "/repo/.git/config"); d != DecisionDeny {
		t.Fatalf("a project deny rule must apply, got %v", d)
	}
	if d, _ := p.CheckPath(FileToolWrite, "/repo/build/out.bin"); d != DecisionAllow {
		t.Fatalf("the global allow still exempts, got %v", d)
	}
}

func TestPathMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*/.ssh/*", "/Users/dev/.ssh/id_rsa", true},
		{"*/.ssh/*", "/Users/dev/.ssh/", true},
		{"*/.ssh/*", "/Users/dev/ssh/id_rsa", false},
		// A literal path names a directory, so it covers what is inside it.
		{"/Users/dev/.ssh", "/Users/dev/.ssh/id_rsa", true},
		{"/Users/dev/.ssh", "/Users/dev/.ssh", true},
		{"/Users/dev/.ssh", "/Users/dev/.ssh-backup/id_rsa", false},
		// A wildcard keeps only its own reading: it is not a prefix rule.
		{"/Users/dev/*", "/Users/dev/x/y", true},
		{"/Users/dev/*.go", "/Users/dev/x/y.go", true},
		{"/Users/dev/*.go", "/Users/dev/main.go/backup", false},
		{"/repo/main.go", "/repo/main.go.bak", false},
	}
	for _, tt := range tests {
		if got := pathMatch(tt.pattern, tt.path); got != tt.want {
			t.Errorf("pathMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestExpandPathRules(t *testing.T) {
	tests := []struct {
		name   string
		rules  Rules
		anchor string
		home   string
		want   Rules
	}{
		{
			// Text stays as the user wrote it — their config file contains `~/.ssh/*`,
			// and a refusal has to name something they can find there.
			name:   "tilde is the user's home, and the TEXT keeps the tilde",
			rules:  Rules{Deny: []string{"~/.ssh/*"}, Ask: []string{"~"}},
			anchor: "/repo",
			home:   "/Users/dev",
			want:   Rules{Deny: []string{"~/.ssh/*"}, Ask: []string{"~"}},
		},
		{
			name:   "relative patterns are anchored for matching, and read as written",
			rules:  Rules{Ask: []string{"secrets/*"}},
			anchor: "/repo",
			home:   "/Users/dev",
			want:   Rules{Ask: []string{"secrets/*"}},
		},
		{
			name:   "absolute patterns are cleaned",
			rules:  Rules{Deny: []string{"/etc/../etc/shadow"}},
			anchor: "/repo",
			home:   "/Users/dev",
			want:   Rules{Deny: []string{"/etc/../etc/shadow"}},
		},
		{
			name:   "empty patterns are dropped rather than kept as rules that match nothing",
			rules:  Rules{Ask: []string{"", "  "}},
			anchor: "/repo",
			home:   "/Users/dev",
			want:   Rules{Ask: []string{}},
		},
		{
			name:   "an unknown home leaves ~ as written (the caller warns)",
			rules:  Rules{Deny: []string{"~/secret"}},
			anchor: "/repo",
			home:   "",
			want:   Rules{Deny: []string{"~/secret"}},
		},
		{
			name:   "a relative pattern with no anchor stays as written",
			rules:  Rules{Ask: []string{"secrets/*"}},
			anchor: "",
			home:   "/Users/dev",
			want:   Rules{Ask: []string{"secrets/*"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandPathRules(tt.rules, tt.anchor, tt.home)
			if strings.Join(texts(got.Deny), ",") != strings.Join(tt.want.Deny, ",") ||
				strings.Join(texts(got.Ask), ",") != strings.Join(tt.want.Ask, ",") ||
				strings.Join(texts(got.Allow), ",") != strings.Join(tt.want.Allow, ",") {
				t.Fatalf("ExpandPathRules = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Expansion is what makes a rule fire: an unexpanded `~/.ssh/*` is a text no
// canonical path can equal. And the rule it REPORTS is the tilde form, because
// that is the line in the user's own config file.
func TestExpandPathRulesFeedsTheMatcher(t *testing.T) {
	expanded := ExpandPathRules(Rules{Deny: []string{"~/.ssh/*"}}, "/repo", "/Users/dev")
	if got := canonicals(expanded.Deny); len(got) != 1 || got[0] != "/Users/dev/.ssh/*" {
		t.Fatalf("the matching form is the anchored one, got %v", got)
	}
	p := NewPolicy(Rules{}, Rules{}, WithFileRules(FileRules{Read: expanded}, FileRules{}))
	if d, rule := p.CheckPath(FileToolRead, "/Users/dev/.ssh/id_rsa"); d != DecisionDeny || rule != "~/.ssh/*" {
		t.Fatalf("the rule should deny and be quoted as written, got (%v, %q)", d, rule)
	}
}

// A pattern that begins with a wildcard is NOT anchored: `*/.ssh/*` says "any
// prefix", and joining it to an anchor would turn it into a rule about one
// directory — which is how the desktop smoke's `*/perm-fixture*` silently stopped
// matching the workspace file it was written for.
func TestExpandPathRulesLeavesLeadingWildcardsUnanchored(t *testing.T) {
	// Anchoring is a property of the MATCHING form; the text stays as written.
	got := ExpandPathRules(Rules{Ask: []string{"*/.ssh/*", "*"}}, "/repo", "/Users/dev")
	if want := "*/.ssh/*,*"; strings.Join(canonicals(got.Ask), ",") != want {
		t.Fatalf("leading wildcards must not be anchored: got %v, want %v", canonicals(got.Ask), want)
	}
	// ...while a pattern that only LOOKS relative still is.
	anchored := ExpandPathRules(Rules{Ask: []string{"secrets/*"}}, "/repo", "/Users/dev")
	if canonicals(anchored.Ask)[0] != "/repo/secrets/*" {
		t.Fatalf("a relative pattern is anchored for matching, got %v", canonicals(anchored.Ask))
	}
	if texts(anchored.Ask)[0] != "secrets/*" {
		t.Fatalf("...and is quoted to the reader as written, got %v", texts(anchored.Ask))
	}
}

// A `dir/*` rule must canonicalise to `dir/*`, not to `dir*`: the second form
// covers every SIBLING whose name starts the same way (`~/.ssh-backup`,
// `~/.ssh2`). This is a regression test for a `filepath.Join` that cleaned the
// separator away — invisible in a match-only test, because `*` spans '/' and the
// widened pattern still matched the file the rule was written for.
func TestCanonicalPatternKeepsTheSeparator(t *testing.T) {
	// A synthetic root that does not exist: nothing below `/` resolves, so the
	// expectation is the pattern itself. (A real-looking path like `/home/dev/...`
	// is a bad fixture on macOS, where `/home` is itself a firmlink.)
	const root = "/zz-nonexistent-fixture"
	tests := []struct {
		pattern string
		want    string
	}{
		{root + "/dev/.ssh/*", root + "/dev/.ssh/*"},
		{root + "/dev/.ssh/**", root + "/dev/.ssh/**"},
		{root + "/dev/*.go", root + "/dev/*.go"},
		{root + "/dev/.ssh", root + "/dev/.ssh"},
		{"/*", "/*"},
		{"*/id_rsa", "*/id_rsa"},
	}
	for _, tt := range tests {
		if got := canonicalPattern(tt.pattern); got != tt.want {
			t.Errorf("canonicalPattern(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}

	// And the widening it prevents, stated as behaviour: a rule about the ssh
	// DIRECTORY does not reach a sibling that merely starts with the same name.
	p := NewPolicy(Rules{}, Rules{}, WithFileRules(FileRules{
		Read: ExpandPathRules(Rules{Deny: []string{root + "/dev/.ssh/*"}}, "", ""),
	}, FileRules{}))
	if d, _ := p.CheckPath(FileToolRead, root+"/dev/.ssh/id_rsa"); d != DecisionDeny {
		t.Fatalf("the directory's contents are covered, got %v", d)
	}
	if d, _ := p.CheckPath(FileToolRead, root+"/dev/.ssh-backup/id_rsa"); d != DecisionAllow {
		t.Fatalf("a sibling that shares a prefix is NOT covered, got %v", d)
	}
}

// A refusal quotes the rule AS WRITTEN: the reader is looking at their own
// permissions file, and a message naming the expanded form sends them hunting for
// a line that is not there (the same failure as naming a rule that did not match,
// issue #40). This is what the user's own `~/.ssh/*` rule has to look like when it
// stops a call.
func TestCheckPathQuotesTheRuleAsWritten(t *testing.T) {
	p := NewPolicy(Rules{}, Rules{}, WithFileRules(FileRules{
		Read: ExpandPathRules(Rules{Deny: []string{"~/.ssh/*", "secrets/*"}}, "/repo", "/Users/dev"),
	}, FileRules{}))
	if _, rule := p.CheckPath(FileToolRead, "/Users/dev/.ssh/id_rsa"); rule != "~/.ssh/*" {
		t.Errorf("tilde rule quoted as %q, want the authored text", rule)
	}
	if _, rule := p.CheckPath(FileToolRead, "/repo/secrets/token"); rule != "secrets/*" {
		t.Errorf("relative rule quoted as %q, want the authored text", rule)
	}
}
