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
	// A searcher is built once per agent (Configure, and again on a pushed
	// settings change), and a fresh one is a fresh attempt: the health flag
	// below only describes the searcher it was set by.
	a.jevSearchDegraded.Store(false)
	m := a.mcpSearchConfig()
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
	return mcp.NewJevSearcher(pool, judge, m.JevThreshold, time.Duration(m.JevBudget), a.usageRecorder(), a.markJevSearchDegraded)
}

// markJevSearchDegraded records that the gate answered with the keyword path,
// and asks for the deferred-tools block to be rendered again — as the per-tool
// catalog, because names are what a keyword search can match on.
//
// It deliberately does not clear itself on a later success: a service that
// failed once may fail again on the next search, and the conservative shape is
// the one that keeps the tools reachable. The flag is per agent, i.e. per
// session, and a new searcher (a settings change, a new agent) starts clean.
func (a *AIAgent) markJevSearchDegraded(err error) {
	a.jevSearchDegraded.Store(true)
	a.Config.Logger.Warn(context.Background(),
		"MCP: tool search degraded to keywords; the reminder will list tools by name again", "err", err)
	a.NotifyDeferredToolsAdded()
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

// mcpSearchConfig is the live mcp_tool_search block (the settings object is
// mutated in place by a save, so reading it here is what makes a pushed change
// take effect).
func (a *AIAgent) mcpSearchConfig() config.MCPToolSearchConfig {
	if cfg := a.Config.FullConfig; cfg != nil {
		return cfg.MCPToolSearch
	}
	return config.MCPToolSearchConfig{}
}

// mcpSearchUsesJev reports whether a prose query would be judged rather than
// keyword-matched under the CURRENT config — the condition mcpSearcher acts on,
// without building the client.
//
// The reminder asks this instead of caching the answer: a server summary guides
// the reader to describe a SCENARIO, which only a matcher that reads sentences
// can use, so the block's shape has to follow the same setting the search does
// (and follow it mid-session, which is when the setting changes).
func (a *AIAgent) mcpSearchUsesJev() bool {
	m := a.mcpSearchConfig()
	if m.Matcher == "bm25" {
		return false
	}
	cfg := a.Config.FullConfig
	// Configured is not the same as working: a gate that already degraded this
	// session is a keyword search in everything but name, so the reminder goes
	// back to the names it matches on (markJevSearchDegraded).
	return cfg != nil && cfg.Jev.Configured() && !a.jevSearchDegraded.Load()
}

// mcpSummarizer builds the per-server summarizer for the reminder: the pool it
// groups, the descriptions the servers gave about themselves at connect time,
// and the generated layer backed by the on-disk cache. Each missing layer just
// means the next one answers (a nil Manager leaves the heuristic).
func (a *AIAgent) mcpSummarizer(pool *mcp.DeferredPool) *mcp.ServerSummarizer {
	sum := &mcp.ServerSummarizer{Pool: pool}
	if mgr := a.Config.MCPManager; mgr != nil {
		sum.Instructions = mgr.ServerInstructions
	}
	sum.Generate = a.mcpSummaryGenerator().Generate
	return sum
}

// mcpSummaryGenerator builds the generator once per agent, and keeps it: the
// in-flight and given-up bookkeeping is per process, and a generator rebuilt per
// reminder would retry a failing generation on every single message.
func (a *AIAgent) mcpSummaryGenerator() *mcp.SummaryGenerator {
	a.summaryGenOnce.Do(func() {
		a.summaryGen = &mcp.SummaryGenerator{
			// The provider is resolved at CALL time: a session can switch
			// providers, and a summary is maintenance work with no session of
			// its own.
			Provider:  a.Provider,
			Cache:     mcp.NewSummaryCache(config.MCPSummariesDir()),
			Logger:    a.Config.Logger,
			SessionID: a.currentSessionID,
		}
	})
	return a.summaryGen
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
