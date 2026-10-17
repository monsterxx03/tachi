package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
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
		m.clampProviderScroll()
	case "down", "ctrl+j", "ctrl+n":
		if m.providerSelIdx < len(m.providerShown)-1 {
			m.providerSelIdx++
		}
		m.clampProviderScroll()
	case "enter":
		if m.providerSelIdx >= 0 && m.providerSelIdx < len(m.providerShown) {
			return m, m.switchToProvider(m.providerSelIdx)
		}
	case "esc":
		m.exitModelSelect("")
	case "backspace":
		// A rune, not a byte: the filter is typed by a reader, and a Chinese IME commits whole
		// characters — cutting one in half would leave a filter that matches nothing and cannot be
		// seen to be wrong.
		if m.providerFilter != "" {
			r := []rune(m.providerFilter)
			m.setProviderFilter(string(r[:len(r)-1]))
		}
	default:
		// Everything else that is a PRINTABLE key goes into the filter. Key().Text is the field that
		// says so (bubbletea fills it only for character keys, and it carries the whole run for a
		// paste or an IME commit), while the modifier guard keeps a Ctrl chord out of the box — the
		// arrows' own Ctrl+P/N are matched above, so they never reach here.
		key := msg.Key()
		if key.Text != "" && key.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			m.setProviderFilter(m.providerFilter + key.Text)
		}
	}
	return m, nil
}

// providerVisibleRows is how many provider rows fit in the picker: what layout() gives the picker
// (min(len+1, height/2)) minus its title line. It is the same number the window is clamped
// against, and the two have to agree or the highlight scrolls past the bottom of the box.
func (m *Model) providerVisibleRows() int {
	return min(max(m.height/2-1, 1), len(m.providerShown))
}

// clampProviderScroll keeps the window over the list AND over the highlight: a highlight below the
// last visible row (or above the first) scrolls the window with it. Same rule the session picker
// uses, for the same reason — an index the reader cannot see is an index they cannot trust.
func (m *Model) clampProviderScroll() {
	visibleRows := m.providerVisibleRows()
	maxScroll := max(len(m.providerShown)-visibleRows, 0)
	if m.providerScrollOff > maxScroll {
		m.providerScrollOff = maxScroll
	}
	if m.providerScrollOff < 0 {
		m.providerScrollOff = 0
	}
	if m.providerSelIdx < m.providerScrollOff {
		m.providerScrollOff = m.providerSelIdx
	}
	if m.providerSelIdx >= m.providerScrollOff+visibleRows {
		m.providerScrollOff = m.providerSelIdx - visibleRows + 1
	}
}

// setProviderFilter re-filters the list and puts the highlight back at the head of it: once the
// query changes, the row the reader was on belongs to a list they are no longer looking at. (The
// desktop picker and the ⌘K/⌘P palettes do the same.)
func (m *Model) setProviderFilter(filter string) {
	m.providerFilter = filter
	m.providerSelIdx = 0
	m.providerScrollOff = 0
	m.refreshProviderShown()
}

// refreshProviderShown recomputes providerShown from providerItems and the current filter.
func (m *Model) refreshProviderShown() {
	q := strings.ToLower(strings.TrimSpace(m.providerFilter))
	if q == "" {
		m.providerShown = m.providerItems
	} else {
		shown := make([]config.ProviderConfig, 0, len(m.providerItems))
		for _, p := range m.providerItems {
			if providerMatches(p, q) {
				shown = append(shown, p)
			}
		}
		m.providerShown = shown
	}
	if m.providerSelIdx >= len(m.providerShown) {
		m.providerSelIdx = max(len(m.providerShown)-1, 0)
	}
	m.clampProviderScroll()
}

// providerMatches reports whether a provider answers the filter `q` (already lowercased and
// trimmed). BOTH the name and the model string count, because the two answer different questions:
// the name is the config entry (`goat-claude-sonnet-5-5`) while the model is what goes to the API
// (`anthropic/claude-sonnet-5-5`) — so a search by vendor only ever matches the second one.
func providerMatches(p config.ProviderConfig, q string) bool {
	return strings.Contains(strings.ToLower(p.Name), q) ||
		strings.Contains(strings.ToLower(p.Model), q)
}

func (m *Model) renderProviderSelection() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render(m.providerSelectionTitle()) + "\n")

	if len(m.providerShown) == 0 {
		b.WriteString(completionNormalStyle.Width(m.width).Render(" 没有匹配的模型（Backspace 删掉过滤词）"))
		b.WriteString("\n")
		return b.String()
	}

	currentInfo := m.statusbar.ProviderInfo()
	// The name column is sized from the WHOLE list, not the visible slice: a width that moved with
	// every keystroke would shuffle every row's alignment while the reader is typing.
	maxNameLen := 0
	for _, p := range m.providerItems {
		maxNameLen = max(maxNameLen, len(p.Name))
	}

	// Only the window is drawn (see providerVisibleRows / clampProviderScroll). Drawing the whole
	// list was the bug: the picker is given at most half the screen, so everything past that was
	// produced and never seen — including the highlight, which the reader could then arrow off the
	// bottom edge of the box with no way to tell where it went.
	end := min(m.providerScrollOff+m.providerVisibleRows(), len(m.providerShown))
	for idx := m.providerScrollOff; idx < end; idx++ {
		p := m.providerShown[idx]
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

// providerSelectionTitle is the picker's title line: the key help, and — once a filter is typed —
// what it is and how much of the list it left. The count is worth the space because an empty result
// and a short one look the same in a box that only draws a few rows.
func (m *Model) providerSelectionTitle() string {
	if m.providerFilter == "" {
		return fmt.Sprintf("Select provider (↑↓ Enter Esc) — %d 个，输入以过滤", len(m.providerItems))
	}
	return fmt.Sprintf("Select provider /%s (↑↓ Enter Esc Backspace) — %d/%d 个",
		m.providerFilter, len(m.providerShown), len(m.providerItems))
}

func (m *Model) exitModelSelect(msg string) {
	if msg != "" {
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
	}
	m.providerItems = nil
	m.providerShown = nil
	m.providerFilter = ""
	m.providerSelIdx = 0
	m.providerScrollOff = 0
	m.setState(stateIdle)
	m.layout()
}

func (m *Model) switchToProvider(idx int) tea.Cmd {
	// From providerShown, because that is the list the highlighted row belongs to (the index the
	// reader sees), and a filtered list has different indices from the whole one.
	pCfg := &m.providerShown[idx]
	resolved, err := llm.BuildProvider(m.cfg, pCfg.Name)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return nil
	}

	providerInfo := fmt.Sprintf("%s (%s)", resolved.Type, resolved.Model)

	// Check whether the current context exceeds the target model's window.
	// If so, compact first using the current (wide-context) provider, then
	// switch — otherwise the compact LLM call itself would fail.
	currentEstimate := m.agent.LastInputEstimate()
	targetCW := resolved.ContextWindow
	if m.shouldCompactBeforeSwitch(currentEstimate, targetCW) {
		m.pendingSwitchProvider = &pendingSwitchProvider{
			providerName: pCfg.Name,
			providerInfo: providerInfo,
		}

		// Exit model selection overlay and show status messages.
		m.providerItems = nil
		m.providerShown = nil
		m.providerFilter = ""
		m.providerSelIdx = 0
		m.providerScrollOff = 0
		m.setState(stateIdle)
		m.layout()

		m.chatview.AddMessage(chatMessage{
			Role:    "user",
			Content: fmt.Sprintf("/model → %s", providerInfo),
		})
		m.chatview.AddMessage(chatMessage{
			Role: "assistant",
			Content: fmt.Sprintf(
				"当前对话上下文（约 **%s tokens**）超过 **%s** 的窗口限制（**%s tokens**）。正在使用当前模型自动压缩后再切换...",
				cmds.FormatTokens(currentEstimate), providerInfo, cmds.FormatTokens(targetCW),
			),
		})

		return m.compactForModelSwitch()
	}

	// Normal switch — context fits in the target model's window.
	// SetResolvedProvider swaps the full resolved config (provider + context
	// window + thinking defaults) in one step; the per-session /thinking
	// override (if any) is re-applied on top.
	if _, err := m.agent.SetResolvedProvider(pCfg.Name); err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return nil
	}
	m.reapplySessionThinking()
	m.statusbar.SetProviderInfo(providerInfo)
	m.statusbar.SetContextWindow(targetCW)

	// Persist the new provider name to the session metadata.
	if sm := m.agent.SessionManager(); sm != nil {
		if curr := sm.Current(); curr != nil {
			if curr.ProviderName != pCfg.Name {
				curr.ProviderName = pCfg.Name
				_ = sm.UpdateMeta(curr) // best-effort
			}
		}
	}

	m.exitModelSelect(fmt.Sprintf("Switched to %s", providerInfo))
	return nil
}

// shouldCompactBeforeSwitch returns true when the current estimated context
// exceeds the target model's context window enough that a compaction should
// happen before switching providers. Uses the configured compact threshold
// (default 0.8) as the trigger ratio.
func (m *Model) shouldCompactBeforeSwitch(currentEstimate int64, targetCW int64) bool {
	if currentEstimate <= 0 || targetCW <= 0 {
		return false
	}
	threshold := m.cfg.Compact.Threshold
	return float64(currentEstimate) >= float64(targetCW)*threshold
}

// compactForModelSwitch starts a compaction using the current (wide-context)
// provider, used when switching to a model with a smaller context window.
// After the compaction completes, handleAgentEvent applies the pending switch.
func (m *Model) compactForModelSwitch() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() || len(m.history) == 0 {
		// No session to compact — switch directly via a tea.Msg so the
		// state mutation happens in Update (not in a goroutine closure).
		if ps := m.pendingSwitchProvider; ps != nil {
			m.pendingSwitchProvider = nil
			m.compactForSwitch = false
			return func() tea.Msg {
				return switchProviderMsg{
					providerName: ps.providerName,
					providerInfo: ps.providerInfo,
				}
			}
		}
		return nil
	}

	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	// Save state (same as handleCompactCommand).
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)
	m.isCompacting = true
	m.compactForSwitch = true

	instruction := cmds.BuildCompactInstruction()

	ctx := m.startTurn()

	// CompactRunOptions: the compact run must not call tools, and must not be
	// auto-compacted from under itself (see handleCompactCommand).
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, instruction, m.systemPrompt, m.chatOpts,
		agent.CompactRunOptions()...)

	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}

// applyPendingSwitch applies a deferred provider switch (stored in
// pendingSwitchProvider) and updates the statusbar, chatview, and agent.
func (m *Model) applyPendingSwitch() {
	if m.pendingSwitchProvider == nil {
		return
	}
	ps := m.pendingSwitchProvider
	m.pendingSwitchProvider = nil
	m.compactForSwitch = false

	// Re-resolve and switch wholesale — SetResolvedProvider swaps the full
	// resolved config (provider + context window + thinking defaults) in one
	// step; the per-session /thinking override (if any) is re-applied on top.
	if _, err := m.agent.SetResolvedProvider(ps.providerName); err != nil {
		m.chatview.AddMessage(chatMessage{Role: "error", Content: fmt.Sprintf("切换 provider 失败：%v", err)})
		m.chatview.FinishStreaming()
		m.syncSessionInfo()
		m.cancelFunc = nil
		m.eventCh = nil
		m.setState(stateIdle)
		return
	}
	m.reapplySessionThinking()
	m.statusbar.SetProviderInfo(ps.providerInfo)
	m.statusbar.SetContextWindow(m.agent.ContextWindow())

	// Persist the new provider name to the session metadata.
	if ps.providerName != "" {
		if sm := m.agent.SessionManager(); sm != nil {
			if curr := sm.Current(); curr != nil {
				if curr.ProviderName != ps.providerName {
					curr.ProviderName = ps.providerName
					_ = sm.UpdateMeta(curr) // best-effort
				}
			}
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("压缩完成，已切换到 **%s**。", ps.providerInfo),
	})
	m.chatview.FinishStreaming()
	m.syncSessionInfo()
	m.cancelFunc = nil
	m.eventCh = nil
	m.setState(stateIdle)
}
