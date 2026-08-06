package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

func (m *Model) handleThinkingCommand() tea.Cmd {
	display := m.subcommandInput
	if display == "" {
		display = "/thinking"
	}
	parts := strings.Fields(m.subcommandInput)
	level := ""
	if len(parts) > 1 {
		level = parts[1]
	}
	if len(parts) > 2 {
		m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("多余参数: `%s`。用法: `/thinking <level>`", strings.Join(parts[2:], " ")),
		})
		return nil
	}

	sm := m.agent.SessionManager()
	hasSession := sm != nil && sm.HasCurrent()
	var curr *session.Session
	if hasSession {
		curr = sm.Current()
	}

	// Resolve the provider config so the effective thinking can be computed
	// (model-specific normalization + "default" fallback). Use the current
	// session's provider, or the active default provider when there's no
	// session yet.
	providerName := ""
	if hasSession {
		providerName = curr.ProviderName
	}
	if m.cfg == nil {
		m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "无法解析默认 provider（配置缺失）。",
		})
		return nil
	}
	sp, err := m.cfg.BuildProvider(providerName) // empty name → default
	if err != nil {
		msg := fmt.Sprintf("无法解析 provider **%s** 的配置，无法设置 thinking level。", providerName)
		if !errors.Is(err, config.ErrProviderNotFound) {
			msg = fmt.Sprintf("解析 provider 配置失败: %v", err)
		}
		m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
		m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
		return nil
	}

	// No argument — show the current level and valid options. Without a
	// session this reflects a pending override (set at startup) or the
	// provider default.
	if level == "" {
		current := ""
		if hasSession {
			current = curr.ThinkingLevel
		} else {
			current = m.agent.PendingSessionThinking()
		}
		if current == "" {
			current = "default"
		}
		m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: cmds.FormatThinkingStatus(current),
		})
		return nil
	}

	if !cmds.IsValidThinkingLevel(level) {
		m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("无效的 thinking level: **%s**\n\n可选级别:\n%s", level, cmds.FormatThinkingOptions()),
		})
		return nil
	}

	// Normalize the per-session override ("" = default, no override).
	store := level
	if level == "default" {
		store = ""
	}

	if hasSession {
		// Persist the per-session override to the active session's meta.
		curr.ThinkingLevel = store
		curr.UpdatedAt = time.Now()
		if err := sm.UpdateMeta(curr); err != nil {
			m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("保存 session 失败: %v", err),
			})
			return nil
		}
	} else {
		// No session yet — record a pending override that the next turn's
		// auto-created session will inherit (see ensureSessionAndRecordUser).
		m.agent.SetPendingSessionThinking(store)
	}

	// Apply to the live agent immediately — the next turn uses it.
	thinking, effort := cmds.EffectiveThinking(store, *sp)
	m.agent.SetThinking(thinking, effort)
	m.syncThinkingBadge()

	shown := store
	if shown == "" {
		shown = "default"
	}
	desc := cmds.ThinkingLevelDescriptions[shown]
	msg := fmt.Sprintf("🧠 当前会话 thinking level 已设为 **%s**（%s）。\n其他会话不受影响。", shown, desc)
	if !hasSession {
		msg = fmt.Sprintf("🧠 thinking level 已设为 **%s**（%s），将在创建首个会话时生效。", shown, desc)
	}
	m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
	m.chatview.AddMessage(chatMessage{Role: "assistant", Content: msg})
	return nil
}

// reapplySessionThinking re-applies the current session's per-session
// thinking override (set via /thinking) to the agent. Called after the
// provider/model changes (e.g. /model switch) so the override survives the
// switch; without an override it does nothing and the new provider's
// config default stays in effect.
func (m *Model) reapplySessionThinking() {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.syncThinkingBadge()
		return
	}
	curr := sm.Current()
	if curr.ThinkingLevel == "" {
		// No per-session override — the provider config default set by
		// /model stays in effect. Still refresh the badge so the new
		// provider's default shows.
		m.syncThinkingBadge()
		return
	}
	if m.cfg == nil {
		// Defensive: never reachable in production (NewModel always sets
		// cfg), but keep the same nil guard as handleThinkingCommand.
		m.syncThinkingBadge()
		return
	}
	providerName := curr.ProviderName
	sp, err := m.cfg.BuildProvider(providerName) // empty name → default
	if err != nil {
		m.syncThinkingBadge()
		return
	}
	thinking, effort := cmds.EffectiveThinking(curr.ThinkingLevel, *sp)
	m.agent.SetThinking(thinking, effort)
	m.syncThinkingBadge()
}

// handleUsageCommand builds a usage report and displays it in the chat view.
func (m *Model) handleUsageCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No active session",
		})
		return nil
	}

	report, err := agent.ComputeSessionUsage(sm, m.agent.UsageRecorder(), m.agent.ContextWindow())
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to compute usage: %v", err),
		})
		return nil
	}

	info := agent.BuildUsageReportInfo(report, m.totalUsage.LastInputTokens, m.agent.LastTokenBreakdown(), m.cfg.Debug.PPROF.Addr())

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: cmds.FormatUsageReport(info),
	})
	return nil
}

// handleTranscriptCommand generates an HTML transcript report for the current
// session, opens it in the default browser, and shows the result in the chat view.
// If the browser cannot be opened, it displays the file path instead.
func (m *Model) handleTranscriptCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil || !sm.HasCurrent() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No active session — start a conversation first",
		})
		return nil
	}

	// Load messages for the current session
	msgs, err := sm.LoadMessages()
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to load session messages: %v", err),
		})
		return nil
	}
	if len(msgs) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No messages in current session yet — send a message first",
		})
		return nil
	}

	curr := sm.Current()

	// Sub-agent sidecar messages are optional — a load failure is non-fatal.
	subagents, _ := sm.LoadSubagentMessages(curr.ID)

	// Build report data from session messages
	data := render.BuildReportDataFromMessages(curr, msgs, subagents)
	html, err := render.GenerateHTML(data)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to generate transcript HTML: %v", err),
		})
		return nil
	}

	path, err := render.OpenInBrowser(html, curr.ID)
	if err != nil {
		// Browser couldn't be opened — show the file path in chat
		m.chatview.AddMessage(chatMessage{
			Role: "assistant",
			Content: fmt.Sprintf(
				"**📋 Transcript Report**\n\nBrowser could not be opened automatically.\n\nReport saved to:\n`%s`",
				path,
			),
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role: "assistant",
		Content: fmt.Sprintf(
			"**📋 Transcript Report**\n\nSession: `%s`\nOpened: `%s`",
			curr.Title, path,
		),
	})
	return nil
}

// handleDreamCommandDispatch parses /dream subcommands and dispatches.
//
//	/dream or /dream run  → trigger AutoDream
//	/dream status         → show current orchestrator status
