package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/container"
)

// MCPSwitchResult summarizes the effect of an MCP profile switch.
type MCPSwitchResult struct {
	Profile  string   // the newly active profile
	Kept     []string // servers unchanged by the switch (connections left intact)
	Removed  []string // servers gone from the new set (disconnected)
	Added    []string // servers newly connected
	Reconned []string // servers whose config changed (disconnected + reconnected)
	Errors   []error  // per-server connection errors (nil on full success)
}

// mcpServerDiff categorizes the servers of two MCP configuration sets.
type mcpServerDiff struct {
	kept    []config.MCPServerConfig // in both, config unchanged — reuse old object
	removed []config.MCPServerConfig // old only — disconnect and unregister
	added   []config.MCPServerConfig // new only — connect
	changed []config.MCPServerConfig // in both but config differs — reconnect
}

// diffMCPServers diffs old and new server lists by name. Servers present in
// both sets with an equivalent config (see sameMCPConfig) count as kept.
func diffMCPServers(oldCfgs, newCfgs []config.MCPServerConfig) mcpServerDiff {
	var diff mcpServerDiff
	for _, old := range oldCfgs {
		new, found := findServerByName(newCfgs, old.Name)
		switch {
		case !found:
			diff.removed = append(diff.removed, old)
		case sameMCPConfig(old, new):
			diff.kept = append(diff.kept, old)
		default:
			diff.changed = append(diff.changed, new)
		}
	}
	for _, new := range newCfgs {
		if _, found := findServerByName(oldCfgs, new.Name); !found {
			diff.added = append(diff.added, new)
		}
	}
	return diff
}

func findServerByName(servers []config.MCPServerConfig, name string) (config.MCPServerConfig, bool) {
	idx := slices.IndexFunc(servers, func(s config.MCPServerConfig) bool { return s.Name == name })
	if idx < 0 {
		return config.MCPServerConfig{}, false
	}
	return servers[idx], true
}

// sameMCPConfig reports whether two server configs are effectively identical.
// Profile is ignored (it is re-stamped on every load) and Enabled is ignored
// (runtime toggles live on the old object and must not force a reconnect).
func sameMCPConfig(a, b config.MCPServerConfig) bool {
	a.Profile, b.Profile = "", ""
	a.Enabled, b.Enabled = nil, nil
	return reflect.DeepEqual(a, b)
}

// SwitchMCPProfile activates the given MCP profile at runtime: it reloads
// mcp.{profile}.json configs on top of the base files and reconciles the
// running server set.
//
// Reconciliation (diff-based, connections of untouched servers stay up):
//   - removed / changed servers are disconnected and fully unregistered
//     (tool registry, deferred pool, every session's discovered set)
//   - added / changed servers are reconnected concurrently; their tools go
//     into the deferred pool (LLM is hinted via the deferred-tools reminder)
//   - kept servers keep their live connection AND their runtime Enabled
//     toggle (the old config object is retained). Kept servers that were
//     never connected (e.g. failed at startup) are retried — a switch is a
//     natural moment to revive them.
//
// The overall operation is serialized: a second concurrent switch fails
// immediately instead of racing the in-flight one.
//
// The profile change is in-memory only — it is not written back to
// config.yaml and reverts on restart (same semantics as /model, /thinking).
//
// Returns an error (without touching any runtime state) if a switch is
// already running, MCP is still initializing, the profile does not exist in
// any scope, or the new profile's config fails to load.
func (a *AIAgent) SwitchMCPProfile(ctx context.Context, profile, workDir string) (MCPSwitchResult, error) {
	if !a.mcpSwitchMu.TryLock() {
		return MCPSwitchResult{}, errors.New("an MCP profile switch is already in progress")
	}
	defer a.mcpSwitchMu.Unlock()

	mgr := a.Config.MCPManager
	if mgr == nil {
		return MCPSwitchResult{}, errors.New("MCP is not configured")
	}
	select {
	case <-mgr.InitDone():
	default:
		return MCPSwitchResult{}, errors.New("MCP initialization still in progress, try again in a moment")
	}

	// Reject unknown profiles explicitly: LoadMCPConfig silently skips
	// missing files, so without this check a typo would "succeed" as a
	// no-op (and stamp a nonexistent profile as active).
	if profile != "" && !slices.Contains(config.ListMCPProfiles(workDir), profile) {
		return MCPSwitchResult{}, fmt.Errorf("MCP profile %q not found (no mcp.%s.json in global or project scope)", profile, profile)
	}

	cfg := a.Config.FullConfig
	oldCfgs := cfg.MCPServers

	// Roll the profile forward; on load failure restore the old value so the
	// in-memory state stays consistent (LoadMCPServers leaves MCPServers
	// untouched on error).
	prevProfile := cfg.ActiveMCPProfile
	cfg.ActiveMCPProfile = profile
	if err := cfg.LoadMCPServers(workDir); err != nil {
		cfg.ActiveMCPProfile = prevProfile
		return MCPSwitchResult{}, fmt.Errorf("loading profile %q: %w", profile, err)
	}

	diff := diffMCPServers(oldCfgs, cfg.MCPServers)
	result := MCPSwitchResult{Profile: profile}

	// Servers to (re)connect: added, changed, plus kept servers that never
	// actually connected — retry those instead of reporting them as running.
	toConnect := slices.Concat(diff.added, diff.changed)
	addedNames := container.NewSet[string]()
	for _, srv := range diff.added {
		addedNames.Add(srv.Name)
	}
	revivedNames := container.NewSet[string]()
	for _, srv := range diff.kept {
		if srv.IsEnabled() && !mgr.IsConnected(srv.Name) {
			toConnect = append(toConnect, srv)
			revivedNames.Add(srv.Name)
		}
	}

	// The overall deadline derives from the servers' own timeouts: each
	// connection is capped by its per-server "timeout" (mcp.json), and they
	// run concurrently, so the batch needs max(timeout) + margin.
	ctx, cancel := context.WithTimeout(ctx, mcpSwitchBudget(toConnect))
	defer cancel()

	// Teardown: removed servers lose their connection; changed servers are
	// unregistered here — their reconnect below replaces the connection.
	for _, srv := range diff.removed {
		if err := mgr.Disconnect(srv.Name); err != nil {
			a.Config.Logger.Error(ctx, "MCP: profile switch disconnect failed", err, "server", srv.Name)
			result.Errors = append(result.Errors, fmt.Errorf("disconnect %q: %w", srv.Name, err))
		}
		a.UnregisterMCPServer(srv.Name)
		result.Removed = append(result.Removed, srv.Name)
	}
	for _, srv := range diff.changed {
		a.UnregisterMCPServer(srv.Name)
	}

	// Rebuild the merged list in the new config's order, but keep the old
	// objects for unchanged servers (preserves runtime Enabled toggles and
	// manager-side identity).
	final := make([]config.MCPServerConfig, 0, len(cfg.MCPServers))
	for _, newSrv := range cfg.MCPServers {
		old, found := findServerByName(diff.kept, newSrv.Name)
		if !found {
			final = append(final, newSrv)
			continue
		}
		final = append(final, old)
		if !revivedNames.Has(old.Name) {
			result.Kept = append(result.Kept, old.Name)
		}
	}
	cfg.MCPServers = final

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, srv := range toConnect {
		if !srv.IsEnabled() {
			continue // configured but toggled off — nothing to connect
		}
		srv := srv
		wg.Go(func() {
			tools, err := mgr.Reconnect(ctx, &srv)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("mcp server %q: %w", srv.Name, err))
				return
			}
			a.AddDeferredMCPTools(tools)
			switch {
			case addedNames.Has(srv.Name):
				result.Added = append(result.Added, srv.Name)
			default:
				result.Reconned = append(result.Reconned, srv.Name)
			}
		})
	}
	wg.Wait()

	a.Config.Logger.Info(ctx, "MCP: profile switched",
		"profile", profile, "kept", len(result.Kept), "removed", len(result.Removed),
		"added", len(result.Added), "reconnected", len(result.Reconned), "errors", len(result.Errors))
	return result, nil
}

// profileSwitchMargin is added on top of the largest per-server timeout to
// cover teardown and pool bookkeeping during a profile switch.
const profileSwitchMargin = 5 * time.Second

// mcpSwitchBudget derives the overall deadline for a batch of server
// connections. Connections run concurrently and each is already capped by
// its own per-server timeout (mcp.json "timeout", default
// config.DefaultMCPServerTimeout), so the batch just needs the largest of
// them plus a small margin.
func mcpSwitchBudget(servers []config.MCPServerConfig) time.Duration {
	var max time.Duration
	for _, srv := range servers {
		if !srv.IsEnabled() {
			continue
		}
		if d := time.Duration(srv.Timeout); d > max {
			max = d
		}
	}
	if max <= 0 {
		max = time.Duration(config.DefaultMCPServerTimeout)
	}
	return max + profileSwitchMargin
}

// FormatMCPProfileList renders the available MCP profiles with the active
// one marked, plus a usage hint. Shared by the TUI and ACP /mcp profile
// handlers. Pass available=nil (no profiles found) or active="" (none set).
func FormatMCPProfileList(available []string, active string) string {
	var sb strings.Builder
	sb.WriteString("MCP profiles (mcp.<name>.json in `~/.tachi/` or `.tachi/`):")
	if len(available) == 0 {
		sb.WriteString("\n  (none found)")
	}
	for _, p := range available {
		marker := "  "
		if p == active {
			marker = "● "
		}
		fmt.Fprintf(&sb, "\n%s%s", marker, p)
	}
	sb.WriteString("\n\nUsage: `/mcp profile <name>` — switch at runtime (active until restart)")
	return sb.String()
}

// FormatMCPSwitchResult renders a profile switch summary as markdown.
// Shared by the TUI and ACP /mcp profile handlers.
func FormatMCPSwitchResult(res MCPSwitchResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "MCP profile switched to **%s** (active until restart)", res.Profile)

	// Label → server names, skipping empty categories.
	for _, section := range []struct {
		label  string
		names  []string
		suffix string
	}{
		{"connected", res.Added, ""},
		{"reconnected", res.Reconned, ""},
		{"unchanged", res.Kept, " (kept running)"},
		{"disconnected", res.Removed, ""},
	} {
		if len(section.names) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n● %s: %s%s", section.label, strings.Join(section.names, ", "), section.suffix)
	}

	for _, err := range res.Errors {
		fmt.Fprintf(&sb, "\n⚠ %v", err)
	}
	return sb.String()
}
