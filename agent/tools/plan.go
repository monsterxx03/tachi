package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// SavePlanTool saves a structured plan document to .tachi/plans/.
// The LLM calls this tool to create or update a plan while in plan mode.
type SavePlanTool struct{}

func (t SavePlanTool) Name() string { return ToolNameSavePlan }
func (t SavePlanTool) Description() string {
	return "Save or update a structured plan document. " +
		"Use this when you have developed a clear plan and want to record it. " +
		"Call multiple times to update the plan as it evolves."
}
func (t SavePlanTool) IsDestructive() bool { return false }
func (t SavePlanTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"title": {
			Type:        "string",
			Description: "A concise title for the plan (e.g. 'Refactor User Module')",
		},
		"content": {
			Type:        "string",
			Description: "Full plan content in markdown — goals, approach, key changes, file list, design decisions",
		},
		"steps": {
			Type:        "array",
			Description: "Structured task list with status tracking",
			Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{"type": "string", "description": "What needs to be done (imperative form)"},
					"status":  map[string]any{"type": "string", "description": "pending | in_progress | completed"},
				},
				"required": []string{"content", "status"},
			},
		},
		"plan_id": {
			Type: "string",
			Description: "Stable identity for THIS plan. On the first save of a plan, invent a short id " +
				"(e.g. \"plan-1\") and pass the SAME value on every later update — including when the " +
				"title changes — so the plan is updated in place instead of leaving a stale copy behind. " +
				"Only use a new id for a genuinely different plan.",
		},
	}
}
func (t SavePlanTool) Required() []string { return []string{"title", "content", "steps"} }
func (t SavePlanTool) Parallel() bool     { return false }

// SavePlanParams mirrors the tool's expected JSON arguments.
//
// It is also the ON-DISK shape of a saved plan (the file is this struct written as JSON),
// which is why the desktop's plan panel and the ACP plan update both decode it.
type SavePlanParams struct {
	Title   string         `json:"title"`
	Content string         `json:"content"`
	Steps   []SavePlanStep `json:"steps"`
	// PlanID is the plan's stable identity. When the caller supplies one, a save with the
	// same id UPDATES that plan — even if the title changed — instead of leaving the old
	// document behind as a separate plan. A title is prose ("修复并发问题" →
	// "修复并发问题（已完成）"), so without an id every rewording forked the plan into a new
	// file. Callers that omit it keep the title-keyed behaviour.
	PlanID string `json:"plan_id,omitempty"`
}

// SavePlanStep is a single step within a plan.
type SavePlanStep struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// PlanFromToolArgs decodes SavePlan arguments into the structured plan they describe.
// Returns false when the args are not a usable plan (empty, invalid JSON, or no steps):
// a call we cannot read is a tool call, not a plan.
//
// This is the ONE parser for "the plan a SavePlan call carries". Three consumers need
// it — the ACP plan session update (an editor's plan card), the desktop's plan panel
// (live) and the plan file reader (after a restart) — and the on-disk shape of a saved
// plan is SavePlanParams, so reading a file and reading a call are the same decode.
func PlanFromToolArgs(argsJSON string) (SavePlanParams, bool) {
	var params SavePlanParams
	if argsJSON == "" {
		return SavePlanParams{}, false
	}
	if err := json.Unmarshal([]byte(argsJSON), &params); err != nil {
		return SavePlanParams{}, false
	}
	if len(params.Steps) == 0 {
		return SavePlanParams{}, false
	}
	return params, true
}

// PlanFromFile reads a plan document written by SavePlanTool.
func PlanFromFile(path string) (SavePlanParams, error) {
	var params SavePlanParams
	if err := fileutil.ReadJSON(path, &params); err != nil {
		return SavePlanParams{}, err
	}
	return params, nil
}

func (t SavePlanTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params SavePlanParams
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Title == "" || params.Content == "" {
		return "", fmt.Errorf("title and content are required")
	}

	// Validate step statuses
	for i, s := range params.Steps {
		switch s.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("step %d (%q): invalid status %q (must be pending, in_progress, or completed)", i, s.Content, s.Status)
		}
	}

	// Determine plan directory:
	//   - With a working-dir context (ACP/agent loop): <projectRoot>/.tachi/plans/
	//   - Without one (defensive; wdctx.Dir currently falls back to cwd or
	//     ".", so this branch is normally unreachable): <baseDir>/plans —
	//     config.BaseDir() respects --home instead of ~/.tachi/plans.
	var planDir string
	if baseDir := wdctx.Dir(ctx); baseDir != "" && baseDir != "." {
		planDir = filepath.Join(baseDir, ".tachi", "plans")
	} else {
		planDir = filepath.Join(config.BaseDir(), "plans")
	}

	// One file per plan per session, so repeated saves overwrite and mean "update".
	// Identity comes from plan_id when the caller supplies one — a title is prose, and
	// rewording it must not fork the plan into a second document; without an id the file
	// is keyed by the title slug, which is what callers have always done.
	//
	// A renamed plan keeps its original file name and only its content changes: renaming
	// would have to pick a name another plan's title might already claim, and the name is
	// not what any UI shows — the title is.
	slug := planSlug(params.Title)
	sessionID := SessionIDFromCtx(ctx)
	filePath := filepath.Join(planDir, fmt.Sprintf("%s-%s.json", slug, sessionID))
	if params.PlanID != "" {
		if prev := findPlanFileByID(planDir, sessionID, params.PlanID); prev != "" {
			filePath = prev
		}
	}

	// Save raw structured data as JSON — preserves the original format from
	// the LLM without flattening to markdown. Consumers (TUI, ACP, external
	// viewers) can render it however they want.
	if err := fileutil.WriteJSONShared(filePath, params); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}

	// Tidy up after the write, scoped to THIS session's plans: the finished ones are
	// archived, and any of its plans older than the retention window are removed. A save
	// is the one moment where "this session's plans" is a meaningful set — a background
	// sweeper would have to guess who owns what.
	archived, deleted := prunePlans(planDir, sessionID, filePath)

	// Compute summary
	var pending, inProg, done int
	for _, s := range params.Steps {
		switch s.Status {
		case "pending":
			pending++
		case "in_progress":
			inProg++
		case "completed":
			done++
		}
	}

	summary := fmt.Sprintf("Plan saved to %s\n\n**%s** — %d steps: %d pending, %d in progress, %d completed",
		filePath, params.Title, len(params.Steps), pending, inProg, done)
	if archived > 0 || deleted > 0 {
		summary += fmt.Sprintf("\nArchived %d finished plan(s); removed %d older than %d days.",
			archived, deleted, planRetentionDays)
	}

	return summary, nil
}

// planArchiveDirName is where finished plans are moved: out of the active list, still
// readable (and still in the same plans tree).
const planArchiveDirName = "archive"

// planRetentionDays is how long this session's plans are kept before a save removes them.
// Archiving is what makes this safe to be aggressive about: a finished plan is moved
// aside — losing nothing — long before it ages out, so only genuinely old files are
// actually deleted.
const planRetentionDays = 30

// findPlanFileByID returns this session's plan file carrying planID ("" when there is
// none). It is how a save finds the document to update: the id, not the title, says which
// plan this is.
func findPlanFileByID(dir, sessionID, planID string) string {
	for _, path := range planFilesForSession(dir, sessionID) {
		plan, err := PlanFromFile(path)
		if err != nil {
			continue
		}
		if plan.PlanID == planID {
			return path
		}
	}
	return ""
}

// planFilesForSession lists <dir>/*-<sessionID>.json — exactly the files a save may touch.
// The session-id suffix is the safety property here: nothing outside it is ever archived,
// moved or deleted, so one session's save cannot damage another session's plans (or any
// file that is not a plan).
func planFilesForSession(dir, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*-"+sessionID+".json"))
	if err != nil {
		return nil
	}
	return matches
}

// prunePlans archives this session's OTHER finished plans and deletes its plans older than
// planRetentionDays, skipping keep (the file just written, which is by definition current).
// Returns how many were archived and deleted so the tool result can say what happened.
//
// Every failure is skipped rather than returned: this runs on the happy path of a save, and
// a plan that could not be tidied is far better than a save that reports failure after the
// document was written.
func prunePlans(dir, sessionID, keep string) (archived, deleted int) {
	archiveDir := filepath.Join(dir, planArchiveDirName)
	deadline := time.Now().Add(-planRetentionDays * 24 * time.Hour)

	for _, path := range planFilesForSession(dir, sessionID) {
		if path == keep {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(deadline) {
			if os.Remove(path) == nil {
				deleted++
			}
			continue
		}
		plan, err := PlanFromFile(path)
		if err != nil || !allStepsCompleted(plan.Steps) {
			continue
		}
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			continue
		}
		if os.Rename(path, filepath.Join(archiveDir, filepath.Base(path))) == nil {
			archived++
		}
	}

	// The archive ages out too, or it would only be a slower leak.
	old, _ := filepath.Glob(filepath.Join(archiveDir, "*-"+sessionID+".json"))
	for _, path := range old {
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().Before(deadline) {
			continue
		}
		if os.Remove(path) == nil {
			deleted++
		}
	}
	return archived, deleted
}

// allStepsCompleted reports whether every step is done. A plan with no steps counts as
// finished: there is nothing left to track.
func allStepsCompleted(steps []SavePlanStep) bool {
	for _, s := range steps {
		if s.Status != "completed" {
			return false
		}
	}
	return true
}

// planSlug converts a plan title into a filesystem-safe slug.
// Supports Unicode letters (CJK, Cyrillic, etc.) — they are preserved
// as-is. Spaces and runs of hyphens are collapsed into a single hyphen.
func planSlug(title string) string {
	var sb strings.Builder
	sb.Grow(len(title))

	prevDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			sb.WriteRune(r)
			prevDash = false
		case r == '-' || r == ' ' || r == '\t':
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}

	slug := strings.Trim(sb.String(), "-")
	if slug == "" {
		slug = "plan"
	}
	// Cap by RUNES, not bytes: a byte slice through "面板" (planSlug keeps CJK as-is)
	// leaves half a character behind, and the resulting file name is not valid UTF-8 —
	// the write then fails with "illegal byte sequence".
	return strutil.TruncatePlain(slug, maxPlanSlugRunes)
}

// maxPlanSlugRunes bounds the title slug. It exists so a plan's file name stays a
// readable, navigable length while the title itself can be as long as it likes.
const maxPlanSlugRunes = 48
