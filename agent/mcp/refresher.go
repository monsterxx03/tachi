package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/pkg/container"
)

// ToolListDelta describes the changes detected during a tool list refresh.
// Added and Modified carry the new MCPTool wrappers for pool updates.
type ToolListDelta struct {
	ServerName string
	Added      []MCPTool // tools that didn't exist before
	Removed    []string  // names of tools that no longer exist
	Modified   []MCPTool // tools whose schema has changed
}

// HasChanges returns true if any tools were added, removed, or modified.
func (d *ToolListDelta) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Modified) > 0
}

// toolSignature uniquely identifies a tool definition for change detection.
type toolSignature struct {
	Name string
	Hash uint64 // fnv64a hash of JSON-serialized input schema
}

// serverToolSigs is the per-server signature list stored in the cache.
// The alias disambiguates container.LockedMap[string][]toolSignature in the
// struct field (Go parses the trailing [] as an index expression there).
type serverToolSigs = []toolSignature

// toolListCache tracks the last known tool state per server and provides
// diff-based change detection. Thread-safe.
type toolListCache struct {
	entries container.LockedMap[string, serverToolSigs] // serverName → sorted signatures
}

func newToolListCache() *toolListCache {
	return &toolListCache{}
}

// computeToolHash computes a hash of the tool's name and input schema for
// change detection. Uses fnv64a which is fast and sufficient for diffing.
func computeToolHash(tool *mcp.Tool) uint64 {
	h := fnv.New64a()
	h.Write([]byte(tool.Name))
	schemaBytes, _ := json.Marshal(tool.InputSchema)
	h.Write(schemaBytes)
	return h.Sum64()
}

// Snapshot saves the current tool state for a server. Call after a successful
// refresh to update the baseline for future diffs.
func (c *toolListCache) Snapshot(serverName string, tools []MCPTool) {
	sigs := make([]toolSignature, len(tools))
	for i, t := range tools {
		sigs[i] = toolSignature{
			Name: t.Name(),
			Hash: computeToolHash(t.serverTool),
		}
	}
	sort.Slice(sigs, func(i, j int) bool {
		return sigs[i].Name < sigs[j].Name
	})

	c.entries.Store(serverName, sigs)
}

// Diff compares new tools against the cached snapshot for the given server
// and returns the delta. Returns all tools as Added if no cache exists
// (first-time use).
func (c *toolListCache) Diff(serverName string, newTools []MCPTool) *ToolListDelta {
	oldSigs, _ := c.entries.Load(serverName)

	delta := &ToolListDelta{ServerName: serverName}

	if oldSigs == nil {
		// First snapshot — treat everything as added
		delta.Added = make([]MCPTool, len(newTools))
		copy(delta.Added, newTools)
		return delta
	}

	// Build lookup maps for old and new
	oldByName := make(map[string]uint64, len(oldSigs))
	for _, sig := range oldSigs {
		oldByName[sig.Name] = sig.Hash
	}

	newByName := make(map[string]MCPTool, len(newTools))
	for _, t := range newTools {
		newByName[t.Name()] = t
	}

	// Find removed and modified
	for _, sig := range oldSigs {
		newTool, exists := newByName[sig.Name]
		if !exists {
			delta.Removed = append(delta.Removed, sig.Name)
		} else if computeToolHash(newTool.serverTool) != sig.Hash {
			delta.Modified = append(delta.Modified, newTool)
		}
	}

	// Find added
	for _, t := range newTools {
		if _, exists := oldByName[t.Name()]; !exists {
			delta.Added = append(delta.Added, t)
		}
	}

	return delta
}

// RefreshCallback is called when a refresh detects changes to a server's
// tool list. The delta describes what changed. The callback runs in the
// refresher's goroutine — it should not block for long.
type RefreshCallback func(delta *ToolListDelta)

// Refresher periodically polls connected MCP servers for tool list changes
// and reports any differences via a callback. Only refreshes HTTP servers
// by default (stdio servers are local and unlikely to change mid-session).
type Refresher struct {
	manager  *Manager
	interval time.Duration

	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	callback RefreshCallback
}

// NewRefresher creates a refresher backed by the given Manager.
func NewRefresher(mgr *Manager, interval time.Duration) *Refresher {
	return &Refresher{
		manager:  mgr,
		interval: interval,
	}
}

// OnRefresh sets the callback invoked when changes are detected.
func (r *Refresher) OnRefresh(cb RefreshCallback) {
	r.mu.Lock()
	r.callback = cb
	r.mu.Unlock()
}

// Start begins background polling in a new goroutine. Returns immediately.
// If already running, this is a no-op.
func (r *Refresher) Start(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	go r.run(ctx)
}

// Stop stops the background polling and waits for the goroutine to exit.
func (r *Refresher) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stopCh)
	r.mu.Unlock()
}

// run is the main polling loop. Runs in a goroutine.
func (r *Refresher) run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshAll(ctx)
		}
	}
}

// refreshAll iterates over all connected HTTP MCP servers and refreshes
// their tool lists. Changes are reported via the callback.
func (r *Refresher) refreshAll(ctx context.Context) {
	servers := r.manager.ConnectedServers()
	if len(servers) == 0 {
		return
	}

	for _, name := range servers {
		// Only refresh HTTP servers — stdio servers don't change mid-session.
		if !r.manager.IsHTTPServer(name) {
			continue
		}

		delta, err := r.manager.RefreshNow(ctx, name)
		if err != nil {
			r.manager.logger.Error(ctx, "MCP: refresh failed", err, "server", name)
			continue
		}
		if delta == nil || !delta.HasChanges() {
			continue
		}

		r.manager.logger.Info(ctx, "MCP: refresh detected changes", "server", name, "added", len(delta.Added), "removed", len(delta.Removed), "modified", len(delta.Modified))

		r.mu.Lock()
		cb := r.callback
		r.mu.Unlock()

		if cb != nil {
			cb(delta)
		}
	}
}

// RefreshNow performs an immediate one-shot refresh of a single server's
// tool list. Returns the delta, or an error if the server is not connected
// or ListTools fails.
//
// On success, the manager's deferred pool and discovered set are updated
// to reflect the changes, and the tool cache snapshot is updated.
func (m *Manager) RefreshNow(ctx context.Context, serverName string) (*ToolListDelta, error) {
	m.mu.RLock()
	c, ok := m.clients[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not connected", serverName)
	}

	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list tools failed for %q: %w", serverName, err)
	}

	// Build new MCPTool wrappers from fresh ListTools response
	newTools := make([]MCPTool, 0, len(result.Tools))
	for i := range result.Tools {
		newTools = append(newTools, MCPTool{
			serverName: serverName,
			serverTool: &result.Tools[i],
			manager:    m,
		})
	}

	// Diff against cache
	delta := m.toolCache.Diff(serverName, newTools)
	if !delta.HasChanges() {
		return delta, nil
	}

	// Apply changes to pool and discovered set
	m.applyToolDelta(serverName, delta, newTools)

	return delta, nil
}

// applyToolDelta updates the deferred pool and discovered set to reflect
// the changes detected in a refresh. newTools is the complete current tool
// list for the server.
func (m *Manager) applyToolDelta(serverName string, delta *ToolListDelta, newTools []MCPTool) {
	prefix := "mcp__" + serverName + "__"

	// 1. Handle removals: remove from pool and discovered set
	for _, name := range delta.Removed {
		fullName := prefix + name
		m.pool.Remove(fullName)
		m.set.Remove(fullName)
		m.logger.Info(context.Background(), "MCP: refresh removed", "tool", fullName)
	}

	// 2. Handle modifications: update pool entries
	for _, t := range delta.Modified {
		fullName := t.Name()
		dt := NewDeferredToolFromMCPTool(t, "")
		m.pool.Add(dt) // overwrite existing
		m.logger.Info(context.Background(), "MCP: refresh updated", "tool", fullName)
	}

	// 3. Handle additions: add to pool as deferred (not auto-discovered),
	// but respect whitelist/blacklist filtering from server config.
	for _, t := range delta.Added {
		if m.shouldSkipTool(serverName, t.ToolName()) {
			m.logger.Info(context.Background(), "MCP: refresh skipped (filtered)", "tool", t.Name())
			continue
		}
		dt := NewDeferredToolFromMCPTool(t, "")
		m.pool.Add(dt)
		m.logger.Info(context.Background(), "MCP: refresh added (deferred)", "tool", t.Name())
	}

	// Update cache snapshot
	m.toolCache.Snapshot(serverName, newTools)
}

// shouldSkipTool checks whether a tool should be excluded from the given
// server based on the server's whitelist/blacklist configuration.
//
// When both are configured, whitelist is applied first (only matching tools
// are kept), then blacklist filters out any matching tools from that narrowed
// set. The result is effectively: whitelist ∩ ¬blacklist.
//
// When only whitelist is configured, only matching tools are kept.
// When only blacklist is configured, matching tools are excluded.
func (m *Manager) shouldSkipTool(serverName string, toolName string) bool {
	srvCfg, hasCfg := m.serverCfgs[serverName]
	if !hasCfg {
		return false
	}
	// Whitelist: if configured, exclude everything not in it.
	if len(srvCfg.Whitelist) > 0 && !isWhitelisted(toolName, srvCfg.Whitelist) {
		return true
	}
	// Blacklist: further exclude any tool that hits the blacklist,
	// even if it passed the whitelist above.
	if len(srvCfg.Blacklist) > 0 && isBlacklisted(toolName, srvCfg.Blacklist) {
		return true
	}
	return false
}
