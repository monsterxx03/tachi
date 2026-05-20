package agent

import (
	"fmt"
	"os"

	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
)

// SkillStore returns the agent's skill store, or nil if skills are not configured.
func (a *AIAgent) SkillStore() *skill.Store {
	return a.skillStore
}

// ActivateSkill injects a skill's instruction as a user message and marks it
// active. userInstruction is optional extra text (e.g. "main.go" from
// "/code-review main.go"). Returns the constructed message string and any error.
func (a *AIAgent) ActivateSkill(name string, userInstruction string) (string, error) {
	if a.skillStore == nil {
		return "", fmt.Errorf("skill store not initialized")
	}
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]bool)
	}

	sk, err := a.skillStore.Load(name)
	if err != nil {
		return "", err
	}

	a.activeSkills[sk.Meta.Name] = true

	return skill.BuildActivationMessage(sk, userInstruction), nil
}

// IsSkillActive returns whether a skill has already been activated in the
// current session.
func (a *AIAgent) IsSkillActive(name string) bool {
	if a.activeSkills == nil {
		return false
	}
	return a.activeSkills[name]
}

// ReloadSkills re-creates the skill store to pick up new or modified skill
// definitions from the filesystem, then re-registers skill tools and rebuilds
// the reminder collector so all references point to the new store.
func (a *AIAgent) ReloadSkills() {
	wd, _ := os.Getwd()
	// Unregister old skill tools before creating new store.
	a.unregisterSkillTools()
	a.skillStore = skill.NewStore(wd)
	a.skillStore.SetLogger(a.logger)
	a.activeSkills = make(map[string]bool)
	a.registerSkillTools()
	a.rebuildSkillCollector()
}

// initSkills initializes the skill store, registers skill tools, and adds the
// SkillListReminder to the collector. Called from Configure.
func (a *AIAgent) initSkills() {
	wd, _ := os.Getwd()
	a.skillStore = skill.NewStore(wd)
	a.skillStore.SetLogger(a.logger)
	a.activeSkills = make(map[string]bool)
	a.registerSkillTools()
	a.rebuildSkillCollector()
}

// registerSkillTools registers the skill tool backed by the current skillStore.
func (a *AIAgent) registerSkillTools() {
	a.RegisterTool(tools.NewSkillTool(a.skillStore))
}

// unregisterSkillTools removes the skill tool from the agent's registry.
func (a *AIAgent) unregisterSkillTools() {
	a.UnregisterTool(tools.ToolNameSkill)
}

// rebuildSkillCollector constructs a new collector from baseReminders plus
// a fresh SkillListReminder pointing at the current skillStore, plus
// a MemoryRecallReminder if memory is configured.
func (a *AIAgent) rebuildSkillCollector() {
	all := make([]systemreminder.Reminder, 0, len(a.baseReminders)+2)
	all = append(all, a.baseReminders...)
	all = append(all, systemreminder.NewSkillListReminder(a.skillStore))

	// Add MemoryRecallReminder if memory backend is configured
	if a.memoryBackend != nil {
		all = append(all, systemreminder.MemoryRecallReminder{
			Backend: a.memoryBackend,
			Limit:   5,
		})
	}

	a.reminderCollector = systemreminder.NewCollector(all...)
	a.reminderCollector.SetLogger(a.logger)
}
