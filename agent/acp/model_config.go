package acp

import (
	"context"
	"errors"
	"fmt"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

const (
	// modelConfigID is the ACP session config option ID for switching the LLM model.
	modelConfigID = "model"
	// modelConfigName is the human-readable label shown in the client.
	modelConfigName = "Model"
	// modelConfigDescription is shown to the user to explain the option.
	modelConfigDescription = "The LLM provider/model to use for this session."
)

// buildModelConfigOption builds a SessionConfigOption (select) exposing the
// configured providers as selectable model values.
// It returns the option and the current value ID (provider name).
func buildModelConfigOption(cfg *config.Config, currentProviderName string) (*acp.SessionConfigOption, string) {
	if cfg == nil || len(cfg.Providers) == 0 {
		return nil, ""
	}

	currentValue := ""
	options := make([]acp.SessionConfigSelectOption, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.Name == "" || p.Model == "" {
			continue
		}
		if p.Name == currentProviderName {
			currentValue = p.Name
		}
		options = append(options, acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(p.Name),
			Name:  p.Name,
		})
	}

	if len(options) == 0 {
		return nil, ""
	}

	// If the current provider name wasn't matched, fall back to the first provider.
	if currentValue == "" {
		currentValue = string(options[0].Value)
	}

	category := acp.SessionConfigOptionCategoryModel
	sessionConfigOption := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(currentValue),
		acp.SessionConfigSelectOptions{Ungrouped: (*acp.SessionConfigSelectOptionsUngrouped)(&options)},
	)
	sessionConfigOption.Select.Id = acp.SessionConfigId(modelConfigID)
	sessionConfigOption.Select.Name = modelConfigName
	sessionConfigOption.Select.Category = &category
	sessionConfigOption.Select.Description = new(modelConfigDescription)

	return &sessionConfigOption, currentValue
}

// buildModeConfigOption builds a SessionConfigOption (select) exposing the
// available session modes (auto, plan, chat) as selectable values.
// Returns nil if modes aren't configured (shouldn't happen in practice).
func buildModeConfigOption(currentMode string) *acp.SessionConfigOption {
	const (
		modeConfigID          = "mode"
		modeConfigName        = "Mode"
		modeConfigDescription = "The operating mode for this session — Auto (full tool access), Plan (read-only planning), or Chat (read-only)"
	)

	category := acp.SessionConfigOptionCategoryMode
	options := []acp.SessionConfigSelectOption{
		{Value: acp.SessionConfigValueId(agent.ModeAuto), Name: "Auto"},
		{Value: acp.SessionConfigValueId(agent.ModePlan), Name: "Plan"},
		{Value: acp.SessionConfigValueId(agent.ModeChat), Name: "Chat"},
	}

	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(currentMode),
		acp.SessionConfigSelectOptions{Ungrouped: (*acp.SessionConfigSelectOptionsUngrouped)(&options)},
	)
	opt.Select.Id = acp.SessionConfigId(modeConfigID)
	opt.Select.Name = modeConfigName
	opt.Select.Category = &category
	desc := modeConfigDescription
	opt.Select.Description = &desc

	return &opt
}

const (
	// thinkingEffortConfigID is the ACP session config option ID for adjusting
	// the reasoning effort / thinking level.
	thinkingEffortConfigID = "reasoning_effort"
	// thinkingEffortConfigName is the human-readable label shown in the client.
	thinkingEffortConfigName = "Reasoning Effort"
	// thinkingEffortConfigDescription explains the option to the user.
	thinkingEffortConfigDescription = "Controls the thinking/reasoning effort for the current model. \"None\" disables thinking where supported; other levels are passed to the API, which maps them to the model's inference effort. \"Default\" restores the provider's configured default."
)

// thinkingEffortOptions lists the selectable effort levels in ascending
// order: the shared concrete levels from cmds.ThinkingEffortLevels plus
// "default" (restore the provider's configured default). Values on the shared
// subset come from cmds so the two surfaces never drift apart; only the
// display names are ACP-specific.
var thinkingEffortOptions = buildThinkingEffortOptions()

func buildThinkingEffortOptions() []acp.SessionConfigSelectOption {
	displayNames := map[string]string{
		"none":    "None (off)",
		"low":     "Low",
		"medium":  "Medium",
		"high":    "High",
		"xhigh":   "Extra High",
		"max":     "Max",
		"default": "Default (provider)",
	}
	levels := append(append([]string{}, cmds.ThinkingEffortLevels...), "default")
	opts := make([]acp.SessionConfigSelectOption, 0, len(levels))
	for _, lvl := range levels {
		opts = append(opts, acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(lvl),
			Name:  displayNames[lvl],
		})
	}
	return opts
}

// buildThinkingEffortConfigOption builds a SessionConfigOption (select)
// exposing the thinking effort levels. currentEffort is the agent's resolved
// effort; empty (no override, provider default) is displayed as "default".
func buildThinkingEffortConfigOption(currentEffort string) *acp.SessionConfigOption {
	if currentEffort == "" {
		currentEffort = "default"
	}
	category := acp.SessionConfigOptionCategoryThoughtLevel
	opt := acp.NewSessionConfigOptionSelect(
		acp.SessionConfigValueId(currentEffort),
		acp.SessionConfigSelectOptions{Ungrouped: (*acp.SessionConfigSelectOptionsUngrouped)(&thinkingEffortOptions)},
	)
	opt.Select.Id = acp.SessionConfigId(thinkingEffortConfigID)
	opt.Select.Name = thinkingEffortConfigName
	opt.Select.Category = &category
	desc := thinkingEffortConfigDescription
	opt.Select.Description = &desc

	return &opt
}

// currentThinkingValue returns the agent's current thinking configuration as
// a config option value id, reusing cmds.ThinkingLevelOf for the shared
// inverse mapping. The "default" fallback is ACP-specific display policy:
// an unset thinking config means "follow the provider default", which the
// option set exposes as an explicit "default" choice (previously a hardcoded
// "high" that misled whenever a provider's default differed).
func currentThinkingValue(a *agent.AIAgent) string {
	if a == nil {
		return "default"
	}
	if v := cmds.ThinkingLevelOf(a.Config.Resolved.Thinking, a.Config.Resolved.ThinkingEffort); v != "" {
		return v
	}
	return "default"
}

// switchSessionThinkingEffort updates the thinking configuration for an ACP
// session. level is one of the config option values ("none"/"low"/.../"max"
// or "default"): "none" disables thinking; the concrete levels set the effort
// (passed through for the API to map); "default" (or empty) clears the
// per-session override and restores the provider config default.
//
// The caller must hold sess.mu.
func switchSessionThinkingEffort(ctx context.Context, sess *ACPSession, level string, l *logger.Logger) error {
	if sess == nil || sess.agent == nil {
		return fmt.Errorf("session not initialized")
	}
	// External, untrusted input: reject anything outside the selectable set
	// before it reaches the LLM API or session meta (the TUI/channel /thinking
	// command validates via IsValidThinkingLevel too). Empty values are
	// tolerated and treated as "restore provider default".
	if level != "" && !cmds.IsValidThinkingLevel(level) {
		return fmt.Errorf("invalid reasoning effort level %q", level)
	}
	if level == "" {
		level = "default"
	}

	var thinking *bool
	var effort string
	stored := level
	if level == "default" {
		// Clear the per-session override: the agent falls back to the
		// provider config default and the persisted override is removed.
		thinking, effort = providerThinkingDefault(sess)
		stored = ""
	} else {
		thinking, effort = cmds.ThinkingOverrideFromLevel(level)
	}
	sess.agent.SetThinking(thinking, effort)

	// Persist the per-session override ("" = provider default) — same field
	// as the TUI/channel /thinking command (session.ThinkingLevel). Skip the
	// write when nothing changed.
	if sess.sessMgr != nil {
		if cur := sess.sessMgr.Current(); cur != nil && cur.ThinkingLevel != stored {
			cur.ThinkingLevel = stored
			cur.UpdatedAt = time.Now()
			if err := sess.sessMgr.UpdateMeta(cur); err != nil {
				if l != nil {
					l.Error(ctx, "ACP: failed to persist reasoning effort to session meta", err)
				}
			}
		}
	}

	if l != nil {
		l.Info(ctx, "ACP: switched reasoning effort",
			"session", sess.ID, "requested", level, "effective", effort)
	}
	return nil
}

// providerThinkingDefault resolves the active provider's configured thinking
// defaults, used when a reasoning effort override is cleared ("default").
func providerThinkingDefault(sess *ACPSession) (*bool, string) {
	if sess == nil || sess.cfg == nil {
		return nil, ""
	}
	providerName := ""
	if cur := sess.sessMgr.Current(); cur != nil {
		providerName = cur.ProviderName
	}
	if rp, err := llm.BuildProvider(sess.cfg, providerName); err == nil { // empty name → default
		return rp.Thinking, rp.ThinkingEffort
	}
	return nil, ""
}

// applySessionThinking applies the per-session thinking override (set via the
// ACP "Reasoning Effort" config option or the TUI/channel /thinking command)
// to a freshly built agent, so a resumed session keeps its thinking level.
//
// The session's own provider config is resolved so the "default" branch (and
// any unresolvable level) restores the true provider defaults rather than
// zero values. Sessions without an override keep the provider config default
// — this is a no-op.
func applySessionThinking(aiAgent *agent.AIAgent, cfg *config.Config, sess *session.Session) {
	if sess == nil || aiAgent == nil || sess.ThinkingLevel == "" {
		return
	}
	providerName := sess.ProviderName
	if cfg != nil {
		// Empty name → default provider (BuildProvider resolves it).
		if rp, err := llm.BuildProvider(cfg, providerName); err == nil {
			thinking, effort := cmds.EffectiveThinking(sess.ThinkingLevel, *rp)
			aiAgent.SetThinking(thinking, effort)
			return
		}
	}
	// Provider config unresolvable: apply the override directly. Concrete
	// levels don't need the provider defaults; only "default" would, and it
	// falls back to the agent's current (constructor-set) config.
	thinking, effort := cmds.EffectiveThinking(sess.ThinkingLevel, llm.ResolvedProvider{})
	aiAgent.SetThinking(thinking, effort)
}

// sendConfigOptionsUpdate sends one or more pre-built session config options as
// a SessionUpdate notification to the ACP client. All options are sent in a
// single update to avoid later updates overwriting earlier ones — the protocol
// treats ConfigOptionUpdate as a full replacement, not a delta.
//
// ctx governs the notification write. Synchronous request handlers pass their
// request ctx.
func sendConfigOptionsUpdate(ctx context.Context, conn *acp.AgentSideConnection, sessionID string, opts ...*acp.SessionConfigOption) {
	sendConfigOptionsUpdateMode(ctx, conn, false, sessionID, opts...)
}

// sendConfigOptionsUpdateAfterResponse is like sendConfigOptionsUpdate, but
// registers the notification via SessionUpdateAfterResponse so it is only
// written after the current request's response. Must be called from inside a
// request handler with the handler's ctx — used for the session-scoped
// notifications (available commands / config / usage) that clients key by the
// sessionId returned in the response.
func sendConfigOptionsUpdateAfterResponse(ctx context.Context, conn *acp.AgentSideConnection, sessionID string, opts ...*acp.SessionConfigOption) {
	sendConfigOptionsUpdateMode(ctx, conn, true, sessionID, opts...)
}

func sendConfigOptionsUpdateMode(ctx context.Context, conn *acp.AgentSideConnection, afterResponse bool, sessionID string, opts ...*acp.SessionConfigOption) {
	if conn == nil {
		return
	}
	configOpts := make([]acp.SessionConfigOption, 0, len(opts))
	for _, opt := range opts {
		if opt != nil {
			configOpts = append(configOpts, *opt)
		}
	}
	if len(configOpts) == 0 {
		return
	}
	send := conn.SessionUpdate
	if afterResponse {
		send = conn.SessionUpdateAfterResponse
	}
	_ = send(ctx, acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update: acp.SessionUpdate{
			ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
				ConfigOptions: configOpts,
			},
		},
	})
}

// toACPUsage converts Tachi's llm.Usage to ACP's PromptResponse usage format.
func toACPUsage(u *llm.Usage) *acp.Usage {
	if u == nil {
		return nil
	}
	return &acp.Usage{
		InputTokens:  int(u.InputTokens),
		OutputTokens: int(u.OutputTokens),
		TotalTokens:  int(u.InputTokens + u.OutputTokens),
	}
}

// switchSessionModel switches the LLM provider/model for the given ACP session
// to the provider identified by providerName. It updates the in-memory agent
// provider, context window, thinking defaults, and persists the new provider
// name to disk via the session manager.
//
// The caller must hold sess.mu.
func switchSessionModel(ctx context.Context, sess *ACPSession, providerName string, l *logger.Logger) error {
	if sess == nil || sess.cfg == nil {
		return fmt.Errorf("session not initialized")
	}
	if sess.agent == nil {
		return fmt.Errorf("agent not available")
	}

	// SetResolvedProvider resolves via the agent's FullConfig and swaps the
	// full resolved config (provider instance + context window + thinking
	// defaults) in one step. The provider is re-wrapped for usage billing —
	// a bare assignment would silently drop the new provider's calls off the
	// ledger, and the provider carries its own config name for row grouping
	// (see NewNamedProvider).
	resolved, err := sess.agent.SetResolvedProvider(providerName)
	if errors.Is(err, llm.ErrProviderNotFound) {
		return fmt.Errorf("provider %q not found", providerName)
	}
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}

	// Persist provider name to the on-disk session metadata.
	if sess.sessMgr != nil {
		if cur := sess.sessMgr.Current(); cur != nil && cur.ProviderName != resolved.Name {
			cur.ProviderName = resolved.Name
			// UpdateMeta is best-effort; don't fail the switch if persistence fails.
			if err := sess.sessMgr.UpdateMeta(cur); err != nil {
				l.Error(ctx, "ACP: failed to persist provider switch to session meta", err)
			}
		}
		// Re-apply the session's per-session thinking override (set via the
		// Reasoning Effort option), so a provider switch doesn't silently
		// drop the user's effort preference. Sessions without an override
		// keep the new provider's config default (applySessionThinking is a
		// no-op then) — symmetric with TUI's reapplySessionThinking.
		if cur := sess.sessMgr.Current(); cur != nil {
			applySessionThinking(sess.agent, sess.cfg, cur)
		}
	}

	return nil
}
