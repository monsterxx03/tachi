package agent

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// mcpSearcher picks what the MCP tool search matches with: the Jev judgment
// gate, or the pool's own keyword search.
//
// mcp_tool_search.matcher decides — "bm25" forces the keyword path (and is how
// the gate is turned off without emptying the jev block), "jev" forces the gate,
// and "auto" takes it only when config.Jev.Configured(), i.e. enabled AND
// holding a key, so an installation that never filled that block keeps exactly
// the behaviour it had. The gate also needs the pool it judges, so a nil pool
// (MCP off) short-circuits to the pool path either way.
//
// A query that NAMES what it wants is not affected by any of this: the gate
// resolves select: / exact and suffix names / mcp__server by lookup and never
// sends them to the service (see mcp.JevSearcher).
func (a *AIAgent) mcpSearcher(pool *mcp.DeferredPool) tools.MCPSearcher {
	var m config.MCPToolSearchConfig
	if cfg := a.Config.FullConfig; cfg != nil {
		m = cfg.MCPToolSearch
	}
	if pool == nil || m.Matcher == "bm25" {
		return mcp.NewPoolSearcher(pool)
	}
	judge := newJevJudge(a.Config.FullConfig, a.Config.Logger)
	if judge == nil {
		if m.Matcher == "jev" {
			a.Config.Logger.Warn(context.Background(),
				"MCP: matcher=jev but the jev block is not usable (needs enabled: true and an api_key); using keyword search")
		}
		return mcp.NewPoolSearcher(pool)
	}
	return mcp.NewJevSearcher(pool, judge, m.JevThreshold, time.Duration(m.JevBudget), a.usageRecorder())
}

// RefreshMCPSearch re-registers this agent's MCP search tool so a settings
// change reaches a session that is already running: the searcher is built at
// Configure time and the tool holds it, so a push that only rewrote the config
// object would leave the live agent matching the old way while the settings
// page described an effect it did not have. A new agent picks the settings up
// on its own; this is for the ones that exist (the desktop's
// mcp_tool_search row calls it on every save).
//
// It is a no-op when MCP is off (no manager, no pool): there is no search tool
// to rebuild.
func (a *AIAgent) RefreshMCPSearch() {
	mgr := a.Config.MCPManager
	pool := a.DeferredPool()
	if mgr == nil || pool == nil {
		return
	}
	a.RegisterTool(tools.NewMCPSearchToolsTool(a.mcpSearcher(pool), mgr))
}

// newJevJudge builds the System One client, or nil when the integration cannot
// run. It is built once per agent (the caller runs at Configure time) and kept
// for that agent's life: the client is concurrency-safe and carries no
// per-session state, and the pinned model belongs to the config the agent was
// built from.
//
// The transport is httpx's, so jev.timeout and jev.proxy are the same knobs the
// other HTTP-backed tools use; a blank model keeps pkg/jev's pinned version
// rather than the service's moving `jev-latest` alias.
func newJevJudge(cfg *config.Config, log *logger.Logger) mcp.Judge {
	if cfg == nil || !cfg.Jev.Configured() {
		return nil
	}
	client, err := jev.NewClient(cfg.Jev.APIKey,
		jev.WithModel(cfg.Jev.Model),
		jev.WithBaseURL(cfg.Jev.BaseURL),
		jev.WithHTTPClient(httpx.NewClient(time.Duration(cfg.Jev.Timeout), cfg.Jev.Proxy)),
		jev.WithLogger(log),
	)
	if err != nil {
		log.Warn(context.Background(), "MCP: jev tool search unavailable", "err", err)
		return nil
	}
	log.Info(context.Background(), "MCP: tool search matched by Jev",
		"model", client.Model(), "base_url", cfg.Jev.BaseURL)
	return client
}
