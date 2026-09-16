package main

import (
	"context"
	"fmt"
	"net/http"
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
	"github.com/monsterxx03/tachi/itest/mcpauth"
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

// --- OAuth authorization ---

// newMCPOAuthApp builds a desktop app + service whose shared MCP manager has one HTTP
// server: the mcpauth fixture, which is an authorization server and an OAuth-protected
// MCP endpoint on one loopback origin.
func newMCPOAuthApp(t *testing.T) (*desktopApp, *AgentService, string, *mcpauth.Server) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	srv, err := mcpauth.New()
	if err != nil {
		t.Fatalf("mcpauth: %v", err)
	}
	t.Cleanup(srv.Close)

	d := newTestApp()
	d.cfg = &config.Config{
		Language: "en",
		MCPServers: []config.MCPServerConfig{{
			Name: oauthServer,
			Type: config.MCPTransportHTTP,
			URL:  srv.MCPURL,
			// A zero timeout expires the connect context immediately.
			Timeout: config.Duration(30 * time.Second),
		}},
	}
	d.mcp = tachimcp.NewManager(t.Context(), d.cfg, logger.Default())
	d.mcp.MarkInitDone()

	const sid = "s1"
	d.runs[sid] = &sessionRun{}
	return d, &AgentService{desk: d}, sid, srv
}

// oauthServer is the fixture's name in the test config.
const oauthServer = "oauth"

// TestAuthorizeMCPServerConnectsAndAnnouncesTheTools is the whole feature in one test:
// the flow runs, the token is used, the tools land in the shared pool and the model is
// told about them.
//
// The browser is the only thing stubbed (a real one would open a window on the machine
// running the tests); everything else — discovery, DCR, PKCE, the loopback callback, the
// token exchange, the authenticated connect — is the real stack against the real fixture.
func TestAuthorizeMCPServerConnectsAndAnnouncesTheTools(t *testing.T) {
	d, svc, sid, srv := newMCPOAuthApp(t)

	orig := tachimcp.BrowserOpener
	tachimcp.BrowserOpener = headlessBrowser()
	t.Cleanup(func() { tachimcp.BrowserOpener = orig })

	// A live agent, so the "these tools exist now" reminder can be observed — the same
	// half of the enable path a manual toggle has to get right.
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

	if res := svc.AuthorizeMCPServer(oauthServer); res != "ok" {
		t.Fatalf("AuthorizeMCPServer: %s", res)
	}

	fullName := "mcp__" + oauthServer + "__" + mcpauth.ToolName
	waitForMCP(t, d, oauthServer, fullName)

	if block := collector.Collect(t.Context(), systemreminder.Context{}); !strings.Contains(block, fullName) {
		t.Errorf("authorizing a server must announce its tools, got %q", block)
	}
	// The badge is what the panel draws the button from: after a successful
	// authorization it has to be gone, or the panel keeps asking for something already
	// granted.
	if d.mcpNeedsAuthSnapshot()[oauthServer] {
		t.Error("a connected server must not still be marked as needing authorization")
	}
	// And the token really was exchanged, not merely "no error".
	if len(srv.TokenRequestsDone()) != 1 {
		t.Errorf("expected one token exchange, got %d", len(srv.TokenRequestsDone()))
	}
}

// TestAuthorizeMCPServerRefusesWhatItCannotDo pins the three refusals: they are the
// difference between a button that explains itself and one that appears to do nothing.
func TestAuthorizeMCPServerRefusesWhatItCannotDo(t *testing.T) {
	d, svc, _, _ := newMCPOAuthApp(t)

	if res := svc.AuthorizeMCPServer("nope"); res != "server not found" {
		t.Errorf("unknown server = %q", res)
	}

	// stdio servers have no OAuth flow at all.
	d.cfg.MCPServers = append(d.cfg.MCPServers, config.MCPServerConfig{
		Name: "stub", Type: config.MCPTransportStdio, Command: "/bin/true",
	})
	if res := svc.AuthorizeMCPServer("stub"); !strings.Contains(res, "stdio") {
		t.Errorf("stdio server = %q, want a refusal naming the transport", res)
	}

	// One flow per server: two would race the same callback port and token file.
	if !d.beginMCPAuth(oauthServer) {
		t.Fatal("fixture: the slot should be free")
	}
	defer d.endMCPAuth(oauthServer)
	if res := svc.AuthorizeMCPServer(oauthServer); !strings.Contains(res, "已经") {
		t.Errorf("a second flow = %q, want a refusal", res)
	}
}

// TestMCPServerNeedingAuthIsVisibleAndSaysSo covers the state this feature exists for:
// a server that answers "authorize me first" MUST be distinguishable from one that is
// simply down, and the enable that discovers it must say what to do about it.
func TestMCPServerNeedingAuthIsVisibleAndSaysSo(t *testing.T) {
	_, svc, _, _ := newMCPOAuthApp(t)

	// No token, so the connect comes back OAuthRequiredError.
	res := svc.SetMCPServerEnabled(oauthServer, true)
	if !strings.Contains(res, "授权") {
		t.Errorf("enabling a server that needs authorization = %q, want the panel's own wording", res)
	}

	servers := svc.ListMCPServers()
	if len(servers) != 1 {
		t.Fatalf("expected one server, got %d", len(servers))
	}
	if !servers[0].NeedsAuth {
		t.Error("the panel has nothing to act on unless the server is marked as needing authorization")
	}
	if servers[0].Connected {
		t.Error("a server that refused the connection is not connected")
	}
}

// TestCompleteMCPServerAuthFromAPastedRedirectURL covers the way through when the
// callback never reaches the app: the browser is open but the redirect cannot be
// delivered, so the user pastes the URL out of the address bar.
//
// It also pins the ordering that makes it work: the running flow is cancelled before the
// exchange, because both ways out spend the same single-use authorization code.
func TestCompleteMCPServerAuthFromAPastedRedirectURL(t *testing.T) {
	d, svc, _, _ := newMCPOAuthApp(t)

	// The "browser" opens the URL but never follows the redirect: it hands the URL to
	// the test instead, which is exactly what a user copying the address bar has.
	authURLs := make(chan string, 1)
	orig := tachimcp.BrowserOpener
	tachimcp.BrowserOpener = func(u string) error {
		select {
		case authURLs <- u:
		default:
		}
		return nil
	}
	t.Cleanup(func() { tachimcp.BrowserOpener = orig })

	if res := svc.AuthorizeMCPServer(oauthServer); res != "ok" {
		t.Fatalf("AuthorizeMCPServer: %s", res)
	}

	var raw string
	select {
	case raw = <-authURLs:
	case <-time.After(15 * time.Second):
		t.Fatal("the flow never reported an authorization URL to the browser")
	}

	if res := svc.CompleteMCPServerAuth(oauthServer, ""); !strings.Contains(res, "粘贴") {
		t.Errorf("an empty paste = %q, want a refusal", res)
	}

	redirect := redirectOf(t, raw)
	if res := svc.CompleteMCPServerAuth(oauthServer, redirect); res != "ok" {
		t.Fatalf("CompleteMCPServerAuth: %s", res)
	}

	fullName := "mcp__" + oauthServer + "__" + mcpauth.ToolName
	waitForMCP(t, d, oauthServer, fullName)
	if d.mcpNeedsAuthSnapshot()[oauthServer] {
		t.Error("the badge must be cleared once the pasted URL produced a token")
	}
}

// waitForMCP waits until the server is connected and its tool is in the shared pool.
//
// The authorization runs in a background goroutine (it waits on a browser and a human),
// so there is no return value to await — the connection IS the completion signal, and it
// is the same fact the panel renders.
func waitForMCP(t *testing.T, d *desktopApp, server, tool string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if d.mcp.IsConnected(server) && d.mcp.Pool().Get(tool) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never connected with %s in the pool", server, tool)
}

// headlessBrowser is the one HTTP GET a browser would make first, with no window.
func headlessBrowser() func(string) error {
	return func(rawURL string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
}

// redirectOf fetches an authorization URL WITHOUT following the redirect and returns the
// Location it points at: what the browser's address bar would show.
func redirectOf(t *testing.T, rawURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("fetch authorization URL: %v", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("the authorization server did not redirect with a code")
	}
	return loc
}
