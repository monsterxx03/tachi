package config

// Tests for the entry-level edits the settings editor drives: adding a server to an mcp.json,
// rewriting one, removing one. The property under test everywhere is the same — the bytes the
// edit does NOT name come back untouched — because these functions rewrite a file the user
// hand-writes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prettyMCP is the shape a hand-written mcp.json usually has: two entries, one with a nested
// object, both across lines.
const prettyMCP = `{
  "servers": [
    {
      "name": "alpha",
      "type": "stdio",
      "command": "/bin/alpha",
      "args": ["-v"]
    },
    {
      "name": "beta",
      "type": "http",
      "url": "https://beta.example.com",
      "headers": {
        "X-A": "1"
      }
    }
  ]
}
`

func writeTmpMCP(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func readTmpMCP(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestUpsertServerInFileReplacesOneEntryAndLeavesTheRest(t *testing.T) {
	path := writeTmpMCP(t, prettyMCP)

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha2", Args: []string{"-v"},
	}))

	// alpha is re-rendered from the struct (canonical key order, the file's own indentation);
	// beta — which the edit never named — is byte for byte what it was.
	want := `{
  "servers": [
    {
      "name": "alpha",
      "type": "stdio",
      "command": "/bin/alpha2",
      "args": [
        "-v"
      ]
    },
    {
      "name": "beta",
      "type": "http",
      "url": "https://beta.example.com",
      "headers": {
        "X-A": "1"
      }
    }
  ]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestUpsertServerInFileAppendsToANonEmptyArray(t *testing.T) {
	path := writeTmpMCP(t, prettyMCP)

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "gamma", Type: MCPTransportHTTP, URL: "https://gamma.example.com",
	}))

	got := readTmpMCP(t, path)
	// The new entry lands at the END, indented like its neighbours, and the closing bracket stays
	// where it was.
	assert.Contains(t, got, `    {
      "name": "gamma",
      "type": "http",
      "url": "https://gamma.example.com"
    }
  ]
}
`)
	assert.Contains(t, got, `      "name": "beta"`)
}

// TestUpsertServerInFileFollowsTheArraysOwnLayout: "does the FILE contain a newline" is not the
// question the appended entry's shape hangs on. An array written on one line inside an otherwise
// pretty file must get a one-liner — and its indent step must not be measured off a one-line
// object, whose objectFieldIndent is the opening brace's COLUMN plus two: that is not a step, and
// answering with it indented the new entry's fields by a column count (measured: fifteen columns
// too deep).
func TestUpsertServerInFileFollowsTheArraysOwnLayout(t *testing.T) {
	path := writeTmpMCP(t, "{\n  \"servers\": [{\"name\": \"alpha\", \"type\": \"stdio\"}]\n}\n")

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "beta", Type: MCPTransportHTTP, URL: "https://beta.example.com",
	}))

	want := `{
  "servers": [{"name": "alpha", "type": "stdio"},{"name":"beta","type":"http","url":"https://beta.example.com"}]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestUpsertServerInFileFillsAnEmptyArray(t *testing.T) {
	path := writeTmpMCP(t, "{\n  \"servers\": []\n}\n")

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha",
	}))

	want := `{
  "servers": [
    {
      "name": "alpha",
      "type": "stdio",
      "command": "/bin/alpha"
    }
  ]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestUpsertServerInFileCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "mcp.json")

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha",
	}))

	want := `{
  "servers": [
    {
      "name": "alpha",
      "type": "stdio",
      "command": "/bin/alpha"
    }
  ]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestUpsertServerInFileAddsAMissingServersKey(t *testing.T) {
	path := writeTmpMCP(t, "{}\n")

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha",
	}))

	want := `{
  "servers": [
    {
      "name": "alpha",
      "type": "stdio",
      "command": "/bin/alpha"
    }
  ]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestUpsertServerInFileKeepsAMinifiedFileMinified(t *testing.T) {
	path := writeTmpMCP(t, `{"servers":[{"name":"alpha","type":"stdio","command":"/bin/alpha"}]}`)

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "beta", Type: MCPTransportHTTP, URL: "https://beta.example.com",
	}))

	assert.Equal(t,
		`{"servers":[{"name":"alpha","type":"stdio","command":"/bin/alpha"},{"name":"beta","type":"http","url":"https://beta.example.com"}]}`,
		readTmpMCP(t, path))
}

func TestUpsertServerInFileHonoursAnIndentStep(t *testing.T) {
	// Four-space indent: the entry that comes back must not be this package's two-space taste.
	path := writeTmpMCP(t, "{\n    \"servers\": [\n        {\n            \"name\": \"alpha\",\n            \"type\": \"stdio\"\n        }\n    ]\n}\n")

	require.NoError(t, UpsertServerInFile(path, MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha",
	}))

	want := `{
    "servers": [
        {
            "name": "alpha",
            "type": "stdio",
            "command": "/bin/alpha"
        }
    ]
}
`
	assert.Equal(t, want, readTmpMCP(t, path))
}

func TestDeleteServerInFileRemovesEachPosition(t *testing.T) {
	entry := func(name string) string {
		return `    {
      "name": "` + name + `",
      "type": "stdio"
    }`
	}
	file := func(entries ...string) string {
		body := "{\n  \"servers\": [\n"
		for i, e := range entries {
			if i > 0 {
				body += ",\n"
			}
			body += e
		}
		return body + "\n  ]\n}\n"
	}

	cases := []struct {
		name    string
		entries []string
		drop    string
		want    string
	}{
		{"first", []string{"alpha", "beta", "gamma"}, "alpha", file(entry("beta"), entry("gamma"))},
		{"middle", []string{"alpha", "beta", "gamma"}, "beta", file(entry("alpha"), entry("gamma"))},
		{"last", []string{"alpha", "beta", "gamma"}, "gamma", file(entry("alpha"), entry("beta"))},
		{"only", []string{"alpha"}, "alpha", "{\n  \"servers\": []\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var entries []string
			for _, name := range tc.entries {
				entries = append(entries, entry(name))
			}
			dir := t.TempDir()
			oldBase := BaseDir()
			defer SetBaseDir(oldBase)
			SetBaseDir(dir)
			path := filepath.Join(dir, "mcp.json")
			require.NoError(t, os.WriteFile(path, []byte(file(entries...)), 0o600))

			require.NoError(t, DeleteServerInFile(path, tc.drop))
			assert.Equal(t, tc.want, readTmpMCP(t, path))

			// Whatever is left still parses and lost exactly one entry.
			servers, err := LoadMCPConfigRaw("")
			require.NoError(t, err)
			assert.Len(t, servers, len(tc.entries)-1)
		})
	}
}

func TestDeleteServerInFileKeepsAMinifiedFileMinified(t *testing.T) {
	path := writeTmpMCP(t, `{"servers":[{"name":"alpha","type":"stdio"},{"name":"beta","type":"http"}]}`)

	require.NoError(t, DeleteServerInFile(path, "alpha"))

	assert.Equal(t, `{"servers":[{"name":"beta","type":"http"}]}`, readTmpMCP(t, path))
}

func TestDeleteServerInFileReportsAMissingServer(t *testing.T) {
	path := writeTmpMCP(t, prettyMCP)

	err := DeleteServerInFile(path, "ghost")
	require.ErrorIs(t, err, ErrServerNotInFile)
	// Nothing was written.
	assert.Equal(t, prettyMCP, readTmpMCP(t, path))
}

func TestUpsertThenLoadRoundTrips(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	dir := t.TempDir()
	SetBaseDir(dir)

	require.NoError(t, UpsertServerInFile(filepath.Join(dir, mcpConfigFileName), MCPServerConfig{
		Name: "alpha", Type: MCPTransportStdio, Command: "/bin/alpha", Args: []string{"-x"},
	}))
	require.NoError(t, UpsertServerInFile(filepath.Join(dir, mcpConfigFileName), MCPServerConfig{
		Name: "beta", Type: MCPTransportHTTP, URL: "https://beta.example.com", Timeout: Duration(0),
	}))

	servers, err := LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, servers, 2)
	assert.Equal(t, "alpha", servers[0].Name)
	assert.Equal(t, []string{"-x"}, servers[0].Args)
	assert.Equal(t, DefaultMCPServerTimeout, servers[0].Timeout) // the loader fills the default in
	assert.True(t, servers[0].IsEnabled())
	assert.Equal(t, "beta", servers[1].Name)
	assert.Equal(t, "https://beta.example.com", servers[1].URL)

	// Editing beta in place must not add a second copy of it.
	require.NoError(t, UpsertServerInFile(filepath.Join(dir, mcpConfigFileName), MCPServerConfig{
		Name: "beta", Type: MCPTransportHTTP, URL: "https://beta2.example.com",
	}))
	servers, err = LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, servers, 2)
	assert.Equal(t, "https://beta2.example.com", servers[1].URL)
}

func TestLoadMCPConfigRawLeavesAbsentValuesAbsent(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	dir := t.TempDir()
	SetBaseDir(dir)

	// What a form would see: nothing written is nothing filled in. This is the property the
	// editor leans on — reading through the finalized loader would turn every absent timeout into
	// "10s" and every absent "enabled" into true, and the next save would write those defaults
	// into a file the user never chose them in.
	body := `{"servers":[{"name":"alpha","type":"stdio","command":"/bin/alpha"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, mcpConfigFileName), []byte(body), 0o600))

	raw, err := LoadMCPConfigRaw("")
	require.NoError(t, err)
	require.Len(t, raw, 1)
	assert.Zero(t, raw[0].Timeout)
	assert.Nil(t, raw[0].Enabled)
	assert.Equal(t, filepath.Join(dir, mcpConfigFileName), raw[0].SourceFile)
	assert.Empty(t, raw[0].Profile)

	final, err := LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, final, 1)
	assert.Equal(t, DefaultMCPServerTimeout, final[0].Timeout)
	assert.True(t, final[0].IsEnabled())
}

func TestLoadMCPConfigRawMergesTheProfileOverTheBase(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	dir := t.TempDir()
	SetBaseDir(dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, mcpConfigFileName),
		[]byte(`{"servers":[{"name":"shared","type":"stdio","command":"/bin/base"},{"name":"base-only","type":"stdio","command":"/bin/b"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, mcpProfileFileName("prod")),
		[]byte(`{"servers":[{"name":"shared","type":"stdio","command":"/bin/prod"}]}`), 0o600))

	raw, err := LoadMCPConfigRaw("prod")
	require.NoError(t, err)
	require.Len(t, raw, 2)
	assert.Equal(t, "/bin/prod", raw[0].Command)
	assert.Equal(t, "prod", raw[0].Profile)
	assert.Equal(t, "base-only", raw[1].Name)
	assert.Empty(t, raw[1].Profile)
}

func TestMCPConfigPathIsTheOneMapping(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	dir := t.TempDir()
	SetBaseDir(dir)

	assert.Equal(t, filepath.Join(dir, "mcp.json"), MCPConfigPath(""))
	assert.Equal(t, filepath.Join(dir, "mcp.prod.json"), MCPConfigPath("prod"))

	// What the loader reads and what the editor writes are the same file, by construction.
	require.NoError(t, UpsertServerInFile(MCPConfigPath("prod"), MCPServerConfig{
		Name: "only-in-prod", Type: MCPTransportStdio, Command: "/bin/p",
	}))
	servers, err := LoadMCPConfig("prod")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, MCPConfigPath("prod"), servers[0].SourceFile)

	servers, err = LoadMCPConfig("")
	require.NoError(t, err)
	assert.Empty(t, servers)
}

func TestUpsertServerInFileWritesWhatTheLoaderReads(t *testing.T) {
	// The two ends of the file format, checked against each other: whatever the editor writes, the
	// loader must be able to parse back into the same server.
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	dir := t.TempDir()
	SetBaseDir(dir)

	in := MCPServerConfig{
		Name: "full", Type: MCPTransportHTTP, URL: "https://x.example.com",
		Headers: map[string]string{"X-A": "1"}, Proxy: "socks5://127.0.0.1:1080",
		Env: map[string]string{"K": "V"}, Args: []string{"a", "b"},
		OAuth: &MCPOAuthConfig{ClientID: "cid", Scopes: []string{"s1"}},
	}
	require.NoError(t, UpsertServerInFile(filepath.Join(dir, mcpConfigFileName), in))

	servers, err := LoadMCPConfigRaw("")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	got := servers[0]
	assert.Equal(t, in.URL, got.URL)
	assert.Equal(t, in.Headers, got.Headers)
	assert.Equal(t, in.Proxy, got.Proxy)
	assert.Equal(t, in.Env, got.Env)
	assert.Equal(t, in.Args, got.Args)
	require.NotNil(t, got.OAuth)
	assert.Equal(t, "cid", got.OAuth.ClientID)
	assert.Equal(t, []string{"s1"}, got.OAuth.Scopes)

	// And it is valid JSON, not just something this package happens to parse.
	var probe map[string]any
	require.NoError(t, json.Unmarshal([]byte(readTmpMCP(t, filepath.Join(dir, mcpConfigFileName))), &probe))
}
