// Package mcp manages MCP (Model Context Protocol) server connections
// and exposes their tools through the Tachi tool registry.
package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// MCPClient is the interface for MCP client operations used by Manager.
// Extracted for testability — the real implementation is *client.Client
// from github.com/mark3labs/mcp-go/client.
type MCPClient interface {
	Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error)
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	// RootListChanges tells the server that the answer to a previous roots/list may have moved
	// (see Manager.NotifyRootsChanged). Part of the interface because the clients are held only
	// through it — a server that cached our workspace must be reachable to be told.
	RootListChanges(ctx context.Context) error
	Close() error
}

// Manager manages the lifecycle of MCP client connections and their tools.
//
// Manager also owns the ToolSearch state — DeferredPool (all discovered MCP
// tools, searchable but not yet registered with the LLM), DiscoveredSet (the
// subset of tools the LLM has explicitly opted into via MCPSearchTools), and
// the initDone channel that signals async ConnectAll completion. These are
// owned here because they share the manager's lifecycle: they're created when
// a manager is built and torn down when it's closed. Callers (AIAgent,
// channel.Manager) used to hold them as separate fields; that worked but
// duplicated lifecycle bookkeeping at every layer. Now there is one source of
// truth.
//
// Pool / DiscoveredSet / InitDone are always non-nil after NewManager.
// MarkInitDone closes the init channel exactly once when ConnectAll-style
// population is finished.
type Manager struct {
	clients map[string]MCPClient // server name -> client
	logger  *logger.Logger
	mu      sync.RWMutex

	pool     *DeferredPool
	initDone chan struct{}

	initDoneOnce sync.Once

	// Server-level usage descriptions from MCP `initialize` (`instructions`),
	// keyed by server name. Empty string = the server did not provide one.
	// Guarded by mu.
	serverInstructions map[string]string

	// Per-session discovered-tool sets, keyed by session ID. Lazily created
	// by SetFor on first use and restored from the session's persistence
	// file, so each session keeps its own set of tools loaded via
	// MCPSearchTools across restarts. Protected by mu.
	sets map[string]*DiscoveredSet

	// autoLoadNames holds the full names of auto-load tools (session-
	// independent baselines). Injected into every per-session set at
	// creation, so they are visible without being searched for. Populated
	// by PopulateFromConnect; protected by mu.
	autoLoadNames []string

	// Tool result truncation — configured from config.ToolResult, and REPLACEABLE at runtime
	// (the settings page's `tool_result` row). Atomically held rather than guarded by mu, so a
	// tool call reading it can never race a save and the setter can never deadlock against the
	// manager's own lock. 0 means no limit (opt-out).
	maxResultChars atomic.Int64
	resultFileDir  atomic.Pointer[string]

	// roots answers a server's roots/list: the directories this client is working in.
	// nil means "declare nothing", which is the right default — a client that advertises the
	// capability owes the server an answer, and one that cannot name its workspace must not
	// promise it. Protected by mu (set once at wiring time; read on every ask).
	roots RootsFunc

	// Tool list refresh — background polling for HTTP MCP servers.
	refresher   *Refresher
	toolCache   *toolListCache
	serverTypes map[string]bool                   // server name -> is HTTP (true) or stdio (false)
	serverCfgs  map[string]config.MCPServerConfig // server name -> config (for whitelist/blacklist filtering)
}

// Root is one workspace root as a server sees it: a file:// URI plus a display name. It is the
// SDK's type, aliased so a caller (the desktop) can build the list without importing the MCP
// SDK for one struct.
type Root = mcp.Root

// RootsFunc supplies the workspace roots a server may treat as its own (file:// URIs).
//
// It is the answer to MCP's `roots` capability: a server that supports it asks the client
// "which directories am I working in", and several confine file access to the answer — the
// official chrome-devtools server, for one, refuses any path outside the roots it was given
// (and, when the client declares no roots at all, falls back to the OS temp dir alone, which
// is why a tool that exports a file into the project fails with "not within any of the
// configured workspace roots").
//
// It is a FUNCTION, not a list, because the answer changes: the desktop hosts many sessions
// in different trees, and a root can be added or moved mid-conversation.
type RootsFunc func() []mcp.Root

// SetRootsFunc makes every connection this manager opens from now on declare the roots
// capability (and answer roots/list with it). nil — the default — declares nothing.
//
// The capability is attached to the CLIENT rather than to the initialize request Tachi
// builds, because that is where the SDK keeps it: `Client.Initialize` merges Roots into the
// capabilities it sends as soon as a roots handler is configured, and answers the server's
// request at the transport level. Both transports therefore have to construct their client
// that way (see connectStdio / connectHTTP).
func (m *Manager) SetRootsFunc(f RootsFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roots = f
}

// rootsFunc returns the configured provider, or nil.
func (m *Manager) rootsFunc() RootsFunc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roots
}

// NotifyRootsChanged tells every connected server that the roots it was given may have moved, so
// a roots-aware one re-asks instead of holding a stale workspace: chrome-devtools, for one, asks
// once (on connect or at its first file operation) and refreshes only when the client says so —
// so a session opened in a NEW directory after that would be denied a write there, which is the
// failure the roots capability exists to fix, one directory later.
//
// Best-effort by design: a server that does not implement roots ignores the notification, and one
// that has gone away must not fail the gesture that triggered this (adding a root, editing a
// project) — the caller's action already succeeded.
func (m *Manager) NotifyRootsChanged(ctx context.Context) {
	if m.rootsFunc() == nil {
		return // we never declared the capability; there is nothing to invalidate
	}
	m.mu.RLock()
	clients := make(map[string]MCPClient, len(m.clients))
	for name, c := range m.clients {
		clients[name] = c
	}
	m.mu.RUnlock()

	for name, c := range clients {
		if err := c.RootListChanges(ctx); err != nil {
			m.logger.Warn(ctx, "MCP: roots-changed notification failed", "server", name, "err", err)
		}
	}
}

// clientOptions returns the SDK options every connection is built with. Today that is the
// roots handler (when a provider is configured) — the one capability Tachi answers.
func (m *Manager) clientOptions() []client.ClientOption {
	f := m.rootsFunc()
	if f == nil {
		return nil
	}
	return []client.ClientOption{client.WithRootsHandler(mcpRoots{f})}
}

// mcpRoots answers a server's roots/list by asking the provider at THAT moment: a server
// re-asks after a roots-changed notification, and a long-lived connection must not keep
// answering with the tree the user was in an hour ago.
type mcpRoots struct{ fn RootsFunc }

func (r mcpRoots) ListRoots(ctx context.Context, _ mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	roots := r.fn()
	if roots == nil {
		// An empty list is a legitimate answer (the client has no workspace yet); nil would
		// marshal as `"roots": null`, which is not what the schema says.
		roots = []mcp.Root{}
	}
	logger.FromContext(ctx).Info(ctx, "MCP: answered roots/list", "count", len(roots))
	return &mcp.ListRootsResult{Roots: roots}, nil
}

// NewManager creates an empty MCP client manager from the given full config.
// It derives the tool result truncation settings (max chars + oversized
// result file dir) from cfg.ToolResult. Discovered-tool persistence is
// per-session: sets are created lazily via SetFor once a session ID is known,
// each persisted to ~/.tachi/session/<id>/mcp_discovered.json.
//
// A nil cfg yields zero-value settings: no result truncation limit and no
// result file dir (used by tests and callers that only need the pool). Old
// tool result files in fileDir are cleaned up on construction (best-effort,
// errors are logged).
func NewManager(ctx context.Context, cfg *config.Config, l *logger.Logger) *Manager {
	var (
		maxChars  int
		resultDir string
	)
	if cfg != nil {
		maxChars = cfg.ToolResult.MaxResultChars()
		resultDir = cfg.ToolResult.ResultFileDir()
	}

	m := &Manager{
		clients:     make(map[string]MCPClient),
		logger:      l,
		pool:        NewDeferredPool(),
		sets:        make(map[string]*DiscoveredSet),
		initDone:    make(chan struct{}),
		toolCache:   newToolListCache(),
		serverTypes: make(map[string]bool),
	}
	m.SetToolResultLimits(resultDir, maxChars)
	if dir := m.ToolResultFileDir(); dir != "" {
		m.cleanupOldToolResults(ctx, dir, defaultToolResultMaxAge)
	}
	return m
}

// Pool returns the deferred-tool pool owned by this manager. Always non-nil.
func (m *Manager) Pool() *DeferredPool { return m.pool }

// SetServerInstructions records a server's own usage description from MCP
// `initialize` (empty when the server did not provide one — which overwrites
// any stale text from an earlier connection, on purpose).
func (m *Manager) SetServerInstructions(server, instructions string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverInstructions == nil {
		m.serverInstructions = make(map[string]string)
	}
	m.serverInstructions[server] = instructions
}

// ServerInstructions returns the description the server gave about itself, or
// "" when it gave none.
func (m *Manager) ServerInstructions(server string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.serverInstructions[server]
}

// ToolResultMaxChars returns the configured max chars for tool results.
// 0 means no limit.
func (m *Manager) ToolResultMaxChars() int { return int(m.maxResultChars.Load()) }

// ToolResultFileDir returns the configured directory for storing oversized results.
func (m *Manager) ToolResultFileDir() string {
	if p := m.resultFileDir.Load(); p != nil {
		return *p
	}
	return ""
}

// SetToolResultLimits replaces where and how large an oversized MCP tool result may be spilled.
//
// It exists because the manager BAKES the two values in at construction (NewManager reads
// config.ToolResult once), and the values are read per tool call — so without a setter the
// settings page's `tool_result` row could only take effect for web/bash tools, leaving MCP
// results truncated by the old limits while the page said the change was live.
func (m *Manager) SetToolResultLimits(dir string, maxChars int) {
	m.maxResultChars.Store(int64(maxChars))
	m.resultFileDir.Store(&dir)
}

// SetFor returns the discovered-tools set for the given session, lazily
// creating it and restoring its persisted state from disk on first use.
// Sessions are isolated: tools discovered in one session never leak into
// another. Returns nil for an empty session ID (no session context — e.g.
// one-off runs without session recording).
func (m *Manager) SetFor(sessionID string) *DiscoveredSet {
	if sessionID == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sets[sessionID]; ok {
		return s
	}

	s := NewDiscoveredSet(config.MCPDiscoveredFile(sessionID))
	m.sets[sessionID] = s
	// Auto-load tools are the session-independent baseline of every set.
	for _, name := range m.autoLoadNames {
		s.addNoPersist(name)
	}
	m.restoreDiscoveredFromDisk(context.Background(), s)
	return s
}

// TrackerFor implements tools.MCPSearchTrackerProvider: it routes the
// MCPSearchTools tool's "mark as discovered" writes to the set of the session
// active in the calling context. Returns nil when there is no session context
// (the search tool then still returns results but does not persist them).
// The explicit empty-ID check avoids boxing a typed nil into a non-nil
// interface.
func (m *Manager) TrackerFor(sessionID string) tools.MCPSearchTracker {
	if sessionID == "" {
		return nil
	}
	return m.SetFor(sessionID)
}

// EachDiscoveredSet iterates over all per-session discovered sets created so
// far. Used for global cleanups that apply to every session (e.g. a server
// being unregistered or a tool removed by refresh). The callback runs while
// the manager's read lock is held — it must not call back into the manager.
func (m *Manager) EachDiscoveredSet(fn func(s *DiscoveredSet)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sets {
		fn(s)
	}
}

// restoreDiscoveredFromDisk merges the persisted discovered-tool names back
// into the given set, keeping only tools that still exist in the deferred
// pool. Called by SetFor when a session's set is first created. Auto-load
// tools already added by PopulateFromConnect are left untouched (AddAll is
// idempotent). Names for servers that were removed from config or renamed are
// warned about and dropped from the persisted file.
func (m *Manager) restoreDiscoveredFromDisk(ctx context.Context, set *DiscoveredSet) {
	if set.persistPath == "" {
		return
	}

	names, err := set.LoadFromDisk()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Error(ctx, "MCP: failed to read persisted discovered tools, ignoring", err, "path", set.persistPath)
			return
		}
		// First run: no history file yet. Persist the current set
		// (auto-load tools) so future restarts have a baseline.
		if saveErr := set.Save(); saveErr != nil {
			m.logger.Error(ctx, "MCP: failed to persist discovered tools", saveErr, "path", set.persistPath)
		}
		return
	}

	// Pool not populated yet (MCP servers still connecting, or all failed):
	// there is no way to tell real stale tools from tools that will come
	// back once a server connects. Keep the whole history — dropping here
	// would permanently erase the session's discovery record. Stale cleanup
	// resumes once the pool is populated (backfillAutoLoadIntoSets).
	if m.pool.Len() == 0 && len(names) > 0 {
		set.AddAll(names)
		m.logger.Info(ctx, "MCP: kept persisted discovered tools (pool not populated yet, stale check deferred)", "count", len(names), "path", set.persistPath)
		return
	}

	// Split persisted names into those still present in the pool and stale
	// ones (server removed from config / tool renamed on a connected
	// server). A tool whose server is still configured but did not connect
	// this run (connection failed or still in progress) is kept — it was
	// not deleted, it just hasn't been discovered yet.
	var restored, stale []string
	for _, name := range names {
		if m.pool.Get(name) != nil {
			restored = append(restored, name)
		} else if m.serverStillConfigured(name) {
			restored = append(restored, name)
		} else {
			stale = append(stale, name)
		}
	}

	// Warn about stale entries — they can no longer be loaded — and drop
	// them from the in-memory set first; the final write below persists the
	// cleaned state.
	for _, name := range stale {
		m.logger.Warn(ctx, "MCP: dropping stale discovered tool from persisted file (not found in pool)", "tool", name, "path", set.persistPath)
		set.removeNoPersist(name)
	}

	switch {
	case len(restored) > 0:
		// Merging restores writes the full set (auto-load + restored) back
		// to disk, which implicitly drops the stale names.
		set.AddAll(restored)
	case len(stale) > 0:
		// Everything was stale: rewrite the file so it no longer lists the
		// dropped names (keeps whatever auto-load tools are in the set).
		if saveErr := set.Save(); saveErr != nil {
			m.logger.Error(ctx, "MCP: failed to persist cleaned discovered tools", saveErr, "path", set.persistPath)
		}
	}

	if len(restored) > 0 {
		m.logger.Info(ctx, "MCP: restored discovered tools from disk", "count", len(restored), "dropped", len(stale), "total", set.Len(), "path", set.persistPath)
	} else if len(stale) > 0 {
		m.logger.Info(ctx, "MCP: no discoverable tools restored; cleaned stale entries", "dropped", len(stale), "path", set.persistPath)
	}
}

// serverStillConfigured reports whether the tool's MCP server is present in
// the configured server set. A configured-but-unconnected server means the
// tool simply hasn't been discovered this run (server still connecting or
// failed this time) — not that the tool was deleted — so the persisted name
// should be kept. Only tools whose server is gone from the config are real
// stale entries.
//
// Caller must hold m.mu (restoreDiscoveredFromDisk runs under it via
// SetFor / backfillAutoLoadIntoSets).
func (m *Manager) serverStillConfigured(toolName string) bool {
	for server := range m.serverCfgs {
		if strings.HasPrefix(toolName, tools.MCPServerPrefix+server+"__") {
			return true
		}
	}
	return false
}

// InitDone returns a channel that is closed once the manager's async
// initialization (ConnectAll + pool population) has finished. Callers can
// select on it (or use WaitInit) to block until tools are ready.
//
// If the manager is never populated (e.g. no servers configured), the channel
// stays open. Callers that don't care should not block on it.
func (m *Manager) InitDone() <-chan struct{} { return m.initDone }

// MarkInitDone closes the InitDone channel, idempotently. Should be called
// once after ConnectAll-style population has finished.
func (m *Manager) MarkInitDone() {
	m.initDoneOnce.Do(func() { close(m.initDone) })
}

// WaitInit blocks until MarkInitDone is called or the context is cancelled.
// Returns ctx.Err() on cancellation, nil on success.
func (m *Manager) WaitInit(ctx context.Context) error {
	select {
	case <-m.initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RecomputeAutoLoad re-derives the auto-load set from the tools ALREADY in the pool under a new
// ToolSearch configuration, and reports what changed.
//
// It exists for the settings page's `mcp_tool_search` row, where reconnecting every server to
// re-learn a decision that depends only on tools we already have would be absurd — and where
// PopulateFromConnect cannot simply be re-run: it APPENDS to autoLoadNames (a second run would
// duplicate every name) and it re-handshakes every server.
//
// Two things move together, because either alone is invisible:
//
//   - `autoLoadNames`, the baseline every session's discovered set is injected from;
//   - the discovered sets of the sessions that already exist, which are what the model
//     actually sees (a session open across the change would otherwise keep the old view).
//
// The caller still owns the REGISTRY half (registering the tools of `added`, unregistering
// `removed`), because a registry is per agent while the pool is per process.
//
// The threshold is compared against the pool's size rather than against a fresh discovery: the
// pool is what a search would have to sift through, and it is the number a reader can be told
// without another round trip. PopulateFromConnect counts the same thing (the tools that survive
// the whitelist/blacklist filter), so the two agree on where the boundary falls.
func (m *Manager) RecomputeAutoLoad(cfg *config.Config) (added, removed []*DeferredTool) {
	pooled := m.pool.All()
	useToolSearch := cfg.MCPToolSearch.IsEnabled() && len(pooled) > cfg.MCPToolSearch.MinToolsForSearch

	m.mu.Lock()
	was := make(map[string]bool, len(m.autoLoadNames))
	for _, n := range m.autoLoadNames {
		was[n] = true
	}
	now := make([]string, 0, len(pooled))
	for _, dt := range pooled {
		if !isAutoLoadTool(dt, useToolSearch, m.serverCfgs[dt.ServerName]) {
			continue
		}
		now = append(now, dt.Name)
		if !was[dt.Name] {
			added = append(added, dt)
		}
	}
	sort.Strings(now)
	m.autoLoadNames = now
	nowSet := make(map[string]bool, len(now))
	for _, n := range now {
		nowSet[n] = true
	}
	for _, dt := range pooled {
		if was[dt.Name] && !nowSet[dt.Name] {
			removed = append(removed, dt)
		}
	}
	// Names that were auto-loaded and have left the POOL entirely (their server went away): they
	// cannot appear in `removed` — there is no pooled tool to hand back to the caller — but a live
	// session's set would go on advertising a tool that no longer exists. They are dropped from the
	// sets below; the REGISTRY half is the tool-list refresher's job (onMCPToolsRefreshed
	// unregisters whatever a server reports gone).
	var gone []string
	for name := range was {
		if !nowSet[name] && m.pool.Get(name) == nil {
			gone = append(gone, name)
		}
	}
	// The live sessions' sets follow in the same critical section, so a set can never be
	// readable in a state where autoLoadNames and the set disagree. The writes are IN MEMORY
	// only: Add/Remove persist on every call, and doing that here would be one full file write
	// per tool per session (250 of them for 50 tools × 5 sessions) with m.mu held, stalling
	// every CallTool behind them. Consistency does not need that — the touched sets are
	// persisted once each below, after the lock is released.
	var touched []*DiscoveredSet
	if len(added) > 0 || len(removed) > 0 || len(gone) > 0 {
		for _, set := range m.sets {
			for _, dt := range added {
				set.addNoPersist(dt.Name)
			}
			for _, dt := range removed {
				set.removeNoPersist(dt.Name)
			}
			for _, name := range gone {
				set.removeNoPersist(name)
			}
			touched = append(touched, set)
		}
	}
	m.mu.Unlock()

	// One write per touched set, outside the manager's lock (DiscoveredSet guards itself). The
	// in-memory set stays authoritative; a failed write is logged, like every other persist
	// here (the next successful one rewrites the whole list).
	for _, set := range touched {
		if err := set.Save(); err != nil {
			m.logger.Error(context.Background(), "MCP: failed to persist recomputed auto-load tools", err)
		}
	}

	if len(added) > 0 || len(removed) > 0 || len(gone) > 0 {
		m.logger.Info(context.Background(), "MCP: auto-load recomputed",
			"auto_load", len(now), "added", len(added), "removed", len(removed), "gone", len(gone),
			"tool_search", useToolSearch)
	}
	return added, removed
}

// isAutoLoadTool is the auto-load decision, in ONE place: PopulateFromConnect and
// RecomputeAutoLoad must agree, or toggling the setting would land somewhere different from
// where a fresh start lands.
func isAutoLoadTool(dt *DeferredTool, useToolSearch bool, srvCfg config.MCPServerConfig) bool {
	if !useToolSearch {
		return true
	}
	bare := dt.Name
	if mt, ok := dt.Tool.(MCPTool); ok {
		bare = mt.ToolName()
	}
	for _, name := range srvCfg.AlwaysLoadTools {
		if matchWildcard(name, bare) {
			return true
		}
	}
	return false
}

// PopulateFromConnect connects to all servers in cfg.MCPServers, inflates the
// manager's deferred pool with every discovered tool, and adds the subset of
// "auto-load" tools (when ToolSearch is disabled, or the per-server
// always_load list matches) to the discovered set.
//
// The discovered set is the contract used by AIAgent.filterActiveSchemas:
// any tool in it is exposed to the LLM. Whether the tool is also eagerly
// registered into a tool.Registry is up to the caller — single-agent callers
// register them on the spot, while channel mode leaves them lazy and lets
// AIAgent.lazyRegisterMCPTool do the registration on first invocation.
//
// Returns:
//   - autoLoad: tools the caller may want to RegisterTool eagerly
//   - all:      every discovered tool (already added to pool)
//   - errs:     per-server connection errors (non-fatal; partial discovery is fine)
//
// PopulateFromConnect does NOT call MarkInitDone — the caller decides when
// initialization is "done" (e.g. after additionally registering reminders).
func (m *Manager) PopulateFromConnect(ctx context.Context, cfg *config.Config) (autoLoad, all []MCPTool, errs []error) {
	all, errs = m.ConnectAll(ctx, cfg.MCPServers)
	if len(all) == 0 {
		return nil, nil, errs
	}

	// The threshold counts the tools that will actually enter the POOL — i.e. after the
	// whitelist/blacklist filter — not everything the servers listed. The pool is the set a
	// search has to sift through, and it is the same basis RecomputeAutoLoad uses (len(pool));
	// counting the unfiltered list here made the two disagree the moment a filter was
	// configured (100 discovered, 95 blacklisted: a fresh start said "search", the settings
	// page said "auto-load"), which is exactly the drift isAutoLoadTool exists to prevent.
	kept := make([]MCPTool, 0, len(all))
	for _, t := range all {
		if _, hasCfg := m.serverCfgs[t.ServerName()]; hasCfg && m.shouldSkipTool(t.ServerName(), t.ToolName()) {
			continue
		}
		kept = append(kept, t)
	}
	useToolSearch := cfg.MCPToolSearch.IsEnabled() &&
		len(kept) > cfg.MCPToolSearch.MinToolsForSearch

	for _, t := range kept {
		srvCfg, hasCfg := m.serverCfgs[t.ServerName()]

		var searchHint string
		if hasCfg && srvCfg.SearchHints != nil {
			searchHint = srvCfg.SearchHints[t.ToolName()]
		}

		dt := NewDeferredToolFromMCPTool(t, searchHint)
		m.pool.Add(dt)

		// The decision lives in isAutoLoadTool (shared with RecomputeAutoLoad): the two must
		// agree, or toggling the setting lands somewhere a fresh start would not.
		isAutoLoad := isAutoLoadTool(dt, useToolSearch, srvCfg)

		if isAutoLoad {
			// Auto-load tools are the baseline of every session's discovered
			// set. They're recorded here and injected into each per-session
			// set lazily by SetFor (and backfilled into any sets created
			// before the pool finished populating).
			m.mu.Lock()
			m.autoLoadNames = append(m.autoLoadNames, t.Name())
			m.mu.Unlock()
			autoLoad = append(autoLoad, t)
		}
	}

	// Initialize tool cache snapshots for refresh diffing.
	// Group tools by server to create one snapshot per server.
	serverTools := make(map[string][]MCPTool)
	for _, t := range all {
		serverTools[t.ServerName()] = append(serverTools[t.ServerName()], t)
	}
	for serverName, tools := range serverTools {
		m.toolCache.Snapshot(serverName, tools)
	}

	// Backfill auto-load tools into any per-session sets that were created
	// before the pool finished populating (e.g. an early filterActiveSchemas
	// call while MCP was still connecting).
	m.backfillAutoLoadIntoSets()

	m.logger.Info(ctx, "MCP: populated tools", "total", m.pool.Len(), "autoLoad", len(autoLoad), "toolSearch", useToolSearch, "threshold", cfg.MCPToolSearch.MinToolsForSearch)
	return autoLoad, all, errs
}

// backfillAutoLoadIntoSets injects the auto-load tool names into every
// per-session set already created, and re-runs the restore for sets that
// were created before the pool finished populating (their earlier restore
// had to keep the whole history because the pool was empty — now that the
// pool is populated, real stale tools can be dropped).
func (m *Manager) backfillAutoLoadIntoSets() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sets {
		for _, name := range m.autoLoadNames {
			s.addNoPersist(name)
		}
		m.restoreDiscoveredFromDisk(context.Background(), s)
	}
}

// isWhitelisted checks whether a tool name matches an entry in the whitelist.
// Matching is case-insensitive and supports wildcards (*, ?, [...]).
// If a whitelist entry contains no wildcard characters, it behaves as an exact
// case-insensitive match (backward compatible).
func isWhitelisted(toolName string, whitelist []string) bool {
	for _, w := range whitelist {
		if matchWildcard(w, toolName) {
			return true
		}
	}
	return false
}

// isBlacklisted checks whether a tool name matches an entry in the blacklist.
// Uses the same matching rules as isWhitelisted (case-insensitive, wildcard
// support). Returns true if the tool should be excluded.
func isBlacklisted(toolName string, blacklist []string) bool {
	for _, b := range blacklist {
		if matchWildcard(b, toolName) {
			return true
		}
	}
	return false
}

// matchWildcard performs case-insensitive wildcard matching using path.Match.
// Patterns may contain *, ?, and [...] glob characters. If the pattern has
// no glob metacharacters it acts as a simple case-insensitive equality check.
func matchWildcard(pattern, s string) bool {
	// path.Match is case-sensitive; lowercase both sides for case-insensitive matching.
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(s))
	return err == nil && matched
}

// ConnectAll connects to all configured MCP servers concurrently. Each server
// connection runs in its own goroutine so a slow or failing server does not
// block others. Returns all discovered MCPTool wrappers and any per-server
// errors (non-fatal; some servers may succeed while others fail).
func (m *Manager) ConnectAll(ctx context.Context, servers []config.MCPServerConfig) ([]MCPTool, []error) {
	var wg sync.WaitGroup

	// Store server configs for whitelist/blacklist filtering during refresh.
	m.mu.Lock()
	if m.serverCfgs == nil {
		m.serverCfgs = make(map[string]config.MCPServerConfig, len(servers))
	}
	for _, srv := range servers {
		m.serverCfgs[srv.Name] = srv
	}
	m.mu.Unlock()

	// Collected concurrently — protected by a single mutex since both
	// slices are always accessed together in critical sections.
	var (
		mu    sync.Mutex
		tools []MCPTool
		errs  []error
	)

	for i := range servers {
		srv := &servers[i]
		if !srv.IsEnabled() {
			logger.FromContext(ctx).Info(ctx, "MCP: server is disabled, skipping", "server", srv.Name)
			continue
		}

		wg.Go(func() {
			serverTools, err := m.connect(ctx, srv)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("mcp server %q: %w", srv.Name, err))
				return
			}
			tools = append(tools, serverTools...)
		})
	}

	wg.Wait()
	return tools, errs
}

// isAuthRequired classifies the two transport sentinels that both mean "this server wants
// an OAuth2 token": ErrOAuthAuthorizationRequired is what the OAuth handler returns when it
// has none to attach, and ErrAuthorizationRequired is what the server's own 401 becomes
// when a token was attached and rejected. They are one user-visible fact, so they must not
// take two different paths through the code — the difference is invisible from the outside
// (whether an OAuth handler happened to be configured) and the two branches drift.
func isAuthRequired(err error) bool {
	return errors.Is(err, transport.ErrOAuthAuthorizationRequired) ||
		errors.Is(err, transport.ErrAuthorizationRequired)
}

// connect establishes a connection to a single MCP server and discovers its tools.
func (m *Manager) connect(ctx context.Context, srv *config.MCPServerConfig) ([]MCPTool, error) {
	timeout := time.Duration(srv.Timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var c MCPClient
	var err error

	switch srv.Type {
	case config.MCPTransportStdio:
		c, err = m.connectStdio(srv)
	case config.MCPTransportHTTP:
		c, err = m.connectHTTP(ctx, srv, timeout)
	default:
		return nil, fmt.Errorf("unsupported MCP transport type: %s", srv.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// Send initialization request with required protocol version support
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "tachi",
		Version: "1.0.0",
	}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	initResult, err := c.Initialize(ctx, initReq)
	if err != nil {
		c.Close()
		// A 401 can arrive here rather than at ListTools — it is the FIRST request that
		// meets it — and it is the same fact: this server wants an OAuth2 token.
		// Mapping only the later failure left an unauthenticated HTTP server reporting
		// "authorization required" as a plain connect error, so no frontend could offer
		// the one action that fixes it.
		if isAuthRequired(err) {
			return nil, &OAuthRequiredError{ServerName: srv.Name}
		}
		logger.FromContext(ctx).Error(ctx, "MCP: server initialize failed", err, "server", srv.Name, "type", srv.Type, "url", srv.URL)
		return nil, fmt.Errorf("initialize failed: %w", err)
	}

	// The server's own usage description (MCP `initialize.instructions`) — how
	// to use this server and its features, in its author's words. It is the
	// one server-level summary nobody has to generate: free, always current,
	// and written by someone who knows what is behind it. Empty is common
	// (many servers leave it out), which is why the reminder has fallbacks.
	m.SetServerInstructions(srv.Name, initResult.Instructions)

	logger.FromContext(ctx).Info(ctx, "MCP: connected to server", "server", srv.Name, "type", srv.Type,
		"instructions_chars", len(initResult.Instructions))

	// Discover tools
	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		// The token this client holds was not accepted (or there is none): the transport
		// says so with one of two sentinels depending on whether an OAuth handler was
		// attached, and both of them mean the same thing to a user.
		if isAuthRequired(err) {
			return nil, &OAuthRequiredError{ServerName: srv.Name}
		}
		logger.FromContext(ctx).Error(ctx, "MCP: server list tools failed", err, "server", srv.Name, "type", srv.Type)
		return nil, fmt.Errorf("list tools failed: %w", err)
	}

	m.mu.Lock()
	m.clients[srv.Name] = c
	m.serverTypes[srv.Name] = srv.Type == config.MCPTransportHTTP
	m.mu.Unlock()

	logger.FromContext(ctx).Info(ctx, "MCP: server has N tools", "server", srv.Name, "toolCount", len(toolsResult.Tools))

	// Wrap tools
	mcpTools := make([]MCPTool, 0, len(toolsResult.Tools))
	for i := range toolsResult.Tools {
		mcpTools = append(mcpTools, MCPTool{
			serverName: srv.Name,
			serverTool: &toolsResult.Tools[i],
			manager:    m,
		})
	}

	return mcpTools, nil
}

func (m *Manager) connectStdio(srv *config.MCPServerConfig) (MCPClient, error) {
	command := srv.Command
	if command == "" {
		return nil, fmt.Errorf("command is required for stdio transport")
	}

	env := make([]string, 0, len(srv.Env))
	for k, v := range srv.Env {
		env = append(env, k+"="+v)
	}

	// The transport is built by hand (rather than via NewStdioMCPClient) so the client can be
	// given the roots handler: that convenience constructor passes no client options, and the
	// capability has to be on the CLIENT (see clientOptions).
	tr := transport.NewStdioWithOptions(command, env, srv.Args)
	c := client.NewClient(tr, m.clientOptions()...)
	// Client.Start, not transport.Start: it starts the transport AND registers the handler for
	// requests the SERVER sends us (sampling, elicitation, roots/list). Skipping it is why a
	// roots request answered "No request handler configured" — the SDK's own
	// NewStdioMCPClient does the same thing in two steps, and this call is the documented
	// requirement ("must be called before using the client").
	if err := c.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to start stdio transport: %w", err)
	}
	return c, nil
}

func (m *Manager) connectHTTP(ctx context.Context, srv *config.MCPServerConfig, timeout time.Duration) (MCPClient, error) {
	if srv.URL == "" {
		return nil, fmt.Errorf("url is required for http transport")
	}
	opts := []transport.StreamableHTTPCOption{
		transport.WithHTTPTimeout(timeout),
	}
	if len(srv.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(srv.Headers))
	}
	// Proxy support for HTTP MCP servers. An invalid proxy config silently
	// falls back to a plain client (same degradation policy as WebFetch /
	// WebSearch tools).
	if srv.Proxy != "" {
		httpClient := httpx.NewClient(timeout, srv.Proxy)
		opts = append(opts, transport.WithHTTPBasicClient(httpClient))
		m.logger.Info(ctx, "MCP: using proxy", "proxy", srv.Proxy, "server", srv.Name)
	}
	// If OAuth isn't explicitly configured, check for persisted token / DCR
	// info on disk — a previous DCR-based auth may have left valid tokens.
	if !srv.HasOAuth() && hasPersistedAuth(srv.TokenStorageName()) {
		srv.OAuth = &config.MCPOAuthConfig{}
	}
	if srv.HasOAuth() {
		opts = append(opts, m.oauthOption(ctx, srv))
	}
	// Same shape as the SDK's own NewStreamableHttpClient, plus the client options (the roots
	// handler): the session ID has to be carried over when the transport already has one, or a
	// server that issues one on initialize stops recognizing this client.
	tr, err := transport.NewStreamableHTTP(srv.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create http transport: %w", err)
	}
	clientOpts := make([]client.ClientOption, 0, 1)
	if tr.GetSessionId() != "" {
		clientOpts = append(clientOpts, client.WithSession())
	}
	clientOpts = append(clientOpts, m.clientOptions()...)
	c := client.NewClient(tr, clientOpts...)
	// Same reason as the stdio path: this is what installs the handler for server→client
	// requests. For HTTP the transport's Start is a no-op unless SSE listening is enabled, so
	// this costs nothing when nothing asks.
	if err := c.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to start http transport: %w", err)
	}
	return c, nil
}

// hasPersistedAuth returns true if there's a token file or DCR info file
// on disk for the given storage key. Used to auto-detect OAuth that was set
// up via DCR in a prior session, without explicit oauth: config.
func hasPersistedAuth(storageKey string) bool {
	tokenPath, err := mcpTokenPath(storageKey)
	if err != nil {
		return false
	}
	if fileutil.Exists(tokenPath) {
		return true
	}
	dcrPath, err := dcrTokenPath(storageKey)
	if err != nil {
		return false
	}
	return fileutil.Exists(dcrPath)
}

// oauthOption builds the WithHTTPOAuth transport option from the server config.
// If ClientID is empty but persisted DCR info exists, it is loaded from disk
// so that token refresh works across process restarts.
func (m *Manager) oauthOption(ctx context.Context, srv *config.MCPServerConfig) transport.StreamableHTTPCOption {
	oauthCfg := srv.OAuth
	// User-scoped store: MCP requests present the current turn participant's
	// own token when the ctx carries one (channel.MCPTokenUserChannel) and
	// the per-user file exists, falling back to the server-level token file.
	tokenStore, err := newUserScopedTokenStore(srv.TokenStorageName())
	if err != nil {
		m.logger.Error(ctx, "MCP: failed to create token store", err, "storageName", srv.TokenStorageName())
		tokenStore = nil
	}

	clientID := oauthCfg.ClientID
	clientSecret := oauthCfg.ClientSecret

	// DCR: if config has no client_id, try persisted DCR info for refresh support
	if clientID == "" && tokenStore != nil {
		if dcr, err := tokenStore.GetDCRInfo(ctx); err == nil {
			clientID = dcr.ClientID
			clientSecret = dcr.ClientSecret
			m.logger.Info(ctx, "MCP: loaded DCR client_id from disk", "server", srv.Name)
		}
	}

	var scopes []string
	if len(oauthCfg.Scopes) > 0 {
		scopes = oauthCfg.Scopes
	}

	return transport.WithHTTPOAuth(transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		ClientURI:             oauthCfg.ClientURI,
		Scopes:                scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: oauthCfg.AuthServerMetadataURL,
		TokenStore:            tokenStore,
	})
}

// CallTool invokes a tool on the named MCP server.
func (m *Manager) CallTool(ctx context.Context, serverName string, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	c, ok := m.clients[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not connected", serverName)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = arguments

	return c.CallTool(ctx, req)
}

// GetOAuthHandler returns the OAuthHandler for a connected HTTP server,
// or nil if the server is not connected or has no OAuth configured.
func (m *Manager) GetOAuthHandler(name string) *transport.OAuthHandler {
	m.mu.RLock()
	c, ok := m.clients[name]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	// Type-assert to *client.Client for GetTransport access.
	// This only works for real mcp-go clients; mock clients return nil.
	if realClient, ok := c.(*client.Client); ok {
		trans := realClient.GetTransport()
		if httpConn, ok := trans.(*transport.StreamableHTTP); ok {
			return httpConn.GetOAuthHandler()
		}
	}
	return nil
}

// IsConnected returns whether an MCP server is currently connected.
func (m *Manager) IsConnected(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[name]
	return ok
}

// StartRefresher begins background polling of HTTP MCP server tool lists at
// the given interval. Changes are reported via the callback. If a refresher
// is already running, this is a no-op.
//
// The callback receives a ToolListDelta describing what changed. It runs in
// the refresher's goroutine and should not block for long. Typically the
// caller updates its own tool registry and marks the DeferredToolReminder
// as dirty for system-reminder notification.
func (m *Manager) StartRefresher(ctx context.Context, interval time.Duration, cb RefreshCallback) {
	if m.refresher != nil {
		m.refresher.Stop()
	}
	m.refresher = NewRefresher(m, interval)
	m.refresher.OnRefresh(cb)
	m.refresher.Start(ctx)
	m.logger.Info(ctx, "MCP: tool list refresher started", "interval", interval)
}

// StopRefresher stops the background tool list polling, if running.
func (m *Manager) StopRefresher() {
	if m.refresher != nil {
		m.refresher.Stop()
		m.refresher = nil
		m.logger.Info(context.Background(), "MCP: tool list refresher stopped")
	}
}

// IsHTTPServer returns true if the given server uses HTTP transport.
func (m *Manager) IsHTTPServer(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.serverTypes[name]
}

// ConnectedServers returns a list of all connected server names.
func (m *Manager) ConnectedServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	servers := make([]string, 0, len(m.clients))
	for name := range m.clients {
		servers = append(servers, name)
	}
	return servers
}

// Disconnect closes the connection to a specific MCP server and removes it
// from the manager. Returns nil if the server was not connected.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[name]
	if !ok {
		return nil
	}
	delete(m.clients, name)
	if err := c.Close(); err != nil {
		m.logger.Error(context.Background(), "MCP: error disconnecting", err, "server", name)
		return fmt.Errorf("disconnect %q: %w", name, err)
	}
	m.logger.Info(context.Background(), "MCP: disconnected", "server", name)
	return nil
}

// Reconnect disconnects an existing server connection (if any) and
// establishes a fresh connection using the given config. Returns the
// server's tools on success.
//
// Whitelist/blacklist filtering is applied: tools matching the blacklist
// or not matching the whitelist are excluded from the returned slice.
func (m *Manager) Reconnect(ctx context.Context, srv *config.MCPServerConfig) ([]MCPTool, error) {
	// Disconnect existing connection (ignore errors)
	if _, ok := m.clients[srv.Name]; ok {
		_ = m.Disconnect(srv.Name)
	}

	tools, err := m.connect(ctx, srv)
	if err != nil {
		return nil, err
	}

	// Ensure server config is cached for shouldSkipTool lookups.
	// ConnectAll populates this for all servers during init, but
	// Reconnect may be called independently (e.g. manual toggle).
	m.mu.Lock()
	if m.serverCfgs == nil {
		m.serverCfgs = make(map[string]config.MCPServerConfig)
	}
	m.serverCfgs[srv.Name] = *srv
	m.mu.Unlock()

	// Apply whitelist/blacklist filtering — same logic as
	// PopulateFromConnect so that enabling a previously-disabled
	// server mid-session doesn't bypass tool filters.
	filtered := make([]MCPTool, 0, len(tools))
	for _, t := range tools {
		if !m.shouldSkipTool(srv.Name, t.ToolName()) {
			filtered = append(filtered, t)
		}
	}

	return filtered, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, c := range m.clients {
		if err := c.Close(); err != nil {
			m.logger.Error(context.Background(), "MCP: error closing client", err, "server", name)
		}
	}
	m.clients = make(map[string]MCPClient)
}

// formatMCPResult formats a CallToolResult into a human-readable string.
func formatMCPResult(result *mcp.CallToolResult) (string, error) {
	if result.IsError {
		return "", &MCPToolError{contentToString(result.Content)}
	}

	var sb strings.Builder
	for _, item := range result.Content {
		if textContent, ok := item.(mcp.TextContent); ok {
			sb.WriteString(textContent.Text)
		}
	}
	return sb.String(), nil
}

// contentToString converts MCP content items to a user-facing string.
func contentToString(content []mcp.Content) string {
	var sb strings.Builder
	for _, item := range content {
		switch c := item.(type) {
		case mcp.TextContent:
			sb.WriteString(c.Text)
		case mcp.ImageContent:
			// Name the image instead of a bare "[non-text content]": an error
			// whose detail lives in the image is otherwise unreadable.
			fmt.Fprintf(&sb, "[image: %s, %s]", c.MIMEType,
				strutil.HumanBytes(int64(base64.StdEncoding.DecodedLen(len(c.Data)))))
		default:
			sb.WriteString("[non-text content]")
		}
	}
	return sb.String()
}

// MCPToolError represents an error returned by an MCP tool (isError=true).
type MCPToolError struct {
	Message string
}

func (e *MCPToolError) Error() string {
	return fmt.Sprintf("MCP tool error: %s", e.Message)
}
