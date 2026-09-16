// Package mcpauth is a test fixture: an OAuth2-protected MCP server, in one process,
// speaking enough of both protocols for a REAL client to authorize against it and then
// list its tools.
//
// It exists because the interesting half of MCP authorization cannot be faked at the
// boundary: the discovery walk (401 → PRM → AS metadata), dynamic client registration,
// PKCE, the redirect to a loopback callback, the token exchange and the authenticated
// tool call are all things the client does to a URL, and a stub that answers them by
// hand proves nothing about whether a flow works. Both the desktop's unit test and the
// desktop smoke scenario run against THIS server, so the thing under test is the real
// client stack end to end.
//
// Nothing here imports `testing`: the smoke runner runs it in its own process, as an
// ordinary HTTP server on a loopback port.
package mcpauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ToolName is the single tool the fake server exposes. A tool call answers with
// ToolResult, so a caller can prove the round trip reached the server rather than
// stopping at "connected".
const (
	ToolName   = "echo"
	ToolResult = "mcpauth-tool-ran"
)

// Option configures the fake.
type Option func(*Server)

// WithAuthorizeDelay makes /authorize wait before redirecting: the consent screen a real
// server shows while a human reads it, and the reason a real authorization has a WAITING
// state to render at all.
//
// A test that wants to observe the state a user actually sits in — a panel saying "open
// this URL", a card with something to copy — needs a flow that does not finish before it
// can be looked at. Asserting on a flow that completes in microseconds can only ever see
// its absence.
func WithAuthorizeDelay(d time.Duration) Option {
	return func(s *Server) { s.authorizeDelay = d }
}

// Server is a running fake: an authorization server plus an OAuth-protected MCP
// endpoint, sharing one loopback origin (which is the shape real deployments have, and
// the reason a token is keyed by host).
type Server struct {
	// URL is the origin, e.g. "http://127.0.0.1:53421". MCPURL is what a client is
	// configured with.
	URL    string
	MCPURL string

	ln  net.Listener
	srv *http.Server

	// authorizeDelay is how long /authorize plays at being a consent screen.
	authorizeDelay time.Duration

	mu sync.Mutex
	// What the client actually did, for the assertions. Every one of these is a step
	// that can silently not happen (a DCR that was skipped, a PKCE verifier that never
	// left the client), which is why they are recorded rather than inferred.
	registrations  []Registration
	authorizations []Authorization
	tokenRequests  []TokenRequest
	mcpRequests    int
	toolCalls      int

	accessToken string
	codes       map[string]string // code → PKCE challenge
	clientSeq   int
}

// Registration is one dynamic client registration request.
type Registration struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	// Raw is the request body as received. A fixture is the wrong place to guess at a
	// client's field names: asserting on the raw JSON is how a mismatch shows up as a
	// failed assertion instead of a silently empty struct.
	Raw string `json:"-"`
}

// Authorization is one authorization request (what the "browser" asked for).
type Authorization struct {
	ClientID        string
	RedirectURI     string
	State           string
	CodeChallenge   string
	ChallengeMethod string
}

// TokenRequest is one token exchange.
type TokenRequest struct {
	GrantType    string
	Code         string
	ClientID     string
	RedirectURI  string
	CodeVerifier string
}

// New starts the fake on a loopback port. Close it when done.
func New(opts ...Option) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:          ln,
		URL:         "http://" + ln.Addr().String(),
		accessToken: "mcpauth-access-token",
		codes:       map[string]string{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.MCPURL = s.URL + "/mcp"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handlePRM)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)

	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Close stops the server.
func (s *Server) Close() {
	_ = s.srv.Close()
}

// Registrations returns the dynamic client registrations seen so far.
func (s *Server) Registrations() []Registration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Registration(nil), s.registrations...)
}

// Authorizations returns the authorization requests seen so far.
func (s *Server) Authorizations() []Authorization {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Authorization(nil), s.authorizations...)
}

// TokenRequestsDone returns the token exchanges seen so far.
func (s *Server) TokenRequestsDone() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TokenRequest(nil), s.tokenRequests...)
}

// MCPRequests is how many requests reached the MCP endpoint AT ALL (authorized or not).
func (s *Server) MCPRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpRequests
}

// ToolCalls is how many authenticated tools/call round trips the server answered.
func (s *Server) ToolCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolCalls
}

// --- OAuth endpoints ---

// handlePRM answers RFC 9728 protected-resource metadata. The fake is its own
// authorization server, so authorization_servers points back at the origin.
func (s *Server) handlePRM(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.MCPURL,
		"authorization_servers": []string{s.URL},
	})
}

// handleASMetadata answers RFC 8414 authorization-server metadata. The
// registration_endpoint is what makes DCR possible — a client with no client_id has no
// other way in.
func (s *Server) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                           s.URL,
		"authorization_endpoint":           s.URL + "/authorize",
		"token_endpoint":                   s.URL + "/token",
		"registration_endpoint":            s.URL + "/register",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req Registration
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	req.Raw = string(body)

	s.mu.Lock()
	s.registrations = append(s.registrations, req)
	s.clientSeq++
	clientID := fmt.Sprintf("mcpauth-client-%d", s.clientSeq)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"token_endpoint_auth_method": "none",
	})
}

// handleAuthorize plays the part of the user's browser reaching the consent screen: it
// records what was asked for and redirects straight back with a code. A real server would
// wait for a human; a test wants the round trip, not the pause.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	auth := Authorization{
		ClientID:        q.Get("client_id"),
		RedirectURI:     q.Get("redirect_uri"),
		State:           q.Get("state"),
		CodeChallenge:   q.Get("code_challenge"),
		ChallengeMethod: q.Get("code_challenge_method"),
	}

	code := fmt.Sprintf("mcpauth-code-%d", len(s.authorizations)+1)

	s.mu.Lock()
	s.authorizations = append(s.authorizations, auth)
	s.codes[code] = auth.CodeChallenge
	delay := s.authorizeDelay
	s.mu.Unlock()

	// The consent screen a real server shows while a human reads it (see WithAuthorizeDelay).
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if auth.RedirectURI == "" {
		http.Error(w, "authorization request has no redirect_uri", http.StatusBadRequest)
		return
	}
	target := fmt.Sprintf("%s?code=%s&state=%s", auth.RedirectURI, code, auth.State)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleToken exchanges a code for the access token, and it REFUSES a PKCE verifier that
// does not hash to the challenge sent at /authorize: a test that accepts anything cannot
// tell a working exchange from one that dropped the verifier on the way.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := parseForm(string(body))
	tr := TokenRequest{
		GrantType:    form["grant_type"],
		Code:         form["code"],
		ClientID:     form["client_id"],
		RedirectURI:  form["redirect_uri"],
		CodeVerifier: form["code_verifier"],
	}

	s.mu.Lock()
	challenge, known := s.codes[tr.Code]
	s.tokenRequests = append(s.tokenRequests, tr)
	s.mu.Unlock()

	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "unknown code"})
		return
	}
	if challenge != "" && pkceChallenge(tr.CodeVerifier) != challenge {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "code_verifier does not match code_challenge"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": s.accessToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

// --- MCP endpoint ---

// handleMCP is the protected resource: no bearer token, no tools. The 401 carries the
// resource_metadata URL, which is how a client that has never seen this server discovers
// where to authorize (RFC 9728 §5.1).
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.mcpRequests++
	s.mu.Unlock()

	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+s.accessToken {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf("Bearer resource_metadata=%q", s.URL+"/.well-known/oauth-protected-resource"))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)

	// A notification (no id) is acknowledged and answered with nothing: the JSON-RPC
	// client does not wait for a reply, and inventing one would break its framing.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		// Echo the client's protocol version: the fake implements no version-specific
		// behavior, and claiming one it does not know would fail a real client's check.
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpauth", "version": "1.0.0"},
		})
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{
			"tools": []map[string]any{{
				"name":        ToolName,
				"description": "echo from the mcpauth fixture",
				// One required string, so a caller can assert the panel renders a SCHEMA rather
				// than a description alone: the parameter's name, type and required mark are
				// three separate claims about three separate code paths.
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{
							"type":        "string",
							"description": "what to echo back",
						},
					},
					"required": []string{"text"},
				},
			}},
		})
	case "tools/call":
		s.mu.Lock()
		s.toolCalls++
		s.mu.Unlock()
		writeRPC(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": ToolResult}},
		})
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

// pkceChallenge is the S256 transform the client uses for its verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// parseForm decodes an application/x-www-form-urlencoded body without the request
// plumbing ParseForm needs (the body has already been read).
func parseForm(body string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(body, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		ku, err := url.QueryUnescape(k)
		if err != nil {
			return nil, err
		}
		vu, err := url.QueryUnescape(v)
		if err != nil {
			return nil, err
		}
		out[ku] = vu
	}
	return out, nil
}
