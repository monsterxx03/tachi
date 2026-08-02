package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/skill"
)

func (m *Model) handleSkillCommand() tea.Cmd {
	store := m.agent.SkillStore()
	if store == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Skill system not available",
		})
		return nil
	}

	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "", "list":
		metas := store.List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: cmds.FormatSkillList(metas),
		})
		return nil

	case "reload":
		// Re-create the store to pick up new/modified skills
		m.agent.ReloadSkills()
		m.refreshSkillCompletions()
		metas := m.agent.SkillStore().List()
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Skills reloaded — %d skill(s) found", len(metas)),
		})
		return nil

	default:
		// /skill <name> [args] — activate a specific skill
		extraArgs := ""
		if len(parts) > 2 {
			extraArgs = strings.Join(parts[2:], " ")
		}
		return m.sendSkillMessage(sub, extraArgs)
	}
}

// sendSkillMessage activates a skill and sends its instructions as a user message.
// skillName is the skill to activate. extraArgs are additional text from the
// command line (e.g., "main.go" from "/code-review main.go").
// If the skill is already active in this session, only a short directive
// message is injected (the full skill body is already in context).
func (m *Model) sendSkillMessage(skillName string, extraArgs string) tea.Cmd {
	var msg string
	if m.agent.IsSkillActive(skillName) {
		// Skill body already in conversation context — send directive only.
		msg = skill.BuildDirectiveMessage(skillName, extraArgs)
	} else {
		var err error
		msg, err = m.agent.ActivateSkill(skillName, extraArgs)
		if err != nil {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("Skill **%s** not found. Use `/skill` to see available skills.", skillName),
			})
			return nil
		}
	}

	// Add the activation message as a system-style user message
	m.chatview.AddMessage(chatMessage{
		Role:    "user",
		Content: fmt.Sprintf("/%s %s", skillName, extraArgs),
	})

	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx := m.startTurn()

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, msg, m.systemPrompt, m.chatOpts)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}
