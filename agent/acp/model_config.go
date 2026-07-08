package acp

import (
	"context"
	"fmt"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

const (
	// modelConfigID is the ACP session config option ID for switching the LLM model.
	modelConfigID = "model"
	// modelConfigName is the human-readable label shown in the client.
	modelConfigName = "Model"
	// modelConfigDescription is shown to the user to explain the option.
	modelConfigDescription = "The LLM provider/model to use for this session."
)

// buildModelState builds a SessionModelState exposing the configured providers
// as selectable models for the Zed model selector.
func buildModelState(cfg *config.Config, currentProviderName string) *acp.SessionModelState {
	if cfg == nil || len(cfg.Providers) == 0 {
		return nil
	}

	models := make([]acp.ModelInfo, 0, len(cfg.Providers))
	var currentModelID acp.ModelId

	for _, p := range cfg.Providers {
		if p.Name == "" || p.Model == "" {
			continue
		}
		modelID := acp.ModelId(p.Name + "/" + p.Model)
		if p.Name == currentProviderName {
			currentModelID = modelID
		}
		models = append(models, acp.ModelInfo{
			ModelId: modelID,
			Name:    fmt.Sprintf("%s (%s)", p.Name, p.Model),
		})
	}

	if len(models) == 0 {
		return nil
	}

	if currentModelID == "" {
		currentModelID = models[0].ModelId
	}

	return &acp.SessionModelState{
		AvailableModels: models,
		CurrentModelId:  currentModelID,
	}
}

// configOptionSlice returns a non-nil config option slice for inclusion in
// session responses.
func configOptionSlice(opt *acp.SessionConfigOption) []acp.SessionConfigOption {
	if opt == nil {
		return nil
	}
	return []acp.SessionConfigOption{*opt}
}

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

// sendModelConfigOption sends a pre-built session config option as a
// SessionUpdate notification to the ACP client.
func sendModelConfigOption(conn *acp.AgentSideConnection, opt *acp.SessionConfigOption, sessionID string) {
	if conn == nil || opt == nil {
		return
	}
	_ = conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update: acp.SessionUpdate{
			ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
				ConfigOptions: []acp.SessionConfigOption{*opt},
			},
		},
	})
}

// sendModelConfigUpdate builds the model config option and sends it to the
// ACP client. This is a convenience wrapper around sendModelConfigOption used
// by NewSession / LoadSession / ResumeSession where the option isn't otherwise
// needed by the caller.
func sendModelConfigUpdate(conn *acp.AgentSideConnection, cfg *config.Config, currentProviderName, sessionID string) {
	if conn == nil || cfg == nil {
		return
	}
	opt, _ := buildModelConfigOption(cfg, currentProviderName)
	sendModelConfigOption(conn, opt, sessionID)
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
// provider, context window, and persists the new provider name to disk via the
// session manager.
//
// The caller must hold sess.mu.
func switchSessionModel(sess *ACPSession, providerName string) error {
	if sess == nil || sess.cfg == nil {
		return fmt.Errorf("session not initialized")
	}
	if sess.agent == nil {
		return fmt.Errorf("agent not available")
	}

	pCfg := sess.cfg.FindProvider(providerName)
	if pCfg == nil {
		return fmt.Errorf("provider %q not found", providerName)
	}

	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}

	provider, err := llm.NewProvider(
		resolved.Type,
		resolved.APIKey,
		resolved.BaseURL,
		resolved.Model,
	)
	if err != nil {
		return fmt.Errorf("create provider: %w", err)
	}

	// Update the agent provider and context window atomically.
	sess.agent.SetProvider(provider)
	sess.agent.SetContextWindow(resolved.ContextWindow)

	// Persist provider name to the on-disk session metadata.
	if sess.sessMgr != nil {
		if cur := sess.sessMgr.Current(); cur != nil && cur.ProviderName != resolved.Name {
			cur.ProviderName = resolved.Name
			// UpdateMeta is best-effort; don't fail the switch if persistence fails.
			if err := sess.sessMgr.UpdateMeta(cur); err != nil {
				debuglog.DefaultLogger.Log("ACP: failed to persist provider switch to session meta: %v", err)
			}
		}
	}

	return nil
}
