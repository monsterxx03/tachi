package tokenbreakdown

import "testing"

// TestScaleToKeepsThePartsConsistent pins the property the context popover depends on: the parts it
// lists must add up to the total printed above them. The reported total is anchored on a REAL
// prompt size (see agent's convState.contextEstimate), which is not what the parts were measured
// as — so they are scaled to it rather than left disagreeing.
func TestScaleToKeepsThePartsSumming(t *testing.T) {
	b := Breakdown{
		SystemPrompt:      1000,
		InternalTools:     7000,
		UserMessages:      500,
		AssistantMessages: 1500,
		ToolResults:       2000,
		Total:             12000,
	}

	// A real prompt 25% bigger than the estimate: every part grows, and the sum follows the total.
	got := b.ScaleTo(15000)
	if got.Total != 15000 {
		t.Errorf("total = %d, want 15000", got.Total)
	}
	if got.InternalTools != 8750 {
		t.Errorf("parts must keep their proportions: InternalTools = %d, want 8750", got.InternalTools)
	}
	if sum := got.SystemPrompt + got.InternalTools + got.MCPTools + got.UserMessages +
		got.AssistantMessages + got.ToolResults + got.Other; sum != got.Total {
		t.Errorf("parts (%d) must sum to the total (%d)", sum, got.Total)
	}

	// Degenerate inputs are left alone: nothing to scale, or nothing to scale to.
	if b.ScaleTo(0) != b || b.ScaleTo(b.Total) != b {
		t.Error("no-op cases must return the breakdown unchanged")
	}
}
