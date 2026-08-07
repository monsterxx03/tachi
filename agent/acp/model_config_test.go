package acp

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// newTestAgent builds an AIAgent wired to cfg. switchSessionModel resolves
// provider names via the agent's FullConfig (SetResolvedProvider), so tests
// that switch models must wire it. A temp usage recorder keeps provider
// calls off <home>/usage.
func newTestAgent(t *testing.T, provider llm.Provider, cfg *config.Config) *agent.AIAgent {
	t.Helper()
	a, _, err := agent.NewAIAgentWithConfig(context.Background(), agent.AgentConfig{
		Resolved:      &config.ResolvedProvider{Provider: provider},
		MaxIterations: 0,
		SkipConfigure: true,
		FullConfig:    cfg,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	return a
}

// newBareTestAgent builds a minimal bare agent (no FullConfig, no system
// setup) for tests that don't resolve provider names.
func newBareTestAgent(t *testing.T, provider llm.Provider, maxIter int) *agent.AIAgent {
	t.Helper()
	a, _, err := agent.NewAIAgentWithConfig(context.Background(), agent.AgentConfig{
		Resolved:      &config.ResolvedProvider{Provider: provider},
		MaxIterations: maxIter,
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	return a
}

func TestBuildModelConfigOption(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet"},
		},
	}

	opt, current := buildModelConfigOption(cfg, "anthropic")
	require.NotNil(t, opt)
	require.NotNil(t, opt.Select)
	assert.Equal(t, acp.SessionConfigId(modelConfigID), opt.Select.Id)
	assert.Equal(t, modelConfigName, opt.Select.Name)
	assert.Equal(t, "anthropic", current)
	require.NotNil(t, opt.Select.Options.Ungrouped)
	assert.Len(t, *opt.Select.Options.Ungrouped, 2)
	assert.Equal(t, acp.SessionConfigValueId("openai"), (*opt.Select.Options.Ungrouped)[0].Value)
	assert.Equal(t, acp.SessionConfigValueId("anthropic"), (*opt.Select.Options.Ungrouped)[1].Value)
}

func TestBuildModelConfigOption_NoProviders(t *testing.T) {
	opt, current := buildModelConfigOption(&config.Config{}, "")
	assert.Nil(t, opt)
	assert.Empty(t, current)
}

func TestSwitchSessionModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet", APIKey: "sk-anthropic"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := newTestAgent(t, provider, cfg)

	sess := &ACPSession{
		cfg:   cfg,
		agent: aiAgent,
	}

	err = switchSessionModel(context.Background(), sess, "anthropic", nil)
	require.NoError(t, err)

	assert.Equal(t, "anthropic", sess.resolveProviderName())
	assert.Equal(t, "anthropic", sess.ProviderType())
	assert.Equal(t, "claude-3-5-sonnet", sess.agent.Model())
}

func TestBuildThinkingEffortConfigOption(t *testing.T) {
	opt := buildThinkingEffortConfigOption("high")
	require.NotNil(t, opt)
	require.NotNil(t, opt.Select)
	assert.Equal(t, acp.SessionConfigId(thinkingEffortConfigID), opt.Select.Id)
	assert.Equal(t, thinkingEffortConfigName, opt.Select.Name)
	require.NotNil(t, opt.Select.Category)
	assert.Equal(t, acp.SessionConfigOptionCategoryThoughtLevel, *opt.Select.Category)
	assert.Equal(t, acp.SessionConfigValueId("high"), opt.Select.CurrentValue)
	require.NotNil(t, opt.Select.Options.Ungrouped)
	assert.Len(t, *opt.Select.Options.Ungrouped, len(thinkingEffortOptions))

	// 空 effort（无覆盖，provider 默认）→ 显示 "default"
	optDefault := buildThinkingEffortConfigOption("")
	require.NotNil(t, optDefault)
	assert.Equal(t, acp.SessionConfigValueId("default"), optDefault.Select.CurrentValue)
}

// TestThinkingEffortOptionsSharedWithCommands pins the value-set contract
// between the ACP "Reasoning Effort" config option and the /thinking command:
// the shared subset (cmds.ThinkingEffortLevels) must match exactly, with
// "default" appended as the ACP-only restore-provider-default choice.
func TestThinkingEffortOptionsSharedWithCommands(t *testing.T) {
	require.NotNil(t, thinkingEffortOptions)
	assert.Len(t, thinkingEffortOptions, len(cmds.ThinkingEffortLevels)+1)
	for i, lvl := range cmds.ThinkingEffortLevels {
		assert.Equal(t, acp.SessionConfigValueId(lvl), thinkingEffortOptions[i].Value)
		assert.NotEmpty(t, thinkingEffortOptions[i].Name, "level %q needs a display name", lvl)
	}
	last := thinkingEffortOptions[len(thinkingEffortOptions)-1]
	assert.Equal(t, acp.SessionConfigValueId("default"), last.Value)
	assert.NotEmpty(t, last.Name)
}

func TestCurrentThinkingValue(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "gpt-4o-mini")
	require.NoError(t, err)
	a := newBareTestAgent(t, provider, 0)

	// 默认（nil/空）→ "default"（跟随 provider 默认，不再硬编码 "high"）
	assert.Equal(t, "default", currentThinkingValue(a))

	// 显式 none → "none"
	f := false
	a.Config.Resolved.Thinking = &f
	assert.Equal(t, "none", currentThinkingValue(a))

	// 显式级别 → 级别本身
	a.Config.Resolved.Thinking = nil
	a.Config.Resolved.ThinkingEffort = "max"
	assert.Equal(t, "max", currentThinkingValue(a))

	// nil agent → "default"
	assert.Equal(t, "default", currentThinkingValue(nil))
}

func TestSwitchSessionThinkingEffort(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	sess := &ACPSession{
		cfg:   &config.Config{},
		agent: aiAgent,
	}

	// none → 显式关闭
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "none", nil))
	require.NotNil(t, sess.agent.Config.Resolved.Thinking)
	assert.False(t, *sess.agent.Config.Resolved.Thinking)
	assert.Equal(t, "", sess.agent.Config.Resolved.ThinkingEffort)

	// max → 原样透传（API 自己映射）
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "max", nil))
	assert.Nil(t, sess.agent.Config.Resolved.Thinking)
	assert.Equal(t, "max", sess.agent.Config.Resolved.ThinkingEffort)

	// nil agent → error
	require.Error(t, switchSessionThinkingEffort(context.Background(), &ACPSession{}, "high", nil))
}

// TestSwitchSessionThinkingEffort_PersistsToSession verifies the Reasoning
// Effort choice is written to session.ThinkingLevel (the same field the
// TUI/channel /thinking command uses), so it survives resume.
func TestSwitchSessionThinkingEffort_PersistsToSession(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("deepseek", "/tmp")
	require.NoError(t, err)

	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	sess := &ACPSession{
		cfg:     &config.Config{},
		agent:   aiAgent,
		sessMgr: sm,
	}

	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "none", nil))
	cur := sm.Current()
	require.NotNil(t, cur)
	assert.Equal(t, "none", cur.ThinkingLevel)

	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "high", nil))
	assert.Equal(t, "high", sm.Current().ThinkingLevel)

	// Persisted to disk — a fresh manager loading by ID sees it.
	loaded, err := sm.Load(cur.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "high", loaded.ThinkingLevel)
}

// TestApplySessionThinking verifies the resume-time restore of a session's
// thinking override onto a freshly built agent.
func TestApplySessionThinking(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com/v1"},
		},
	}
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)

	t.Run("no override is a no-op", func(t *testing.T) {
		a := newBareTestAgent(t, provider, 0)
		applySessionThinking(a, cfg, &session.Session{})
		assert.Nil(t, a.Config.Resolved.Thinking)
		assert.Equal(t, "", a.Config.Resolved.ThinkingEffort)
	})

	t.Run("effort override passes through unchanged", func(t *testing.T) {
		a := newBareTestAgent(t, provider, 0)
		applySessionThinking(a, cfg, &session.Session{ThinkingLevel: "max", ProviderName: "deepseek"})
		assert.Nil(t, a.Config.Resolved.Thinking)
		assert.Equal(t, "max", a.Config.Resolved.ThinkingEffort) // API maps it server-side
	})

	t.Run("none disables thinking", func(t *testing.T) {
		a := newBareTestAgent(t, provider, 0)
		applySessionThinking(a, cfg, &session.Session{ThinkingLevel: "none"})
		require.NotNil(t, a.Config.Resolved.Thinking)
		assert.False(t, *a.Config.Resolved.Thinking)
		assert.Equal(t, "", a.Config.Resolved.ThinkingEffort)
	})

	t.Run("nil session is a no-op", func(t *testing.T) {
		a := newBareTestAgent(t, provider, 0)
		applySessionThinking(a, cfg, nil) // must not panic
		assert.Nil(t, a.Config.Resolved.Thinking)
	})
}

func TestSwitchSessionModel_UnknownProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := newTestAgent(t, provider, cfg)
	sess := &ACPSession{cfg: cfg, agent: aiAgent}

	err = switchSessionModel(context.Background(), sess, "unknown", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSetSessionConfigOption(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet", APIKey: "sk-anthropic"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := newTestAgent(t, provider, cfg)

	ta := NewTachiAgent(cfg, "test")
	sess := ta.sessions.New(t.Context(), "/tmp", cfg, aiAgent, nil, nil)

	resp, err := ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId(modelConfigID),
			Value:     acp.SessionConfigValueId("anthropic"),
		},
	})
	require.NoError(t, err)
	assert.Len(t, resp.ConfigOptions, 3) // model + mode + reasoning_effort
	assert.Equal(t, "anthropic", sess.resolveProviderName())
	assert.Equal(t, "claude-3-5-sonnet", sess.agent.Model())
}

func TestSetSessionConfigOption_ReasoningEffort(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", APIKey: "sk-ds"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-ds", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	ta := NewTachiAgent(cfg, "test")
	sess := ta.sessions.New(t.Context(), "/tmp", cfg, aiAgent, nil, nil)

	// high → effort 原样写入 agent
	resp, err := ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId(thinkingEffortConfigID),
			Value:     acp.SessionConfigValueId("high"),
		},
	})
	require.NoError(t, err)
	assert.Nil(t, sess.agent.Config.Resolved.Thinking)
	assert.Equal(t, "high", sess.agent.Config.Resolved.ThinkingEffort)
	assert.Len(t, resp.ConfigOptions, 3) // model + mode + reasoning_effort

	// max → 原样透传（API 自己映射，flash 支持 max）
	_, err = ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId(thinkingEffortConfigID),
			Value:     acp.SessionConfigValueId("max"),
		},
	})
	require.NoError(t, err)
	assert.Nil(t, sess.agent.Config.Resolved.Thinking)
	assert.Equal(t, "max", sess.agent.Config.Resolved.ThinkingEffort)

	// none → 显式关闭思考
	_, err = ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId(thinkingEffortConfigID),
			Value:     acp.SessionConfigValueId("none"),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, sess.agent.Config.Resolved.Thinking)
	assert.False(t, *sess.agent.Config.Resolved.Thinking)
	assert.Equal(t, "", sess.agent.Config.Resolved.ThinkingEffort)
}

func TestSetSessionConfigOption_UnsupportedConfig(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	ta := NewTachiAgent(cfg, "test")
	sess := ta.sessions.New(t.Context(), "/tmp", cfg, aiAgent, nil, nil)

	resp, err := ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId("unsupported"),
			Value:     acp.SessionConfigValueId("anthropic"),
		},
	})
	assert.Error(t, err)
	assert.Empty(t, resp.ConfigOptions)
}

// TestSwitchSessionThinkingEffort_InvalidLevel pins Bug 2: the Reasoning
// Effort option is an external (untrusted) input — anything outside the
// selectable set must be rejected before it reaches the LLM API or session
// meta (symmetric with the TUI/channel /thinking command validation).
func TestSwitchSessionThinkingEffort_InvalidLevel(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("deepseek", "/tmp")
	require.NoError(t, err)

	sess := &ACPSession{cfg: &config.Config{}, agent: aiAgent, sessMgr: sm}

	// 非法级别 → 报错，且不落盘、不改 agent。
	err = switchSessionThinkingEffort(context.Background(), sess, "garbage", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid reasoning effort level")
	assert.Equal(t, "", sm.Current().ThinkingLevel)
	assert.Nil(t, aiAgent.Config.Resolved.Thinking)
	assert.Equal(t, "", aiAgent.Config.Resolved.ThinkingEffort)

	// 空串被容忍并视为恢复默认（不报错、不关闭思考）。
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "", nil))
	assert.Equal(t, "", sm.Current().ThinkingLevel)
	assert.Nil(t, aiAgent.Config.Resolved.Thinking)
	assert.Equal(t, "", aiAgent.Config.Resolved.ThinkingEffort)
}

// TestSwitchSessionThinkingEffort_DefaultClearsOverride pins the "default"
// option: it clears the per-session override AND restores the provider's
// configured thinking defaults (not just the model default).
func TestSwitchSessionThinkingEffort_DefaultClearsOverride(t *testing.T) {
	cfg := &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-test",
				Spec: config.ModelSpec{ThinkingLevel: "max"}},
		},
	}
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newBareTestAgent(t, provider, 0)

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("deepseek", "/tmp")
	require.NoError(t, err)

	sess := &ACPSession{cfg: cfg, agent: aiAgent, sessMgr: sm}

	// 先设 none（override 写盘）。
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "none", nil))
	assert.Equal(t, "none", sm.Current().ThinkingLevel)

	// default → 清除 override，agent 恢复 provider 配置的默认（max）。
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "default", nil))
	assert.Equal(t, "", sm.Current().ThinkingLevel)
	assert.Nil(t, aiAgent.Config.Resolved.Thinking)
	assert.Equal(t, "max", aiAgent.Config.Resolved.ThinkingEffort)
}

// TestSwitchSessionModel_KeepsThinkingOverride pins Addition A: switching the
// provider via the "model" config option must NOT silently drop a per-session
// reasoning effort override (the first request after the switch used to fall
// back to the new provider's default).
func TestSwitchSessionModel_KeepsThinkingOverride(t *testing.T) {
	cfg := &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-a"},
			{Name: "pro", Type: "openai", Model: "deepseek-v4-pro", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-b",
				Spec: config.ModelSpec{ThinkingLevel: "high"}},
		},
	}
	provider, err := llm.NewProvider("openai", "sk-a", "", "deepseek-v4-flash")
	require.NoError(t, err)
	aiAgent := newTestAgent(t, provider, cfg)

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("deepseek", "/tmp")
	require.NoError(t, err)

	sess := &ACPSession{cfg: cfg, agent: aiAgent, sessMgr: sm}

	// 设 per-session override: max。
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "max", nil))
	assert.Equal(t, "max", sm.Current().ThinkingLevel)
	assert.Equal(t, "max", aiAgent.Config.Resolved.ThinkingEffort)

	// 切换 provider（新 provider pro 配置默认 high）→ override 必须保持。
	require.NoError(t, switchSessionModel(context.Background(), sess, "pro", nil))
	assert.Equal(t, "pro", sm.Current().ProviderName)
	assert.Equal(t, "deepseek-v4-pro", aiAgent.Model())
	assert.Equal(t, "max", aiAgent.Config.Resolved.ThinkingEffort, "per-session override must survive a provider switch")

	// 无 override 时切 provider → 采用新 provider 的配置默认。
	require.NoError(t, switchSessionThinkingEffort(context.Background(), sess, "default", nil))
	require.NoError(t, switchSessionModel(context.Background(), sess, "deepseek", nil))
	// deepseek(flash) 无 thinking_level 配置 → 默认空 effort。
	assert.Equal(t, "", aiAgent.Config.Resolved.ThinkingEffort)
}
