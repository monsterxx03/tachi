package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Which searcher the MCP search tool gets: the Jev gate or the pool's own
// keyword search. The choice is what decides whether a sentence in Chinese can
// find a tool at all, so each combination is pinned here rather than inferred
// from the config tags.
func TestMCPSearcherPick(t *testing.T) {
	pool := mcp.NewDeferredPool()

	for _, tc := range []struct {
		name    string
		cfg     *config.Config // nil = the agent has no full config
		matcher string
		jev     config.JevConfig
		wantJev bool
	}{
		{name: "no config at all", cfg: nil, wantJev: false},
		{name: "matcher bm25 forces keywords", cfg: &config.Config{}, matcher: "bm25", jev: config.JevConfig{Enabled: true, APIKey: "k"}, wantJev: false},
		{name: "auto without a key", cfg: &config.Config{}, matcher: "auto", jev: config.JevConfig{Enabled: true}, wantJev: false},
		{name: "auto while disabled", cfg: &config.Config{}, matcher: "auto", jev: config.JevConfig{APIKey: "k"}, wantJev: false},
		{name: "auto with a usable block", cfg: &config.Config{}, matcher: "auto", jev: config.JevConfig{Enabled: true, APIKey: "k"}, wantJev: true},
		{name: "jev forced but unusable", cfg: &config.Config{}, matcher: "jev", jev: config.JevConfig{}, wantJev: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg != nil {
				cfg.MCPToolSearch.Matcher = tc.matcher
				cfg.MCPToolSearch.JevThreshold = 0.35
				cfg.Jev = tc.jev
			}
			a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
				Resolved:      &llm.ResolvedProvider{},
				SkipConfigure: true,
				UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
				FullConfig:    cfg,
			})
			require.NoError(t, err)

			got := a.mcpSearcher(pool)
			_, isJev := got.(*mcp.JevSearcher)
			assert.Equal(t, tc.wantJev, isJev, "searcher = %T", got)
		})
	}
}

// The settings object is the LIVE one: the same agent asked again after a save
// gets the searcher the new settings describe. That is what the desktop row's
// RefreshMCPSearch relies on, and why a push does not have to rebuild the agent.
func TestMCPSearcherFollowsTheLiveConfig(t *testing.T) {
	cfg := &config.Config{
		MCPToolSearch: config.MCPToolSearchConfig{Matcher: "bm25"},
		Jev:           config.JevConfig{Enabled: true, APIKey: "k"},
	}
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{},
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
		FullConfig:    cfg,
	})
	require.NoError(t, err)
	pool := mcp.NewDeferredPool()

	_, beforeIsJev := a.mcpSearcher(pool).(*mcp.JevSearcher)
	assert.False(t, beforeIsJev)

	cfg.MCPToolSearch.Matcher = "auto" // as a save would leave it
	after := a.mcpSearcher(pool)
	_, afterIsJev := after.(*mcp.JevSearcher)
	assert.True(t, afterIsJev, "searcher = %T", after)
	assert.Equal(t, tools.ProseSearchAdvice, after.(tools.MCPSearchAdvice).SearchAdvice())
}

// The reminder's shape follows the matcher, read LIVE: a server summary tells
// the reader to describe a scenario, which only the Jev search can use. Under
// the keyword matcher that line would be a promise nothing keeps, so the catalog
// (which carries the names the keywords match) stays.
func TestMCPSummariesOnlyUnderTheJevMatcher(t *testing.T) {
	cfg := &config.Config{
		MCPToolSearch: config.MCPToolSearchConfig{Matcher: "auto"},
		Jev:           config.JevConfig{Enabled: true, APIKey: "k"},
	}
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{},
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
		FullConfig:    cfg,
	})
	require.NoError(t, err)

	pool := mcp.NewDeferredPool()
	tool := &mcp.DeferredTool{Name: "mcp__lixinger__list_companies", ServerName: "lixinger",
		Description: "获取 A 股公司详细信息列表"}
	pool.Add(tool)
	adapter := &deferredToolProviderAdapter{a: a, pool: pool}

	// Jev gate active: one line per server, no LLM and no manager yet, so the
	// heuristic layer is what answers (and the line is still worth rendering).
	got := adapter.Summaries()
	require.Len(t, got, 1)
	assert.Equal(t, "lixinger", got[0].Server)
	assert.NotEmpty(t, got[0].Text)

	// Flipping the setting switches the shape with no rebuild — the reminder
	// asks on every generation.
	cfg.MCPToolSearch.Matcher = "bm25"
	assert.Empty(t, adapter.Summaries())

	// And a half-filled jev block is not the gate either.
	cfg.MCPToolSearch.Matcher = "auto"
	cfg.Jev.APIKey = ""
	assert.Empty(t, adapter.Summaries())
}

// A gate that degraded is a keyword search in everything but name, so the
// reminder goes back to listing tools by name — the block the keyword matcher
// can actually use — and says so in the log. A fresh searcher (a settings save,
// a new agent) starts clean.
func TestMCPSummariesStopAfterADegradedSearch(t *testing.T) {
	cfg := &config.Config{
		MCPToolSearch: config.MCPToolSearchConfig{Matcher: "auto"},
		Jev:           config.JevConfig{Enabled: true, APIKey: "k"},
	}
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{},
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
		FullConfig:    cfg,
	})
	require.NoError(t, err)

	pool := mcp.NewDeferredPool()
	pool.Add(&mcp.DeferredTool{Name: "mcp__lixinger__list_companies", ServerName: "lixinger", Description: "公司列表"})
	adapter := &deferredToolProviderAdapter{a: a, pool: pool}
	require.NotEmpty(t, adapter.Summaries())

	a.markJevSearchDegraded(errors.New("529 overloaded"))
	assert.False(t, a.mcpSearchUsesJev())
	assert.Empty(t, adapter.Summaries(), "a degraded gate gets the catalog, not summaries")

	// Building the searcher again is a fresh attempt.
	a.mcpSearcher(pool)
	assert.True(t, a.mcpSearchUsesJev())
	assert.NotEmpty(t, adapter.Summaries())
}

// Refreshing with MCP off is a no-op rather than a panic: there is no search
// tool to rebuild.
func TestRefreshMCPSearchWithoutMCP(t *testing.T) {
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{},
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	require.NoError(t, err)
	a.RefreshMCPSearch()
}

// A nil pool (MCP off) must still produce a working searcher: the tool is
// registered before the servers connect, and a search during that window has to
// answer with an empty list rather than a nil dereference.
func TestMCPSearcherNilPool(t *testing.T) {
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{},
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
		FullConfig: &config.Config{
			MCPToolSearch: config.MCPToolSearchConfig{Matcher: "auto"},
			Jev:           config.JevConfig{Enabled: true, APIKey: "k"},
		},
	})
	require.NoError(t, err)

	got := a.mcpSearcher(nil)
	_, isJev := got.(*mcp.JevSearcher)
	assert.False(t, isJev, "no pool means nothing to judge")
	assert.Empty(t, got.Search(context.Background(), "anything", 5))
}
