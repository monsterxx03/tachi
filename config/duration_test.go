package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDurationYAML verifies Duration fields tagged with yaml load from
// natural duration strings — the LSP config (lsp.request_timeout: 15s) was
// previously broken because Duration only implemented JSON serialization.
func TestDurationYAML(t *testing.T) {
	var lsp LSPConfig
	if err := yaml.Unmarshal([]byte("request_timeout: 15s\nstartup_timeout: 20s"), &lsp); err != nil {
		t.Fatalf("yaml.Unmarshal of Duration fields failed: %v", err)
	}
	if got := lsp.RequestTimeout.String(); got != "15s" {
		t.Errorf("RequestTimeout = %s, want 15s", got)
	}
	if got := lsp.StartupTimeout.String(); got != "20s" {
		t.Errorf("StartupTimeout = %s, want 20s", got)
	}

	// Marshal round-trip: YAML output should be a string, not nanoseconds.
	var server LSPServerConfig
	server.StartupTimeout = Duration(10e9) // 10s
	out, err := yaml.Marshal(server)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if !strings.Contains(string(out), "10s") {
		t.Errorf("yaml.Marshal should emit duration string, got: %s", out)
	}
}

// TestDurationJSONRegression guards the original JSON behavior (used by MCP
// config files): both string and numeric forms must still load.
func TestDurationJSONRegression(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"5m"`), &d); err != nil {
		t.Fatalf("json.Unmarshal string: %v", err)
	}
	if d.String() != "5m0s" {
		t.Errorf("string form: got %s, want 5m0s", d.String())
	}

	var numeric Duration
	if err := json.Unmarshal([]byte(`5000000000`), &numeric); err != nil {
		t.Fatalf("json.Unmarshal numeric: %v", err)
	}
	if numeric.String() != "5s" {
		t.Errorf("numeric form: got %s, want 5s", numeric.String())
	}
}
