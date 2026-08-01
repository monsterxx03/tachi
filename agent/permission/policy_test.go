package permission

import (
	"testing"
)

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"git status", "git status", true},
		{"git status", "git statuss", false},
		{"git *", "git status", true},
		{"git *", "git push --force", true},
		{"git *", "gitx status", false},
		{"* --force*", "git push --force origin", true},
		{"* --force*", "git push origin", false},
		{"rm -rf /*", "rm -rf / home", true},
		{"rm -rf /*", "rm -rf /tmp/x", true},
		{"*", "anything at all", true},
		{"*", "", true},
		{"go test ./...", "go test ./...", true},
		{"go * ./...", "go test ./...", true},
		{"go * ./...", "go test ./pkg", false},
		{"ls*", "ls -la", true},
		{"ls*", "lsof", true}, // glob semantics: "ls" + anything
		{"ls *", "lsof", false},
		{"中文 *", "中文 命令", true},
		{"a*c*e", "abcde", true},
		{"a*c*e", "aec", false},
	}
	for _, tt := range tests {
		if got := wildcardMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestCheckBash_DefaultAllow(t *testing.T) {
	// With built-ins disabled and no user rules, everything is allowed.
	p := NewPolicyNoBuiltins(Rules{}, Rules{})
	d, _ := p.CheckBash("rm -rf /")
	if d != DecisionAllow {
		t.Errorf("no rules (builtins off) should default to allow, got %v", d)
	}
	if !p.Empty() {
		t.Error("policy without rules should report Empty()")
	}

	// A default NewPolicy is never Empty — the built-in rm guard always applies.
	pb := NewPolicy(Rules{}, Rules{})
	if pb.Empty() {
		t.Error("policy with built-in guard enabled should not report Empty()")
	}
	d, _ = pb.CheckBash("rm -rf /")
	if d != DecisionDeny {
		t.Errorf("built-in guard should deny rm -rf / even with no user rules, got %v", d)
	}
}

func TestCheckBash_Deny(t *testing.T) {
	p := NewPolicy(Rules{Deny: []string{"rm -rf /*", "git push --force*"}}, Rules{})

	tests := []struct {
		cmd      string
		wantDec  Decision
		wantRule string
	}{
		{"rm -rf /", DecisionDeny, "rm -rf /*"},
		{"git push --force origin main", DecisionDeny, "git push --force*"},
		// deny inside a compound command — naive prefix matching would miss this
		{"git status && rm -rf /", DecisionDeny, "rm -rf /*"},
		{"echo hi; rm -rf /tmp/x", DecisionDeny, "rm -rf /*"},
		// deny inside a pipeline
		{"cat foo | rm -rf / bar", DecisionDeny, "rm -rf /*"},
		// deny inside command substitution
		{"echo $(rm -rf /)", DecisionDeny, "rm -rf /*"},
		{"echo `rm -rf /`", DecisionDeny, "rm -rf /*"},
		// safe commands still allowed
		{"git status", DecisionAllow, ""},
		{"ls -la && echo done", DecisionAllow, ""},
		// quoted semicolon must NOT be split — no rm here at all
		{`echo "a; rm -rf fake"`, DecisionAllow, ""},
	}
	for _, tt := range tests {
		d, rule := p.CheckBash(tt.cmd)
		if d != tt.wantDec {
			t.Errorf("CheckBash(%q) decision = %v, want %v", tt.cmd, d, tt.wantDec)
		}
		if rule != tt.wantRule {
			t.Errorf("CheckBash(%q) rule = %q, want %q", tt.cmd, rule, tt.wantRule)
		}
	}
}

func TestCheckBash_DenyRedirect(t *testing.T) {
	p := NewPolicy(Rules{Deny: []string{"*> /dev/*"}}, Rules{})
	if d, _ := p.CheckBash("echo x > /dev/sda"); d != DecisionDeny {
		t.Errorf("redirect to /dev should be denied, got %v", d)
	}
	if d, _ := p.CheckBash("echo x > /tmp/file"); d != DecisionAllow {
		t.Errorf("redirect to /tmp should be allowed, got %v", d)
	}
}

func TestCheckBash_AskAndAllowExemption(t *testing.T) {
	p := NewPolicy(Rules{
		Ask:   []string{"git *"},
		Allow: []string{"git status*", "git diff*"},
	}, Rules{})

	tests := []struct {
		cmd  string
		want Decision
	}{
		{"git status", DecisionAllow},      // exempted by allow
		{"git diff HEAD~1", DecisionAllow}, // exempted by allow
		{"git push", DecisionAsk},          // ask, no allow exemption
		{"git status && git push", DecisionAsk},
		{"git push && git status", DecisionAsk},
		{"ls -la", DecisionAllow}, // no rule at all
	}
	for _, tt := range tests {
		if d, _ := p.CheckBash(tt.cmd); d != tt.want {
			t.Errorf("CheckBash(%q) = %v, want %v", tt.cmd, d, tt.want)
		}
	}
}

func TestCheckBash_UnparseableIsAsk(t *testing.T) {
	p := NewPolicy(Rules{}, Rules{})
	d, reason := p.CheckBash("if [ ; then")
	if d != DecisionAsk {
		t.Errorf("unparseable command should be Ask, got %v", d)
	}
	if reason == "" {
		t.Error("expected a reason for unparseable command")
	}
}

func TestCheckBash_SessionExact(t *testing.T) {
	p := NewPolicy(Rules{Ask: []string{"git push*"}}, Rules{})

	// Before approval: ask.
	if d, _ := p.CheckBash("git push origin main"); d != DecisionAsk {
		t.Fatalf("expected Ask before approval")
	}

	// Approve the exact command.
	p.AllowExactSession("git push origin main")

	// Exact match (with different whitespace) now allowed.
	if d, _ := p.CheckBash("git  push   origin main"); d != DecisionAllow {
		t.Error("exact session approval should allow the same command")
	}

	// A different command matching the same ask rule still asks.
	if d, _ := p.CheckBash("git push origin dev"); d != DecisionAsk {
		t.Error("session approval must not widen to other commands")
	}
}

func TestCheckBash_ProjectUnion(t *testing.T) {
	global := Rules{Ask: []string{"git *"}, Allow: []string{"git diff*"}}
	project := Rules{Deny: []string{"git push*"}, Ask: []string{"npm *"}, Allow: []string{"git status*"}}
	p := NewPolicy(global, project)

	if d, _ := p.CheckBash("git push origin main"); d != DecisionDeny {
		t.Error("project deny should be effective")
	}
	if d, _ := p.CheckBash("git commit -m x"); d != DecisionAsk {
		t.Error("global ask should still apply")
	}
	if d, _ := p.CheckBash("npm install"); d != DecisionAsk {
		t.Error("project ask should tighten (union)")
	}
	if d, _ := p.CheckBash("git diff HEAD~1"); d != DecisionAllow {
		t.Error("global allow should exempt from global ask")
	}
	// Project-level allow is dropped: it must NOT exempt the global ask rule.
	if d, _ := p.CheckBash("git status"); d != DecisionAsk {
		t.Error("project allow must be ignored — git status should still hit global ask")
	}
}

func TestCheckBash_EmptyCommand(t *testing.T) {
	p := NewPolicy(Rules{Deny: []string{"*"}}, Rules{})
	if d, _ := p.CheckBash("   "); d != DecisionAllow {
		t.Error("empty command should be allowed (no-op)")
	}
}

func TestBuiltinDenyRules(t *testing.T) {
	if len(BuiltinDenyRules) == 0 {
		t.Fatal("BuiltinDenyRules should not be empty")
	}
	for _, r := range BuiltinDenyRules {
		if r == "" {
			t.Error("BuiltinDenyRules contains an empty pattern")
		}
	}

	p := NewPolicy(Rules{Deny: BuiltinDenyRules}, Rules{})

	denied := []string{
		"rm -rf /",
		"rm -rf / home", // "rm -rf /*" with space-normalized glob
		"rm -fr ~",
		"rm -rf $HOME",
		"rm -rf ${HOME}/docs",
		"sudo rm -rf /",      // hmm: sudo prefix — see below
		"rm -rf /etc",        // root-level directory (single component below /)
		"rm -rf /tmp",        // root-level directory itself is guarded
		"rm -rf \"/tmp\"",    // quoting does not bypass the guard
		"rm -rf /tmp/../etc", // traversal normalizes to /etc
		"rm -rf ~/docs",      // home direct child
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"dd of=/dev/nvme0n1",
		"mkfs.ext4 /dev/sda1",
		"wipefs -a /dev/sda",
		"echo x > /dev/sda",
		"cat img > /dev/nvme0n1",
		"shutdown -h now",
		"sudo reboot",
		"poweroff",
		"systemctl poweroff",
		"git status && rm -rf /", // compound evasion
		"echo $(dd of=/dev/sda)", // substitution evasion
	}
	for _, cmd := range denied {
		if d, _ := p.CheckBash(cmd); d != DecisionDeny {
			t.Errorf("CheckBash(%q) = %v, want Deny", cmd, d)
		}
	}

	allowed := []string{
		"git status",
		"rm -rf ./build", // relative cleanup is a user-policy decision
		"rm -rf node_modules",
		"rm -rf /tmp/xxx/*", // deeper absolute paths are user-policy decisions
		"rm -rf /var/log/*",
		"rm -rf /tmp/x",
		"rm -rf ~/.cache/session", // deeper than ~/X — not builtin-guarded
		"git push --force",        // deliberately not builtin
		"dd if=x of=/dev/null",    // /dev/null must not be blocked
		"echo x > /dev/null",
		"ls /dev/sda", // reading is fine
	}
	for _, cmd := range allowed {
		if d, _ := p.CheckBash(cmd); d != DecisionAllow {
			t.Errorf("CheckBash(%q) = %v, want Allow", cmd, d)
		}
	}
}

// TestBuiltinRmGuard_Precision verifies the built-in rm guard's exact
// boundary: root / root-level directories / home are denied, but deeper
// paths (the classic "rm -rf /tmp/xxx/*" cleanup) fall through to user
// policy. This is the regression test for the old glob "rm -rf /*" that
// matched every absolute-path rm -rf because '*' spans '/' in wildcardMatch.
func TestBuiltinRmGuard_Precision(t *testing.T) {
	p := NewPolicy(Rules{}, Rules{}) // no user rules — guard alone decides

	denied := []string{
		"rm -rf /",
		"rm -rf /*",
		"rm -rf / *",
		"rm -fr /",
		"rm -rf /etc",
		"rm -rf /tmp",
		"rm -rf /home",
		"rm -rf /./etc",      // normalized to /etc
		"rm -rf /tmp/../etc", // normalized to /etc
		"rm -rf ~",
		"rm -rf ~/*",
		"rm -rf ~/docs",
		"rm -rf $HOME",
		"rm -rf $HOME/*",
		"rm -rf ${HOME}",
		"rm -rf ${HOME}/docs",
		"rm -rf \"$HOME\"",
		"sudo rm -rf /etc",
		"git status && rm -rf /usr", // compound: still caught
	}
	for _, cmd := range denied {
		if d, _ := p.CheckBash(cmd); d != DecisionDeny {
			t.Errorf("CheckBash(%q) = %v, want Deny", cmd, d)
		}
	}

	allowed := []string{
		"rm -rf /tmp/xxx/*", // the original false positive — now allowed
		"rm -rf /var/log/*",
		"rm -rf /tmp/x",
		"rm -rf /tmp/x/y",
		"rm -rf /data/project/build",
		"rm -rf ~/.cache/session",
		"rm -rf ~/docs/cache",
		"rm -rf ./build",
		"rm -rf node_modules",
		"rm -rf foo",
		"rm /etc/hosts", // file under a root-level dir is fine
		"rm -rf -- /tmp/cache",
	}
	for _, cmd := range allowed {
		if d, _ := p.CheckBash(cmd); d != DecisionAllow {
			t.Errorf("CheckBash(%q) = %v, want Allow", cmd, d)
		}
	}
}

// TestBuiltinRmGuard_Disabled verifies NewPolicyNoBuiltins disables the
// structured rm guard (the disable_builtin_deny path).
func TestBuiltinRmGuard_Disabled(t *testing.T) {
	p := NewPolicyNoBuiltins(Rules{}, Rules{})
	for _, cmd := range []string{"rm -rf /", "rm -rf /etc", "rm -rf ~"} {
		if d, _ := p.CheckBash(cmd); d != DecisionAllow {
			t.Errorf("CheckBash(%q) with builtins off = %v, want Allow", cmd, d)
		}
	}
}

// TestBuiltinRmGuard_UserRulePriority verifies a user deny rule is reported
// ahead of the built-in guard when both match (deny rules run first).
func TestBuiltinRmGuard_UserRulePriority(t *testing.T) {
	p := NewPolicy(Rules{Deny: []string{"rm -rf /etc"}}, Rules{})
	d, rule := p.CheckBash("rm -rf /etc")
	if d != DecisionDeny {
		t.Fatalf("CheckBash = %v, want Deny", d)
	}
	if rule != "rm -rf /etc" {
		t.Errorf("matched rule = %q, want user rule %q", rule, "rm -rf /etc")
	}

	// Guard still catches targets the user did not list.
	d, rule = p.CheckBash("rm -rf /usr")
	if d != DecisionDeny || rule != "rm -rf /usr (builtin)" {
		t.Errorf("guard fallback: got (%v, %q), want (Deny, %q)", d, rule, "rm -rf /usr (builtin)")
	}
}
