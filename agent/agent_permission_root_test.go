package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// SetPermissionTrees is how a frontend that hosts several sessions in different
// trees gets the RIGHT projects' rules.
//
// The default build in configure resolves the project from the PROCESS working
// directory. For a GUI app that directory is "/" (macOS hands a Finder-launched
// app the filesystem root), so the file it looked for was
// /.tachi/permissions.yaml — a project's own deny/ask rules were silently
// absent, and only the global half of the policy was ever in force. The
// frontend that knows which tree a conversation is in re-points it.
//
// Two properties have to hold together, and they pull in opposite directions:
// the RULES must come from the new tree, while the session's APPROVALS
// (「本会话全部允许」) must survive the rebuild — re-asking a question the user
// already waved through is a regression they cannot see coming.

// writeProjectPermission drops a .tachi/permissions.yaml under root.
func writeProjectPermission(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".tachi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write permissions.yaml: %v", err)
	}
}

// newPermissionTestAgent builds an agent whose FullConfig has no global rules
// at all, so every decision observed below comes from a project file.
func newPermissionTestAgent(t *testing.T) *AIAgent {
	t.Helper()
	full := config.DefaultConfig()
	// Global builtin deny stays on (it is not what this test is about), but no
	// user global rules, so a rule that fires is unambiguously project-derived.
	full.Permissions.Bash.Deny = nil
	full.Permissions.Bash.Ask = nil
	full.Permissions.Bash.Allow = nil
	// The constructor resolves a main provider; a bare stub is enough because
	// nothing here reaches the LLM.
	full.Providers = []config.ProviderConfig{
		{Name: "p", Type: "openai", Model: "m", BaseURL: "https://api.test/v1", APIKey: "k"},
	}

	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: &mockStreamProvider{name: "m", providerName: "p"}},
		Logger:        logger.Default(),
		FullConfig:    full,
		SkipConfigure: true, // no tools/reminders needed — this is a policy test
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	return a
}

// The rule that a tree's own permissions.yaml is read — the whole point of the
// method. Two different trees, each with its own rule, and each checked.
func TestSetPermissionRoot_UsesThatTreesProjectRules(t *testing.T) {
	a := newPermissionTestAgent(t)

	treeA := t.TempDir()
	treeB := t.TempDir()
	writeProjectPermission(t, treeA, "permissions:\n  bash:\n    deny: [\"terraform destroy*\"]\n")
	writeProjectPermission(t, treeB, "permissions:\n  bash:\n    deny: [\"kubectl delete*\"]\n")

	// Point at tree A: its rule fires, tree B's does not.
	a.SetPermissionTrees(context.Background(), treeA, nil)
	if d, _ := a.PermissionPolicy().CheckBash("terraform destroy -auto-approve"); d != permission.DecisionDeny {
		t.Errorf("tree A deny rule did not fire: decision = %v, want deny", d)
	}
	if d, _ := a.PermissionPolicy().CheckBash("kubectl delete pod x"); d == permission.DecisionDeny {
		t.Error("tree B's rule fired while the session was in tree A")
	}

	// Point at tree B: now the other way round. This is the case the process cwd
	// can never express — one process, two trees.
	a.SetPermissionTrees(context.Background(), treeB, nil)
	if d, _ := a.PermissionPolicy().CheckBash("kubectl delete pod x"); d != permission.DecisionDeny {
		t.Errorf("tree B deny rule did not fire after re-point: decision = %v, want deny", d)
	}
	if d, _ := a.PermissionPolicy().CheckBash("terraform destroy -auto-approve"); d == permission.DecisionDeny {
		t.Error("tree A's rule still fires after the session moved to tree B")
	}
}

// A session-wide approval must survive a rebuild: the user answered
// 「本会话全部允许」 and a folder change is not an answer to that question.
func TestSetPermissionTrees_KeepsSessionApprovals(t *testing.T) {
	a := newPermissionTestAgent(t)

	tree := t.TempDir()
	writeProjectPermission(t, tree, "permissions:\n  bash:\n    ask: [\"npm publish*\"]\n")

	a.SetPermissionTrees(context.Background(), tree, nil)
	if d, _ := a.PermissionPolicy().CheckBash("npm publish"); d != permission.DecisionAsk {
		t.Fatalf("setup: decision = %v, want ask (the rules must be live before the approval)", d)
	}

	// The user answers the card with 「本会话全部允许」.
	a.PermissionPolicy().AllowAllAsksForSession()
	if d, _ := a.PermissionPolicy().CheckBash("npm publish"); d != permission.DecisionAllow {
		t.Fatalf("setup: decision = %v, want allow after AllowAllAsksForSession", d)
	}

	// The session moves (or the frontend merely re-resolves the same tree).
	a.SetPermissionTrees(context.Background(), tree, nil)

	if d, _ := a.PermissionPolicy().CheckBash("npm publish"); d != permission.DecisionAllow {
		t.Error("the session-wide approval was lost by the rebuild — the user is asked again " +
			"for a command they already allowed for this conversation")
	}
}

// The approval must not become a way to run what the NEW tree forbids: it
// covers asks only. CheckBash decides deny before any ask is reached, so an
// inherited approval can never resurrect a forbidden command.
func TestSetPermissionTrees_ApprovalDoesNotBypassNewTreesDeny(t *testing.T) {
	a := newPermissionTestAgent(t)

	writeProjectPermission(t, t.TempDir(), "permissions:\n  bash:\n    ask: [\"npm *\"]\n")
	a.SetPermissionTrees(context.Background(), t.TempDir(), nil)
	a.PermissionPolicy().AllowAllAsksForSession()

	forbidding := t.TempDir()
	writeProjectPermission(t, forbidding, "permissions:\n  bash:\n    deny: [\"rm -rf /tmp/data*\"]\n")
	a.SetPermissionTrees(context.Background(), forbidding, nil)

	if d, _ := a.PermissionPolicy().CheckBash("rm -rf /tmp/data"); d != permission.DecisionDeny {
		t.Errorf("decision = %v, want deny — an inherited ask-approval must never outrank the "+
			"new tree's deny rule", d)
	}
}

// A rule-free project still has to leave the GLOBAL half intact: re-pointing
// must not drop back to "everything allowed".
func TestSetPermissionTrees_KeepsGlobalRules(t *testing.T) {
	a := newPermissionTestAgent(t)
	a.Config.FullConfig.Permissions.Bash.Deny = []string{"git push --force*"}

	a.SetPermissionTrees(context.Background(), t.TempDir(), nil) // project with no file at all

	if d, _ := a.PermissionPolicy().CheckBash("git push --force origin main"); d != permission.DecisionDeny {
		t.Errorf("decision = %v, want deny — the global rules must survive a re-point", d)
	}
}

// An empty directory means "no project": global rules alone, and no crash on
// the nil that FindProjectRootFrom's empty input can produce.
func TestSetPermissionTrees_EmptyDirIsGlobalOnly(t *testing.T) {
	a := newPermissionTestAgent(t)
	a.Config.FullConfig.Permissions.Bash.Ask = []string{"git *"}

	a.SetPermissionTrees(context.Background(), "", nil)

	p := a.PermissionPolicy()
	if p == nil {
		t.Fatal("policy is nil after SetPermissionTrees(\"\") — global rules were dropped")
	}
	if d, _ := p.CheckBash("git status"); d != permission.DecisionAsk {
		t.Errorf("decision = %v, want ask (global rule)", d)
	}
}

// A bare agent (no FullConfig) has nothing to build from; the call must be a
// no-op rather than a panic — it is reached from frontends that cannot know
// whether their bootstrap succeeded.
func TestSetPermissionTrees_NoFullConfigIsNoop(t *testing.T) {
	a := newPermissionTestAgent(t)
	a.Config.FullConfig = nil
	a.SetPermissionPolicy(nil)

	a.SetPermissionTrees(context.Background(), t.TempDir(), nil) // must not panic

	if a.PermissionPolicy() != nil {
		t.Error("a policy was installed despite there being no FullConfig to build it from")
	}
}

// The gap this method now closes: a session spanning SEVERAL trees has to honour
// every tree's tripwires. With only the primary's file read, a deny rule the
// user wrote in the SECOND repository was silently absent — the rules that
// fired were the ones they thought they had, minus that one.
func TestSetPermissionTrees_EveryTreeContributes(t *testing.T) {
	a := newPermissionTestAgent(t)

	primary := t.TempDir()
	extra := t.TempDir()
	writeProjectPermission(t, primary, "permissions:\n  bash:\n    deny: [\"terraform destroy*\"]\n")
	writeProjectPermission(t, extra, "permissions:\n  bash:\n    deny: [\"kubectl delete*\"]\n")

	a.SetPermissionTrees(context.Background(), primary, []string{extra})

	for _, cmd := range []string{"terraform destroy -auto-approve", "kubectl delete pod x"} {
		if d, _ := a.PermissionPolicy().CheckBash(cmd); d != permission.DecisionDeny {
			t.Errorf("%q -> decision = %v, want deny: both trees' rules must be in force", cmd, d)
		}
	}
	if got := a.Config.PermissionTrees; len(got) != 2 {
		t.Errorf("PermissionTrees = %v, want both trees recorded", got)
	}
}

// A tree that resolves onto another (a root INSIDE the primary's own
// repository, which is what a project's second root usually is) is read once —
// and the resolution happens per tree, so a subdirectory contributes its
// repository's file rather than a file of its own that cannot exist.
func TestSetPermissionTrees_DeduplicatesResolvedTrees(t *testing.T) {
	a := newPermissionTestAgent(t)

	repo := t.TempDir()
	// A .git directory is what makes repo the project root of its own subdirs
	// (FindProjectRootFrom walks up to it) — without one every directory is its
	// own root and there is nothing to de-duplicate.
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeProjectPermission(t, repo, "permissions:\n  bash:\n    ask: [\"npm publish*\"]\n")

	a.SetPermissionTrees(context.Background(), repo, []string{filepath.Join(repo, "src"), filepath.Join(repo, "docs")})

	got := a.Config.PermissionTrees
	if len(got) != 1 || got[0] != repo {
		t.Fatalf("PermissionTrees = %v, want exactly [%s]", got, repo)
	}
	if d, _ := a.PermissionPolicy().CheckBash("npm publish"); d != permission.DecisionAsk {
		t.Errorf("decision = %v, want ask — the de-duplicated tree's rules must still apply", d)
	}
}
