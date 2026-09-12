package main

import (
	"testing"

	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
)

func TestContextParts(t *testing.T) {
	// Every bucket has to be mapped, and the order has to stay the one /usage
	// uses (prompt → tool schemas → conversation → tool output) so the popover
	// reads the same way as the report.
	full := tokenbreakdown.Breakdown{
		SystemPrompt:      1200,
		InternalTools:     3400,
		MCPTools:          600,
		UserMessages:      800,
		AssistantMessages: 2200,
		ToolResults:       9000,
		Other:             100,
		Total:             17300,
	}
	got := contextParts(full)
	wantKeys := []string{"system", "internalTools", "mcpTools", "userMessages", "assistantMessages", "toolResults", "other"}
	if len(got) != len(wantKeys) {
		t.Fatalf("contextParts() returned %d parts, want %d", len(got), len(wantKeys))
	}
	for i, key := range wantKeys {
		if got[i].Key != key {
			t.Errorf("part %d = %q, want %q", i, got[i].Key, key)
		}
		if got[i].Label == "" {
			t.Errorf("part %q has no label", key)
		}
	}
	if got[0].Tokens != 1200 || got[5].Tokens != 9000 {
		t.Errorf("tokens not carried through: %+v", got)
	}

	// Empty buckets are dropped: a popover listing "10 tokens" is useless.
	sparse := contextParts(tokenbreakdown.Breakdown{SystemPrompt: 500, ToolResults: 1500, Total: 2000})
	if len(sparse) != 2 || sparse[0].Key != "system" || sparse[1].Key != "toolResults" {
		t.Errorf("contextParts() with empty buckets = %+v, want system + toolResults only", sparse)
	}

	// An untouched agent (no estimate yet) must yield no parts at all, not a
	// full list of zeros.
	if got := contextParts(tokenbreakdown.Breakdown{}); len(got) != 0 {
		t.Errorf("contextParts(zero) = %+v, want empty", got)
	}
}

// The context ring and the popover read the same rule (contextUsageOf), on paths where the
// agent may legitimately not be built yet — a failed bootstrap, or a run the backend created
// but never prepared. Both must read as "nothing to measure", never panic.
func TestContextUsageOfWithoutAnAgent(t *testing.T) {
	d := &desktopApp{runs: make(map[string]*sessionRun)}
	if est, win := d.contextUsageOf(nil); est != 0 || win != 0 {
		t.Errorf("contextUsageOf(nil) = (%d, %d), want (0, 0)", est, win)
	}
	if est, win := d.contextUsageOf(&sessionRun{}); est != 0 || win != 0 {
		t.Errorf("contextUsageOf(agentless run) = (%d, %d), want (0, 0)", est, win)
	}
}
