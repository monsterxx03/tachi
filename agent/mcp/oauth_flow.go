package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

const (
	oauthLocalPortStart  = 18273
	oauthLocalPortEnd    = 18283
	oauthCallbackPath    = "/callback"
	oauthCallbackTimeout = 5 * time.Minute
)

// oauthRedirectURI returns the OAuth2 redirect_uri for the server's config.
// CallbackHost defaults to 127.0.0.1; CallbackPort defaults to the automatic
// port range start (18273) so the URI is consistent between auth request and
// token exchange even in manual mode (no actual listener needed).
func oauthRedirectURI(srv *config.MCPServerConfig) string {
	host := srv.OAuth.CallbackHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := srv.OAuth.CallbackPort
	if port == 0 {
		port = oauthLocalPortStart
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, oauthCallbackPath)
}

// OAuthRequiredError signals that a server requires OAuth2 authorization
// before tools can be discovered. The caller should run the OAuth flow
// and then retry the connection.
type OAuthRequiredError struct {
	ServerName string
}

func (e *OAuthRequiredError) Error() string {
	return fmt.Sprintf("MCP server %q requires OAuth authorization — run /mcp auth %s to authorize", e.ServerName, e.ServerName)
}

// RunOAuthFlow performs the interactive OAuth2 flow for an MCP server.
// It first tries browser callback (opens browser, listens on a local port);
// on failure it falls back to instructing the user to paste the redirect URL.
//
// If ClientID is not configured, dynamic client registration (DCR) is
// attempted automatically. PKCE is always enabled.
func RunOAuthFlow(ctx context.Context, srv *config.MCPServerConfig, runErrFn func(string)) error {
	// Initialize OAuth config if not present — the whole flow works with
	// auto-discovery + DCR, no manual configuration required.
	if srv.OAuth == nil {
		srv.OAuth = &config.MCPOAuthConfig{}
	}

	store, err := NewFileTokenStore(srv.Name)
	if err != nil {
		return fmt.Errorf("token store: %w", err)
	}

	if err := ensureClientID(ctx, srv, store); err != nil {
		return fmt.Errorf("DCR: %w", err)
	}

	// 1) Browser callback
	if err := tryBrowserCallback(ctx, srv); err == nil {
		return nil
	} else {
		debuglog.Log("MCP: browser callback failed for %q: %v", srv.Name, err)
	}

	// 2) Manual fallback
	return startManualFlow(ctx, srv, runErrFn)
}

// CompleteManualAuth finishes the manual OAuth flow by exchanging the
// redirect URL pasted by the user for a token.
func CompleteManualAuth(ctx context.Context, srv *config.MCPServerConfig, redirectURL string) error {
	if srv.OAuth == nil {
		srv.OAuth = &config.MCPOAuthConfig{}
	}

	store, err := NewFileTokenStore(srv.Name)
	if err != nil {
		return fmt.Errorf("token store: %w", err)
	}

	if err := ensureClientID(ctx, srv, store); err != nil {
		return fmt.Errorf("DCR: %w", err)
	}

	code, redirectState, err := parseRedirectURL(redirectURL)
	if err != nil {
		return err
	}

	// Load the pending state persisted by startManualFlow
	pending, err := store.GetPendingState(ctx)
	if err != nil {
		return fmt.Errorf("no pending OAuth state (may have expired or already been used): %w", err)
	}
	if pending.State != "" && pending.State != redirectState {
		return fmt.Errorf("OAuth state mismatch — possible CSRF attack")
	}

	baseURL := stripQueryFragment(srv.URL)
	oauthCfg := transport.OAuthConfig{
		ClientID:              srv.OAuth.ClientID,
		ClientSecret:          srv.OAuth.ClientSecret,
		ClientURI:             srv.OAuth.ClientURI,
		Scopes:                srv.OAuth.Scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: srv.OAuth.AuthServerMetadataURL,
		TokenStore:            store,
		RedirectURI:           oauthRedirectURI(srv),
	}

	return exchangeCode(ctx, oauthCfg, baseURL, code, pending.CodeVerifier)
}

// ensureClientID guarantees that srv.OAuth.ClientID is non-empty by the
// time OAuth flow proceeds. If ClientID is already in the config, this is
// a no-op. Otherwise it checks the persisted DCR info; if still empty, it
// performs dynamic client registration.
//
// Discovery follows the MCP spec but works around mcp-go's overly strict
// advertised-PRM validation (which rejects PRM responses lacking a
// "resource" field):
//  1. Probe server URL → 401 → extract resource_metadata from WWW-Authenticate
//  2. Fetch PRM → authorization_servers[]
//  3. Fetch AS metadata from each authorization server → find registration_endpoint
//  4. Feed AuthServerMetadataURL (not ProtectedResourceMetadataURL) to mcp-go
//     so it skips the advertised-PRM check entirely
//  5. Call handler.RegisterClient
func ensureClientID(ctx context.Context, srv *config.MCPServerConfig, store *FileTokenStore) error {
	if srv.OAuth.ClientID != "" {
		return nil
	}

	// 1) Try persistent DCR info from last time
	if dcr, err := store.GetDCRInfo(ctx); err == nil {
		srv.OAuth.ClientID = dcr.ClientID
		srv.OAuth.ClientSecret = dcr.ClientSecret
		if dcr.AuthServerMetadataURL != "" {
			srv.OAuth.AuthServerMetadataURL = dcr.AuthServerMetadataURL
		}
		debuglog.Log("MCP: restored DCR client_id for %q", srv.Name)
		return nil
	}

	// 2) Discover AS metadata URL with registration_endpoint
	asMetaURL, err := discoverAuthServerMetadataURL(ctx, srv)
	if err != nil {
		return fmt.Errorf("DCR not available: %w", err)
	}

	// 3) Perform DCR — mcp-go fetches the AS metadata URL directly (no PRM validation)
	handler := transport.NewOAuthHandler(transport.OAuthConfig{
		ClientID:              "",
		ClientURI:             srv.OAuth.ClientURI,
		ClientSecret:          srv.OAuth.ClientSecret,
		Scopes:                srv.OAuth.Scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: asMetaURL,
		TokenStore:            store,
	})
	handler.SetBaseURL(stripQueryFragment(srv.URL))

	if err := handler.RegisterClient(ctx, "tachi"); err != nil {
		return fmt.Errorf("dynamic client registration failed: %w", err)
	}

	clientID := handler.GetClientID()
	clientSecret := handler.GetClientSecret()

	srv.OAuth.ClientID = clientID
	srv.OAuth.ClientSecret = clientSecret

	// Persist the discovered AS metadata URL so subsequent OAuth flow
	// steps (browser callback, manual flow, token refresh) can use the
	// correct authorization_endpoint / token_endpoint.
	srv.OAuth.AuthServerMetadataURL = asMetaURL

	dcrInfo := &DCRInfo{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		AuthServerMetadataURL: asMetaURL,
	}
	if err := store.SaveDCRInfo(ctx, dcrInfo); err != nil {
		debuglog.Log("MCP: failed to persist DCR info for %q: %v", srv.Name, err)
	}

	debuglog.Log("MCP: DCR succeeded for %q — client_id=%s", srv.Name, clientID)
	return nil
}

// discoverAuthServerMetadataURL finds an authorization server metadata URL
// that carries a registration_endpoint, by walking:
//  401 resource_metadata → PRM → authorization_servers → AS metadata.
func discoverAuthServerMetadataURL(ctx context.Context, srv *config.MCPServerConfig) (string, error) {
	baseURL := stripQueryFragment(srv.URL)
	client := &http.Client{Timeout: 30 * time.Second}
	debuglog.Log("MCP: discovering auth server metadata for %q (baseURL=%s)", srv.Name, baseURL)

	// 1) Get PRM URL(s)
	var prmURLs []string
	if url := probe401ResourceMetadata(ctx, baseURL); url != "" {
		prmURLs = []string{url}
		debuglog.Log("MCP: got PRM URL from 401 WWW-Authenticate: %s", url)
	} else {
		debuglog.Log("MCP: no resource_metadata in 401 response, constructing PRM URLs from base URL")
	}
	if len(prmURLs) == 0 {
		prmURLs = buildWellKnownURLs(baseURL, "oauth-protected-resource")
		debuglog.Log("MCP: constructed %d PRM URL(s): %v", len(prmURLs), prmURLs)
	}

	// 2) From each PRM, extract authorization_servers
	var authServers []string
	for _, prmURL := range prmURLs {
		as := fetchAuthorizationServers(ctx, client, prmURL)
		if len(as) > 0 {
			authServers = as
			debuglog.Log("MCP: PRM %s returned %d authorization server(s): %v", prmURL, len(as), as)
			break
		}
		debuglog.Log("MCP: PRM %s returned no authorization_servers", prmURL)
	}
	if len(authServers) == 0 {
		debuglog.Log("MCP: no authorization_servers from PRM, falling back to base URL %s", baseURL)
		authServers = []string{baseURL}
	}

	// 3) Try AS metadata URLs from each authorization server
	for _, asBase := range authServers {
		debuglog.Log("MCP: trying AS metadata from %s", asBase)
		if url := findAuthServerMetadataURL(ctx, client, asBase); url != "" {
			debuglog.Log("MCP: found AS metadata with registration_endpoint: %s", url)
			return url, nil
		}
		debuglog.Log("MCP: no AS metadata with registration_endpoint at %s", asBase)
	}

	debuglog.Log("MCP: exhausted all authorization servers, no registration_endpoint found")
	return "", fmt.Errorf("no authorization server with registration_endpoint found")
}

// fetchAuthorizationServers fetches a PRM document and returns its
// authorization_servers slice.
func fetchAuthorizationServers(ctx context.Context, client *http.Client, prmURL string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prmURL, nil)
	if err != nil {
		debuglog.Log("MCP: PRM request creation failed for %s: %v", prmURL, err)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")

	resp, err := client.Do(req)
	if err != nil {
		debuglog.Log("MCP: PRM request %s failed: %v", prmURL, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		debuglog.Log("MCP: PRM %s returned status %d", prmURL, resp.StatusCode)
		return nil
	}

	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prm); err != nil {
		debuglog.Log("MCP: failed to decode PRM from %s: %v", prmURL, err)
		return nil
	}
	return prm.AuthorizationServers
}

// findAuthServerMetadataURL tries the raw asBase first (some PRM responses
// return the full .well-known URL directly), then falls back to derived
// AS metadata URLs (simple-append + RFC 8414 path-insertion). Returns the
// first URL whose response contains a registration_endpoint.
func findAuthServerMetadataURL(ctx context.Context, client *http.Client, asBase string) string {
	// Some authorization servers return the full AS metadata URL from PRM.
	// Try the raw asBase first before deriving candidates.
	candidates := append([]string{asBase}, asMetadataURLs(asBase)...)
	for _, metaURL := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
		if err != nil {
			debuglog.Log("MCP: AS metadata request creation failed for %s: %v", metaURL, err)
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2025-03-26")

		resp, err := client.Do(req)
		if err != nil {
			debuglog.Log("MCP: AS metadata request %s failed: %v", metaURL, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			debuglog.Log("MCP: AS metadata %s returned status %d", metaURL, resp.StatusCode)
			resp.Body.Close()
			continue
		}

		var meta struct {
			RegistrationEndpoint string `json:"registration_endpoint"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&meta)
		resp.Body.Close()

		if decodeErr != nil {
			debuglog.Log("MCP: failed to decode AS metadata from %s: %v", metaURL, decodeErr)
			continue
		}
		if meta.RegistrationEndpoint != "" {
			debuglog.Log("MCP: found registration_endpoint=%s via %s", meta.RegistrationEndpoint, metaURL)
			return metaURL
		}
		debuglog.Log("MCP: AS metadata %s has no registration_endpoint", metaURL)
	}
	return ""
}

// asMetadataURLs returns the ordered list of AS metadata discovery URLs
// for the given issuer. Tries both simple-append (common in practice) and
// RFC 8414 §3 path-insertion (well-known between authority and path),
// plus OpenID Connect Discovery fallbacks.
func asMetadataURLs(issuerURL string) []string {
	u, err := url.Parse(issuerURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	u.RawQuery = ""
	u.Fragment = ""
	originalPath := strings.Trim(u.Path, "/")

	var urls []string

	// Simple append: /.well-known/<suffix> after the path (most servers)
	for _, suffix := range []string{"oauth-authorization-server", "openid-configuration"} {
		v := *u
		if originalPath == "" {
			v.Path = "/.well-known/" + suffix
		} else {
			v.Path = "/" + originalPath + "/.well-known/" + suffix
		}
		urls = append(urls, v.String())
	}

	// RFC 8414 path-insertion: /.well-known/<suffix> between authority and path
	for _, suffix := range []string{"oauth-authorization-server", "openid-configuration"} {
		v := *u
		if originalPath == "" {
			v.Path = "/.well-known/" + suffix
		} else {
			v.Path = "/.well-known/" + suffix + "/" + originalPath
		}
		urls = append(urls, v.String())
	}

	return urls
}

// probe401ResourceMetadata sends a request to baseURL and extracts the
// resource_metadata parameter from the WWW-Authenticate header on a 401.
func probe401ResourceMetadata(ctx context.Context, baseURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}

	return extractResourceMetadataURL(resp.Header.Values("WWW-Authenticate"))
}

// extractResourceMetadataURL parses the resource_metadata parameter from
// WWW-Authenticate headers per RFC 9728 §5.1.
func extractResourceMetadataURL(headers []string) string {
	for _, h := range headers {
		u := extractQuotedParam(h, "resource_metadata")
		if u != "" {
			return u
		}
	}
	return ""
}

// extractQuotedParam extracts a named parameter value from a WWW-Authenticate
// header. Supports quoted-string values only.
func extractQuotedParam(header, paramName string) string {
	lower := strings.ToLower(header)
	target := strings.ToLower(paramName) + "=\""
	idx := strings.Index(lower, target)
	if idx < 0 {
		return ""
	}
	start := idx + len(target)
	end := strings.IndexByte(header[start:], '"')
	if end < 0 {
		return ""
	}
	return header[start : start+end]
}

// buildWellKnownURLs returns both simple-append (path/.well-known/<suffix>)
// and RFC 8414 path-insertion (/.well-known/<suffix>/path) URLs.
func buildWellKnownURLs(baseURL, suffix string) []string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	u.RawQuery = ""
	u.Fragment = ""
	path := strings.Trim(u.Path, "/")

	var urls []string

	v := *u
	if path == "" {
		v.Path = "/.well-known/" + suffix
	} else {
		v.Path = "/" + path + "/.well-known/" + suffix
	}
	urls = append(urls, v.String())

	w := *u
	if path == "" {
		w.Path = "/.well-known/" + suffix
	} else {
		w.Path = "/.well-known/" + suffix + "/" + path
	}
	urls = append(urls, w.String())

	return urls
}

// --- browser callback ---

func tryBrowserCallback(ctx context.Context, srv *config.MCPServerConfig) error {
	store, err := NewFileTokenStore(srv.Name)
	if err != nil {
		return fmt.Errorf("token store: %w", err)
	}

	baseURL := stripQueryFragment(srv.URL)

	if srv.OAuth.CallbackHost == "" {
		srv.OAuth.CallbackHost = "127.0.0.1"
	}

	if srv.OAuth.CallbackPort == 0 {
		port, err := findAvailablePort(srv.OAuth.CallbackHost)
		if err != nil {
			return err
		}
		srv.OAuth.CallbackPort = port
	}

	oauthCfg := transport.OAuthConfig{
		ClientID:              srv.OAuth.ClientID,
		ClientSecret:          srv.OAuth.ClientSecret,
		ClientURI:             srv.OAuth.ClientURI,
		Scopes:                srv.OAuth.Scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: srv.OAuth.AuthServerMetadataURL,
		TokenStore:            store,
		RedirectURI:           oauthRedirectURI(srv),
	}

	handler := transport.NewOAuthHandler(oauthCfg)
	handler.SetBaseURL(baseURL)

	state, err := transport.GenerateState()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	codeVerifier, _ := transport.GenerateCodeVerifier()
	codeChallenge := transport.GenerateCodeChallenge(codeVerifier)

	authURL, err := handler.GetAuthorizationURL(ctx, state, codeChallenge)
	if err != nil {
		return fmt.Errorf("auth URL: %w", err)
	}

	// Local callback server
	var (
		mu      sync.Mutex
		gotCode string
		gotErr  string
	)
	done := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc(oauthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")

		mu.Lock()
		defer mu.Unlock()

		if e := q.Get("error"); e != "" {
			gotErr = fmt.Sprintf("OAuth error: %s — %s", e, q.Get("error_description"))
			fmt.Fprintf(w, "<html><body><h1>Auth Failed</h1><p>%s</p></body></html>", gotErr)
			close(done)
			return
		}
		if q.Get("state") != state {
			gotErr = "state mismatch — possible CSRF attack"
			fmt.Fprintf(w, "<html><body><h1>Auth Failed</h1><p>State mismatch</p></body></html>")
			close(done)
			return
		}

		gotCode = code
		fmt.Fprintf(w, "<html><body><h1>Auth Successful ✓</h1><p>You can close this window.</p></body></html>")
		close(done)
	})

	serverAddr := fmt.Sprintf("%s:%d", srv.OAuth.CallbackHost, srv.OAuth.CallbackPort)
	httpSrv := &http.Server{
		Addr:           serverAddr,
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    oauthCallbackTimeout,
		MaxHeaderBytes: 1 << 20,
	}

	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	// Open the browser. If that fails (e.g. headless server), bail immediately
	// rather than waiting 5 minutes for a callback that will never arrive.
	if err := openBrowser(authURL); err != nil {
		return fmt.Errorf("cannot open browser: %w", err)
	}

	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		if gotErr != "" {
			return errors.New(gotErr)
		}
		if gotCode == "" {
			return errors.New("no authorization code received")
		}
		if err := handler.ProcessAuthorizationResponse(ctx, gotCode, state, codeVerifier); err != nil {
			return fmt.Errorf("token exchange: %w", err)
		}
		debuglog.Log("MCP: browser OAuth succeeded for %q", srv.Name)
		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-time.After(oauthCallbackTimeout):
		return errors.New("timed out waiting for browser redirect")
	}
}

// --- manual flow ---

func startManualFlow(ctx context.Context, srv *config.MCPServerConfig, runErrFn func(string)) error {
	store, err := NewFileTokenStore(srv.Name)
	if err != nil {
		return err
	}

	// Resolve callback address before building the OAuth config so the
	// redirect_uri in the auth request matches the actual listener.
	if srv.OAuth.CallbackHost == "" {
		srv.OAuth.CallbackHost = "127.0.0.1"
	}
	if srv.OAuth.CallbackPort == 0 {
		p, err := findAvailablePort(srv.OAuth.CallbackHost)
		if err != nil {
			// Can't listen anywhere — skip straight to pure manual.
			// We still need the oauthCfg to build the auth URL, but
			// there's no point trying to listen.
			return deliverManualInstructions(runErrFn, srv.Name, buildManualAuthURL(ctx, srv, store))
		}
		srv.OAuth.CallbackPort = p
	}

	oauthCfg := transport.OAuthConfig{
		ClientID:              srv.OAuth.ClientID,
		ClientSecret:          srv.OAuth.ClientSecret,
		ClientURI:             srv.OAuth.ClientURI,
		Scopes:                srv.OAuth.Scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: srv.OAuth.AuthServerMetadataURL,
		TokenStore:            store,
		RedirectURI:           oauthRedirectURI(srv),
	}

	baseURL := stripQueryFragment(srv.URL)
	handler := transport.NewOAuthHandler(oauthCfg)
	handler.SetBaseURL(baseURL)

	state, _ := transport.GenerateState()
	codeVerifier, _ := transport.GenerateCodeVerifier()
	codeChallenge := transport.GenerateCodeChallenge(codeVerifier)

	// Persist state + verifier so CompleteManualAuth can pick them up
	_ = store.SavePendingState(ctx, &OAuthPendingState{
		State:        state,
		CodeVerifier: codeVerifier,
	})

	authURL, _ := handler.GetAuthorizationURL(ctx, state, codeChallenge)
	debuglog.Log("MCP: manual OAuth authorize URL for %q: %s", srv.Name, authURL)

	mux := http.NewServeMux()
	var (
		mu      sync.Mutex
		gotCode string
		gotErr  string
	)
	done := make(chan struct{})

	mux.HandleFunc(oauthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")

		mu.Lock()
		defer mu.Unlock()

		if e := q.Get("error"); e != "" {
			gotErr = fmt.Sprintf("OAuth error: %s — %s", e, q.Get("error_description"))
			fmt.Fprintf(w, "<html><body><h1>Auth Failed</h1><p>%s</p></body></html>", gotErr)
			close(done)
			return
		}
		if q.Get("state") != state {
			gotErr = "state mismatch — possible CSRF attack"
			fmt.Fprintf(w, "<html><body><h1>Auth Failed</h1><p>State mismatch</p></body></html>")
			close(done)
			return
		}

		gotCode = code
		fmt.Fprintf(w, "<html><body><h1>Auth Successful ✓</h1><p>You can close this window.</p></body></html>")
		close(done)
	})

	serverAddr := fmt.Sprintf("%s:%d", srv.OAuth.CallbackHost, srv.OAuth.CallbackPort)
	httpSrv := &http.Server{
		Addr:           serverAddr,
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    oauthCallbackTimeout,
		MaxHeaderBytes: 1 << 20,
	}

	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return deliverManualInstructions(runErrFn, srv.Name, authURL)
	}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	// Show the URL and wait for callback or timeout
	runErrFn(fmt.Sprintf(
		"**%s** requires OAuth authorization.\n\n"+
			"1. Open this URL:\n   %s\n\n"+
			"Waiting for authorization...",
		srv.Name, authURL,
	))

	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		if gotErr != "" {
			return errors.New(gotErr)
		}
		if gotCode == "" {
			return errors.New("no authorization code received")
		}
		if err := handler.ProcessAuthorizationResponse(ctx, gotCode, state, codeVerifier); err != nil {
			return fmt.Errorf("token exchange: %w", err)
		}
		debuglog.Log("MCP: manual callback OAuth succeeded for %q", srv.Name)
		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-time.After(oauthCallbackTimeout):
		return deliverManualInstructions(runErrFn, srv.Name, authURL)
	}
}

// buildManualAuthURL generates an authorization URL for the pure-manual
// path (no listener possible), persisting state + verifier for later exchange.
func buildManualAuthURL(ctx context.Context, srv *config.MCPServerConfig, store *FileTokenStore) string {
	oauthCfg := transport.OAuthConfig{
		ClientID:              srv.OAuth.ClientID,
		ClientSecret:          srv.OAuth.ClientSecret,
		ClientURI:             srv.OAuth.ClientURI,
		Scopes:                srv.OAuth.Scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: srv.OAuth.AuthServerMetadataURL,
		TokenStore:            store,
		RedirectURI:           oauthRedirectURI(srv),
	}

	baseURL := stripQueryFragment(srv.URL)
	handler := transport.NewOAuthHandler(oauthCfg)
	handler.SetBaseURL(baseURL)

	state, _ := transport.GenerateState()
	codeVerifier, _ := transport.GenerateCodeVerifier()
	codeChallenge := transport.GenerateCodeChallenge(codeVerifier)

	_ = store.SavePendingState(ctx, &OAuthPendingState{
		State:        state,
		CodeVerifier: codeVerifier,
	})

	authURL, _ := handler.GetAuthorizationURL(ctx, state, codeChallenge)
	debuglog.Log("MCP: no-listener OAuth authorize URL for %q: %s", srv.Name, authURL)
	return authURL
}

// deliverManualInstructions informs the user how to complete the OAuth flow
// by pasting the redirect URL back.
func deliverManualInstructions(runErrFn func(string), serverName, authURL string) error {
	runErrFn(fmt.Sprintf(
		"**%s** requires OAuth authorization.\n\n"+
			"1. Open this URL:\n   %s\n\n"+
			"2. Complete authorization.\n\n"+
			"3. **Copy the full URL** from your browser's address bar\n"+
			"   (the page may not load — that's expected).\n\n"+
			"4. Run: `/mcp auth %s <pasted-url>`",
		serverName, authURL, serverName,
	))
	return &OAuthRequiredError{ServerName: serverName}
}

// --- helpers ---

func exchangeCode(ctx context.Context, cfg transport.OAuthConfig, baseURL string, code string, codeVerifier string) error {
	handler := transport.NewOAuthHandler(cfg)
	handler.SetBaseURL(baseURL)

	meta, err := handler.GetServerMetadata(ctx)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("client_id", cfg.ClientID)
	data.Set("redirect_uri", cfg.RedirectURI)
	if cfg.ClientSecret != "" {
		data.Set("client_secret", cfg.ClientSecret)
	}
	if codeVerifier != "" {
		data.Set("code_verifier", codeVerifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var token transport.Token
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return fmt.Errorf("decode token: %w", err)
	}

	if token.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}

	if err := cfg.TokenStore.SaveToken(ctx, &token); err != nil {
		return fmt.Errorf("save token: %w", err)
	}

	debuglog.Log("MCP: manual OAuth succeeded")
	return nil
}

func findAvailablePort(host string) (int, error) {
	for port := oauthLocalPortStart; port < oauthLocalPortEnd; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no port in %d-%d", oauthLocalPortStart, oauthLocalPortEnd-1)
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func parseRedirectURL(raw string) (code, state string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	code = u.Query().Get("code")
	state = u.Query().Get("state")
	// Also try the fragment (implicit flow)
	if code == "" && u.Fragment != "" {
		fv, _ := url.ParseQuery(u.Fragment)
		code = fv.Get("code")
		if state == "" {
			state = fv.Get("state")
		}
	}
	if code == "" {
		return "", "", fmt.Errorf("no authorization code in URL")
	}
	return code, state, nil
}

func stripQueryFragment(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
