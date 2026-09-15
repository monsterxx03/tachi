package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/monsterxx03/tachi/agent"
	tachimcp "github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// TestMain doubles as the stub MCP server's entry point. A stdio MCP server is a
// subprocess, and the cheapest honest one is this very test binary re-executed:
// the env var makes the child serve MCP over stdin/stdout instead of running
// tests (the same trick the standard library uses for helper processes).
func TestMain(m *testing.M) {
	if os.Getenv(stubServerEnv) == "1" {
		serveStubMCP()
		return
	}
	os.Exit(m.Run())
}

// stubServerEnv marks a re-executed test binary as the stub MCP server.
const stubServerEnv = "TACHI_TEST_MCP_SERVER"

const (
	// stubServer is the stub server's name in the test config.
	stubServer = "stub"
	// stubServerTool is the tool the stub server exposes.
	stubServerTool = "ping"
)

// serveStubMCP runs a minimal MCP server over stdio and exits.
func serveStubMCP() {
	s := server.NewMCPServer("stub", "1.0.0")
	s.AddTool(
		mcp.NewTool(stubServerTool, mcp.WithDescription("ping the stub server")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("pong"), nil
		},
	)
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// TestSetMCPServerEnabledNotifiesLiveAgents pins the desktop's mid-session MCP
// toggle: connecting a server must tell the live agents to announce the tools
// they just gained.
//
// The bug this covers: the binding wrote the tools into the shared deferred pool
// and stopped there. The pool is a fact, but the reminder that makes the model
// AWARE of the pool fires at most once per session — so once a conversation was
// past its first turn, enabling a server was invisible and its tools unreachable
// unless the model happened to search for them blind. A real session's debug log
// showed exactly this: a server enabled mid-session produced zero
// "DeferredToolReminder fired" for the rest of the process's life.
func TestSetMCPServerEnabledNotifiesLiveAgents(t *testing.T) {
	d, svc, sid := newMCPToggleApp(t)

	// A live agent bound to the shared manager, with a collector this test can
	// inspect. Its deferred-tools reminder is registered lazily — exactly as in
	// production, where it is attached at agent-build time.
	a := &agent.AIAgent{Config: agent.AgentConfig{
		ToolRegistry: tools.NewRegistry(),
		Logger:       logger.Default(),
		MCPManager:   d.mcp,
	}}
	collector := systemreminder.NewCollector()
	a.SetReminderCollector(collector)

	d.mu.Lock()
	d.runs[sid].agent = a
	d.mu.Unlock()

	// Quiet to begin with: nothing has been loaded, so there is nothing to hint.
	if block := collector.Collect(t.Context(), systemreminder.Context{}); block != "" {
		t.Fatalf("fixture: expected no hint before the toggle, got %q", block)
	}

	if res := svc.SetMCPServerEnabled(stubServer, true); res != "ok" {
		t.Fatalf("SetMCPServerEnabled: %s", res)
	}

	// The tool really did connect...
	if d.mcp.Pool().Get("mcp__"+stubServer+"__"+stubServerTool) == nil {
		t.Fatal("the enabled server's tool should be in the shared pool")
	}

	// ...and the model must be told about it. This is the assertion that fails
	// when the binding only writes the pool.
	block := collector.Collect(t.Context(), systemreminder.Context{})
	if block == "" {
		t.Fatal("enabling a server must notify live agents, or the new tools are never announced")
	}
	if want := "mcp__" + stubServer + "__" + stubServerTool; !strings.Contains(block, want) {
		t.Errorf("expected the new tool %q in the hint, got %q", want, block)
	}
}

// TestSetMCPServerEnabledDisableUnregistersTools pins the cleanup half: turning a
// server off must also drop its tools from the agent's registry and every
// session's discovered set. Leaving them behind lets the model keep calling a
// tool whose server is gone — the same cleanup the TUI's /mcp toggle performs.
func TestSetMCPServerEnabledDisableUnregistersTools(t *testing.T) {
	d, svc, sid := newMCPToggleApp(t)

	a := &agent.AIAgent{Config: agent.AgentConfig{
		ToolRegistry: tools.NewRegistry(),
		Logger:       logger.Default(),
		MCPManager:   d.mcp,
	}}
	// A tool the user had explicitly loaded from this server — the state after a
	// successful MCPSearchTools opt-in.
	fullName := "mcp__" + stubServer + "__" + stubServerTool
	a.RegisterTool(stubNamedTool{name: fullName})

	d.mu.Lock()
	d.runs[sid].agent = a
	d.mu.Unlock()

	if res := svc.SetMCPServerEnabled(stubServer, false); res != "ok" {
		t.Fatalf("SetMCPServerEnabled: %s", res)
	}

	for _, name := range a.ToolNames() {
		if name == fullName {
			t.Error("disabling a server must unregister its tools, or the model keeps calling a dead server")
		}
	}
}

// --- fixtures ---

// newMCPToggleApp builds a desktop app + service whose shared MCP manager has one
// server: a stub served by this test binary over stdio.
func newMCPToggleApp(t *testing.T) (*desktopApp, *AgentService, string) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	d := newTestApp()
	d.cfg = &config.Config{
		Language: "en",
		MCPServers: []config.MCPServerConfig{{
			Name:    stubServer,
			Type:    config.MCPTransportStdio,
			Command: self,
			Env:     map[string]string{stubServerEnv: "1"},
			// A connect timeout of 0 would expire the context immediately.
			Timeout: config.Duration(30 * time.Second),
		}},
	}
	d.mcp = tachimcp.NewManager(t.Context(), d.cfg, logger.Default())
	d.mcp.MarkInitDone()

	const sid = "s1"
	d.runs[sid] = &sessionRun{}
	return d, &AgentService{desk: d}, sid
}

// stubNamedTool is a minimal tools.Tool for registry fixtures.
type stubNamedTool struct{ name string }

func (s stubNamedTool) Name() string        { return s.name }
func (s stubNamedTool) Description() string { return "stub" }
func (s stubNamedTool) Properties() map[string]tools.PropertySchema {
	return nil
}
func (s stubNamedTool) Required() []string { return nil }
func (s stubNamedTool) Parallel() bool     { return false }
func (s stubNamedTool) ExecuteContext(context.Context, string) (string, error) {
	return "", nil
}
