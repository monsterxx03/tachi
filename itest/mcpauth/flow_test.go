package mcpauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mcpauth"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the fixture's own contract test, and it is also the shortest description
// of what the fixture is for: the REAL client stack (discovery → DCR → PKCE → callback →
// token → tool call) runs against it here, with nothing stubbed but the browser.
//
// It lives beside the mock for the same reason it exists: a fixture that only the
// feature's own test exercises is a fixture whose breakage is reported as the feature's.

// headlessBrowser replaces the browser with the one HTTP GET a browser would make first.
// The fake authorizes instantly and redirects back, so the app's own callback listener
// completes the round trip — no window, no tab.
func headlessBrowser(t *testing.T) func(string) error {
	t.Helper()
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

func TestRunOAuthFlowAndConnectAgainstTheFake(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	srv, err := mcpauth.New()
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	// The client's browser is swapped for the duration of the test: the flow would
	// otherwise try to open a real window on the machine running it.
	orig := mcp.BrowserOpener
	mcp.BrowserOpener = headlessBrowser(t)
	t.Cleanup(func() { mcp.BrowserOpener = orig })

	// Timeout is not optional: a zero connect timeout expires the context immediately,
	// and the failure reads as "context deadline exceeded" even though nothing is wrong
	// with the server.
	srvCfg := &config.MCPServerConfig{
		Name:    "fake",
		Type:    config.MCPTransportHTTP,
		URL:     srv.MCPURL,
		Timeout: config.Duration(30 * time.Second),
	}

	var phases []string
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, mcp.RunOAuthFlowWithProgress(ctx, srvCfg, func(p mcp.OAuthProgress) {
		phases = append(phases, string(p.Phase))
	}))

	// The flow reported the browser phase with the URL it opened...
	require.Contains(t, phases, string(mcp.OAuthPhaseBrowser))

	// ...it registered a client (no client_id was configured)...
	regs := srv.Registrations()
	require.Len(t, regs, 1)
	require.Len(t, regs[0].RedirectURIs, 1, "registration body: %s", regs[0].Raw)
	assert.Contains(t, regs[0].RedirectURIs[0], "127.0.0.1")

	// ...asked for PKCE, and proved it at the token endpoint (the fake refuses a
	// verifier that does not match the challenge).
	auths := srv.Authorizations()
	require.Len(t, auths, 1)
	assert.Equal(t, "S256", auths[0].ChallengeMethod)
	assert.NotEmpty(t, auths[0].CodeChallenge)
	tokens := srv.TokenRequestsDone()
	require.Len(t, tokens, 1)
	assert.NotEmpty(t, tokens[0].CodeVerifier)

	// The token that came back is what the connection now uses: the tool is listed and
	// callable, which is the only proof that the authorization was worth anything.
	cfg := &config.Config{MCPServers: []config.MCPServerConfig{*srvCfg}}
	mgr := mcp.NewManager(ctx, cfg, logger.Default())
	t.Cleanup(mgr.Close)
	tools, errs := mgr.ConnectAll(ctx, cfg.MCPServers)
	require.Empty(t, errs)
	require.Len(t, tools, 1)
	assert.Equal(t, mcpauth.ToolName, tools[0].ToolName())

	out, err := tools[0].ExecuteContext(ctx, "{}")
	require.NoError(t, err)
	assert.Contains(t, out, mcpauth.ToolResult)
	assert.Equal(t, 1, srv.ToolCalls())
}

// TestManualFlowCompletesWithAPastedRedirectURL covers the way through when the callback
// never reaches the app: the authorization URL is opened elsewhere (a browser on another
// machine, a firewalled port) and the redirect URL is pasted back.
//
// The trigger is a browser that cannot be opened, which is exactly the real-world one —
// and the reason the flow's progress callback reports the URL as a FIELD: recovering it
// from the instruction sentence is the coupling this test would otherwise be pinning.
func TestManualFlowCompletesWithAPastedRedirectURL(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	srv, err := mcpauth.New()
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	orig := mcp.BrowserOpener
	mcp.BrowserOpener = func(string) error { return errors.New("no browser available") }
	t.Cleanup(func() { mcp.BrowserOpener = orig })

	srvCfg := &config.MCPServerConfig{
		Name:    "fake",
		Type:    config.MCPTransportHTTP,
		URL:     srv.MCPURL,
		Timeout: config.Duration(30 * time.Second),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 任何**带着 AuthURL** 的阶段都算数，不钉死 WAITING。
	//
	// 这一条以前只收 `OAuthPhaseWaiting`，而那一阶段只在「回调监听器起得来」时才发：
	// 监听器绑的是 `OAuth.CallbackPort`（默认 18273），**端口被占**就静默降级成
	// `OAuthPhaseManual` —— 同一个 URL、同一件要做的事，只是没有监听器。于是测试在等一个
	// 永远不会来的阶段，15 秒后报「never reported an authorization URL」，而失败日志里的
	// phases 明明写着 `browser (authURL=true …)` 和 `manual (authURL=true …)`。
	//
	// 这条用例要验的是「URL 能从一个**字段**里拿到，不用去猜指令那句话」，两条路都满足；
	// 真正需要监听器的是另一条用例。所以这里收 AuthURL，并且把见过的阶段留在超时信息里
	// （下面那句 Fatalf）—— 下一次红能直接看出走的是哪条路。
	var mu sync.Mutex
	var phases []string
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), phases...)
	}

	authURL := make(chan string, 1)
	go func() {
		_ = mcp.RunOAuthFlowWithProgress(ctx, srvCfg, func(p mcp.OAuthProgress) {
			mu.Lock()
			phases = append(phases, fmt.Sprintf("%s (authURL=%t, %q)", p.Phase, p.AuthURL != "", p.Message))
			mu.Unlock()
			if p.AuthURL != "" {
				select {
				case authURL <- p.AuthURL:
				default:
				}
			}
		})
	}()

	var raw string
	start := time.Now()
	select {
	case raw = <-authURL:
	case <-time.After(15 * time.Second):
		t.Fatalf("the manual flow never reported an authorization URL after %s; phases seen: %v",
			time.Since(start).Round(time.Millisecond), snapshot())
	}

	// The test plays the browser: it fetches the authorization URL WITHOUT following the
	// redirect, which is what a user copying the address bar has in hand.
	redirect := fetchWithoutRedirect(t, raw)
	require.NoError(t, mcp.CompleteManualAuth(ctx, srvCfg, redirect))

	tokens := srv.TokenRequestsDone()
	require.Len(t, tokens, 1)
	assert.NotEmpty(t, tokens[0].CodeVerifier)
}

// fetchWithoutRedirect returns the Location the authorization server redirects to, which
// is what a user copies out of their browser's address bar.
func fetchWithoutRedirect(t *testing.T, rawURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(rawURL)
	require.NoError(t, err)
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	require.NotEmpty(t, loc, "the fake must redirect with the code")
	return loc
}
