package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// oauthRedirectURI
// ---------------------------------------------------------------------------

func TestOAuthRedirectURI_Defaults(t *testing.T) {
	srv := &config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{}}
	uri := oauthRedirectURI(srv)
	assert.Equal(t, "http://127.0.0.1:18273/callback", uri)
}

func TestOAuthRedirectURI_CustomHost(t *testing.T) {
	srv := &config.MCPServerConfig{
		OAuth: &config.MCPOAuthConfig{
			CallbackHost: "0.0.0.0",
		},
	}
	uri := oauthRedirectURI(srv)
	assert.Equal(t, "http://0.0.0.0:18273/callback", uri)
}

func TestOAuthRedirectURI_CustomPort(t *testing.T) {
	srv := &config.MCPServerConfig{
		OAuth: &config.MCPOAuthConfig{
			CallbackPort: 9999,
		},
	}
	uri := oauthRedirectURI(srv)
	assert.Equal(t, "http://127.0.0.1:9999/callback", uri)
}

func TestOAuthRedirectURI_CustomHostAndPort(t *testing.T) {
	srv := &config.MCPServerConfig{
		OAuth: &config.MCPOAuthConfig{
			CallbackHost: "localhost",
			CallbackPort: 8080,
		},
	}
	uri := oauthRedirectURI(srv)
	assert.Equal(t, "http://localhost:8080/callback", uri)
}

// ---------------------------------------------------------------------------
// asMetadataURLs
// ---------------------------------------------------------------------------

func TestAsMetadataURLs(t *testing.T) {
	urls := asMetadataURLs("https://auth.example.com")
	require.Len(t, urls, 4)
	// Simple append: path/.well-known/oauth-authorization-server
	assert.Equal(t, "https://auth.example.com/.well-known/oauth-authorization-server", urls[0])
	// Simple append: path/.well-known/openid-configuration
	assert.Equal(t, "https://auth.example.com/.well-known/openid-configuration", urls[1])
	// RFC 8414: /.well-known/oauth-authorization-server/path (path is empty, so just /.well-known/...)
	assert.Equal(t, "https://auth.example.com/.well-known/oauth-authorization-server", urls[2])
	assert.Equal(t, "https://auth.example.com/.well-known/openid-configuration", urls[3])
}

func TestAsMetadataURLs_WithPath(t *testing.T) {
	urls := asMetadataURLs("https://auth.example.com/issuer")
	require.Len(t, urls, 4)
	// Simple append: path/.well-known/oauth-authorization-server
	assert.Equal(t, "https://auth.example.com/issuer/.well-known/oauth-authorization-server", urls[0])
	// RFC 8414 path-insertion: /.well-known/oauth-authorization-server/path
	assert.Equal(t, "https://auth.example.com/.well-known/oauth-authorization-server/issuer", urls[2])
}

func TestAsMetadataURLs_InvalidURL(t *testing.T) {
	urls := asMetadataURLs("not-a-url")
	assert.Nil(t, urls)
}

func TestAsMetadataURLs_Empty(t *testing.T) {
	urls := asMetadataURLs("")
	assert.Nil(t, urls)
}

func TestAsMetadataURLs_WithPort(t *testing.T) {
	urls := asMetadataURLs("http://localhost:8080/auth")
	assert.True(t, len(urls) > 0)
	for _, u := range urls {
		assert.Contains(t, u, "localhost:8080")
	}
}

// ---------------------------------------------------------------------------
// extractResourceMetadataURL
// ---------------------------------------------------------------------------

func TestExtractResourceMetadataURL_Found(t *testing.T) {
	headers := []string{
		`Bearer realm="example", resource_metadata="https://rs.example.com/.well-known/oauth-protected-resource"`,
	}
	url := extractResourceMetadataURL(headers)
	assert.Equal(t, "https://rs.example.com/.well-known/oauth-protected-resource", url)
}

func TestExtractResourceMetadataURL_NotFound(t *testing.T) {
	headers := []string{`Bearer realm="example"`}
	url := extractResourceMetadataURL(headers)
	assert.Empty(t, url)
}

func TestExtractResourceMetadataURL_EmptyHeaders(t *testing.T) {
	url := extractResourceMetadataURL(nil)
	assert.Empty(t, url)
}

func TestExtractResourceMetadataURL_MultipleHeaders(t *testing.T) {
	headers := []string{
		`Basic realm="other"`,
		`Bearer resource_metadata="https://rs.example.com/protected"`,
	}
	url := extractResourceMetadataURL(headers)
	assert.Equal(t, "https://rs.example.com/protected", url)
}

// ---------------------------------------------------------------------------
// extractQuotedParam
// ---------------------------------------------------------------------------

func TestExtractQuotedParam_Found(t *testing.T) {
	val := extractQuotedParam(`Bearer realm="example"`, "realm")
	assert.Equal(t, "example", val)
}

func TestExtractQuotedParam_NotFound(t *testing.T) {
	val := extractQuotedParam(`Bearer realm="example"`, "scope")
	assert.Empty(t, val)
}

func TestExtractQuotedParam_EmptyHeader(t *testing.T) {
	val := extractQuotedParam("", "realm")
	assert.Empty(t, val)
}

func TestExtractQuotedParam_CaseInsensitive(t *testing.T) {
	val := extractQuotedParam(`Bearer REALM="example"`, "realm")
	assert.Equal(t, "example", val)
}

// ---------------------------------------------------------------------------
// buildWellKnownURLs
// ---------------------------------------------------------------------------

func TestBuildWellKnownURLs_NoPath(t *testing.T) {
	urls := buildWellKnownURLs("https://example.com", "oauth-protected-resource")
	require.Len(t, urls, 2)
	assert.Equal(t, "https://example.com/.well-known/oauth-protected-resource", urls[0])
	assert.Equal(t, "https://example.com/.well-known/oauth-protected-resource", urls[1])
}

func TestBuildWellKnownURLs_WithPath(t *testing.T) {
	urls := buildWellKnownURLs("https://example.com/api", "oauth-protected-resource")
	require.Len(t, urls, 2)
	// Simple append
	assert.Equal(t, "https://example.com/api/.well-known/oauth-protected-resource", urls[0])
	// RFC 8414 path-insertion
	assert.Equal(t, "https://example.com/.well-known/oauth-protected-resource/api", urls[1])
}

func TestBuildWellKnownURLs_WithQueryString(t *testing.T) {
	// Query and fragment should be stripped
	urls := buildWellKnownURLs("https://example.com/path?query=val#frag", "test")
	require.Len(t, urls, 2)
	assert.NotContains(t, urls[0], "query=val")
	assert.NotContains(t, urls[0], "frag")
}

// ---------------------------------------------------------------------------
// parseRedirectURL
// ---------------------------------------------------------------------------

func TestParseRedirectURL_Valid(t *testing.T) {
	code, state, err := parseRedirectURL("http://localhost:18273/callback?code=auth-code-123&state=csrf-state")
	require.NoError(t, err)
	assert.Equal(t, "auth-code-123", code)
	assert.Equal(t, "csrf-state", state)
}

func TestParseRedirectURL_MissingCode(t *testing.T) {
	_, _, err := parseRedirectURL("http://localhost:18273/callback?state=abc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no authorization code")
}

func TestParseRedirectURL_InvalidURL(t *testing.T) {
	_, _, err := parseRedirectURL(":")
	assert.Error(t, err)
}

func TestParseRedirectURL_Empty(t *testing.T) {
	_, _, err := parseRedirectURL("")
	assert.Error(t, err)
}

func TestParseRedirectURL_ImplicitFlowFragment(t *testing.T) {
	// Implicit flow: code in fragment
	code, state, err := parseRedirectURL("http://localhost/callback#code=fragment-code&state=csrf-state")
	require.NoError(t, err)
	assert.Equal(t, "fragment-code", code)
	assert.Equal(t, "csrf-state", state)
}

func TestParseRedirectURL_NoState(t *testing.T) {
	code, state, err := parseRedirectURL("http://localhost/callback?code=abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", code)
	assert.Empty(t, state)
}

// ---------------------------------------------------------------------------
// stripQueryFragment
// ---------------------------------------------------------------------------

func TestStripQueryFragment_FullURL(t *testing.T) {
	result := stripQueryFragment("https://example.com/path?query=val&other=val2#section")
	assert.Equal(t, "https://example.com/path", result)
}

func TestStripQueryFragment_NoQuery(t *testing.T) {
	result := stripQueryFragment("https://example.com/path")
	assert.Equal(t, "https://example.com/path", result)
}

func TestStripQueryFragment_OnlyFragment(t *testing.T) {
	result := stripQueryFragment("https://example.com/path#section")
	assert.Equal(t, "https://example.com/path", result)
}

func TestStripQueryFragment_Invalid(t *testing.T) {
	// url.Parse returns a best-effort result even for invalid URLs;
	// the spaces get URL-encoded. Just verify no panic.
	result := stripQueryFragment("not a url")
	assert.NotEqual(t, "", result)
}

func TestStripQueryFragment_Empty(t *testing.T) {
	result := stripQueryFragment("")
	assert.Equal(t, "", result)
}

// ---------------------------------------------------------------------------
// probe401ResourceMetadata — with httptest.Server
// ---------------------------------------------------------------------------

func TestProbe401ResourceMetadata_WithWWWAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="example", resource_metadata="https://rs.example.com/prm"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	url := probe401ResourceMetadata(context.Background(), srv.Client(), srv.URL)
	assert.Equal(t, "https://rs.example.com/prm", url)
}

func TestProbe401ResourceMetadata_NoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	url := probe401ResourceMetadata(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, url)
}

func TestProbe401ResourceMetadata_Not401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	url := probe401ResourceMetadata(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, url)
}

func TestProbe401ResourceMetadata_ConnectionError(t *testing.T) {
	// No server running — connection refused
	url := probe401ResourceMetadata(context.Background(), http.DefaultClient, "http://127.0.0.1:1")
	assert.Empty(t, url)
}

// ---------------------------------------------------------------------------
// fetchAuthorizationServers — with httptest.Server
// ---------------------------------------------------------------------------

func TestFetchAuthorizationServers_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"authorization_servers":["https://auth1.example.com","https://auth2.example.com"]}`))
	}))
	defer srv.Close()

	servers := fetchAuthorizationServers(context.Background(), srv.Client(), srv.URL)
	assert.Equal(t, []string{"https://auth1.example.com", "https://auth2.example.com"}, servers)
}

func TestFetchAuthorizationServers_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	servers := fetchAuthorizationServers(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, servers)
}

func TestFetchAuthorizationServers_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	servers := fetchAuthorizationServers(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, servers)
}

func TestFetchAuthorizationServers_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	servers := fetchAuthorizationServers(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, servers)
}

// ---------------------------------------------------------------------------
// findAuthServerMetadataURL — with httptest.Server
// ---------------------------------------------------------------------------

func TestFindAuthServerMetadataURL_Found(t *testing.T) {
	// Server that returns AS metadata with registration_endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"registration_endpoint":"https://auth.example.com/register","token_endpoint":"https://auth.example.com/token"}`))
	}))
	defer srv.Close()

	url := findAuthServerMetadataURL(context.Background(), srv.Client(), srv.URL)
	assert.Equal(t, srv.URL, url)
}

func TestFindAuthServerMetadataURL_NoRegistrationEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token_endpoint":"https://auth.example.com/token"}`))
	}))
	defer srv.Close()

	url := findAuthServerMetadataURL(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, url)
}

func TestFindAuthServerMetadataURL_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	url := findAuthServerMetadataURL(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, url)
}

func TestFindAuthServerMetadataURL_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	url := findAuthServerMetadataURL(context.Background(), srv.Client(), srv.URL)
	assert.Empty(t, url)
}

// ---------------------------------------------------------------------------
// OAuthRequiredError
// ---------------------------------------------------------------------------

func TestOAuthRequiredError(t *testing.T) {
	err := &OAuthRequiredError{ServerName: "postgres"}
	assert.Contains(t, err.Error(), "postgres")
	assert.Contains(t, err.Error(), "OAuth")
}
