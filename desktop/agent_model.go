package main

import (
	"context"

	"github.com/monsterxx03/tachi/config"
)

// GetMode returns the active session's mode: auto, chat or plan.
func (s *AgentService) GetMode() string {
	return s.desk.sessionMode(s.desk.currentID())
}

// SetMode switches the active session's mode and returns "ok", or a sentence saying why
// it could not (no agent yet, unknown mode, a turn in flight).
//
// A mode is not just a label: chat and plan hide the destructive tools from the schema
// the model sees, and plan additionally appends the plan-mode rules to the system prompt.
// That filter runs every iteration, so switching mid-turn would pull tools out from under
// a running agent — a running session has to wait.
func (s *AgentService) SetMode(mode string) string {
	d := s.desk
	id := d.currentID()

	d.mu.Lock()
	r := d.runs[id]
	running := false
	if r != nil {
		running = r.running
	}
	d.mu.Unlock()

	if r == nil || r.agent == nil {
		return "agent not ready"
	}
	if running {
		return "会话正在运行中，等这一轮结束再切模式"
	}
	if err := r.agent.SetMode(mode); err != nil {
		return err.Error()
	}
	return "ok"
}

// ListProviders returns the configured providers (priority-ordered).
func (s *AgentService) ListProviders() []config.ProviderConfig {
	if s.desk.cfg == nil {
		return nil
	}
	return s.desk.cfg.Providers
}

// SwitchProvider switches the active session's agent to the given config
// provider, persisting it to that session's metadata (like TUI does).
func (s *AgentService) SwitchProvider(name string) string {
	d := s.desk
	if d.cfg == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return "no current session"
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return "agent not ready"
	}
	if _, err := r.agent.SetResolvedProvider(name); err != nil {
		return err.Error()
	}
	// Re-apply this session's thinking override (or its default) against the new
	// provider, so switching models doesn't inherit a stale thinking setting.
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil {
			applyThinking(r.agent, curr.ThinkingLevel)
			if curr.ProviderName != name {
				curr.ProviderName = name
				_ = r.sm.UpdateMeta(curr) // best-effort
			}
		}
	}
	d.mu.Lock()
	r.agentProvider = name
	d.mu.Unlock()
	return "ok"
}

// GetProviderInfo returns the active session's provider/model/context-window.
func (s *AgentService) GetProviderInfo() map[string]any {
	d := s.desk
	r := d.activeRun()
	if r == nil || r.agent == nil {
		return nil
	}
	provider := ""
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil && curr.ProviderName != "" {
			provider = curr.ProviderName
		}
	}
	if provider == "" && d.cfg != nil {
		provider = d.cfg.DefaultProviderName()
	}
	// Context estimate: prefer the agent's live estimate; when the session was
	// just opened (no turn yet, estimate is 0) fall back to the most recent
	// message's persisted estimate (usage.estimated_input_tokens) so the
	// context ring shows a sensible value for a resumed session.
	est := r.agent.LastInputEstimate()
	if est <= 0 {
		est = d.estimateFromMessages(r)
	}
	return map[string]any{
		"provider":        provider,
		"model":           r.agent.Model(),
		"contextWindow":   r.agent.ContextWindow(),
		"contextEstimate": est,
	}
}

// GetThinkingLevel returns the active session's thinking level. Empty =
// follow the provider default (surfaced as "default").
func (s *AgentService) GetThinkingLevel() string {
	r := s.desk.activeRun()
	if r == nil || r.sm == nil {
		return "default"
	}
	curr := r.sm.Current()
	if curr == nil || curr.ThinkingLevel == "" {
		return "default"
	}
	return curr.ThinkingLevel
}

// SetThinkingLevel sets the active session's thinking level. "default"/""
// means DON'T set it — follow the provider's own default; "none" disables
// thinking. Persisted to the session's metadata.
func (s *AgentService) SetThinkingLevel(level string) string {
	d := s.desk
	if d.cfg == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return "no current session"
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return "agent not ready"
	}
	applyThinking(r.agent, level)
	store := level
	if level == "default" || level == "" {
		store = ""
	}
	if r.sm != nil {
		if curr := r.sm.Current(); curr != nil {
			curr.ThinkingLevel = store
			_ = r.sm.UpdateMeta(curr) // best-effort
		}
	}
	return "ok"
}
