package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// The bash permission policy's project half is read from
// <tree>/.tachi/permissions.yaml, and the agent-side builder resolves that tree
// from the PROCESS cwd unless it is told otherwise — for a Finder-launched app
// that cwd is "/", so a project's own deny/ask rules were silently absent while
// only the global half stayed in force.
//
// These tests pin the two places desktop has to tell it otherwise: when the
// session's agent is built, and when the session MOVES to another tree (the
// same re-point the skill store gets, which is why they share one flag).

// writePermissionsFixture drops a project permission file under tree.
func writePermissionsFixture(t *testing.T, tree, content string) {
	t.Helper()
	dir := filepath.Join(tree, ".tachi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write permissions.yaml: %v", err)
	}
}

// permissionTestAgent is the state buildAgentForSession leaves behind: a live
// agent carrying the desktop's FullConfig, re-pointed at the session's tree.
// A bare agent suffices — policy evaluation needs no tools or provider.
func permissionTestAgent(t *testing.T, tree string) *agent.AIAgent {
	t.Helper()
	full := config.DefaultConfig()
	full.Permissions.Bash.Deny = nil
	full.Permissions.Bash.Ask = nil
	full.Permissions.Bash.Allow = nil
	full.Providers = []config.ProviderConfig{
		{Name: "p", Type: "openai", Model: "m", BaseURL: "https://api.test/v1", APIKey: "k"},
	}
	// A real provider instance (no network is touched): the constructor wants a
	// main provider, and a hand-rolled stub would only duplicate llm's interface.
	prov, err := llm.NewProvider("openai", "k", "https://api.test/v1", "m")
	if err != nil {
		t.Fatalf("llm.NewProvider: %v", err)
	}
	a, _, err := agent.NewAIAgentWithConfig(t.Context(), agent.AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: prov},
		Logger:        logger.Default(),
		FullConfig:    full,
		ToolRegistry:  tools.NewRegistry(),
		SkipConfigure: true, // no tools/reminders needed — this is a policy test
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	a.SetPermissionTrees(t.Context(), tree, nil)
	return a
}

// TestSessionTreeMoveRepointsPermissions covers the move: a session that
// changes folder must be judged by the NEW tree's rules, not the one it left.
//
// The move only MARKS the session (sessionRun.treeStale) — the caller is the UI
// goroutine, which cannot prove this session is not mid-turn. The turn's own
// start applies it. Same flag, same place and same reason as the skill store:
// they are both derived from the session's tree and must never be able to
// describe two different ones.
func TestSessionTreeMoveRepointsPermissions(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writePermissionsFixture(t, treeA, "permissions:\n  bash:\n    deny: [\"terraform destroy*\"]\n")
	writePermissionsFixture(t, treeB, "permissions:\n  bash:\n    deny: [\"kubectl delete*\"]\n")

	d, svc, sid := newRootsApp(t, treeA)
	a := permissionTestAgent(t, treeA)
	if d, _ := a.PermissionPolicy().CheckBash("terraform destroy -auto-approve"); d != permission.DecisionDeny {
		t.Fatalf("fixture: tree A's rule is not in force, decision = %v", d)
	}
	r := d.getRun(sid)
	r.agent = a

	if res := svc.SetSessionWorkingDir(sid, treeB); res != "ok" {
		t.Fatalf("SetSessionWorkingDir: %s", res)
	}

	// Nothing happens yet, on purpose: a live agent's policy is not rewritten
	// from the UI goroutine while a turn of that session may be reading it.
	if d, _ := a.PermissionPolicy().CheckBash("kubectl delete pod x"); d == permission.DecisionDeny {
		t.Error("SetSessionWorkingDir must not rewrite a live agent's permission policy")
	}
	if !r.treeStale {
		t.Fatal("the session must be marked as needing a tree re-point")
	}

	_, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)

	if d, _ := a.PermissionPolicy().CheckBash("kubectl delete pod x"); d != permission.DecisionDeny {
		t.Errorf("the policy must follow the session to its new tree, decision = %v, want deny", d)
	}
	if d, _ := a.PermissionPolicy().CheckBash("terraform destroy -auto-approve"); d == permission.DecisionDeny {
		t.Error("the old tree's rule still fires after the session moved")
	}
}

// The skill store and the permission policy are re-pointed by ONE flag, so a
// tree move must leave both on the new tree. This is the property that a second
// parallel "stale" flag would quietly break.
func TestSessionTreeMoveRepointsSkillsAndPermissionsTogether(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writeProjectSkillFixture(t, treeA, "skill-a", "from tree A")
	writeProjectSkillFixture(t, treeB, "skill-b", "from tree B")
	writePermissionsFixture(t, treeB, "permissions:\n  bash:\n    deny: [\"kubectl delete*\"]\n")

	d, svc, sid := newRootsApp(t, treeA)
	a := permissionTestAgent(t, treeA)
	a.ReloadSkillsIn(treeA)
	r := d.getRun(sid)
	r.agent = a

	if res := svc.SetSessionWorkingDir(sid, treeB); res != "ok" {
		t.Fatalf("SetSessionWorkingDir: %s", res)
	}
	_, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)

	names := storeSkillNames(a.SkillStore())
	if !slices.Contains(names, "skill-b") || slices.Contains(names, "skill-a") {
		t.Errorf("the skill store did not follow the move: %v", names)
	}
	if d, _ := a.PermissionPolicy().CheckBash("kubectl delete pod x"); d != permission.DecisionDeny {
		t.Errorf("the permission policy did not follow the move (decision = %v) — the two "+
			"tree-derived members must move together", d)
	}
}
