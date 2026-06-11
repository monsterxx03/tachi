package agent

import (
	"fmt"

	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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
// definitions from the filesystem, then re-registers skill tools and updates
// the SkillListReminder in-place so the collector reflects the new store.
func (a *AIAgent) ReloadSkills() {
	a.unregisterSkillTools()
	a.initSkills()
	a.skillListReminder.SetProvider(a.skillStore)
}

// initSkills initializes (or re-initializes) the skill store and registers
// skill tools. The SkillListReminder is created on the first call and
// reused thereafter — ReloadSkills calls SetProvider to update it.
func (a *AIAgent) initSkills() {
	a.skillStore = skill.NewStore(config.FindProjectRoot())
	a.skillStore.SetLogger(a.logger)
	a.activeSkills = make(map[string]bool)
	a.registerSkillTools()
	if a.skillListReminder == nil {
		a.skillListReminder = systemreminder.NewSkillListReminder(a.skillStore)
	}
}

// registerSkillTools registers the skill tool backed by the current skillStore.
func (a *AIAgent) registerSkillTools() {
	a.RegisterTool(tools.NewSkillTool(a.skillStore))
}

// unregisterSkillTools removes the skill tool from the agent's registry.
func (a *AIAgent) unregisterSkillTools() {
	a.UnregisterTool(tools.ToolNameSkill)
}
