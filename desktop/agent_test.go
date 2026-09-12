package main

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// The lookups the UI bindings share must not leave empty runs behind: `getRun` creates
// one, so a "is there a session?" question asked with it always answered yes (the nil
// guards after it were dead code) and every unknown id — including the empty one — grew
// the map.
func TestRunLookupDoesNotCreate(t *testing.T) {
	d := &desktopApp{runs: make(map[string]*sessionRun)}

	if r := d.runOf("nope"); r != nil {
		t.Errorf("runOf returned a run for an unknown id: %+v", r)
	}
	if a, refuse := d.agentOf(""); a != nil || refuse != refuseNoSession {
		t.Errorf("agentOf(\"\") = (%v, %q), want (nil, %q)", a, refuse, refuseNoSession)
	}
	if n := len(d.runs); n != 0 {
		t.Errorf("lookups created %d run(s): %v", n, d.runs)
	}
}

// Every binding that needs the current session's agent refuses with the SAME string, and
// it is Chinese: the desktop shows it verbatim (the mode selector renders it as a notice).
func TestBindingsRefuseWithOneMessage(t *testing.T) {
	// A config is present on purpose: the cfg-less branch is a different refusal
	// ("the app has no provider at all"), and this test is about the session one.
	s := &AgentService{desk: &desktopApp{runs: make(map[string]*sessionRun), cfg: &config.Config{}}}

	if got := s.SetMode("chat"); got != refuseNoSession {
		t.Errorf("SetMode without a session = %q, want %q", got, refuseNoSession)
	}
	if got := s.SetThinkingLevel("high"); got != refuseNoSession {
		t.Errorf("SetThinkingLevel without a session = %q, want %q", got, refuseNoSession)
	}
	// The MCP bindings check "is MCP even configured?" first — a different refusal,
	// for a different situation.
	if got := s.SetMCPProfile("default"); got != "mcp not configured" {
		t.Errorf("SetMCPProfile without an MCP runtime = %q, want %q", got, "mcp not configured")
	}
}
