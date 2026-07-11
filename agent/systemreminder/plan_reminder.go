package systemreminder

import (
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

func (r PlanTrackingReminder) Generate(ctx Context) []string {
	// Don't fire on the first message of a new conversation — there's no plan yet.
	if ctx.IsFirstMessage {
		return nil
	}
	if ctx.SessionID == "" {
		return nil
	}

	plan := findActivePlan(ctx.SessionID)
	if plan == nil {
		return nil
	}

	return []string{
		fmt.Sprintf("Active plan: `%s` — %s", plan.Path, plan.Title),
		"",
		"Periodically call the SavePlan tool to update step statuses as you complete each step.",
		"Mark steps as `in_progress` when starting work and `completed` when finished.",
	}
}

// planInfo holds metadata about an active plan file.
type planInfo struct {
	Path    string
	Title   string
	ModTime time.Time
}

// planFile is used to parse the JSON structure of saved plans.
type planFile struct {
	Title string     `json:"title"`
	Steps []planStep `json:"steps"`
}

type planStep struct {
	Status string `json:"status"`
}

// findActivePlan scans .tachi/plans/ under the project root, finds the most
// recent plan file for the given session that has at least one non-completed
// step, and returns it. Returns nil if no active plan is found.
func findActivePlan(sessionID string) *planInfo {
	root := config.FindProjectRoot()
	if root == "" {
		return nil
	}

	planDir := filepath.Join(root, ".tachi", "plans")
	matches, err := filepath.Glob(filepath.Join(planDir, "*.json"))
	if err != nil || len(matches) == 0 {
		return nil
	}

	var candidates []planInfo

	for _, path := range matches {
		// Filter by session ID: filename format is {timestamp}-{slug}-{sessionID}.json.
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
		candidates = append(candidates, planInfo{
			Path:    path,
			Title:   pf.Title,
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
// Filename format: {timestamp}-{slug}-{sessionID}.json
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
