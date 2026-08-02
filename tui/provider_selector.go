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
	case "down", "ctrl+j", "ctrl+n":
		if m.providerSelIdx < len(m.providerItems)-1 {
			m.providerSelIdx++
		}
	case "enter":
		return m, m.switchToProvider(m.providerSelIdx)
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

func (m *Model) switchToProvider(idx int) tea.Cmd {
	pCfg := &m.providerItems[idx]
	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		m.exitModelSelect("Error: " + err.Error())
		return nil
	}
	provider, err := config.NewProviderFromResolved(resolved)
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
			provider:       provider,
			providerName:   pCfg.Name,
			providerInfo:   providerInfo,
			contextWindow:  targetCW,
			thinking:       resolved.Thinking,
			thinkingEffort: resolved.ThinkingEffort,
		}

		// Exit model selection overlay and show status messages.
		m.providerItems = nil
		m.providerSelIdx = 0
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
	m.agent.SetProvider(provider)
	m.agent.SetThinking(resolved.Thinking, resolved.ThinkingEffort)
	m.reapplySessionThinking() // keep the per-session /thinking override, if any
	m.agent.SetContextWindow(targetCW)
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
					provider:       ps.provider,
					providerName:   ps.providerName,
					providerInfo:   ps.providerInfo,
					contextWindow:  ps.contextWindow,
					thinking:       ps.thinking,
					thinkingEffort: ps.thinkingEffort,
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

	// WithNoTools: the compact run must not call tools (see handleCompactCommand).
	m.eventCh = m.agent.RunConversationStream(ctx, m.history, instruction, m.systemPrompt, m.chatOpts,
		agent.WithNoTools())

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

	m.agent.SetProvider(ps.provider)
	m.agent.SetThinking(ps.thinking, ps.thinkingEffort)
	m.reapplySessionThinking() // keep the per-session /thinking override, if any
	m.agent.SetContextWindow(ps.contextWindow)
	m.statusbar.SetProviderInfo(ps.providerInfo)
	m.statusbar.SetContextWindow(ps.contextWindow)

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
