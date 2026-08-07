// Package harness builds the isolated environment shared by the integration
// test suites: a --home directory with a minimal config.yaml pointing the
// default provider at a mockllm server, a seeded work directory for tool
// side effects, and the real tachi binary for process-boundary layers
// (itest/run, itest/acp).
//
// Isolation invariants (docs/2026-07-31-tui-integration-test.md §六):
//   - --home is a t.TempDir() — config/session/logs/memory all isolated.
//   - The config fixture is rendered AFTER the mock server starts (its random
//     port is only known then); title_generation is always off so the script
//     only faces the main loop's streaming calls.
//   - No MCP servers, no web search key, no channel config → zero outbound
//     network besides the mock.
package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/monsterxx03/tachi/itest/mockllm"
)

// configYAML renders the minimal provider fixture. The base_url differs per
// protocol: go-openai concatenates baseURL + "/chat/completions" (needs the
// /v1 suffix) while anthropic-sdk-go appends a trailing slash and resolves
// "v1/messages" against it (needs the bare host) — Server.BaseURL() already
// returns the correct value per protocol.
func configYAML(p mockllm.Protocol, baseURL string, bashAllow bool) string {
	s := fmt.Sprintf(`provider: mock
providers:
  - name: mock
    type: %s
    model: mock-model
    base_url: %s
    api_key: test-key
    spec:
      context_window: 128000
title_generation: false
language: zh
`, p, baseURL)
	if bashAllow {
		s += "permissions:\n  bash:\n    allow:\n      - \"*\"\n"
	}
	return s
}

// TB is the minimal testing surface harness needs. Satisfied by *testing.T
// and by ginkgo.GinkgoT() (which does not implement testing.TB due to its
// private method).
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

// Option configures the harness home.
type Option func(*options)

type options struct {
	protocol  mockllm.Protocol
	bashAllow bool
}

// WithProtocol selects the mock's wire protocol (default openai); the config
// fixture's provider type follows automatically.
func WithProtocol(p mockllm.Protocol) Option {
	return func(o *options) { o.protocol = p }
}

// WithBashAllow adds permissions.bash.allow: ["*"] so Bash tool calls never
// hit an ask rule. Default bash policy is allow already, but this is a
// defense-in-depth pin for scenarios that must execute Bash.
func WithBashAllow() Option {
	return func(o *options) { o.bashAllow = true }
}

// NewHome creates an isolated --home directory with a config.yaml wired to
// the given mock server. The mock must be started first (its port is baked
// into the fixture).
func NewHome(t TB, mock *mockllm.Server, opts ...Option) string {
	t.Helper()
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	home := t.TempDir()
	cfg := configYAML(o.protocol, mock.BaseURL(), o.bashAllow)
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("harness: write config fixture: %v", err)
	}
	return home
}

// SeedWorkDir creates a fresh working directory (no .tachi.md, no .tachi/)
// and seeds the given files so tool outputs are assertable (e.g. a README.md
// for `ls` to find). Returns the directory path.
func SeedWorkDir(t TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("harness: seed dir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("harness: seed file %s: %v", name, err)
		}
	}
	return dir
}

// BuildBinary compiles the real tachi binary once per suite into a temp dir
// and returns its path. Call from BeforeSuite. The build runs from the
// repository root (located relative to this package — itest/harness → ../..)
// because the itest packages are all behind the integration build tag.
func BuildBinary(t TB) string {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("harness: locate repo root (expected go.mod in %s): %v", root, err)
	}
	bin := filepath.Join(t.TempDir(), "tachi")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness: build tachi binary: %v\n%s", err, out)
	}
	return bin
}
