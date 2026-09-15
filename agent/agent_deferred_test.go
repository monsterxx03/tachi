package agent

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

// TestDeferredReminderAttachedWithEmptyPoolStillFires pins the half of the
// deferred-tools path that a shared pool breaks: the reminder must be REGISTERED
// at attach time even though the pool is still empty.
//
// Registration used to be gated on `pool.Len() > 0`. That holds for a single
// agent that connects its own servers, but a SHARED pool is populated in the
// background (desktop, channel), so an agent built before its servers finished
// connecting saw an empty pool, registered nothing, and no later event came back
// to register it — the model never learned the tools existed.
//
// The assertion deliberately does NOT call NotifyDeferredToolsAdded: being
// registered is what makes the reminder able to fire on its own, and a test that
// notifies first would exercise the fallback registration instead of this path.
func TestDeferredReminderAttachedWithEmptyPoolStillFires(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newMCPTestAgent(t, cfg)

	// The state a desktop agent is built in: shared manager injected, servers
	// still connecting, pool empty.
	a.attachSharedMCPReminder()

	// The servers finish connecting — tools land in the pool, and nothing else
	// happens (no toggle, no explicit notification).
	a.DeferredPool().Add(&mcp.DeferredTool{
		Name:        "mcp__pg__query",
		ServerName:  "pg",
		Description: "Query the PostgreSQL database",
	})

	block := a.collectReminders(t.Context(), a.buildReminderContext(true, false))
	if !strings.Contains(block, "mcp__pg__query") {
		t.Fatalf("a reminder attached against an empty pool must still announce later tools, got %q", block)
	}
}

// TestDeferredReminderAnnouncesToolsAddedAfterItFired covers the mid-session case
// the desktop toggle produces: the session has already had its one hint, the user
// enables another server, and the model must be told about the new tools —
// because the once-per-session guard would otherwise keep it silent.
func TestDeferredReminderAnnouncesToolsAddedAfterItFired(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newMCPTestAgent(t, cfg)
	a.attachSharedMCPReminder()

	a.DeferredPool().Add(&mcp.DeferredTool{
		Name:        "mcp__pg__query",
		ServerName:  "pg",
		Description: "Query the PostgreSQL database",
	})

	// First message: the hint fires and the once-per-session guard closes.
	if block := a.collectReminders(t.Context(), a.buildReminderContext(true, false)); !strings.Contains(block, "mcp__pg__query") {
		t.Fatalf("fixture: the first message should carry the hint, got %q", block)
	}

	// Second server enabled mid-session.
	a.DeferredPool().Add(&mcp.DeferredTool{
		Name:        "mcp__gh__pr",
		ServerName:  "gh",
		Description: "Create and manage pull requests",
	})
	a.NotifyDeferredToolsAdded()

	block := a.collectReminders(t.Context(), a.buildReminderContext(false, false))
	if !strings.Contains(block, "mcp__gh__pr") {
		t.Fatalf("the newly enabled tool must be announced, got %q", block)
	}
}

// TestNotifyDeferredToolsAddedIsIdempotentAsRegistration covers the write side of
// the same path: notifying repeatedly must leave the reminder registered ONCE.
// The collector only appends, so a per-notification AddReminder would run the
// same reminder N times per message, growing the injected block every time.
func TestNotifyDeferredToolsAddedIsIdempotentAsRegistration(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newMCPTestAgent(t, cfg)

	a.DeferredPool().Add(&mcp.DeferredTool{
		Name:        "mcp__pg__query",
		ServerName:  "pg",
		Description: "Query the PostgreSQL database",
	})

	for range 3 {
		a.NotifyDeferredToolsAdded()
	}

	block := a.collectReminders(t.Context(), a.buildReminderContext(true, false))
	if n := strings.Count(block, "mcp__pg__query"); n != 1 {
		t.Errorf("expected the tool announced exactly once, got %d occurrences in %q", n, block)
	}
}

// TestNotifyDeferredToolsAddedWithoutMCPIsSafe pins the no-MCP case: the entry
// point is called from frontends that do not necessarily have a manager (MCP
// disabled, or bootstrap failure), and it must be inert rather than panic.
func TestNotifyDeferredToolsAddedWithoutMCPIsSafe(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newTestAgent(t, &mockStreamProvider{})
	a.Config.FullConfig = cfg
	a.Config.MCPManager = nil

	a.NotifyDeferredToolsAdded()

	if a.deferredToolReminder != nil {
		t.Error("no manager means no reminder to create")
	}
}
