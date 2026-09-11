package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/agent/tools"
)

// TestExtraForkTools pins the review-only tool rule: the main agent must not have
// ReportFinding (it has no business declaring findings about its own work), and a
// review fork must have it. Keeping this as a pure function is what makes the rule
// testable without building an agent.
func TestExtraForkTools(t *testing.T) {
	if got := extraForkTools(ForkConfig{}); len(got) != 0 {
		t.Errorf("a plain fork got extra tools: %v", got)
	}

	got := extraForkTools(ForkConfig{ForReview: true})
	if len(got) != 1 || got[0].Name() != tools.ToolNameReportFinding {
		t.Fatalf("a review fork got %v, want ReportFinding", got)
	}

}
