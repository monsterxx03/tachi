package tui

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// --- Session selection ---
//
// Session selection provides a UI overlay (activated by /sessions) that lists
// all saved sessions, allowing the user to switch between them. Each session
// entry shows the date, title, provider/model, and an active marker (*).
// Selecting a session ends the current one, loads the selected session's
// messages, and restores the original provider/model if available.

func (m *Model) sessionVisibleRows() int {
	// Calculate visible rows (excluding the title line)
	// This matches layout(): inputHeight = min(len+2, height/2), minus 1 for title
	n := min(max(m.height/2-1, 1), len(m.sessionList))
	return n
}

func (m *Model) clampSessionScroll() {
	visibleRows := m.sessionVisibleRows()
	// Ensure scroll offset is within valid range
	maxScroll := max(len(m.sessionList)-visibleRows, 0)
	if m.sessionScrollOff > maxScroll {
		m.sessionScrollOff = maxScroll
	}
	if m.sessionScrollOff < 0 {
		m.sessionScrollOff = 0
	}
	// Ensure the selected index is visible after clamping
	if m.sessionSelIdx < m.sessionScrollOff {
		m.sessionScrollOff = m.sessionSelIdx
	}
	if m.sessionSelIdx >= m.sessionScrollOff+visibleRows {
		m.sessionScrollOff = m.sessionSelIdx - visibleRows + 1
	}
}

func (m *Model) handleKeySelectingSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+k", "ctrl+p":
		if m.sessionSelIdx > 0 {
			m.sessionSelIdx--
		}
		m.clampSessionScroll()
	case "down", "ctrl+j", "ctrl+n":
		if m.sessionSelIdx < len(m.sessionList)-1 {
			m.sessionSelIdx++
		}
		m.clampSessionScroll()
	case "enter":
		if m.sessionSelIdx >= 0 && m.sessionSelIdx < len(m.sessionList) {
			return m.loadSession(m.sessionSelIdx)
		}
	case "esc":
		m.exitSessionSelect("")
	}
	return m, nil
}

func (m *Model) renderSessionSelection() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("Sessions (↑↓ Enter Esc)"))
	b.WriteString("\n")

	currentID := ""
	if m.agent.SessionManager() != nil {
		if curr := m.agent.SessionManager().Current(); curr != nil {
			currentID = curr.ID
		}
	}

	visibleRows := m.sessionVisibleRows()
	end := min(m.sessionScrollOff+visibleRows, len(m.sessionList))

	for idx := m.sessionScrollOff; idx < end; idx++ {
		s := m.sessionList[idx]
		dateStr := s.CreatedAt.Format("2006-01-02 15:04")
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		// Truncate title for display alignment (rune-aware)
		displayTitle := title
		titleRunes := []rune(displayTitle)
		if len(titleRunes) > 40 {
			displayTitle = string(titleRunes[:37]) + "…"
		}
		modelInfo := fmt.Sprintf("%s", s.ProviderName)

		active := " "
		if s.ID == currentID {
			active = "*"
		}

		line := fmt.Sprintf(" %s %s  %-40s  %s", active, dateStr, displayTitle, modelInfo)
		if idx == m.sessionSelIdx {
			b.WriteString(completionSelectedStyle.Width(m.width).Render(line))
		} else {
			b.WriteString(completionNormalStyle.Width(m.width).Render(line))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) exitSessionSelect(msg string) {
	if msg != "" {
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
	}
	m.sessionList = nil
	m.sessionSelIdx = 0
	m.sessionScrollOff = 0
	m.setState(stateIdle)
	m.layout()
}

// loadSession loads the session at the given index from the session list.
// If it's the current session, shows a message and exits. Otherwise, ends
// the current session, loads the selected one, and reloads chat history.
func (m *Model) loadSession(idx int) (tea.Model, tea.Cmd) {
	sm := m.agent.SessionManager()
	if sm == nil {
		m.exitSessionSelect("No session manager available")
		return m, nil
	}

	s := m.sessionList[idx]
	current := sm.Current()

	// If selecting the current session, just exit
	if current != nil && current.ID == s.ID {
		m.exitSessionSelect(fmt.Sprintf("Already viewing session: **%s**", s.Title))
		return m, nil
	}

	// End current session (don't delete, just end tracking)
	sm.EndCurrent()

	// Load the selected session
	if _, err := sm.Load(s.ID); err != nil {
		m.exitSessionSelect(fmt.Sprintf("Failed to load session: %v", err))
		return m, nil
	}

	// Restore working directory if recorded
	if s.WorkingDir != "" {
		if err := os.Chdir(s.WorkingDir); err != nil {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("⚠ Failed to change to session's working directory %s: %v", s.WorkingDir, err),
			})
		}
	}

	m.syncSessionInfo()

	// Load messages and convert to LLM format
	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		m.exitSessionSelect(fmt.Sprintf("Failed to load messages: %v", err))
		return m, nil
	}

	// Rebuild usage and cost from stored messages
	m.rebuildTotalUsage(sessionMsgs)
	m.refreshSessionCost()

	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, s.ProviderName, m.cfg)
	if err != nil {
		m.exitSessionSelect(fmt.Sprintf("Failed to convert session: %v", err))
		return m, nil
	}

	// Prepend system prompt if available
	if m.systemPrompt != "" {
		llmMsgs = append([]llm.Message{{Role: "system", Content: m.systemPrompt}}, llmMsgs...)
	}

	// Update model state
	m.history = llmMsgs
	m.chatview.Clear()
	m.chatview.LoadHistory(sessionMsgs)

	// Rebuild provider to match the session's original provider/model.
	providerInfo, providerRestored := m.restoreSessionProvider(s.ProviderName)
	m.statusbar.SetProviderInfo(providerInfo)
	if providerRestored {
		if cw := llm.ModelContextWindow(m.agent.Model()); cw > 0 {
			m.statusbar.SetContextWindow(cw)
		}
	}

	title := s.Title
	if title == "" {
		title = s.ID
	}
	msg := fmt.Sprintf("Switched to session: **%s**", title)
	if !providerRestored {
		msg += fmt.Sprintf("\n⚠ Provider %s not found in config — using current provider. Messages may not be compatible.", s.ProviderName)
	}
	m.exitSessionSelect(msg)
	return m, nil
}

// rebuildTotalUsage reconstructs the cumulative totalUsage from session messages.
// InputTokens is summed across all messages (cumulative input + cost basis).
// LastInputTokens prefers the local estimate (EstimatedInputTokens) so the
// statusbar context % matches what was shown during the active conversation.
// Falls back to the API-returned InputTokens for sessions saved before this field existed.
func (m *Model) rebuildTotalUsage(msgs []session.Message) {
	m.totalUsage = llm.Usage{}

	var lastInput int64
	for _, msg := range msgs {
		if msg.Usage != nil {
			if msg.Usage.EstimatedInputTokens > 0 {
				lastInput = msg.Usage.EstimatedInputTokens
			} else if msg.Usage.InputTokens > 0 {
				lastInput = msg.Usage.InputTokens // fallback for old sessions
			}
			m.totalUsage.InputTokens += msg.Usage.InputTokens
			m.totalUsage.OutputTokens += msg.Usage.OutputTokens
			m.totalUsage.CacheCreationInputTokens += msg.Usage.CacheCreationInputTokens
			m.totalUsage.CacheReadInputTokens += msg.Usage.CacheReadInputTokens
		}
	}
	m.totalUsage.LastInputTokens = lastInput
	m.statusbar.SetUsage(&m.totalUsage)
}

// restoreSessionProvider resolves and switches the agent's provider to match
// the given session providerName. Returns the display string and whether the
// provider was successfully restored.
func (m *Model) restoreSessionProvider(providerName string) (string, bool) {
	pCfg := m.cfg.FindProvider(providerName)
	if pCfg == nil {
		// Keep current provider, show the session's expected info
		return fmt.Sprintf("%s [unmatched]", providerName), false
	}

	sp, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		return fmt.Sprintf("%s [error]", providerName), false
	}

	provider, err := llm.NewProvider(sp.Type, sp.APIKey, sp.BaseURL, sp.Model)
	if err != nil {
		return fmt.Sprintf("%s (%s) [error]", sp.Type, sp.Model), false
	}

	m.agent.SetProvider(provider)
	return fmt.Sprintf("%s (%s)", sp.Type, sp.Model), true
}
