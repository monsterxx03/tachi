package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// writeTestSkill drops one valid skill at <tree>/.tachi/skills/<name>/SKILL.md.
func writeTestSkill(t *testing.T, tree, name string) {
	t.Helper()
	dir := filepath.Join(tree, ".tachi", "skills", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	md := "---\nname: " + name + "\ndescription: fixture skill " + name + "\n---\n\nbody of " + name + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644))
}

// skillNames lists the names the agent's store currently offers.
func skillNames(a *AIAgent) []string {
	var names []string
	for _, m := range a.SkillStore().List() {
		names = append(names, m.Name)
	}
	return names
}

// TestReloadSkillsInFollowsTheGivenTree pins what ReloadSkillsIn adds over
// ReloadSkills: a store's scan roots are fixed when it is built, so only the former
// can point the agent at the tree its caller works in — which is the ONLY way a host
// running several sessions in different trees (the desktop) offers each session its
// own project skills. ReloadSkills would resolve them from the process cwd.
//
// The agent starts with skills OFF, so this also pins that turning them on later
// registers the Skill tool.
func TestReloadSkillsInFollowsTheGivenTree(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writeTestSkill(t, treeA, "skill-a")
	writeTestSkill(t, treeB, "skill-b")

	a := newBareTestAgent(t, &mockStreamProvider{name: "mock"}, 5)
	defer a.Close()
	// The store logs through the agent's logger, and an injected store keeps
	// whatever the agent carries — so it must be a real one.
	a.Config.Logger = logger.Default()

	require.Nil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameSkill), "fixture: skills start off")

	a.ReloadSkillsIn(treeA)

	assert.Equal(t, filepath.Join(treeA, ".tachi", "skills"), a.SkillStore().Dirs()[0],
		"the store must scan the given tree's project skills first")
	assert.Contains(t, skillNames(a), "skill-a")
	assert.NotContains(t, skillNames(a), "skill-b", "another tree's skills must not leak in")
	require.NotNil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameSkill),
		"the Skill tool must be registered by the same call")

	// Moving the agent to another tree re-points it — both ways, so a stale store
	// cannot survive by accident.
	a.ReloadSkillsIn(treeB)

	assert.Equal(t, filepath.Join(treeB, ".tachi", "skills"), a.SkillStore().Dirs()[0])
	assert.Contains(t, skillNames(a), "skill-b")
	assert.NotContains(t, skillNames(a), "skill-a")
}

// TestReloadSkillsInWithoutDirectory covers a session that never picked a folder: the
// scan scope is the global skills alone (skill.NewStore("")), and it must not fall back
// to the process cwd — a Finder-launched desktop's cwd is "/", so a fallback would
// advertise the filesystem root as the project.
func TestReloadSkillsInWithoutDirectory(t *testing.T) {
	a := newBareTestAgent(t, &mockStreamProvider{name: "mock"}, 5)
	defer a.Close()
	a.Config.Logger = logger.Default()

	a.ReloadSkillsIn("")

	assert.Equal(t, []string{config.GlobalSkillsDir()}, a.SkillStore().Dirs(),
		"an unset working directory must mean the global scope only, never the process cwd")
}
