package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDurationYAML verifies Duration fields tagged with yaml load from natural duration strings
// and marshal back as strings — Duration used to implement JSON only, so a hand-written
// `15s` in config.yaml failed to load.
func TestDurationYAML(t *testing.T) {
	var ms MCPToolSearchConfig
	if err := yaml.Unmarshal([]byte("jev_budget: 15s"), &ms); err != nil {
		t.Fatalf("yaml.Unmarshal of Duration fields failed: %v", err)
	}
	if got := ms.JevBudget.String(); got != "15s" {
		t.Errorf("JevBudget = %s, want 15s", got)
	}

	// Marshal round-trip: YAML output should be a string, not nanoseconds.
	out, err := yaml.Marshal(MCPToolSearchConfig{JevBudget: Duration(10e9)}) // 10s
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
