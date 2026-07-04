package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// --- Provider selection ---
//
// Provider selection provides a UI overlay (activated by /model) that lists
// all configured providers, allowing the user to switch between them.
// Each entry shows the provider name, type, model, and an active marker (*)
// on the currently selected provider. The selection is live — switching
// immediately changes the agent's LLM provider.

func (m *Model) handleKeySelectingModel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+k", "ctrl+p":
		if m.providerSelIdx > 0 {
			m.providerSelIdx--
		}
	case "down", "ctrl+j", "ctrl+n":
		if m.providerSelIdx < len(m.providerItems)-1 {
			m.providerSelIdx++
		}
	case "enter":
		m.switchToProvider(m.providerSelIdx)
	case "esc":
		m.exitModelSelect("")
	}
	return m, nil
}

func (m *Model) renderProviderSelection() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("Select provider (↑↓ Enter Esc)") + "\n")

	currentInfo := m.statusbar.ProviderInfo()
	maxNameLen := 0
	for _, p := range m.providerItems {
		if len(p.Name) > maxNameLen {
			maxNameLen = len(p.Name)
		}
	}
	for idx, p := range m.providerItems {
		info := fmt.Sprintf("%s (%s)", p.Type, p.Model)
		active := " "
		if info == currentInfo {
			active = "*"
		}
		line := fmt.Sprintf(" %s %-*s  %s", active, maxNameLen, p.Name, info)
		if idx == m.providerSelIdx {
			b.WriteString(completionSelectedStyle.Width(m.width).Render(line))
		} else {
			b.WriteString(completionNormalStyle.Width(m.width).Render(line))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) exitModelSelect(msg string) {
	if msg != "" {
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
	}
	m.providerItems = nil
	m.providerSelIdx = 0
	m.setState(stateIdle)
	m.layout()
}

func (m *Model) switchToProvider(idx int) {
	pCfg := &m.providerItems[idx]
	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return
	}
	provider, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return
	}
	m.agent.SetProvider(provider)
	providerInfo := fmt.Sprintf("%s (%s)", resolved.Type, resolved.Model)
	m.statusbar.SetProviderInfo(providerInfo)
	m.statusbar.SetContextWindow(resolved.ContextWindow)
	m.refreshSessionCost()
	m.exitModelSelect(fmt.Sprintf("Switched to %s", providerInfo))
}