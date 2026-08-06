package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// --- /model ---

// handleModelCommand lists available providers/models or switches to a named provider
// for the given thread. /model only affects the current thread; other threads
// continue using the global default provider.
// /model          — list all configured providers with current thread's choice marked
// /model <name>   — switch the current thread to the named provider
func (m *Manager) handleModelCommand(threadID, args string) (string, error) {
	if m.cfg == nil || len(m.cfg.Providers) == 0 {
		return "No providers configured.", nil
	}

	args = strings.TrimSpace(args)

	if args == "" {
		// List mode: show all providers.
		return m.handleModelList(threadID)
	}

	// Switch mode: resolve and activate the named provider for this thread.
	return m.handleModelSwitch(threadID, args)
}

// handleModelList returns a formatted list of all configured providers,
// marking the current thread's active provider with a star. Falls back to the
// global default when the thread has no per-thread override.
func (m *Manager) handleModelList(threadID string) (string, error) {
	// Determine which provider is active for this thread.
	currentName := m.providerNameForThread(threadID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Configured models (%d):\n", len(m.cfg.Providers))

	for _, p := range m.cfg.Providers {
		marker := " "
		if p.Name == currentName {
			marker = "*"
		}
		fmt.Fprintf(&sb, "\n%s %s\n", marker, p.Name)
		fmt.Fprintf(&sb, "  Type: %s  Model: %s\n", p.Type, p.Model)
	}

	sb.WriteString("\nUse /model <name> to switch (per-thread).")

	return sb.String(), nil
}

// handleModelSwitch resolves and activates the named provider for the
// current thread by persisting the choice to the session's meta.json.
// Other threads (sessions) are unaffected. On next message, the provider
// is resolved from the session's ProviderName field.
//
// If the current session's context exceeds the target model's context
// window, a compaction is triggered first using the current (wide-context)
// provider — otherwise the next API call would fail with context overflow.
func (m *Manager) handleModelSwitch(threadID, name string) (string, error) {
	resolved, err := m.cfg.BuildProvider(name)
	if errors.Is(err, config.ErrProviderNotFound) {
		return fmt.Sprintf("Provider %q not found. Use /model to see available models.", name), nil
	}
	if err != nil {
		return "", err
	}

	sm := m.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /model find session failed", err, "thread", threadID)
	}

	compactNote := ""

	// ── Pre-switch compact check ──────────────────────────────────────
	// If the current session's context exceeds the new model's window,
	// compact first using the current (wide-context) provider so the next
	// message doesn't fail with context overflow.
	if sess != nil && sm.HasCurrent() {
		sessionMsgs, loadErr := sm.LoadMessages()
		if loadErr == nil && len(sessionMsgs) > 0 {
			currentEstimate := agent.EstimateContentTokens(sessionMsgs, sess.ProviderName)
			threshold := m.cfg.Compact.Threshold

			if currentEstimate > 0 && resolved.ContextWindow > 0 &&
				float64(currentEstimate) >= float64(resolved.ContextWindow)*threshold {

				m.logger.Info(context.Background(), "channel: /model pre-switch compact triggered", "thread", threadID, "estimate", currentEstimate, "targetCW", resolved.ContextWindow)

				summary, compactErr := m.runCompactForSwitch(threadID, sm, sessionMsgs)
				if compactErr != nil {
					m.logger.Error(context.Background(), "channel: /model pre-switch compact failed", compactErr)
					compactNote = "\n\n⚠ 自动压缩失败，切换后如果遇到上下文溢出错误，请运行 /compact。"
				} else {
					sid := ""
					if cur := sm.Current(); cur != nil {
						sid = cur.ID
					}
					systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "", sid)
					_, finalizeErr := agent.FinalizeCompact(sm, systemPrompt, summary)
					if finalizeErr != nil {
						m.logger.Error(context.Background(), "channel: /model FinalizeCompact failed", finalizeErr)
						compactNote = "\n\n⚠ 压缩后创建新 session 失败，请运行 /compact。"
					} else {
						// Migrate ThreadID to the new (current) session.
						sm.SetThreadID(threadID)
						compactNote = "\n\n🔍 当前上下文超过目标模型窗口，已自动压缩完成后切换。"
						m.logger.Info(context.Background(), "channel: /model pre-switch compact completed", "thread", threadID)
					}
				}
			}
		}
	}

	// ── Persist the new provider name ─────────────────────────────────
	// After a potential compact, sm.Current() is either the new session
	// (compact succeeded) or the original session (no compact needed).
	// resolved.Name is already alias-normalized (BuildProvider resolves
	// provider_aliases before returning the resolved config).
	curr := sm.Current()
	if curr == nil {
		// No session at all — create one now to persist the model choice.
		wd, _ := os.Getwd()
		newSess, err := sm.New(resolved.Name, wd)
		if err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
		sm.SetThreadID(threadID)
		curr = newSess
	}
	curr.ProviderName = resolved.Name
	curr.UpdatedAt = time.Now()
	if err := sm.UpdateMeta(curr); err != nil {
		m.logger.Error(context.Background(), "channel: /model update session meta failed", err, "thread", threadID)
	}

	// Evict only the current thread's cached agent so the next message
	// rebuilds with the new provider. Other threads are unaffected.
	m.evictAgent(threadID)

	m.logger.Info(context.Background(), "channel: /model switched provider", "thread", threadID, "name", name, "type", resolved.Type, "model", resolved.Model)

	return fmt.Sprintf("✅ Switched to **%s** (%s, %s).\nThis thread will now use this model. Other threads are unchanged.%s",
		name, resolved.Type, resolved.Model, compactNote), nil
}

// runCompactForSwitch runs a compaction LLM call using the thread's CURRENT
// (wide-context) provider. It's called before switching to a smaller-context
// model so the summarization call itself doesn't overflow.
//
// The caller (handleModelSwitch) is responsible for calling FinalizeCompact
// to create the new session and migrate the ThreadID.
func (m *Manager) runCompactForSwitch(threadID string, sm *session.Manager, sessionMsgs []session.Message) (string, error) {
	// Get the current (old) provider — before switching. The provider itself
	// carries the THREAD's actual provider name (per-session /model override
	// wins over the global one) — the wrapping layer reads it from there, so
	// a non-default thread provider is neither mislabeled nor mispriced.
	oldProvider := m.getProviderForThread(threadID).Provider
	// Usage billing: the manager-owned provider is outside the agent's
	// recording chain — wrap it for this call so the summary cost is billed
	// (kind + session anchoring set below). The row's provider name and price
	// come from the provider itself (thread-resolved via BuildProvider).
	oldProvider = agent.WrapProviderForUsage(oldProvider, m.cfg)

	// Convert session messages to LLM messages for the provider API.
	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, oldProvider.Name())
	if err != nil {
		return "", fmt.Errorf("convert messages: %w", err)
	}

	// Build messages: system prompt + history + compact instruction.
	sid := ""
	if cur := sm.Current(); cur != nil {
		sid = cur.ID
	}
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "", sid)
	compactMsgs := make([]llm.Message, 0, len(llmMsgs)+2)
	if systemPrompt != "" {
		compactMsgs = append(compactMsgs, llm.Message{Role: "system", Content: systemPrompt})
	}
	compactMsgs = append(compactMsgs, llmMsgs...)
	compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: cmds.BuildCompactInstruction()})

	// Resolve timeout and max tokens from config.
	compactTimeout := 5 * time.Minute
	if m.cfg != nil && m.cfg.Compact.Timeout > 0 {
		compactTimeout = m.cfg.Compact.Timeout
	}
	maxTokens := 4096
	if m.cfg != nil && m.cfg.Compact.MaxTokens > 0 {
		maxTokens = m.cfg.Compact.MaxTokens
	}

	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	defer cancel()
	ctx = llm.WithUsageKind(ctx, llm.UsageKindCompact)
	if sid != "" {
		ctx = llm.WithSessionID(ctx, sid)
	}

	resp, err := oldProvider.CreateChat(ctx, compactMsgs, nil, llm.ChatOptions{
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("compact LLM call: %w", err)
	}

	return resp.Content, nil
}

// --- /thinking ---

// handleThinkingCommand shows or sets the per-session thinking level for the
// given thread. The setting is persisted to the session's meta.json and only
// affects this session — other threads/sessions are unchanged.
//
//	/thinking           → show current level + valid options
//	/thinking <level>   → set level: none | low | medium | high | xhigh | max | default
func (m *Manager) handleThinkingCommand(threadID, args string) (string, error) {
	args = strings.TrimSpace(args)

	sm := m.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /thinking find session failed", err, "thread", threadID)
		return "", fmt.Errorf("find session: %w", err)
	}

	// No argument — show the current level and valid options.
	if args == "" {
		current := "default"
		if sess != nil && sess.ThinkingLevel != "" {
			current = sess.ThinkingLevel
		}
		return cmds.FormatThinkingStatus(current), nil
	}

	if !cmds.IsValidThinkingLevel(args) {
		return fmt.Sprintf("无效的 thinking level: **%s**\n\n可选级别:\n%s",
			args, cmds.FormatThinkingOptions()), nil
	}

	// Ensure a provider is available before persisting. The per-session
	// override is applied to the cached agent below via a single
	// getProviderForThread (session meta already updated); a thread-level
	// override that fails to resolve falls back to the global provider.

	if sess == nil {
		// No session yet — create one bound to this thread so the override
		// persists across turns (same pattern as /model).
		wd, _ := os.Getwd()
		newSess, err := sm.New(m.defaultResolvedProvider.Name, wd)
		if err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
		sm.SetThreadID(threadID)
		sess = newSess
	}

	// Persist the per-session override ("" = default, no override).
	if args == "default" {
		sess.ThinkingLevel = ""
	} else {
		sess.ThinkingLevel = args
	}
	sess.UpdatedAt = time.Now()
	if err := sm.UpdateMeta(sess); err != nil {
		m.logger.Error(context.Background(), "channel: /thinking update session meta failed", err, "thread", threadID)
		return "", fmt.Errorf("update session meta: %w", err)
	}

	// Apply to the cached agent immediately so the next turn uses it.
	// Future agent rebuilds pick the override up via getProviderForThread.
	m.applyThinkingToCachedAgent(threadID, sess)

	shown := sess.ThinkingLevel
	if shown == "" {
		shown = "default"
	}
	return fmt.Sprintf("✅ 当前会话 thinking level 已设为 **%s**（%s）。\n其他会话不受影响。",
		shown, cmds.ThinkingLevelDescriptions[shown]), nil
}

// applyThinkingToCachedAgent updates the cached agent for a thread with the
// session's effective thinking config. If no agent is cached yet, the next
// turn's build picks the override up via getProviderForThread — no-op here.
func (m *Manager) applyThinkingToCachedAgent(threadID string, sess *session.Session) {
	resolved := m.getProviderForThread(threadID)
	thinking, effort := cmds.EffectiveThinking(sess.ThinkingLevel, *resolved)

	m.agentCacheMu.Lock()
	ca, ok := m.agentCache[threadID]
	m.agentCacheMu.Unlock()
	if !ok {
		return
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.agent != nil {
		ca.agent.SetThinking(thinking, effort)
	}
}
