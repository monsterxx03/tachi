package systemreminder

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// The reminder is what drives plan updates in AUTO mode, where the plan-mode prompt is not
// in play — so it is the only place that can tell the model which document it is updating.
// The tool schema asks for a plan_id, and a reminder that stayed silent about it would
// invite a fresh id on every save: each update would land in a new file, which is worse
// than the title-keyed drift plan_id was added to fix.
func TestPlanTrackingReminderCarriesPlanID(t *testing.T) {
	const sid = "sess-id"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi", "plans", "demo-"+sid+".json"), `{
  "title": "带 id 的计划", "plan_id": "plan-7", "content": "",
  "steps": [{"content": "第一步", "status": "pending"}]
}`)

	lines := PlanTrackingReminder{}.Generate(wdctx.WithDir(t.Context(), dir),
		Context{IsFirstMessage: false, SessionID: sid})

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "plan-7") {
		t.Fatalf("the reminder must carry the plan_id (nothing else names the document): %q", joined)
	}
	if !strings.Contains(joined, "SAME value") {
		t.Errorf("the reminder must say to reuse the id unchanged: %q", joined)
	}
	// The update instruction itself must survive: the id line is an addition, not a
	// replacement for what the reminder is for.
	if !strings.Contains(joined, "Periodically call the SavePlan tool") {
		t.Errorf("the reminder lost its actual instruction: %q", joined)
	}
}

// A plan saved before plan_id existed has none — the reminder closes the loop by telling the
// model to pin one on its next update, after which the id line above takes over.
func TestPlanTrackingReminderWithoutPlanID(t *testing.T) {
	const sid = "sess-legacy"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tachi", "plans", "legacy-"+sid+".json"), `{
  "title": "老计划", "content": "",
  "steps": [{"content": "第一步", "status": "pending"}]
}`)

	lines := PlanTrackingReminder{}.Generate(wdctx.WithDir(t.Context(), dir),
		Context{IsFirstMessage: false, SessionID: sid})

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "(none yet)") {
		t.Fatalf("a plan without an id must be reported as such: %q", joined)
	}
	if !strings.Contains(joined, "pass a plan_id") {
		t.Errorf("the reminder must tell the model how to pin the identity: %q", joined)
	}
}
