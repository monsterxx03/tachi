package systemreminder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// PlanTrackingReminder checks for active plan files in .tachi/plans/ and
// reminds the LLM to update step statuses using SavePlan as work progresses.
// It only fires when incomplete plan files exist (plans with at least one
// step not yet marked "completed").
type PlanTrackingReminder struct{}

func (r PlanTrackingReminder) Generate(ctx context.Context, rctx Context) []string {
	// Don't fire on the first message of a new conversation — there's no plan yet.
	if rctx.IsFirstMessage {
		return nil
	}
	if rctx.SessionID == "" {
		return nil
	}

	plan := findActivePlan(config.FindProjectRootFrom(workDir(ctx)), rctx.SessionID)
	if plan == nil {
		return nil
	}

	return reminderLines(plan)
}

// reminderLines renders the active plan as the lines the model sees.
//
// The plan_id is part of it because it is the thing that has to stay STABLE across updates:
// nothing else tells the model which document it is updating, and the tool schema asks for
// an id — so a reminder that stayed silent about it would invite a fresh id on every save,
// turning each update into yet another file (the very drift plan_id exists to prevent).
func reminderLines(plan *planInfo) []string {
	lines := []string{fmt.Sprintf("Active plan: `%s` — %s", plan.Path, plan.Title)}
	if plan.PlanID != "" {
		lines = append(lines, fmt.Sprintf(
			"plan_id: `%s` — pass this SAME value when you update the plan; that is what keeps a retitled "+
				"plan updating in place instead of forking into a second file.", plan.PlanID))
	} else {
		lines = append(lines, "plan_id: (none yet) — pass a plan_id of your choice together with the same "+
			"title on your next update to pin this plan's identity, then keep reusing it.")
	}
	return append(lines,
		"",
		"Periodically call the SavePlan tool to update step statuses as you complete each step.",
		"Mark steps as `in_progress` when starting work and `completed` when finished.",
	)
}

// planInfo holds metadata about an active plan file.
type planInfo struct {
	Path    string
	Title   string
	PlanID  string
	ModTime time.Time
}

// planFile is used to parse the JSON structure of saved plans.
type planFile struct {
	Title  string     `json:"title"`
	PlanID string     `json:"plan_id"`
	Steps  []planStep `json:"steps"`
}

type planStep struct {
	Status string `json:"status"`
}

// findActivePlan scans <root>/.tachi/plans, finds the most recent plan file for
// the given session that has at least one non-completed step, and returns it.
// Returns nil if no active plan is found.
//
// root is the PROJECT root the plan was written under — the git root of the turn's
// working directory (see workDir), never config.FindProjectRoot(): the process
// working directory of a GUI app is not a project, and SavePlan resolves its own
// directory the same session-scoped way.
func findActivePlan(root, sessionID string) *planInfo {
	if root == "" || sessionID == "" {
		return nil
	}

	planDir := filepath.Join(root, ".tachi", "plans")
	matches, err := filepath.Glob(filepath.Join(planDir, "*.json"))
	if err != nil || len(matches) == 0 {
		return nil
	}

	var candidates []planInfo

	for _, path := range matches {
		// Filter by session ID: filename format is {slug}-{sessionID}.json.
		// Extract session ID by stripping .json and taking the last dash-delimited segment.
		if !belongsToSession(path, sessionID) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var pf planFile
		if err := json.Unmarshal(data, &pf); err != nil {
			continue
		}
		if pf.Title == "" {
			continue
		}
		if allCompleted(pf.Steps) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		// Skip plans older than 24 hours — they've likely been abandoned.
		if time.Since(info.ModTime()) > 24*time.Hour {
			continue
		}
		candidates = append(candidates, planInfo{
			Path:    path,
			Title:   pf.Title,
			PlanID:  pf.PlanID,
			ModTime: info.ModTime(),
		})
	}

	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ModTime.After(candidates[j].ModTime)
	})

	return &candidates[0]
}

// belongsToSession checks if a plan filename belongs to the given session ID.
// Filename format: {slug}-{sessionID}.json
func belongsToSession(path, sessionID string) bool {
	return strings.HasSuffix(path, "-"+sessionID+".json")
}

// allCompleted returns true when every step has status "completed".
// A plan with zero steps is considered complete (nothing to track).
func allCompleted(steps []planStep) bool {
	for _, s := range steps {
		if s.Status != "completed" {
			return false
		}
	}
	return true
}
