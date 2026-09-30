package main

import (
	"context"

	"github.com/monsterxx03/tachi/config"
)

// ListProviders returns the configured providers (priority-ordered).
func (s *AgentService) ListProviders() []config.ProviderConfig {
	cfg := s.desk.config()
	if cfg == nil {
		return nil
	}
	return cfg.Providers
}

// SwitchProvider switches the active session's agent to the given config
// provider, persisting it to that session's metadata (like TUI does).
func (s *AgentService) SwitchProvider(name string) string {
	d := s.desk
	if d.config() == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return refuseNoSession
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return refuseNoSession
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

// contextUsageOf is the ONE rule for "how full is this session's window": the agent's live
// estimate (recomputed before every API call — see agent/token_estimate.go), falling back to
// the most recent message's persisted estimate for a session that has not run a turn in this
// process yet. GetProviderInfo and the per-call "agent:cost" payload both go through it, so
// the ring and the popover cannot drift apart by construction.
func (d *desktopApp) contextUsageOf(r *sessionRun) (est, window int64) {
	if r == nil || r.agent == nil {
		return 0, 0
	}
	est = r.agent.LastInputEstimate()
	if est <= 0 {
		est = d.estimateFromMessages(r)
	}
	return est, r.agent.ContextWindow()
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
	if provider == "" {
		if cfg := d.config(); cfg != nil {
			provider = cfg.DefaultProviderName()
		}
	}
	// Context estimate: prefer the agent's live estimate; when the session was
	// just opened (no turn yet, estimate is 0) fall back to the most recent
	// message's persisted estimate (usage.estimated_input_tokens) so the
	// context ring shows a sensible value for a resumed session.
	est, win := d.contextUsageOf(r)
	return map[string]any{
		"provider":        provider,
		"model":           r.agent.Model(),
		"contextWindow":   win,
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
	if d.config() == nil {
		return "agent not ready"
	}
	id := d.currentID()
	if id == "" {
		return refuseNoSession
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	if r.agent == nil {
		return refuseNoSession
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
